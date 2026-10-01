package flock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestForwardsTheSharedLock verifies that the CLI's locks are the shared
// filelock ones: an Acquire and a LockFile on the same path exclude each other
// and report the shared ErrBusy. The lock semantics are tested in filelock.
func TestForwardsTheSharedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	held, err := Acquire(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	f, err := OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := LockFile(f, false, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("LockFile under an exclusive Acquire = %v, want ErrBusy", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := LockFile(f, false, true); err != nil {
		t.Fatalf("LockFile after Release: %v", err)
	}
	if err := UnlockFile(f); err != nil {
		t.Fatal(err)
	}
}
