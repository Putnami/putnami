//go:build windows

package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"golang.org/x/sys/windows"
)

// hold opens the path as another process reading it would and returns the
// function that closes the handle. exclusive opens the path with no sharing at
// all, so even a reader is refused; otherwise it opens the path the way Go
// does, which refuses a rename or a delete.
func hold(t *testing.T, path string, exclusive bool) (release func()) {
	t.Helper()
	var share uint32 = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	if exclusive {
		share = 0
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatalf("hold %s: %v", path, err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = windows.CloseHandle(h) }) }
	t.Cleanup(release)
	return release
}

// releaseAfter closes the holder d from now and returns a channel that yields
// once the handle is closed.
func releaseAfter(t *testing.T, release func(), d time.Duration) <-chan struct{} {
	t.Helper()
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(d)
		release()
	}()
	t.Cleanup(func() { <-released })
	return released
}

// requireRefused checks a test's precondition: the operation the test tried
// while a handle held the path was refused with the error the store waits out.
// When the operation went through, this host cannot show that refusal, so the
// test skips and says why instead of passing without proving anything. Any
// other error fails the test. release closes the holder before the test stops.
func requireRefused(t *testing.T, what string, err error, release func()) {
	t.Helper()
	if hostHeldOpen(err) {
		return
	}
	release()
	if err == nil {
		t.Skipf("precondition: %s went through while another handle held the path, so this host cannot show the refusal the test waits out. "+
			"A read goes through an exclusive hold under an elevated token with SeBackupPrivilege, because Go opens a file read-only "+
			"with FILE_FLAG_BACKUP_SEMANTICS; run the test under a non-elevated token", what)
	}
	t.Fatalf("precondition: %s = %v, want access denied or a sharing violation", what, err)
}

// renameProbeOver renames a new file over path and returns the rename's error.
func renameProbeOver(t *testing.T, path string) error {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	return os.Rename(probe, path)
}

// TestLookupTaskFailureWaitsForAnExclusiveHolderOfTheRecord pins F3's reader
// on Windows: while another process holds the record without sharing it, the
// open fails with ERROR_SHARING_VIOLATION, and the lookup waits instead of
// reporting a miss.
func TestLookupTaskFailureWaitsForAnExclusiveHolderOfTheRecord(t *testing.T) {
	s := newFailureStore(t)
	if _, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult("boom")}); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	record := filepath.Join(s.blobDir(TaskFailureAddress(failureKeyA)), taskFailureRecordFilename)
	release := hold(t, record, true)
	_, err := os.ReadFile(record)
	requireRefused(t, "reading the exclusively held record", err, release)
	released := releaseAfter(t, release, 150*time.Millisecond)

	got := s.LookupTaskFailure(failureKeyA)
	<-released
	if got == nil {
		t.Fatal("the lookup read a held record as a miss")
	}
}

// TestRecordTaskFailureWaitsForAReaderOfTheRecord pins F3's writer on Windows:
// a rename over a record another process reads fails with ERROR_ACCESS_DENIED
// until the reader closes it, and the record waits instead of failing.
func TestRecordTaskFailureWaitsForAReaderOfTheRecord(t *testing.T) {
	s := newFailureStore(t)
	if _, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult("boom")}); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	record := filepath.Join(s.blobDir(TaskFailureAddress(failureKeyA)), taskFailureRecordFilename)
	release := hold(t, record, false)
	requireRefused(t, "a rename over the held record", renameProbeOver(t, record), release)
	released := releaseAfter(t, release, 150*time.Millisecond)

	got, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult("again")})
	<-released
	if err != nil {
		t.Fatalf("RecordTaskFailure while a reader held the record: %v", err)
	}
	if got.Attempts != 2 || got.Result.Error.Message != "again" {
		t.Errorf("record = attempts %d, message %q; want 2, again", got.Attempts, got.Result.Error.Message)
	}
}

// TestMaterializeTaskOutputWaitsForAReaderInsideTheDestination pins F4 on
// Windows: a directory cannot be renamed while a file inside it is open, so
// the swap waits for the reader instead of reporting the tree contended.
func TestMaterializeTaskOutputWaitsForAReaderInsideTheDestination(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	want := map[string]string{"main.js": "built", "sub/chunk.js": "chunk"}
	entry := ingestTree(t, s, "held-destination-key", want)
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	release := hold(t, filepath.Join(dest, "sub", "chunk.js"), false)
	requireRefused(t, "renaming the destination while a file inside it is held", os.Rename(dest, dest+"-probe"), release)
	released := releaseAfter(t, release, 150*time.Millisecond)

	_, err := s.MaterializeTaskOutput(entry, "dist", dest, "")
	<-released
	if err != nil {
		t.Fatalf("materialize while a reader held a file inside the destination: %v", err)
	}
	assertTree(t, dest, want)
	assertOnlyEntries(t, filepath.Dir(dest), "out")
}

// TestMaterializeTaskOutputWaitsForAReaderOfTheDestinationFile pins F4 for a
// file output on Windows: the rename over a file another process reads waits
// for the reader.
func TestMaterializeTaskOutputWaitsForAReaderOfTheDestinationFile(t *testing.T) {
	const content = "tool bytes"
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()
	tool := fileOutput("tool", "bin/tool")
	stage(t, staging, tool, "", content)
	entry, err := s.IngestTaskEntry(staging, taskSpec("held-file-key", tool))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	release := hold(t, dest, false)
	requireRefused(t, "a rename over the held destination file", renameProbeOver(t, dest), release)
	released := releaseAfter(t, release, 150*time.Millisecond)

	_, err = s.MaterializeTaskOutput(entry, "tool", dest, "")
	<-released
	if err != nil {
		t.Fatalf("materialize while a reader held the destination file: %v", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != content {
		t.Errorf("published file = %q, %v; want %q", data, err, content)
	}
}

// TestPublishBlobOverAHeldReadOnlyBlobIsALostRace pins F2 on Windows: a rename
// over a CAS blob that is read-only and held open fails with
// ERROR_ACCESS_DENIED for as long as robustio waits, and the blob that is
// already there is the one this call would have published.
func TestPublishBlobOverAHeldReadOnlyBlobIsALostRace(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	content := []byte("blob bytes")
	digest := cache.DigestOf(content)
	blob := s.casBlobPath(digest)
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, content, 0o444); err != nil {
		t.Fatal(err)
	}
	release := hold(t, blob, false)
	requireRefused(t, "a rename over the held read-only blob", renameProbeOver(t, blob), release)
	releaseAfter(t, release, 3*time.Second)
	staged := filepath.Join(filepath.Dir(blob), "blob-staged")
	if err := os.WriteFile(staged, content, 0o644); err != nil {
		t.Fatal(err)
	}

	published, err := s.publishBlob(staged, digest)
	if err != nil || published {
		t.Fatalf("publishBlob over a held blob = %v, %v; want a lost race", published, err)
	}
	if data, err := os.ReadFile(blob); err != nil || string(data) != string(content) {
		t.Errorf("blob after the lost race = %q, %v", data, err)
	}
}
