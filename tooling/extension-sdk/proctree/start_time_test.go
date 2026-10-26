package proctree

import (
	"os"
	"os/exec"
	"testing"
)

func TestStartTimeIsStableAndEndsWithTheProcess(t *testing.T) {
	first, ok := StartTime(os.Getpid())
	if !ok || first == "" {
		t.Fatalf("StartTime(own pid) = %q, %v", first, ok)
	}
	if second, _ := StartTime(os.Getpid()); second != first {
		t.Fatalf("StartTime changed between two reads: %q then %q", first, second)
	}

	// Wait reaps the child, so its pid names nothing.
	child := exec.Command(os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatalf("run a child that exits: %v", err)
	}
	if start, ok := StartTime(child.Process.Pid); ok {
		t.Fatalf("StartTime(%d) = %q for a child that exited", child.Process.Pid, start)
	}
	for _, pid := range []int{0, -1} {
		if _, ok := StartTime(pid); ok {
			t.Fatalf("StartTime(%d) reported a start time", pid)
		}
	}
}
