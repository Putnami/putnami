//go:build windows

package compose

import (
	"os"
	"os/exec"
	"testing"
)

func TestWindowsProcessIdentityIsStableAndEndsWithTheProcess(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive(own pid) = false")
	}
	first, ok := processStartTime(os.Getpid())
	if !ok || first == "" {
		t.Fatalf("processStartTime(own pid) = %q, %v", first, ok)
	}
	if second, _ := processStartTime(os.Getpid()); second != first {
		t.Fatalf("processStartTime changed between two reads: %q then %q", first, second)
	}

	// Wait releases the last handle to the child, so its pid names nothing.
	child := exec.Command(os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatalf("run a child that exits: %v", err)
	}
	if processAlive(child.Process.Pid) {
		t.Fatalf("processAlive(%d) = true for a child that exited", child.Process.Pid)
	}
	for _, pid := range []int{0, -1} {
		if processAlive(pid) {
			t.Fatalf("processAlive(%d) = true", pid)
		}
		if _, ok := processStartTime(pid); ok {
			t.Fatalf("processStartTime(%d) reported a start time", pid)
		}
	}
}
