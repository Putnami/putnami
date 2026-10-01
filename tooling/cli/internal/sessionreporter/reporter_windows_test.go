//go:build windows

package sessionreporter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestSaveWaitsForAReaderOfTheCheckpoint pins that a checkpoint save survives
// a concurrent reader on Windows. The reader holds reporting.json open without
// the reporting lock, as a scanner or a test does; the rename that replaces the
// checkpoint fails with ERROR_ACCESS_DENIED until the reader closes it. The
// save waits instead of failing: a failed save stops the delivery worker, and
// with it the provider, in the middle of a run.
func TestSaveWaitsForAReaderOfTheCheckpoint(t *testing.T) {
	dir := t.TempDir()
	r, err := openRun(context.Background(), SessionReporter, dir, "session", "@test/provider")
	if err != nil {
		t.Fatal(err)
	}
	defer r.release()
	checkpoint := filepath.Join(dir, StateFile)
	reader, err := os.Open(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, []byte("probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(probe, checkpoint); !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		_ = reader.Close()
		if err == nil {
			t.Skip("precondition: a rename over the open checkpoint went through, so this host cannot show the refusal the save waits out. " +
				"A read goes through an exclusive hold under an elevated token with SeBackupPrivilege; run the test under a non-elevated token")
		}
		t.Fatalf("precondition: a rename over the open checkpoint = %v, want access denied or a sharing violation", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(150 * time.Millisecond)
		_ = reader.Close()
	}()
	t.Cleanup(func() { <-released })

	r.state.Events.Offset = 7
	err = r.save()
	<-released
	if err != nil {
		t.Fatalf("save while a reader held reporting.json: %v", err)
	}
	data, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	var saved state
	if err := json.Unmarshal(data, &saved); err != nil || saved.Events.Offset != 7 {
		t.Fatalf("checkpoint = %s, %v; want the events cursor at 7", data, err)
	}
}
