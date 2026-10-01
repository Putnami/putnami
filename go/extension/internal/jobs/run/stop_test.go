package run

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

const stopHelperEnv = "PUTNAMI_GO_RUN_STOP_HELPER"

// TestStopHelperProcess is not a test: it is a workload that runs until it is
// killed, and it returns at once unless a test started it as one.
func TestStopHelperProcess(t *testing.T) {
	if os.Getenv(stopHelperEnv) != "1" {
		return
	}
	time.Sleep(10 * time.Minute)
	os.Exit(3)
}

// startStopHelper starts a workload that runs until it is killed, and returns
// it with the channel its wait error arrives on.
func startStopHelper(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStopHelperProcess$")
	cmd.Env = append(os.Environ(), stopHelperEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the workload: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, done
}

// runStop runs stopWorkload and fails the test when it has not returned
// within bound.
func runStop(t *testing.T, cmd *exec.Cmd, done <-chan error, grace, bound time.Duration, ask func(*os.Process) error) error {
	t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- stopWorkload(cmd.Process, done, grace, ask) }()
	select {
	case err := <-stopped:
		return err
	case <-time.After(bound):
		t.Fatalf("the stop did not return within %v", bound)
		return nil
	}
}

// A request that could not be delivered, as a signal to a single process on
// Windows, is followed by a kill at once instead of a wait for the grace.
func TestStopWorkload_KillsAtOnceWhenTheRequestFails(t *testing.T) {
	cmd, done := startStopHelper(t)
	err := runStop(t, cmd, done, time.Hour, 10*time.Second, func(*os.Process) error {
		return errors.New("signal not delivered")
	})
	if err == nil {
		t.Fatal("the workload exited cleanly, want it killed")
	}
}

// A workload that does not exit after a delivered request is killed once the
// grace runs out.
func TestStopWorkload_KillsAWorkloadThatOutlivesTheGrace(t *testing.T) {
	cmd, done := startStopHelper(t)
	err := runStop(t, cmd, done, 50*time.Millisecond, 10*time.Second, func(*os.Process) error { return nil })
	if err == nil {
		t.Fatal("the workload exited cleanly, want it killed")
	}
}

// runnerProcessGroupKillDelay mirrors processGroupKillDelay in
// tooling/cli/internal/jobs/runner.go: the delay between the SIGTERM the CLI
// job runner sends a job's process group and its SIGKILL.
const runnerProcessGroupKillDelay = 5 * time.Second

func TestStopGrace_EndsBeforeTheRunnerKillsTheJob(t *testing.T) {
	if stopGrace >= runnerProcessGroupKillDelay {
		t.Errorf("stopGrace = %v, want less than the runner's %v so the extension reaps the workload first", stopGrace, runnerProcessGroupKillDelay)
	}
}
