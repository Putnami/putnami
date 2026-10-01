//go:build windows

package robustio

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// requireRefused checks a test's precondition: the operation the test tried
// while a handle held the file was refused with access denied or a sharing
// violation, the refusal the code under test waits out. When the operation went
// through, this host cannot show that refusal, so the test skips and says why
// instead of passing without proving anything. Any other error fails the test.
// release closes the holder before the test stops.
func requireRefused(t *testing.T, what string, err error, release func()) {
	t.Helper()
	if transient(err) {
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

// A reader that opened the destination the way os.Open does, without sharing
// delete access, makes a plain rename over it fail. Rename waits for the
// reader to close it.
func TestRenameWaitsForAReaderOfTheDestination(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	for path, body := range map[string]string{from: "new", to: "old"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := os.Open(to)
	if err != nil {
		t.Fatal(err)
	}
	requireRefused(t, "a rename over an open file", os.Rename(from, to), func() { _ = reader.Close() })
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(100 * time.Millisecond)
		_ = reader.Close()
	}()
	err = Rename(from, to)
	<-closed
	if err != nil {
		t.Fatalf("Rename after the reader closed: %v", err)
	}
	if got, err := os.ReadFile(to); err != nil || string(got) != "new" {
		t.Fatalf("the destination holds %q, %v; want the renamed file", got, err)
	}
}

// A reader that opened a file the way os.Open does, without sharing delete
// access, makes a plain removal of it fail. Remove waits for the reader to
// close it.
func TestRemoveWaitsForAReaderOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	requireRefused(t, "removing an open file", os.Remove(path), func() { _ = reader.Close() })
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(100 * time.Millisecond)
		_ = reader.Close()
	}()
	err = Remove(path)
	<-closed
	if err != nil {
		t.Fatalf("Remove after the reader closed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file survived Remove: %v", err)
	}
}
