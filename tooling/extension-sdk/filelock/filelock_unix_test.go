//go:build unix

package filelock

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestDescriptorRejectsIntOverflow(t *testing.T) {
	maxInt := uintptr(^uint(0) >> 1)
	if _, err := descriptor(maxInt); err != nil {
		t.Fatalf("maximum int-sized descriptor rejected: %v", err)
	}
	if _, err := descriptor(maxInt + 1); err == nil {
		t.Fatal("descriptor above int range accepted")
	}
}

// TestSyncDirOpensTheDirectory pins that SyncDir on Unix flushes a directory
// it opens, so a directory that is gone is an error, not a silent success.
func TestSyncDirOpensTheDirectory(t *testing.T) {
	err := SyncDir(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("SyncDir on a missing directory = %v, want fs.ErrNotExist", err)
	}
}
