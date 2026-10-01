package store

import (
	"os"
	"os/exec"
	"testing"

	"go.putnami.dev/tooling/cli/internal/flock"
)

// TestAttachScratchPassesTheLeaseOnlyWhereInheritable verifies that a child
// receives the scratch lease descriptor exactly where a held lock passes to a
// child process, and starts either way.
func TestAttachScratchPassesTheLeaseOnlyWhereInheritable(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	lease, err := AttachScratch(cmd, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if flock.Inheritable && (len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != lease.File()) {
		t.Fatalf("the child does not inherit the scratch lease: %v", cmd.ExtraFiles)
	}
	if !flock.Inheritable && len(cmd.ExtraFiles) != 0 {
		t.Fatalf("the child was given descriptors this platform cannot pass: %v", cmd.ExtraFiles)
	}
	startErr := cmd.Start()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if startErr != nil {
		t.Fatalf("start with the scratch lease attached: %v", startErr)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
