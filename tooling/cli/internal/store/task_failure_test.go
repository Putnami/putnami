package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

const failureKeyA = "ff11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
const failureKeyB = "ee11223344556677889900aabbccddeeff00112233445566778899aabbccddee"

func failedResult(message string) *EntryResult {
	return &EntryResult{
		Status: "failed",
		Error:  &EntryError{Message: message},
		Events: []cache.ActionEvent{{Type: "log", Level: "error", Message: message}},
	}
}

func newFailureStore(t *testing.T) *LocalStore {
	t.Helper()
	return NewLocalStore(t.TempDir())
}

// TestRecordedFailureReplaysAtTheSameKey is the whole point of the model: the
// same v5 cache key that would have served a positive entry serves the recorded
// failure, with its payload intact.
func TestRecordedFailureReplaysAtTheSameKey(t *testing.T) {
	t.Parallel()
	s := newFailureStore(t)

	recorded, err := s.RecordTaskFailure(failureKeyA, TaskFailure{
		ExitCode:      2,
		FirstFailedAt: time.Now().Add(-time.Hour),
		Result:        failedResult("assertion failed"),
	})
	if err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	if recorded.Attempts != 1 {
		t.Errorf("first record attempts = %d, want 1", recorded.Attempts)
	}

	replayed := s.ReplayTaskFailure(failureKeyA)
	if replayed == nil {
		t.Fatal("a recorded failure did not replay at its own key")
	}
	if replayed.ExitCode != 2 {
		t.Errorf("replayed exit code = %d, want the recorded 2", replayed.ExitCode)
	}
	if replayed.Result == nil || replayed.Result.Error == nil ||
		replayed.Result.Error.Message != "assertion failed" {
		t.Errorf("replayed result lost the original failure payload: %+v", replayed.Result)
	}
	if len(replayed.Result.Events) != 1 || replayed.Result.Events[0].Type != "log" {
		t.Errorf("replayed result lost the log events the failure detail is rebuilt from: %+v",
			replayed.Result.Events)
	}
}

// TestReplayCountsAnObservationAndKeepsTheFirstFailureTime pins the two numbers
// the replay line reports: the attempt count grows with every observation
// (executed or replayed) and the first-failure time never moves.
func TestReplayCountsAnObservationAndKeepsTheFirstFailureTime(t *testing.T) {
	t.Parallel()
	s := newFailureStore(t)

	first := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if _, err := s.RecordTaskFailure(failureKeyA, TaskFailure{
		FirstFailedAt: first,
		Result:        failedResult("boom"),
	}); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}

	replayed := s.ReplayTaskFailure(failureKeyA)
	if replayed.Attempts != 2 {
		t.Errorf("replay attempts = %d, want 2: a replay is an observation", replayed.Attempts)
	}

	// A re-execution at the same key records again: the count keeps growing and
	// the first-failure time is NOT overwritten by the new run's clock.
	reRecorded, err := s.RecordTaskFailure(failureKeyA, TaskFailure{
		FirstFailedAt: time.Now(),
		Result:        failedResult("boom"),
	})
	if err != nil {
		t.Fatalf("re-record: %v", err)
	}
	if reRecorded.Attempts != 3 {
		t.Errorf("re-record attempts = %d, want 3", reRecorded.Attempts)
	}
	if !reRecorded.FirstFailedAt.Equal(first) {
		t.Errorf("first failure time moved to %s, want the original %s",
			reRecorded.FirstFailedAt, first)
	}
}

// TestSuccessForgetsTheRecordedFailure pins the invalidation a fix at the same
// key performs: no stale verdict survives it.
func TestSuccessForgetsTheRecordedFailure(t *testing.T) {
	t.Parallel()
	s := newFailureStore(t)

	if _, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult("boom")}); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	s.ForgetTaskFailure(failureKeyA)

	if replayed := s.ReplayTaskFailure(failureKeyA); replayed != nil {
		t.Fatalf("a forgotten failure still replayed: %+v", replayed)
	}
	// The next failure at that key starts the count again, so a user never sees
	// an attempt number inherited from a verdict that was already disproved.
	recorded, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult("boom")})
	if err != nil {
		t.Fatalf("RecordTaskFailure after forget: %v", err)
	}
	if recorded.Attempts != 1 {
		t.Errorf("attempts after forget = %d, want 1", recorded.Attempts)
	}
}

// TestFailureRecordIsAddressedApartFromEveryPositiveEntry is contract point 3
// made structural: the negative address is derived from its own domain, so no
// positive-entry reader — including the remote-cache code, which only ever
// derives TaskEntryAddress — can name it.
func TestFailureRecordIsAddressedApartFromEveryPositiveEntry(t *testing.T) {
	t.Parallel()
	if TaskFailureAddress(failureKeyA) == TaskEntryAddress(failureKeyA) {
		t.Fatal("the negative entry shares the positive entry's address: a failure could be published remotely")
	}
	if TaskFailureAddress(failureKeyA) == TaskFailureAddress(failureKeyB) {
		t.Fatal("two keys derive one negative address")
	}

	s := newFailureStore(t)
	if _, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult("boom")}); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}
	// The positive readers must see nothing at all for that key.
	entry, err := s.LookupTaskEntry(failureKeyA)
	if err != nil || entry != nil {
		t.Errorf("LookupTaskEntry served a negative record: entry=%v err=%v", entry, err)
	}
	legacy, err := s.Get(TaskFailureAddress(failureKeyA))
	if err != nil || legacy != nil {
		t.Errorf("the legacy reader interpreted the negative blob: entry=%v err=%v", legacy, err)
	}
}

// TestUnreadableFailureRecordIsAMiss pins the fail-closed rule: a torn, foreign,
// wrongly formatted, or non-failing record costs one re-execution and never an
// error or a wrong verdict.
func TestUnreadableFailureRecordIsAMiss(t *testing.T) {
	t.Parallel()
	blobFor := func(t *testing.T, s *LocalStore, key string) string {
		t.Helper()
		return s.blobDir(TaskFailureAddress(key))
	}
	write := func(t *testing.T, dir, content string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, taskFailureRecordFilename), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := map[string]func(t *testing.T, s *LocalStore){
		"torn json": func(t *testing.T, s *LocalStore) {
			write(t, blobFor(t, s, failureKeyA), `{"format":1,"key":"`)
		},
		"another format": func(t *testing.T, s *LocalStore) {
			record, _ := json.Marshal(TaskFailure{
				Format: CurrentTaskFailureFormat + 1, Key: failureKeyA,
				Attempts: 1, Result: failedResult("boom"),
			})
			write(t, blobFor(t, s, failureKeyA), string(record))
		},
		"another key": func(t *testing.T, s *LocalStore) {
			record, _ := json.Marshal(TaskFailure{
				Format: CurrentTaskFailureFormat, Key: failureKeyB,
				Attempts: 1, Result: failedResult("boom"),
			})
			write(t, blobFor(t, s, failureKeyA), string(record))
		},
		"not a failure": func(t *testing.T, s *LocalStore) {
			record, _ := json.Marshal(TaskFailure{
				Format: CurrentTaskFailureFormat, Key: failureKeyA,
				Attempts: 1, Result: &EntryResult{Status: "success"},
			})
			write(t, blobFor(t, s, failureKeyA), string(record))
		},
	}

	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newFailureStore(t)
			corrupt(t, s)
			if got := s.LookupTaskFailure(failureKeyA); got != nil {
				t.Errorf("lookup served an uninterpretable record: %+v", got)
			}
			if got := s.ReplayTaskFailure(failureKeyA); got != nil {
				t.Errorf("replay served an uninterpretable record: %+v", got)
			}
		})
	}
}

// TestFailureRecordParticipatesInGarbageCollection proves the record is an
// ordinary blob to GC: it is counted in the store's usage, carries a recency
// sidecar, and references no CAS blob of its own.
func TestFailureRecordParticipatesInGarbageCollection(t *testing.T) {
	t.Parallel()
	s := newFailureStore(t)
	if _, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult("boom")}); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}

	scan, state := scanStore(s.Root(), false)
	if state != scanDone {
		t.Fatal("scanStore refused a store holding only a negative entry")
	}
	entries, total := scan.entries, scan.total
	if len(entries) != 1 {
		t.Fatalf("scanStore found %d entries, want the negative one", len(entries))
	}
	if entries[0].lastUsed.IsZero() {
		t.Error("the negative entry has no recency, so idle reclaim would never see it")
	}
	if len(entries[0].blobs) != 0 {
		t.Errorf("the negative entry claims CAS blobs: %+v", entries[0].blobs)
	}
	if total <= 0 {
		t.Errorf("store usage = %d bytes: the negative entry escapes the byte budget", total)
	}

	// Eviction removes it like any other blob, and the key then misses.
	if _, err := RunGC([]string{s.Root()}, GCOptions{MaxBytes: 1}); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if got := s.ReplayTaskFailure(failureKeyA); got != nil {
		t.Errorf("an evicted negative entry still replayed: %+v", got)
	}
}

// TestConcurrentRecordsNeverPublishATornRecord pins the atomicity of the write
// path: every reader observes a complete record while writers keep recording,
// and no write fails because a reader holds the record. The writer finishes
// before the test reports, so a failure never races the temp dir's removal.
func TestConcurrentRecordsNeverPublishATornRecord(t *testing.T) {
	t.Parallel()
	s := newFailureStore(t)
	if _, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult("boom")}); err != nil {
		t.Fatalf("RecordTaskFailure: %v", err)
	}

	writeErr := make(chan error, 1)
	go func() {
		var first error
		for i := 0; i < 50; i++ {
			_, err := s.RecordTaskFailure(failureKeyA, TaskFailure{Result: failedResult(strings.Repeat("x", i*16))})
			if first == nil {
				first = err
			}
		}
		writeErr <- first
	}()
	missed := 0
	for i := 0; i < 200; i++ {
		if got := s.LookupTaskFailure(failureKeyA); got == nil {
			missed++
		}
	}
	if err := <-writeErr; err != nil {
		t.Errorf("a write failed while readers held the record: %v", err)
	}
	if missed > 0 {
		t.Errorf("readers observed no record %d times while the writer was replacing it", missed)
	}
}
