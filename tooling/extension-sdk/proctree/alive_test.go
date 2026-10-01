package proctree

import (
	"os"
	"os/exec"
	"testing"
)

func TestProcessAliveIsTrueOnlyForARunningProcess(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("ProcessAlive(own pid) = false")
	}
	for _, pid := range []int{0, -1} {
		if ProcessAlive(pid) {
			t.Fatalf("ProcessAlive(%d) = true", pid)
		}
	}
	// Wait reaps the child, so its pid names nothing.
	child := exec.Command(os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatalf("run a child that exits: %v", err)
	}
	if ProcessAlive(child.Process.Pid) {
		t.Fatalf("ProcessAlive(%d) = true for a child that exited", child.Process.Pid)
	}
}
