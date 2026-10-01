package serve

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/proctree"
)

// The stop helpers are the served programs of these tests. stopHelperEnv
// selects the role of the test binary: "program" is a served program that
// ignores every stop request, writes its pid to the file named by
// stopHelperPIDEnv, and runs until it is killed; "go-run" stands in for
// `go run`: it starts a "program" that inherits its output and exits on
// SIGTERM without passing it on.
const (
	stopHelperEnv    = "PUTNAMI_GO_SERVE_STOP_HELPER"
	stopHelperPIDEnv = "PUTNAMI_GO_SERVE_STOP_HELPER_PID"
)

// TestStopHelperProcess is not a test: it is one of the stop helpers, and it
// returns at once unless a test started it as one.
func TestStopHelperProcess(t *testing.T) {
	switch os.Getenv(stopHelperEnv) {
	case "program":
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
		pidFile := os.Getenv(stopHelperPIDEnv)
		if err := os.WriteFile(pidFile+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(5)
		}
		if err := os.Rename(pidFile+".tmp", pidFile); err != nil {
			os.Exit(5)
		}
		time.Sleep(10 * time.Minute)
		os.Exit(3)
	case "go-run":
		program := exec.Command(os.Args[0], "-test.run=^TestStopHelperProcess$")
		program.Env = append(os.Environ(), stopHelperEnv+"=program")
		program.Stdout = os.Stdout
		program.Stderr = os.Stderr
		if err := program.Run(); err != nil {
			os.Exit(4)
		}
		os.Exit(3)
	}
}

// startStopHelper starts the stop helper role as a watched server and returns
// it once the served program runs, with that program's pid.
func startStopHelper(t *testing.T, role string) (*watchedServer, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "program.pid")
	env := append(os.Environ(), stopHelperEnv+"="+role, stopHelperPIDEnv+"="+pidFile)
	emit := jsonl.NewForVersion(runtimeproto.ProtocolVersion2)
	server, err := startWatchedServer([]string{os.Args[0], "-test.run=^TestStopHelperProcess$"}, t.TempDir(), env, emit)
	if err != nil {
		t.Fatalf("start the served program: %v", err)
	}
	t.Cleanup(func() {
		_ = server.tree.Kill()
		_ = server.tree.Close()
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil {
				t.Fatalf("the program pid %q: %v", data, parseErr)
			}
			t.Cleanup(func() {
				if process, findErr := os.FindProcess(pid); findErr == nil {
					_ = process.Kill()
				}
			})
			return server, pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the served program did not start within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runStop runs server.stop and fails the test when it has not returned within
// bound.
func runStop(t *testing.T, server *watchedServer, grace, bound time.Duration) error {
	t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- server.stop(grace) }()
	select {
	case err := <-stopped:
		return err
	case <-time.After(bound):
		t.Fatalf("the stop did not return within %v", bound)
		return nil
	}
}

// A served program that does not exit after the stop request is killed once
// the grace runs out.
func TestWatchedServerStop_KillsAProgramThatOutlivesTheGrace(t *testing.T) {
	server, _ := startStopHelper(t, "program")
	if err := runStop(t, server, 50*time.Millisecond, 10*time.Second); err == nil {
		t.Fatal("the served program exited cleanly, want it killed")
	}
}

// `go run` exits on the stop request without passing it on, and the program it
// started still holds the output pipes: the stop reaches that program too, so
// it returns within the grace plus the kill instead of waiting on the pipes
// forever, and the program no longer runs.
func TestWatchedServerStop_EndsTheProgramGoRunStarted(t *testing.T) {
	server, pid := startStopHelper(t, "go-run")
	if err := runStop(t, server, 200*time.Millisecond, 10*time.Second); err == nil {
		t.Fatal("go run exited cleanly, want it stopped")
	}
	deadline := time.Now().Add(5 * time.Second)
	for proctree.ProcessAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the program go run started (pid %d) still runs after the stop", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// runnerProcessGroupKillDelay mirrors processGroupKillDelay in
// tooling/cli/internal/jobs/runner.go: the delay between the SIGTERM the CLI
// job runner sends a job's process group and its SIGKILL.
const runnerProcessGroupKillDelay = 5 * time.Second

func TestStopGrace_EndsBeforeTheRunnerKillsTheJob(t *testing.T) {
	if stopGrace >= runnerProcessGroupKillDelay {
		t.Errorf("stopGrace = %v, want less than the runner's %v so the extension reaps the program first", stopGrace, runnerProcessGroupKillDelay)
	}
}
