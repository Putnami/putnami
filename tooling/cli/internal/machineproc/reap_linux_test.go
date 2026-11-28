package machineproc

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// reapHelperEnv runs TestSupervisorReapsAnOrphanAsSoonAsItExits as the
// subreaper itself: a test binary that reaped every orphan would take the exit
// status of the package's other tests' children.
const reapHelperEnv = "MACHINEPROC_REAP_HELPER"

// TestSupervisorReapsAnOrphanAsSoonAsItExits proves that an orphan a run left
// is reaped when it exits, not at the next idle check: a zombie still answers
// kill(pid, 0) as if it were alive, so a process watching it would wait.
func TestSupervisorReapsAnOrphanAsSoonAsItExits(t *testing.T) {
	if os.Getenv(reapHelperEnv) == "1" {
		reapHelper(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorReapsAnOrphanAsSoonAsItExits$", "-test.v")
	cmd.Env = append(os.Environ(), reapHelperEnv+"=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "--- PASS") {
		t.Fatalf("the helper did not run:\n%s", out.String())
	}
}

func reapHelper(t *testing.T) {
	becomeSubreaper()
	// A shell starts an orphan that outlives it by half a second, and ends:
	// the orphan is reparented to this process.
	shell := exec.Command("sh", "-c", "sleep 0.5 >/dev/null 2>&1 & echo $!")
	printed, err := shell.Output()
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(printed)))
	if err != nil {
		t.Fatalf("orphan pid %q: %v", printed, err)
	}
	reapOnExit()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			state, _ := processState(t, pid)
			t.Fatalf("orphan %d still answers 5 s after it should have ended (state %c)", pid, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
