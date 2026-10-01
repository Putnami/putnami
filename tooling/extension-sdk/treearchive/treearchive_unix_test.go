//go:build unix

package treearchive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A symbolic link to a file is refused with the target it stores.
func TestAFileLinkIsRefusedWithItsTarget(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]os.FileMode{"config/defaults.json": 0o644}, time.Now())
	if err := os.Symlink("defaults.json", filepath.Join(root, "config", "current.json")); err != nil {
		t.Fatal(err)
	}
	err := CopyTree(root, filepath.Join(t.TempDir(), "stage"), nil)
	var link *LinkError
	if !errors.As(err, &link) || err.Error() != "config/current.json is a symbolic link to defaults.json" {
		t.Fatalf("CopyTree error = %v, want config/current.json is a symbolic link to defaults.json", err)
	}
}

// A special file that is not a link, here a named pipe, is refused by name.
func TestASpecialFileIsRefused(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
		t.Skipf("this host cannot create a named pipe: %v", err)
	}
	err := WriteTarGz(filepath.Join(t.TempDir(), "out.tar.gz"), root)
	if err == nil || !strings.Contains(err.Error(), "pipe is neither a regular file nor a directory") {
		t.Fatalf("WriteTarGz error = %v, want the pipe refused by name", err)
	}
}
