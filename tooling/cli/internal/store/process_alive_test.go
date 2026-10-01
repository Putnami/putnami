package store

import (
	"os"
	"os/exec"
	"testing"
)

// TestProcessAliveTellsARunningProcessFromAnExitedOne pins the liveness check
// that invocation reaping and the task output lock holder record share.
func TestProcessAliveTellsARunningProcessFromAnExitedOne(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("the running test process reads as dead")
	}
	for _, pid := range []int{0, -1} {
		if ProcessAlive(pid) {
			t.Fatalf("pid %d reads as alive", pid)
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	if ProcessAlive(child.Process.Pid) {
		t.Fatalf("exited child %d reads as alive", child.Process.Pid)
	}
}
