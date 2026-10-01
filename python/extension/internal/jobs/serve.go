package jobs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"go.putnami.dev/python/extension/internal/toolchain"
	"go.putnami.dev/python/extension/internal/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/proctree"
)

// serverStopGrace is how long a watched server is given to exit after it was
// asked to stop before it is killed. A package var so tests can shorten it.
var serverStopGrace = 5 * time.Second

// Serve runs a Python application with optional hot-reload.
func Serve(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if ctx.Project.Name == "" {
		return "SKIP", nil, nil
	}

	flags := cli.ParseFlags(args)
	entrypoint := cli.FlagString(flags, "entrypoint", ctx.Params.String("entrypoint"))
	if entrypoint == "" {
		entrypoint = "src/main.py"
	}
	port := cli.FlagInt(flags, "port", ctx.Params.Int("port", 3000))

	// Handle --watch and -w flags
	watch := cli.FlagBool(flags, "watch", ctx.Params.Bool("watch", true))
	if cli.FlagBool(flags, "w", false) {
		watch = true
	}

	wsRoot := ctx.WorkspaceRoot
	projectRoot := ctx.Project.FullPath

	if !SyncWorkspace(wsRoot, emit) {
		return "FAILED", nil, nil
	}

	packageName := ResolvePackageName(ctx)
	cmdArgs := toolchain.UVRunArgs(packageName, wsRoot, entrypoint)
	env := MakeEnv(wsRoot, projectRoot, map[string]string{"PORT": fmt.Sprintf("%d", port)})

	// Signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	emit.PhaseStart("serve")

	if !watch {
		exitCode := runServer(emit, cmdArgs, projectRoot, env, sigChan)
		if exitCode == 0 || (exitCode >= 128 && exitCode <= 143) {
			emit.PhaseEnd("serve", "success")
			return "OK", nil, nil
		}
		emit.PhaseEnd("serve", "failed")
		return "FAILED", nil, nil
	}

	// Watch mode
	snapshot, _ := workspace.FileSnapshot(projectRoot)
	proc, err := spawnServer(emit, cmdArgs, projectRoot, env)
	if err != nil {
		emit.PhaseEnd("serve", "failed")
		emit.Diagnostic("error", fmt.Sprintf("Failed to start server: %v", err), "", 0)
		return "FAILED", nil, nil
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigChan:
			stopProcess(proc, proctree.Relay)
			emit.PhaseEnd("serve", "success")
			return "OK", nil, nil

		case <-proc.done:
			// Process exited on its own; waitErr is set before done closes.
			if proc.waitErr == nil {
				emit.PhaseEnd("serve", "success")
				return "OK", nil, nil
			}
			emit.PhaseEnd("serve", "failed")
			return "FAILED", nil, nil

		case <-ticker.C:
			// Check for file changes
			current, _ := workspace.FileSnapshot(projectRoot)
			if snapshotsEqual(snapshot, current) {
				continue
			}

			snapshot = current
			emit.Log("info", "Restarting server due to source change")
			stopProcess(proc, terminateServer)

			// Check if we got a signal during stop
			select {
			case <-sigChan:
				emit.PhaseEnd("serve", "success")
				return "OK", nil, nil
			default:
			}

			proc, err = spawnServer(emit, cmdArgs, projectRoot, env)
			if err != nil {
				emit.PhaseEnd("serve", "failed")
				emit.Diagnostic("error", fmt.Sprintf("Failed to restart server: %v", err), "", 0)
				return "FAILED", nil, nil
			}
		}
	}
}

// serverProcess wraps a running server with its pipe goroutines.
//
// done is closed once the process has exited and waitErr has been set, so it
// acts as a broadcast: any goroutine can observe exit via the channel without
// racing on exec.Cmd.ProcessState (which cmd.Wait writes from its own
// goroutine). waitErr is written before done is closed, so reading it after a
// receive on done is safe.
type serverProcess struct {
	cmd *exec.Cmd
	// tree is the process tree the server roots on Windows (see startServer),
	// and nil on every other OS.
	tree    *proctree.Tree
	done    chan struct{}
	waitErr error
}

func (p *serverProcess) wait() {
	<-p.done
}

// exited reports whether the process has finished, without blocking and
// without touching cmd.ProcessState (which would race with cmd.Wait).
func (p *serverProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func spawnServer(emit *jsonl.Emitter, cmdArgs []string, cwd string, env []string) (*serverProcess, error) {
	return spawnServerOn(runtime.GOOS, emit, cmdArgs, cwd, env)
}

// spawnServerOn is spawnServer for a server started the way it is on goos.
func spawnServerOn(goos string, emit *jsonl.Emitter, cmdArgs []string, cwd string, env []string) (*serverProcess, error) {
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = cwd
	cmd.Env = env

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	tree, err := startServer(cmd, goos)
	if err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go jsonl.ForwardPipe(emit, stdoutPipe, "info", &wg)
	go jsonl.ForwardPipe(emit, stderrPipe, "warn", &wg)

	proc := &serverProcess{cmd: cmd, tree: tree, done: make(chan struct{})}
	go func() {
		wg.Wait()
		proc.waitErr = cmd.Wait()
		_ = proc.tree.Close()
		close(proc.done)
	}()

	return proc, nil
}

// startServer starts cmd, the `uv run` that starts python. On Windows cmd
// roots a process tree of its own and startServer returns it: Windows cannot
// ask a single process to stop, and killing uv alone would leave python
// running and holding the port. On every other OS cmd starts as a plain child
// that receives the stop requests this process relays, and the tree is nil.
func startServer(cmd *exec.Cmd, goos string) (*proctree.Tree, error) {
	if goos != "windows" {
		return nil, cmd.Start()
	}
	tree := proctree.New(cmd)
	if err := tree.Start(); err != nil {
		return nil, err
	}
	return tree, nil
}

// runServer runs a server without watch mode, blocking until exit or signal.
func runServer(emit *jsonl.Emitter, cmdArgs []string, cwd string, env []string, sigChan <-chan os.Signal) int {
	proc, err := spawnServer(emit, cmdArgs, cwd, env)
	if err != nil {
		emit.Diagnostic("error", fmt.Sprintf("Failed to start server: %v", err), "", 0)
		return 1
	}

	select {
	case <-sigChan:
		awaitStop(proc, proctree.Relay)
		return 0
	case <-proc.done:
		err := proc.waitErr
		if err == nil {
			return 0
		}
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	}
}

// stopProcess asks the server to exit through ask and returns once it has
// exited. A server still running serverStopGrace after the request is killed.
func stopProcess(proc *serverProcess, ask func(*os.Process) error) {
	stopProcessWithin(proc, serverStopGrace, ask)
}

// stopProcessWithin is stopProcess with the grace as a parameter. A server that
// could not be asked is killed at once: waiting on a request that was never
// delivered would be bounded by nothing. A server that roots a tree is asked
// and killed as a whole tree instead, whatever ask is: it runs in a console
// process group of its own, which no stop request this process receives
// reaches.
func stopProcessWithin(proc *serverProcess, grace time.Duration, ask func(*os.Process) error) {
	process := proc.cmd.Process
	if process == nil || proc.exited() {
		return
	}
	if proc.tree != nil {
		proctree.Stop(proc.done, grace, proc.tree.Terminate, proc.tree.Kill)
		return
	}
	proctree.Stop(proc.done, grace, func() error { return ask(process) }, process.Kill)
}

// awaitStop asks the server to exit through ask and returns once it has
// exited, however long its shutdown takes: the job that runs this extension
// bounds it, not this stop. A server that could not be asked is killed at
// once, and a server that roots a tree is asked and killed as a whole tree, as
// in stopProcessWithin.
func awaitStop(proc *serverProcess, ask func(*os.Process) error) {
	process := proc.cmd.Process
	terminate, kill := func() error { return ask(process) }, process.Kill
	if proc.tree != nil {
		terminate, kill = proc.tree.Terminate, proc.tree.Kill
	}
	if err := terminate(); err != nil {
		_ = kill()
	}
	<-proc.done
}

// terminateServer asks process to exit before a restart that the extension
// decided itself, when no stop request reached the server: it sends SIGTERM.
// It is not used on Windows, where the server roots a tree and
// stopProcessWithin asks the tree instead.
func terminateServer(process *os.Process) error { return process.Signal(syscall.SIGTERM) }

func snapshotsEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
