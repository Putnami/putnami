// Package serve implements the serve job (dev server with hot-reload).
package serve

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pexec "go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/project"
)

// execRunFunc is the function used to run subprocesses. Defaults to pexec.Run.
var execRunFunc = pexec.Run

// terminateGrace is how long the served bun process is given to exit after
// SIGTERM before it is force-killed with SIGKILL. It ends before the CLI job
// runner's own deadline, five seconds from the SIGTERM it sends the job's
// process group to the SIGKILL that follows (processGroupKillDelay in
// tooling/cli/internal/jobs/runner.go), so the extension still escalates and
// reaps bun itself before the runner kills the whole group.
const terminateGrace = 3 * time.Second

// signalGroup asks the process tree pgid to exit on SIGTERM and ends it on
// SIGKILL. The `run` workload is started as the root of its own process tree
// (proctree), so the stop reaches it and every server/worker it spawned —
// preventing orphaned grandchildren that would otherwise keep ports bound after
// Ctrl-C or a timeout. It is a package var so tests can observe the signals
// without affecting the test process.
var signalGroup = func(pgid int, sig syscall.Signal) error {
	if sig == syscall.SIGKILL {
		return proctree.KillGroup(pgid)
	}
	return proctree.TerminateGroup(pgid)
}

// signalProcess sends sig to the process pid alone. The served bun process
// shares the extension's process group, which its launcher signals as a whole,
// so the extension forwards a signal to bun only and never to a group that may
// be its launcher's. A package var so tests can observe the signals.
var signalProcess = sendSignal

// Params holds serve job parameters.
type Params struct {
	Entrypoint  string
	Watch       bool
	KillPort    bool
	Port        int
	Inspect     bool
	InspectWait bool
	InspectBrk  bool
	Debug       bool
}

// ResolveEntrypoint determines the serve entrypoint.
// If not explicitly provided, resolves ./serve from package.json exports.
func ResolveEntrypoint(projectPath string, entrypoint string) (string, error) {
	if entrypoint != "" {
		return entrypoint, nil
	}

	pkgPath := project.ReadPackageJSONSafe(projectPath + "/package.json")
	if pkgPath == nil {
		return "", errs.New(errs.CodeNoServeExport, "no package.json found").WithCategory(errs.CategoryUser)
	}

	serveExport := project.ResolveExportPath(pkgPath.Exports, "./serve")
	if serveExport == "" {
		return "", errs.New(errs.CodeNoServeExport, "no ./serve export found in package.json").WithCategory(errs.CategoryUser)
	}
	return serveExport, nil
}

// ResolvePort determines the port.
// Priority: explicit param > PORT env > default 3000.
func ResolvePort(paramPort int) int {
	if paramPort > 0 {
		return paramPort
	}
	if envPort := os.Getenv("PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			return p
		}
	}
	return 3000
}

// KillPortUnavailable reports why --kill-port cannot free a port on goos, or
// returns nil when it can. KillProcessOnPort relies on fuser, lsof and kill,
// which Windows does not have.
func KillPortUnavailable(goos string) error {
	if goos == "windows" {
		return errors.New("kill-port is not available on Windows: free the port yourself " +
			"(netstat -ano lists the process id, taskkill /PID <pid> /F ends it)")
	}
	return nil
}

// KillProcessOnPort attempts to kill any process using the given port.
// Returns true only when a process was actually killed. Check
// KillPortUnavailable first.
func KillProcessOnPort(port int) bool {
	// Prefer fuser (Linux): it finds and kills the listener in one step.
	if result, err := execRunFunc("fuser", []string{"-k", fmt.Sprintf("%d/tcp", port)}); err == nil && result.Success {
		return true
	}

	// Fallback (e.g. macOS, where fuser is unavailable): lsof lists the PIDs
	// listening on the port; kill each one explicitly.
	lsof, err := execRunFunc("lsof", []string{"-ti", fmt.Sprintf(":%d", port), "-sTCP:LISTEN"})
	if err != nil || lsof == nil {
		return false
	}

	killed := false
	for _, line := range strings.Split(strings.TrimSpace(lsof.Stdout), "\n") {
		pid := strings.TrimSpace(line)
		if pid == "" {
			continue
		}
		if res, err := execRunFunc("kill", []string{"-9", pid}); err == nil && res.Success {
			killed = true
		}
	}
	return killed
}

// buildServeArgs builds the bun run args for the serve command.
func buildServeArgs(entrypoint string, port int, params Params, isProd bool) []string {
	// --no-orphans (Bun 1.4): the workload and every descendant it spawns die
	// with the serve process, so a killed dev server never leaks port holders.
	args := []string{"run", "--no-orphans", fmt.Sprintf("--port=%d", port)}
	if isProd {
		args = append(args, "--no-install", "--smol")
	} else {
		if params.Watch {
			args = append(args, "--watch")
		}
		if params.InspectWait || params.Inspect {
			args = append(args, "--inspect-wait")
		}
		if params.InspectBrk {
			args = append(args, "--inspect-brk")
		}
	}
	args = append(args, entrypoint)
	return args
}

// buildServeEnv builds the environment variables for the serve command.
func buildServeEnv(params Params, isProd bool) []string {
	env := os.Environ()
	if !isProd {
		if os.Getenv("NODE_ENV") == "" {
			env = append(env, "NODE_ENV=development")
		}
	}
	if params.Port > 0 {
		env = append(env, "PORT="+strconv.Itoa(params.Port))
	}
	if params.Debug {
		env = append(env, "LOG_LEVEL=debug")
	}
	return env
}

// RunBunServe starts the bun serve process and forwards its logs as JSONL events.
func RunBunServe(emit *jsonl.Emitter, bunBin, projectPath, entrypoint string, port int, params Params) (bool, error) {
	isProd := os.Getenv("NODE_ENV") == "production" || os.Getenv("ENV") == "production"
	args := buildServeArgs(entrypoint, port, params, isProd)
	env := buildServeEnv(params, isProd)
	// Resolve symlinks so PUTNAMI_PROJECT_ROOT matches Bun's resolved paths
	// (e.g., macOS /tmp → /private/tmp). Without this, relativePath() between
	// projectRoot and stack-trace file paths produces wrong results.
	resolvedProjectPath, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		resolvedProjectPath = projectPath
	}
	env = append(env, "PUTNAMI_PROJECT_ROOT="+resolvedProjectPath)

	// Set up signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	cmd := serveCommand(bunBin, args, projectPath, env)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return false, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("starting bun: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go jsonl.ForwardPipe(emit, stdoutPipe, "info", &wg)
	go jsonl.ForwardPipe(emit, stderrPipe, "warn", &wg)

	doneCh := make(chan error, 1)
	go func() {
		wg.Wait()
		doneCh <- cmd.Wait()
	}()

	waitErr, stopped := superviseServe(cmd.Process, sigCh, doneCh, terminateGrace)
	return classifyServeExit(waitErr, stopped)
}

// serveCommand is the bun process a serve job supervises. It stays in the
// extension's process group: the job's own group, which the CLI job runner
// starts with Setpgid and a composition records for orphan recovery. Every
// signal the runner or a reaper sends that group reaches bun and whatever bun
// spawned, and no group the supervisor never recorded can outlive the job.
func serveCommand(bunBin string, args []string, dir string, env []string) *exec.Cmd {
	cmd := exec.Command(bunBin, args...) //nolint:gosec // args are constructed programmatically, not from user input
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = os.Stdin
	return cmd
}

// superviseServe waits for the served process to exit on its own, or for a
// termination signal. On signal it forwards SIGTERM to bun, escalating to
// SIGKILL after grace, then returns the process's eventual wait error. Bun's
// descendants share the job's process group, which the launcher signals.
// stopped reports that the exit followed a termination signal.
//
// It is decoupled from RunBunServe (taking sigCh/doneCh as parameters) so it
// can be unit-tested without delivering real OS signals to the test process.
func superviseServe(proc *os.Process, sigCh <-chan os.Signal, doneCh <-chan error, grace time.Duration) (waitErr error, stopped bool) {
	select {
	case <-sigCh:
		return terminate(proc, doneCh, grace, signalProcess), true
	case waitErr := <-doneCh:
		return waitErr, false
	}
}

// SuperviseRun supervises a single-shot `run` workload: on a termination
// signal it terminates the process group (SIGTERM, escalating to SIGKILL
// after grace) and returns 130, the conventional SIGINT exit code; on
// natural exit it maps the wait error to the child's exit code.
func SuperviseRun(proc *os.Process, sigCh <-chan os.Signal, doneCh <-chan error, grace time.Duration) int {
	select {
	case <-sigCh:
		_ = terminateProcessGroup(proc, doneCh, grace)
		return 130
	case waitErr := <-doneCh:
		return runExitCode(waitErr)
	}
}

// runExitCode maps a process wait error to the run verb's exit-code
// contract: 0 on clean exit, the child's own code when it exited
// non-zero, 1 for any other failure.
func runExitCode(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

// terminateProcessGroup signals the process group led by proc with SIGTERM and,
// if it has not exited within grace, escalates to SIGKILL. It then waits for
// the process to actually exit (via doneCh, written by the caller's Wait
// goroutine) and returns that wait error, so the whole tree is reaped before
// returning. doneCh is consumed exactly once.
func terminateProcessGroup(proc *os.Process, doneCh <-chan error, grace time.Duration) error {
	return terminate(proc, doneCh, grace, signalGroup)
}

// terminate sends SIGTERM through send to proc's pid, SIGKILL when proc has
// not exited within grace, and returns proc's wait error once it arrives on
// doneCh. A SIGTERM that could not be sent is followed by SIGKILL at once:
// waiting on a request that was never delivered would be bounded by nothing.
// A process that is already gone is not a failure to send (see sendSignal
// and proctree.TerminateGroup).
func terminate(proc *os.Process, doneCh <-chan error, grace time.Duration, send func(int, syscall.Signal) error) error {
	if proc == nil {
		return <-doneCh
	}
	pgid := proc.Pid
	return proctree.Stop(doneCh, grace,
		func() error { return send(pgid, syscall.SIGTERM) },
		func() error { return send(pgid, syscall.SIGKILL) })
}

// classifyServeExit maps a process wait error to the (success, error) contract
// of RunBunServe. A clean exit and signal-induced exits (128+signal, e.g. from
// Ctrl-C terminating a long-running dev server) are treated as success.
// stopped reports that the exit followed a stop this extension relayed.
func classifyServeExit(waitErr error, stopped bool) (bool, error) {
	return classifyServeExitOn(runtime.GOOS, waitErr, stopped)
}

// classifyServeExitOn is classifyServeExit on goos. On Windows a stopped bun
// is a success whatever its exit code: the stop reaches it as a console
// control event, or ends it through TerminateProcess after the grace, and
// neither leaves a signal in the exit status. The code (STATUS_CONTROL_C_EXIT,
// 1 after a kill, or what bun's own handler chose) cannot tell that stop from
// a failure, so the stop decides.
func classifyServeExitOn(goos string, waitErr error, stopped bool) (bool, error) {
	if waitErr == nil {
		return true, nil
	}

	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false, waitErr
	}
	if stopped && goos == "windows" {
		return true, nil
	}

	exitCode := exitErr.ExitCode()
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return true, nil
	}
	isSignalExit := exitCode >= 128 && exitCode <= 143
	return exitCode == 0 || isSignalExit, nil
}
