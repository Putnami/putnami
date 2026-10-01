//go:build windows

package pkgmeta

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// holdOpen opens path the way every Go reader does, without sharing delete
// access, and closes it 100 ms later, well inside robustio's retry budget. It
// first requires that a plain rename over the held file is refused, so the
// test proves the retry and not a host that lets the rename through. The
// returned channel is closed once the reader closed.
func holdOpen(t *testing.T, path string) <-chan struct{} {
	t.Helper()
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(filepath.Dir(path), ".probe")
	if err := os.WriteFile(probe, []byte("{}\n"), 0o600); err != nil {
		_ = reader.Close()
		t.Fatal(err)
	}
	requireRefused(t, "a rename over the open file", os.Rename(probe, path), func() { _ = reader.Close() })
	_ = os.Remove(probe)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		time.Sleep(100 * time.Millisecond)
		_ = reader.Close()
	}()
	return closed
}

// requireRefused checks a test's precondition: the operation the test tried
// while a handle held the file was refused with access denied or a sharing
// violation, the refusal the code under test waits out. When the operation went
// through, this host cannot show that refusal, so the test skips and says why
// instead of passing without proving anything. Any other error fails the test.
// release closes the holder before the test stops.
func requireRefused(t *testing.T, what string, err error, release func()) {
	t.Helper()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
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

// A reader of a channel record holds it open without sharing delete access.
// Rewriting the record waits for that reader instead of failing the rename.
func TestWriteChannelRecordWaitsForAReaderOfTheRecord(t *testing.T) {
	outputDir := t.TempDir()
	if err := WriteChannelRecord(outputDir, ChannelRecord{Version: "1.0.0", Channels: []string{"npm"}}); err != nil {
		t.Fatal(err)
	}
	closed := holdOpen(t, filepath.Join(outputDir, ChannelRecordFile))
	err := WriteChannelRecord(outputDir, ChannelRecord{Version: "1.0.1", Channels: []string{"npm"}})
	<-closed
	if err != nil {
		t.Fatalf("WriteChannelRecord after the reader closed: %v", err)
	}
	record, err := readJSON[ChannelRecord](filepath.Join(outputDir, ChannelRecordFile))
	if err != nil || record.Version != "1.0.1" {
		t.Fatalf("the record holds %+v, %v; want version 1.0.1", record, err)
	}
}

// The same holds for the publish evidence.
func TestWritePublishedImageManifestWaitsForAReaderOfTheManifest(t *testing.T) {
	outputDir := t.TempDir()
	manifest := PublishedImageManifest{ImmutableRef: "registry.example/app@sha256:1", Digest: "sha256:1", Verified: true}
	if err := WritePublishedImageManifest(outputDir, manifest); err != nil {
		t.Fatal(err)
	}
	closed := holdOpen(t, PublishedImageManifestPath(outputDir))
	manifest.ImmutableRef, manifest.Digest = "registry.example/app@sha256:2", "sha256:2"
	err := WritePublishedImageManifest(outputDir, manifest)
	<-closed
	if err != nil {
		t.Fatalf("WritePublishedImageManifest after the reader closed: %v", err)
	}
	got, err := ReadPublishedImageManifest(outputDir)
	if err != nil || got.Digest != "sha256:2" {
		t.Fatalf("the manifest holds %+v, %v; want digest sha256:2", got, err)
	}
}
