//go:build unix

package provider

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/local-collaboration/internal/store"
)

func TestAHeldLockIsUnavailableAndRetryable(t *testing.T) {
	root := t.TempDir()
	s := store.Open(root)
	if failure := update(s, func(*store.State) error { return nil }); failure != nil {
		t.Fatal(failure.Error.Message)
	}
	fd, err := syscall.Open(filepath.Join(root, "lock"), syscall.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	s.LockTimeout = 50 * time.Millisecond
	ran := false
	failure := update(s, func(*store.State) error { ran = true; return nil })
	if failure == nil || failure.Outcome != collab.OutcomeUnavailable || failure.Error.Reason != "store.busy" || !failure.Error.Retryable || ran {
		t.Fatalf("a write under a held lock: %+v (change ran: %v)", failure, ran)
	}
}
