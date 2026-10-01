package serve

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// ---- runExitCode ----

func TestRunExitCode_CleanExit(t *testing.T) {
	if got := runExitCode(nil); got != 0 {
		t.Errorf("clean exit code = %d, want 0", got)
	}
}

func TestRunExitCode_ChildExitCode(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-supervision", "the-child-exit-code-is-forwarded")
	// A real ExitError carries the child's own exit code through.
	cmd := exec.Command("sh", "-c", "exit 7")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	if got := runExitCode(err); got != 7 {
		t.Errorf("child exit code = %d, want 7", got)
	}
}

func TestRunExitCode_UnknownError(t *testing.T) {
	if got := runExitCode(errors.New("pipe broke")); got != 1 {
		t.Errorf("unknown error exit code = %d, want 1", got)
	}
}

// ---- SuperviseRun ----

func TestSuperviseRun_NaturalExit(t *testing.T) {
	// No signal: the supervisor maps the wait result to an exit code.
	doneCh := make(chan error, 1)
	doneCh <- nil
	sigCh := make(chan os.Signal, 1)

	if got := SuperviseRun(nil, sigCh, doneCh, terminateGrace); got != 0 {
		t.Errorf("natural exit code = %d, want 0", got)
	}
}

func TestSuperviseRun_NaturalFailureExit(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	waitErr := cmd.Run()
	doneCh := make(chan error, 1)
	doneCh <- waitErr
	sigCh := make(chan os.Signal, 1)

	if got := SuperviseRun(nil, sigCh, doneCh, terminateGrace); got != 3 {
		t.Errorf("failure exit code = %d, want 3", got)
	}
}

func TestSuperviseRun_SignalTerminatesGroupAndReturns130(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-supervision", "an-interrupt-terminates-the-group-and-reports-130")
	// On signal the supervisor must SIGTERM the process group, wait for the
	// reap, and report 130 regardless of the child's wait error.
	var gotSignals []syscall.Signal
	withMockSignalGroup(t, func(pgid int, sig syscall.Signal) error {
		gotSignals = append(gotSignals, sig)
		return nil
	})

	proc := &os.Process{Pid: 4321}
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)

	sigCh <- syscall.SIGTERM
	go func() {
		time.Sleep(10 * time.Millisecond)
		doneCh <- errors.New("terminated")
	}()

	if got := SuperviseRun(proc, sigCh, doneCh, terminateGrace); got != 130 {
		t.Errorf("signal exit code = %d, want 130", got)
	}
	if len(gotSignals) != 1 || gotSignals[0] != syscall.SIGTERM {
		t.Errorf("signals sent = %v, want [SIGTERM]", gotSignals)
	}
}

func TestSuperviseRun_SignalEscalatesToSigkillAfterGrace(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-supervision", "a-group-that-outlives-the-grace-period-is-sigkilled")
	// A workload ignoring SIGTERM must be SIGKILLed after the grace period
	// instead of wedging `putnami run` indefinitely.
	var gotSignals []syscall.Signal
	killed := make(chan struct{})
	withMockSignalGroup(t, func(pgid int, sig syscall.Signal) error {
		gotSignals = append(gotSignals, sig)
		if sig == syscall.SIGKILL {
			close(killed)
		}
		return nil
	})

	proc := &os.Process{Pid: 4321}
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)

	sigCh <- syscall.SIGTERM
	go func() {
		// Only "exit" once the kill has been delivered.
		<-killed
		doneCh <- errors.New("killed")
	}()

	if got := SuperviseRun(proc, sigCh, doneCh, 10*time.Millisecond); got != 130 {
		t.Errorf("escalated exit code = %d, want 130", got)
	}
	if len(gotSignals) != 2 || gotSignals[0] != syscall.SIGTERM || gotSignals[1] != syscall.SIGKILL {
		t.Errorf("signals sent = %v, want [SIGTERM SIGKILL]", gotSignals)
	}
}
