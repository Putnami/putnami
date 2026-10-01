//go:build windows

package infra

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

// releaseAfter calls release 100 ms from now, well inside the retry budget,
// and returns a channel that is closed once release returned.
func releaseAfter(release func()) <-chan struct{} {
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
		close(released)
	}()
	return released
}

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

func storageManifest(retention string) PerProjectManifest {
	return PerProjectManifest{Storage: []StorageBucket{{Name: "uploads", Retention: retention}}}
}

// A reader that opened the committed requirements the way os.Open does,
// without sharing delete access, makes a plain rename over them fail.
// SyncGeneratedRequirements waits for the reader to close them.
func TestSyncGeneratedRequirementsWaitsForAReaderOfTheCommittedManifest(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSidecar(dir, "storage", storageManifest("7d")); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncGeneratedRequirements(dir); err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecar(dir, "storage", storageManifest("30d")); err != nil {
		t.Fatal(err)
	}
	path := ProjectRequirementsPath(dir)
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, "a rename over the open file", os.Rename(probe, path), func() { _ = reader.Close() })
	closed := releaseAfter(func() { _ = reader.Close() })
	diags, err := SyncGeneratedRequirements(dir)
	<-closed
	if err != nil || diag.HasErrors(diags) {
		t.Fatalf("SyncGeneratedRequirements after the reader closed: err=%v diags=%v", err, diags)
	}
	m, diags := LoadGeneratedPerProjectManifest(path)
	if diag.HasErrors(diags) || len(m.Storage) != 1 || m.Storage[0].Retention != "30d" {
		t.Fatalf("committed manifest = %+v, %v; want the rewritten retention", m, diags)
	}
}

// The same holds for a producer rewriting its sidecar under a reader.
func TestWriteSidecarWaitsForAReaderOfTheSidecar(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSidecar(dir, "storage", storageManifest("7d")); err != nil {
		t.Fatal(err)
	}
	path := SidecarPath(dir, "storage")
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, "a rename over the open file", os.Rename(probe, path), func() { _ = reader.Close() })
	closed := releaseAfter(func() { _ = reader.Close() })
	err = WriteSidecar(dir, "storage", storageManifest("30d"))
	<-closed
	if err != nil {
		t.Fatalf("WriteSidecar after the reader closed: %v", err)
	}
}

// A rename that replaces a file opens it without sharing, so a reader's open
// fails with a sharing violation until the rename is done. The loader waits
// for the holder instead of reporting a parse error.
func TestLoadGeneratedPerProjectManifestWaitsForAnExclusiveHolder(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSidecar(dir, "storage", storageManifest("7d")); err != nil {
		t.Fatal(err)
	}
	path := SidecarPath(dir, "storage")
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.ReadFile(path)
	requireRefused(t, "reading the exclusively held file", err, func() { _ = syscall.CloseHandle(holder) })
	closed := releaseAfter(func() { _ = syscall.CloseHandle(holder) })
	m, diags := LoadGeneratedPerProjectManifest(path)
	<-closed
	if diag.HasErrors(diags) || len(m.Storage) != 1 {
		t.Fatalf("LoadGeneratedPerProjectManifest after the holder closed: %+v, %v", m, diags)
	}
}

// A producer that clears its sidecar under a reader waits for the reader too.
func TestRemoveSidecarWaitsForAReaderOfTheSidecar(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSidecar(dir, "storage", storageManifest("7d")); err != nil {
		t.Fatal(err)
	}
	path := SidecarPath(dir, "storage")
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	requireRefused(t, "removing the open file", os.Remove(path), func() { _ = reader.Close() })
	closed := releaseAfter(func() { _ = reader.Close() })
	err = RemoveSidecar(dir, "storage")
	<-closed
	if err != nil {
		t.Fatalf("RemoveSidecar after the reader closed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the sidecar survived RemoveSidecar: %v", err)
	}
}
