//go:build unix

package jobs

import (
	"path/filepath"
	"syscall"
	"testing"
)

// A file that is neither regular nor a symbolic link, such as a named pipe,
// names the tree by its path and type: the digest never opens it, stays the
// same on every read, and moves when the file appears.
func TestInstalledTreeDigestNamesASpecialFileByItsType(t *testing.T) {
	root := t.TempDir()
	writeInstalledExtension(t, root, canaryVersion, "  ")
	without := mustTreeDigest(t, root)
	if err := syscall.Mkfifo(filepath.Join(root, "runtime.fifo"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	with := mustTreeDigest(t, root)
	if with == without {
		t.Fatal("a named pipe did not move the installed tree digest")
	}
	if again := mustTreeDigest(t, root); again != with {
		t.Fatalf("a tree holding a named pipe digests differently on two reads: %s vs %s", with, again)
	}
}
