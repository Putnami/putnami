//go:build unix

package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLookupTaskFailureWaitsOutAHeldRecord pins F3's reader: a record the host
// refuses to open while another process holds it is waited for, not read as a
// miss, when the policy reports the refusal as a held handle. Unix never
// refuses an open for that reason, so the test stands in a permission refusal
// that it lifts after a moment.
func TestLookupTaskFailureWaitsOutAHeldRecord(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file whatever its mode")
	}
	refuse := func(t *testing.T) (*LocalStore, string) {
		s := newFailureStore(t)
		if _, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult("boom")}); err != nil {
			t.Fatalf("RecordTaskFailure: %v", err)
		}
		record := filepath.Join(s.blobDir(TaskFailureAddress(failureKeyA)), taskFailureRecordFilename)
		if err := os.Chmod(record, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(record, 0o644) })
		return s, record
	}

	t.Run("paced", func(t *testing.T) {
		s, record := refuse(t)
		waitOnHeld(t, func(err error) bool { return errors.Is(err, fs.ErrPermission) }, time.Minute)
		released := make(chan error, 1)
		go func() {
			time.Sleep(50 * time.Millisecond)
			released <- os.Chmod(record, 0o644)
		}()
		got := s.LookupTaskFailure(failureKeyA)
		if err := <-released; err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Result == nil || got.Result.Error == nil || got.Result.Error.Message != "boom" {
			t.Fatalf("lookup while the record was held = %+v, want the recorded failure", got)
		}
	})

	t.Run("never held", func(t *testing.T) {
		s, _ := refuse(t)
		waitOnHeld(t, neverHeld, time.Minute)
		if got := s.LookupTaskFailure(failureKeyA); got != nil {
			t.Errorf("an unreadable record served a replay: %+v", got)
		}
	})
}
