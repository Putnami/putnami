//go:build windows

package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// errorSharingViolation is ERROR_SHARING_VIOLATION, which syscall does not name.
const errorSharingViolation = syscall.Errno(32)

// requireRefused checks a test's precondition: the operation the test tried
// while a handle held the file was refused with access denied or a sharing
// violation, the refusal the code under test waits out. When the operation went
// through, this host cannot show that refusal, so the test skips and says why
// instead of passing without proving anything. Any other error fails the test.
// release closes the holder before the test stops.
func requireRefused(t *testing.T, what string, err error, release func()) {
	t.Helper()
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errorSharingViolation) {
		return
	}
	release()
	if err == nil {
		t.Skipf("precondition: %s went through while another handle held the file, so this host cannot show the refusal the test waits out. "+
			"A read goes through an exclusive hold under an elevated token with SeBackupPrivilege, because Go opens a file read-only "+
			"with FILE_FLAG_BACKUP_SEMANTICS; run the test under a non-elevated token", what)
	}
	t.Fatalf("precondition: %s = %v, want access denied or a sharing violation", what, err)
}

// renameProbeOver renames a new file over path, the plain rename the code
// under test replaces with one that waits, and returns its error.
func renameProbeOver(t *testing.T, path string) error {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return os.Rename(probe, path)
}

// A reader holds the state file open the way os.ReadFile does, without
// sharing delete access. An update renames over it once the reader closes it,
// instead of failing with "Access is denied".
func TestAnUpdateWaitsForAReaderOfTheStateFile(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "store"))
	if _, err := s.Update(func(state *State) error { state.Next.Task = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(filepath.Join(s.Root(), stateFile))
	if err != nil {
		t.Fatal(err)
	}
	requireRefused(t, "a rename over the open state file", renameProbeOver(t, filepath.Join(s.Root(), stateFile)), func() { _ = reader.Close() })
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(100 * time.Millisecond)
		_ = reader.Close()
	}()
	_, err = s.Update(func(state *State) error { state.Next.Task = 2; return nil })
	<-closed
	if err != nil {
		t.Fatalf("Update while a reader held the state file: %v", err)
	}
	if state, err := s.Read(); err != nil || state.Next.Task != 2 {
		t.Fatalf("read %+v, %v after the update", state, err)
	}
}
