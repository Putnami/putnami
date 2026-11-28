package machineproc

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// reapRaceHelperEnv runs TestTheReaperNeverTakesTheExitOfAWorker as a
// subreaper of its own: the reaper waits any child of the process it runs in.
const reapRaceHelperEnv = "MACHINEPROC_REAP_RACE_HELPER"

// TestTheReaperNeverTakesTheExitOfAWorker proves that the supervisor's reaper
// leaves a worker's exit to the wait of its own (supervisor.follow), also a
// worker started while the reaper lists the supervisor's children: the set of
// workers is read once they are listed, and a worker is in it from its start.
func TestTheReaperNeverTakesTheExitOfAWorker(t *testing.T) {
	if os.Getenv(reapRaceHelperEnv) == "1" {
		reapRaceHelper(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTheReaperNeverTakesTheExitOfAWorker$", "-test.v")
	cmd.Env = append(os.Environ(), reapRaceHelperEnv+"=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "--- PASS") {
		t.Fatalf("the helper did not run:\n%s", out.String())
	}
}

// exitsWith starts a child that exits with code 3 and waits until it is a
// zombie, its exit left to collect.
func exitsWith(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Skipf("no shell to start: %v", err)
	}
	if !eventually(t, cmd.Process.Pid, func(state byte, exists bool) bool { return exists && state == 'Z' }) {
		t.Fatal("the child did not exit")
	}
	return cmd
}

// exitCode is the exit code cmd's own wait collects, -1 when it collects none.
func exitCode(cmd *exec.Cmd) int {
	var exit *exec.ExitError
	if err := cmd.Wait(); errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func reapRaceHelper(t *testing.T) {
	becomeSubreaper()
	workers := map[int]bool{}
	previous := liveWorkers
	t.Cleanup(func() { liveWorkers = previous })
	liveWorkers = func() map[int]bool { return workers }

	// A worker that exited and that its wait did not collect yet.
	worker := exitsWith(t)
	workers[worker.Process.Pid] = true
	reapOrphans()
	if code := exitCode(worker); code != 3 {
		t.Fatalf("a worker's wait collected exit %d after the reaper ran, want 3", code)
	}

	// A worker started, and exited, while the set of workers is read: the
	// set read leaves it out, as a set read before its start would, and the
	// reaper reads the set once it listed the children.
	var started *exec.Cmd
	liveWorkers = func() map[int]bool {
		read := maps.Clone(workers)
		if started == nil {
			started = exitsWith(t)
			workers[started.Process.Pid] = true
		}
		return read
	}
	reapOrphans()
	if started == nil {
		t.Fatal("the reaper read no set of workers")
	}
	if code := exitCode(started); code != 3 {
		t.Fatalf("a worker started while the reaper listed the children: its wait collected exit %d, want 3", code)
	}

	// An orphan is reaped.
	liveWorkers = func() map[int]bool { return workers }
	orphan := exitsWith(t)
	reapOrphans()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, exists := processState(t, orphan.Process.Pid); !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("an orphan that exited was not reaped")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
