// Package serve runs Go applications in development or production mode.
// Emits JSONL events compatible with the Putnami orchestrator.
//
// Serve is BUILD-AND-EXEC. Both modes compile the
// resolved entrypoint for the host and run it; neither reads, nor asks any
// other task to produce, a persisted binary. The `serve` pipeline therefore
// carries no compile step: it used to schedule build-compile, emit a binary
// tree into the command's output directory, and then ignore it — `go run`
// compiled the program a second time — which is one whole compile of every
// served project spent on an artifact nothing consumed.
package serve

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.putnami.dev/go/extension/internal/platform"
	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/proctree"
)

// stopGrace is how long the served program is given to exit after it was
// asked to stop before it is killed. It ends before the CLI job runner's own
// deadline, five seconds from the SIGTERM it sends the job's process group to
// the SIGKILL that follows (processGroupKillDelay in
// tooling/cli/internal/jobs/runner.go), so the extension reaps the program
// itself.
const stopGrace = 3 * time.Second

// Run executes the serve job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	entrypoint := ctx.Params.String("entrypoint")
	watch := ctx.Params.Bool("watch", true, "w")
	killPort := ctx.Params.Bool("kill-port", false)
	port := ctx.Params.String("port")
	race := ctx.Params.Bool("race", false)
	extraArgs := ctx.Params.String("args")

	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "FAILED", nil, err
	}

	emit.PhaseStart("setup")

	// Resolve port
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		port = "8080"
	}
	os.Setenv("PORT", port)

	emit.PhaseEnd("setup", "success")

	// Kill port if requested
	if killPort {
		emit.PhaseStart("kill-port")
		if err := killPortUnavailable(runtime.GOOS); err != nil {
			emit.Diagnostic("warning", err.Error(), "", 0)
			emit.PhaseEnd("kill-port", "skipped")
		} else if killProcessOnPort(port) {
			emit.Log("info", "Killed process(es) on port "+port)
			emit.PhaseEnd("kill-port", "success")
		} else {
			emit.PhaseEnd("kill-port", "skipped")
		}
	}

	// Determine run mode
	isProd := os.Getenv("NODE_ENV") == "production"

	// Parse extra args
	var binaryArgs []string
	if extraArgs != "" {
		binaryArgs = strings.Fields(extraArgs)
	}

	// Resolve the entrypoint once: both modes run the SAME package, they only
	// differ in how they get from source to a running process.
	ep, err := platform.ResolveServeEntrypoint(ctx.Project.FullPath, ctx.Project.Name, entrypoint)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}

	// Build command
	var cmdArgs []string
	if isProd {
		// Production: compile the entrypoint for the host into a scratch
		// directory and exec the result. Nothing is emitted into the project's
		// .gen tree or the command's output directory — a served process is not
		// an artifact, and the binary is deleted when serve returns.
		//
		// The earlier path instead looked for a binary a previous `putnami
		// build` had left in the build command's output tree. That coupled
		// `serve` to another command's artifact layout (a coupling that broke
		// the moment `build` started writing per-platform subdirectories), and
		// it failed with "no binaries found" rather than simply building what it
		// needed.
		binDir, tempErr := os.MkdirTemp("", "putnami-serve-")
		if tempErr != nil {
			emit.Diagnostic("error", "stage serve binary: "+tempErr.Error(), "", 0)
			return "FAILED", nil, nil
		}
		defer os.RemoveAll(binDir)
		binary := filepath.Join(binDir, platform.HostBinaryName(runtime.GOOS, ep, ctx.Project.Name))

		emit.PhaseStart("build")
		if !buildServeBinary(goBinary, binary, ep, race, ctx.Project.FullPath, emit) {
			emit.PhaseEnd("build", "failed")
			return "FAILED", nil, nil
		}
		emit.PhaseEnd("build", "success")

		cmdArgs = append([]string{binary}, binaryArgs...)
	} else {
		// Development: `go run` is build-and-exec in one step, and its rebuild
		// on every watch restart is what makes hot reload work.
		cmdArgs = []string{goBinary, "run"}
		if race {
			cmdArgs = append(cmdArgs, "-race")
		}
		cmdArgs = append(cmdArgs, ep)
		cmdArgs = append(cmdArgs, binaryArgs...)
	}

	// Execute
	emit.PhaseStart("serve")
	emit.Log("debug", "Starting "+ctx.Project.Name+" on port "+port+"...")

	runEnv := serveRunEnv(isProd, ctx.Project.FullPath, goBinary)

	exitCode := 0
	if !isProd && watch {
		emit.Log("debug", "Watch mode enabled")
		exitCode = runWithWatch(cmdArgs, ctx.Project.FullPath, runEnv, emit)
	} else {
		exitCode = runCommand(cmdArgs, ctx.Project.FullPath, runEnv, emit)
	}

	// Serve processes are long-running. Signal exits (128-143) are normal shutdown.
	if exitCode == 0 || (exitCode >= 128 && exitCode <= 143) {
		emit.PhaseEnd("serve", "success")
		return "OK", nil, nil
	}

	emit.PhaseEnd("serve", "failed")
	return "FAILED", nil, nil
}

// serveRunEnv chooses the environment for the served process. The dev `go run`
// path resolves modules, so GOWORK is re-pointed at the governing go.work to
// defend against a leaked GOWORK=off (from a standalone CLI build) that would
// otherwise disable workspace resolution and 404-storm the proxy with framework
// v0.0.0 placeholders. The production path execs an already-compiled
// binary and must inherit the user's environment verbatim — rewriting GOWORK
// there would strip a value the server (or Go tooling it spawns) relies on — so
// it returns nil, leaving cmd.Env unset to inherit the parent process's
// environment. The compile that produced that binary is workspace-scoped on its
// own (see buildServeBinary); the boundary is between BUILDING and RUNNING, not
// between dev and prod.
func serveRunEnv(isProd bool, projectDir, goBinary string) []string {
	if isProd {
		return nil
	}
	return toolchain.WorkspaceBuildEnv(os.Environ(), projectDir, goBinary)
}

// buildServeBinary compiles the entrypoint for the host into binPath.
// Diagnostics are streamed; it returns false on failure.
//
// The build resolves modules, so GOWORK is re-pointed at the governing go.work:
// an inherited GOWORK=off (leaked from a parent that built standalone) would
// otherwise disable workspace resolution and send framework v0.0.0 placeholders
// to the proxy as doomed 404s. The RUN step keeps inheriting the user's
// environment verbatim (see serveRunEnv) — only the compile is workspace-scoped.
func buildServeBinary(goBinary, binPath, entrypoint string, race bool, dir string, emit *jsonl.Emitter) bool {
	buildArgs := []string{"build", "-o", binPath}
	if race {
		buildArgs = append(buildArgs, "-race")
	}
	buildArgs = append(buildArgs, entrypoint)

	cmd := exec.Command(goBinary, buildArgs...)
	cmd.Dir = dir
	cmd.Env = toolchain.WorkspaceBuildEnv(os.Environ(), dir, goBinary)

	output, err := cmd.CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
			emit.Diagnostic("error", trimmed, "", 0)
		} else {
			emit.Diagnostic("error", "build failed: "+err.Error(), "", 0)
		}
		return false
	}
	return true
}

func runCommand(cmdArgs []string, dir string, env []string, emit *jsonl.Emitter) int {
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = dir
	// env is the workspace-resolved environment for the dev `go run` path and nil
	// for the prod pre-built binary; nil leaves cmd.Env unset so the child
	// inherits the parent environment verbatim (see serveRunEnv).
	cmd.Env = env
	cmd.Stdin = os.Stdin

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return 1
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return 1
	}

	if err := cmd.Start(); err != nil {
		return 1
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go jsonl.ForwardPipe(emit, stdoutPipe, "info", &wg)
	go jsonl.ForwardPipe(emit, stderrPipe, "warn", &wg)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

func runWithWatch(cmdArgs []string, projectPath string, env []string, emit *jsonl.Emitter) int {
	snapshot := computeWatchSnapshot(projectPath)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	for {
		// Watch mode is dev-only, so env is always the workspace-resolved
		// environment (GOWORK re-pointed at the governing go.work).
		server, err := startWatchedServer(cmdArgs, projectPath, env, emit)
		if err != nil {
			return 1
		}

		restart := false
		ticker := time.NewTicker(time.Second)

	loop:
		for {
			select {
			case <-sigChan:
				_ = server.stop(stopGrace)
				return 0
			case err := <-server.done:
				if err != nil {
					var exitErr *exec.ExitError
					if errors.As(err, &exitErr) {
						return exitErr.ExitCode()
					}
					return 1
				}
				break loop
			case <-ticker.C:
				current := computeWatchSnapshot(projectPath)
				if current != snapshot {
					snapshot = current
					restart = true
					_ = server.stop(stopGrace)
					break loop
				}
			}
		}

		ticker.Stop()
		if !restart {
			return 0
		}
	}
}

// watchedServer is one run of the served program in watch mode: `go run` and
// the program it compiled, started as one process tree. `go run` does not pass
// a stop request on to that program, which inherits its output pipes, so only
// a stop that reaches the whole tree ends both and closes the pipes.
type watchedServer struct {
	tree *proctree.Tree
	// done delivers the wait error of `go run` once every process holding
	// its output pipes has exited; the tree is closed by then.
	done chan error
}

// startWatchedServer starts cmdArgs in dir with env as the root of a new
// process tree and forwards its output to emit.
func startWatchedServer(cmdArgs []string, dir string, env []string, emit *jsonl.Emitter) (*watchedServer, error) {
	//nolint:gosec // G702: cmdArgs is the resolved go command running the project's own entry point, as runCommand runs it; argv-form exec, no shell.
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = dir
	cmd.Env = env

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	tree := proctree.New(cmd)
	if err := tree.Start(); err != nil {
		return nil, err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go jsonl.ForwardPipe(emit, stdoutPipe, "info", &wg)
	go jsonl.ForwardPipe(emit, stderrPipe, "warn", &wg)

	done := make(chan error, 1)
	go func() {
		wg.Wait()
		err := cmd.Wait()
		_ = tree.Close()
		done <- err
	}()
	return &watchedServer{tree: tree, done: done}, nil
}

// stop asks every process of the tree to exit, kills the tree when it still
// runs grace later, and returns the wait error from done. The tree runs in a
// process group of its own, which no stop request this process receives
// reaches, so the extension asks the tree itself for both a stop it received
// and a restart it decided.
func (s *watchedServer) stop(grace time.Duration) error {
	return proctree.Stop(s.done, grace, s.tree.Terminate, s.tree.Kill)
}

func computeWatchSnapshot(root string) string {
	var entries []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == ".putnami" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(name)
		if ext == ".go" || name == "go.mod" || name == "go.sum" || name == "go.work" {
			info, infoErr := d.Info()
			if infoErr == nil {
				entries = append(entries, fmt.Sprintf("%s:%d", path, info.ModTime().Unix()))
			}
		}
		return nil
	})

	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// killPortUnavailable reports why --kill-port cannot free a port on goos, or
// returns nil when it can. killProcessOnPort relies on lsof, fuser and kill,
// which Windows does not have.
func killPortUnavailable(goos string) error {
	if goos == "windows" {
		return errors.New("kill-port is not available on Windows: free the port yourself " +
			"(netstat -ano lists the process id, taskkill /PID <pid> /F ends it)")
	}
	return nil
}

func killProcessOnPort(port string) bool {
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return false
	}
	port = strconv.Itoa(portNumber)

	// Try lsof first
	if lsof, err := exec.LookPath("lsof"); err == nil {
		// lsof is resolved by LookPath and port is a validated decimal TCP port.
		out, err := exec.Command(lsof, "-ti", ":"+port).Output() //nolint:gosec
		if err == nil && len(out) > 0 {
			pids := strings.Fields(strings.TrimSpace(string(out)))
			for _, pid := range pids {
				pidNumber, err := strconv.Atoi(pid)
				if err != nil || pidNumber < 1 {
					continue
				}
				// PID is normalized from lsof's decimal output; the kill is best-effort.
				exec.Command("kill", "-9", strconv.Itoa(pidNumber)).Run() //nolint:errcheck,gosec
			}
			return true
		}
	}

	// Try fuser
	if fuser, err := exec.LookPath("fuser"); err == nil {
		// fuser is resolved by LookPath and port is a validated decimal TCP port.
		exec.Command(fuser, "-k", port+"/tcp").Run() //nolint:errcheck,gosec
		return true
	}

	return false
}
