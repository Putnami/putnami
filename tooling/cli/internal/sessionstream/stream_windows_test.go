//go:build windows

package sessionstream

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	protocolcli "go.putnami.dev/protocol/cli"
)

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

// TestRecordEvidenceWaitsForAReaderOfTheDocument pins that recording evidence
// survives a concurrent ReadEvidence on Windows: the reader holds
// subscribers.json open without the writers' lock, the rename that replaces
// the document fails with ERROR_ACCESS_DENIED until the reader closes it, and
// the write waits instead of failing.
func TestRecordEvidenceWaitsForAReaderOfTheDocument(t *testing.T) {
	log, dir := newLog(t)
	_ = log.Append([]byte(`{"i":0}`))
	_ = log.Close()
	end, _ := log.Extent()
	first := subscribe(t, log, "first", 0)
	if err := first.RecordEvidence(); err != nil {
		t.Fatal(err)
	}

	document := filepath.Join(dir, protocolcli.SessionSubscribersFileName)
	reader, err := os.Open(document)
	if err != nil {
		t.Fatal(err)
	}
	requireRefused(t, "a rename over the open document", renameProbeOver(t, document), func() { _ = reader.Close() })
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(150 * time.Millisecond)
		_ = reader.Close()
	}()
	t.Cleanup(func() { <-released })

	second := subscribe(t, log, "second", 0)
	if err := second.Ack(end.Offset, true); err != nil {
		t.Fatal(err)
	}
	err = second.RecordEvidence()
	<-released
	if err != nil {
		t.Fatalf("RecordEvidence while a reader held subscribers.json: %v", err)
	}
	file, err := ReadEvidence(dir)
	if err != nil || len(file.Subscribers) != 2 {
		t.Fatalf("evidence = %+v, %v; want both subscribers", file, err)
	}
}
