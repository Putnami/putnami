package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/githooks"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/machineproc"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/template"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// shutdownTimeout caps how long we wait for graceful cancellation before
// forcing exit. The per-job runner already terminates subprocesses with
// SIGTERM then SIGKILL within its own WaitDelay, so this bound only has
// to outlast the worst single-job teardown plus the scheduler's drain.
const shutdownTimeout = 10 * time.Second

// interactiveShutdownTimeout caps the wait for a run whose interactive child
// runs when the first signal lands: the child has abort.InteractiveStopBudget
// to exit on its own, and the run then has shutdownTimeout to unwind.
const interactiveShutdownTimeout = abort.InteractiveStopBudget + shutdownTimeout

// shutdownTimeoutNow is the bound for a run signaled now.
func shutdownTimeoutNow() time.Duration {
	if abort.InteractiveHeld() {
		return interactiveShutdownTimeout
	}
	return shutdownTimeout
}

// shutdownNotices is where waitForShutdown writes its interrupt notice:
// stderr, replaceable by tests.
var shutdownNotices io.Writer = os.Stderr

// gcPercent is the garbage collector's target for the CLI process: a run of
// an unchanged workspace keeps a few hundred megabytes of plan, keys and
// records live, and Go's default of 100 spent a sixth of the engine's CPU
// collecting them (tooling/cli/doc/12-profiling-and-telemetry.md).
const gcPercent = 200

func main() {
	// The client sends the environment as the process started: the CLI takes
	// variables out of it as it runs (CLI ADR 0116 §4).
	machineproc.SaveProcessContext()
	applyGCTarget(os.Getenv)
	if code, ok := runMachine(os.Args[1:]); ok {
		os.Exit(code)
	}
	code := runMain()
	machineproc.StartIfWanted()
	os.Exit(code)
}

// runMachine runs the machine process's own commands: `__machine supervise`
// and `__machine work`, which a CLI starts, never a person.
func runMachine(args []string) (int, bool) {
	if len(args) != 2 || args[0] != "__machine" {
		return 0, false
	}
	switch args[1] {
	case "supervise":
		return machineproc.Supervise(), true
	case "work":
		// A worker answers an unchanged run from what it holds, through the
		// engine, which skips loading and planning, and the scheduler, which
		// skips the tasks (CLI ADR 0116 §6a); no other process holds anything.
		engine.ServeHeldRuns(machineproc.RunMemory{})
		scheduler.ServeHeldRuns(machineproc.RunMemory{})
		return machineproc.Work(func(ctx context.Context, args []string) int {
			// What a CLI process memoizes for its life, a worker forgets
			// before each run: the run must read what is on disk now.
			workspace.ForgetProcessMemos()
			extension.ForgetProcessMemos()
			lifecycle.ForgetProcessMemos()
			template.ForgetProcessMemos()
			app, err := cli.NewApp()
			if err != nil {
				iox.Fprintf(os.Stderr, "putnami: %v\n", err)
				return cli.ExitError
			}
			return app.Run(ctx, args)
		}), true
	}
	return 0, false
}

// applyGCTarget sets gcPercent unless GOGC is set, which then decides as it
// always does, and returns the percent the process collects at.
func applyGCTarget(getenv func(string) string) int {
	if getenv("GOGC") != "" {
		percent := debug.SetGCPercent(-1)
		debug.SetGCPercent(percent)
		return percent
	}
	debug.SetGCPercent(gcPercent)
	return gcPercent
}

func runMain() int {
	abort.NoteParent()
	// Git hooks are invoked directly by git (not through the workspace command
	// pipeline), so handle them before the App, which expects a workspace.
	//
	// A CLI inside the workspace-fetch of a hosted run refuses every command,
	// the git hooks included; App.Run refuses the others the same way.
	if err := runcredential.RefuseInHostedFetch(os.Getenv); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return cli.ExitUsage
	}
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "install-hooks":
			githooks.Install(selfBinaryPath())
			return 0
		case "commit-msg":
			githooks.CommitMsg(os.Args[2:])
			return 0
		}
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bounded-shutdown signal handler: first SIGINT/SIGTERM cancels
	// the root context and starts a hard-deadline timer; the second signal
	// (or the timer firing) kills an interactive child and calls os.Exit
	// immediately so a wedged child cannot make Ctrl-C feel unresponsive.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	runDone := make(chan struct{})
	go waitForShutdown(runDone, cancel, sigCh, shutdownTimeoutNow, os.Exit, launch.Delegated)

	code := run(rootCtx)
	close(runDone)
	return code
}

// waitForShutdown implements the two-signal shutdown policy. runDone is
// closed by main when run() returns so the handler can distinguish "first
// signal canceled the run and it's now finished" from "first signal
// canceled the run but it's still tearing down". timeout returns the bound
// for a run signaled at the time of the call, read at the first signal. A
// forced exit first kills the interactive child of the run, if one runs
// (abort.KillInteractive): it shares the terminal and would outlive the CLI.
// exitFn is taken as a parameter so tests can verify forced-exit calls
// without actually terminating the test process.
//
// delegated reports whether a relaunched child owns this invocation
// (launch.Delegated). The child receives the same console interrupts and runs
// this policy itself, so the waiting parent neither cancels nor forces an exit:
// it exits with the child's status once the child is gone.
func waitForShutdown(
	runDone <-chan struct{},
	cancel context.CancelFunc,
	sigCh <-chan os.Signal,
	timeout func() time.Duration,
	exitFn func(int),
	delegated func() bool,
) {
	var sig os.Signal
	for sig == nil {
		select {
		case received := <-sigCh:
			if !delegated() {
				sig = received
			}
		case <-runDone:
			return
		}
	}
	// Record the source before canceling: the scheduler reads it when the
	// run unwinds, so the end-of-run summary can say who stopped the build
	// instead of reporting the leftover work as an ordinary skip.
	abort.Record(sig)
	bound := timeout()
	// The notice addresses a user at a terminal. A supervised run's stderr is
	// its supervisor's serve log, and the supervisor reports its own stop.
	if !abort.Supervised() {
		iox.Fprintln(shutdownNotices, "\nputnami: interrupting... press Ctrl-C again to force quit")
	}
	cancel()
	select {
	case <-sigCh:
		if delegated() {
			return
		}
		iox.Fprintln(os.Stderr, "putnami: force quit")
		abort.KillInteractive()
		exitFn(cli.ExitSignalReceived)
	case <-time.After(bound):
		if delegated() {
			return
		}
		iox.Fprintln(os.Stderr, "putnami: shutdown timeout exceeded, forcing exit")
		abort.KillInteractive()
		exitFn(cli.ExitSignalReceived)
	case <-runDone:
		// run() returned cleanly between signals; nothing to force.
	}
}

func run(ctx context.Context) int {
	app, err := cli.NewApp()
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return cli.ExitError
	}
	return app.Run(ctx, os.Args[1:])
}

// selfBinaryPath returns the absolute path of the running putnami binary, used as
// the commit-msg hook's re-invocation target. Falls back to os.Args[0].
func selfBinaryPath() string {
	if p, err := os.Executable(); err == nil && p != "" {
		return p
	}
	return os.Args[0]
}
