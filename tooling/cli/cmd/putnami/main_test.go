package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

func notDelegated() bool { return false }

// TestWaitForShutdown_SecondSignalForcesExit verifies the two-signal
// policy: one SIGINT cancels and warns; a second SIGINT calls
// exitFn(130) without waiting for the timeout or for run() to finish.
func TestWaitForShutdown_SecondSignalForcesExit(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	runDone := make(chan struct{})
	var exitedWith int32
	exited := make(chan struct{})

	go waitForShutdown(runDone, cancel, sigCh, time.Minute, func(code int) {
		atomic.StoreInt32(&exitedWith, int32(code))
		close(exited)
	}, notDelegated)

	// First signal: cancels via the handler.
	sigCh <- syscall.SIGINT
	// Give the handler a beat to process the first signal.
	time.Sleep(20 * time.Millisecond)

	// Second signal: must trigger forced exit.
	sigCh <- syscall.SIGINT
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("exitFn should be called after second signal")
	}
	if got := atomic.LoadInt32(&exitedWith); got != cli.ExitSignalReceived {
		t.Errorf("exitFn called with %d, want %d (ExitSignalReceived)", got, cli.ExitSignalReceived)
	}
}

// TestWaitForShutdown_TimeoutForcesExit covers the deadline branch:
// after the first signal, if the bounded timeout elapses before either
// a second signal or run() returning, exitFn fires.
func TestWaitForShutdown_TimeoutForcesExit(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	runDone := make(chan struct{}) // never closed — run() is hung
	var exitedWith int32
	exited := make(chan struct{})

	go waitForShutdown(runDone, cancel, sigCh, 50*time.Millisecond, func(code int) {
		atomic.StoreInt32(&exitedWith, int32(code))
		close(exited)
	}, notDelegated)

	sigCh <- syscall.SIGINT

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("exitFn should be called after timeout elapses")
	}
	if got := atomic.LoadInt32(&exitedWith); got != cli.ExitSignalReceived {
		t.Errorf("exitFn called with %d, want %d", got, cli.ExitSignalReceived)
	}
}

// TestWaitForShutdown_CleanRunReturnNoExit ensures that when run()
// completes between the first signal and the deadline, exitFn is NOT
// called — the process exits naturally via main()'s os.Exit(code).
func TestWaitForShutdown_CleanRunReturnNoExit(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	runDone := make(chan struct{})
	var exitCalled int32
	done := make(chan struct{})

	go func() {
		waitForShutdown(runDone, cancel, sigCh, time.Minute, func(_ int) {
			atomic.StoreInt32(&exitCalled, 1)
		}, notDelegated)
		close(done)
	}()

	sigCh <- syscall.SIGINT
	// Wait briefly for the first signal to be observed by the handler.
	time.Sleep(20 * time.Millisecond)
	// Now simulate run() finishing cleanly after the cancel.
	close(runDone)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForShutdown should return when runDone closes")
	}
	if atomic.LoadInt32(&exitCalled) != 0 {
		t.Error("exitFn should NOT be called on clean run return after first signal")
	}
}

// TestWaitForShutdown_NoSignalsCleanExit covers the common case: no
// signal arrives, run() finishes, the handler observes runDone and
// returns without ever calling exitFn.
func TestWaitForShutdown_NoSignalsCleanExit(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	runDone := make(chan struct{})
	var exitCalled int32
	done := make(chan struct{})

	go func() {
		waitForShutdown(runDone, cancel, sigCh, time.Minute, func(_ int) {
			atomic.StoreInt32(&exitCalled, 1)
		}, notDelegated)
		close(done)
	}()

	// Simulate run() returning by closing runDone.
	close(runDone)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForShutdown should return when runDone closes before any signal")
	}
	if atomic.LoadInt32(&exitCalled) != 0 {
		t.Error("exitFn should NOT be called when no signals were received")
	}
}

// TestWaitForShutdown_DelegatedParentWaitsForTheChild covers the Windows
// relaunch: a relaunched child shares the console and handles every interrupt
// itself, so the waiting parent neither cancels nor forces an exit, however
// many signals arrive and however long the child takes to unwind.
func TestWaitForShutdown_DelegatedParentWaitsForTheChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	runDone := make(chan struct{})
	var exitCalled int32
	done := make(chan struct{})

	go func() {
		waitForShutdown(runDone, cancel, sigCh, 10*time.Millisecond, func(_ int) {
			atomic.StoreInt32(&exitCalled, 1)
		}, func() bool { return true })
		close(done)
	}()

	sigCh <- syscall.SIGINT
	sigCh <- syscall.SIGINT
	time.Sleep(50 * time.Millisecond)
	if ctx.Err() != nil {
		t.Error("a delegated parent canceled the run")
	}
	close(runDone)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForShutdown should return when runDone closes")
	}
	if atomic.LoadInt32(&exitCalled) != 0 {
		t.Error("a delegated parent forced an exit")
	}
}

// Inside the workspace-fetch of a hosted run the CLI refuses
// even the git hooks it handles before the App, with exit 2. Were the hook to
// run, it would accept the message and exit the test binary.
func TestRunMainRefusesInsideAHostedFetch(t *testing.T) {
	t.Setenv(runcredential.HostedFetchEnv, "1")
	message := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	if err := os.WriteFile(message, []byte("fix(cli): a valid message\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	os.Args = []string{"putnami", "commit-msg", message}
	if code := runMain(); code != cli.ExitUsage {
		t.Errorf("runMain() = %d, want %d", code, cli.ExitUsage)
	}
}
