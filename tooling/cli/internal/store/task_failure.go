package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.putnami.dev/sdk/extension/robustio"
)

// Negative (failure) cache entries — "this exact task, with these exact
// inputs, already failed".
//
// # Why a negative entry is not a task-owned entry
//
// A task-owned entry (task_entry.go) records WHAT A SUCCESSFUL TASK PRODUCED:
// declared outputs, CAS blobs, a manifest, a descriptor. A negative entry
// records ONLY THAT THE TASK FAILED, plus what it said while failing. It
// carries no declared output and no CAS blob on purpose: a failed task's
// output tree is untrusted, and restoring it would materialize a half-written
// artifact that no run ever validated.
//
// It is therefore a single small JSON record in a blob directory of its own,
// written at an address derived from a DISTINCT domain:
//
//	address = sha256("putnami/store/task-failure\x00" + <format> + "\x00" + key)
//
// Three properties follow, and they are the contract:
//
//   - THE KEY IS THE SAME KEY. A negative entry is invalidated by exactly what
//     invalidates a positive one — the v5 task cache key. There is no second
//     key, no extra input, and no expiry clock: an entry survives until its
//     inputs change, until --retry-failed or --no-cache bypasses it, until a
//     success at the same key deletes it, or until GC reclaims it.
//   - A NEGATIVE ENTRY CANNOT BE PUBLISHED REMOTELY. remote.go and
//     task_remote.go only ever derive TaskEntryAddress, so nothing on the
//     remote path can reach this address. Contract point 3 ("never poison
//     another workspace") is enforced by construction rather than by a
//     conditional that a later edit could drop.
//   - A NEGATIVE ENTRY CANNOT BE READ AS A POSITIVE ONE. The addresses are
//     disjoint, the record has no entry.json, and LookupTaskEntry/Get both
//     require files this record does not have.
//
// # GC participates with no special case
//
// The record lives at blobs/<prefix>/<address>/ with a lastUsed sidecar, so
// scanStore enumerates it like any other blob directory: it counts against the
// store's byte budget through blobMetaBytes and is reclaimed by idle or budget
// eviction. readManifestBlobs returns nil for a directory with no manifest, so
// a negative entry references no CAS blob and its eviction frees exactly its
// own bytes.
//
// # Every unreadable record is a MISS
//
// A torn write, an unknown format, a record naming another key: all read as
// "no negative entry", never as an error. Failing closed here costs one
// re-execution; failing open would replay a verdict that does not belong to
// this key.

const (
	// TaskFailureFormatV1 is the record shape this build writes and the only
	// one it reads. The format is part of the ADDRESS (like the task-owned
	// entry format), so bumping it relocates every record and the old ones age
	// out through ordinary GC with no migration pass.
	TaskFailureFormatV1 = 1

	// CurrentTaskFailureFormat is the format RecordTaskFailure writes.
	CurrentTaskFailureFormat = TaskFailureFormatV1
)

// taskFailureAddressDomain namespaces the negative-entry address derivation.
// It is deliberately different from taskEntryAddressDomain: that difference is
// what makes a negative record unreachable from every positive-entry code path,
// local or remote.
const taskFailureAddressDomain = "putnami/store/task-failure"

// taskFailureRecordFilename is the single file a negative entry is made of.
const taskFailureRecordFilename = "failure.json"

// TaskFailure is one recorded failure of one task at one cache key.
type TaskFailure struct {
	// Format is the record format version; always CurrentTaskFailureFormat for
	// a record this build wrote.
	Format int `json:"format"`

	// Key is the v5 task cache key this failure belongs to. It is re-checked on
	// read, so an address collision cannot serve another task's verdict.
	Key string `json:"key"`

	// ExitCode is the failing subprocess's exit status, preserved so a replayed
	// failure reports what the original one reported.
	ExitCode int `json:"exitCode"`

	// FirstFailedAt is when the task FIRST failed at this key. It is preserved
	// across re-records: the age it yields is the age of the failure itself.
	FirstFailedAt time.Time `json:"firstFailedAt"`

	// Attempts counts how many times this failure has been observed at this key
	// — executed or replayed — including the observation that wrote the record.
	Attempts int `json:"attempts"`

	// Result is the structured job result, in the same shape a positive entry
	// stores, so the error and event encodings are shared. Unlike a positive
	// entry it retains log events: the human failure detail is reconstructed
	// from them, and a replayed failure must not be quieter than a fresh one.
	Result *EntryResult `json:"result"`

	// Address is the derived store address, resolved on read and never encoded.
	Address string `json:"-"`
}

// TaskFailureAddress maps a task cache key to the store address its negative
// entry lives at, binding the record format into the address.
func TaskFailureAddress(key string) string {
	sum := sha256.Sum256([]byte(
		taskFailureAddressDomain + "\x00" + strconv.Itoa(CurrentTaskFailureFormat) + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// LookupTaskFailure returns the negative entry recorded for key, or nil for a
// miss. It does not modify the record; ReplayTaskFailure is the read that
// counts an observation.
func (s *LocalStore) LookupTaskFailure(key string) *TaskFailure {
	if key == "" {
		return nil
	}
	s.ensureGeneration()
	release := s.lockShared()
	defer release()

	address := TaskFailureAddress(key)
	record := loadTaskFailure(s.blobDir(address), address, key)
	if record == nil {
		return nil
	}
	writeLastUsed(s.blobDir(address), time.Now(), s.gen)
	return record
}

// ReplayTaskFailure consumes a negative entry for replay: it returns the
// recorded failure with its attempt counter already advanced to include THIS
// observation, and persists that count. A miss returns nil and writes nothing.
//
// Counting a replay is what makes the reported attempt number the number of
// times the workspace has been told about this failure, which is the number a
// user checking "am I looping?" needs.
func (s *LocalStore) ReplayTaskFailure(key string) *TaskFailure {
	return s.advanceTaskFailure(key, nil)
}

// RecordTaskFailure records a freshly executed failure at key. An existing
// record's FirstFailedAt is preserved and its attempt counter advanced, so a
// task that keeps failing at unchanged inputs keeps ONE record with a growing
// count instead of losing when it first broke.
//
// Publication is last-writer-wins on the record file, staged in the blob
// directory and committed with a rename, so a concurrent reader observes either
// the previous record or the new one and never a torn one.
func (s *LocalStore) RecordTaskFailure(key string, failure TaskFailure) (*TaskFailure, error) {
	if key == "" {
		return nil, fmt.Errorf("record task failure: cache key required")
	}
	record := s.advanceTaskFailure(key, &failure)
	if record == nil {
		return nil, fmt.Errorf("record task failure %s: record is not readable after write", key)
	}
	return record, nil
}

// ForgetTaskFailure removes any negative entry for key. It is called when the
// task SUCCEEDS at that exact key, so a genuinely fixed input never leaves a
// stale verdict behind for a later run to replay.
func (s *LocalStore) ForgetTaskFailure(key string) {
	if key == "" {
		return
	}
	release := s.lockShared()
	defer release()

	os.RemoveAll(s.blobDir(TaskFailureAddress(key)))
}

// advanceTaskFailure is the single read-modify-write of a negative entry.
// fresh is nil for a replay (carry the stored payload forward, count one more
// observation) and non-nil for a newly executed failure (take its payload,
// keep the stored first-failure time).
func (s *LocalStore) advanceTaskFailure(key string, fresh *TaskFailure) *TaskFailure {
	if key == "" {
		return nil
	}
	s.ensureGeneration()
	release := s.lockShared()
	defer release()

	// The in-process mutex serializes this repo's own workers; the shared file
	// lock keeps GC out. Two CLI processes racing on the same key can still
	// settle on the same count — the counter is advisory provenance, never an
	// input to a verdict, so a lost increment costs accuracy and nothing else.
	s.mu.Lock()
	defer s.mu.Unlock()

	address := TaskFailureAddress(key)
	blobDir := s.blobDir(address)
	previous := loadTaskFailure(blobDir, address, key)

	next := TaskFailure{Format: CurrentTaskFailureFormat, Key: key}
	switch {
	case fresh != nil:
		next.ExitCode = fresh.ExitCode
		next.Result = fresh.Result
		next.FirstFailedAt = fresh.FirstFailedAt
	case previous != nil:
		next.ExitCode = previous.ExitCode
		next.Result = previous.Result
		next.FirstFailedAt = previous.FirstFailedAt
	default:
		return nil // nothing to replay
	}
	if previous != nil {
		next.Attempts = previous.Attempts
		if !previous.FirstFailedAt.IsZero() {
			next.FirstFailedAt = previous.FirstFailedAt
		}
	}
	next.Attempts++
	if next.FirstFailedAt.IsZero() {
		next.FirstFailedAt = time.Now()
	}

	if err := writeTaskFailure(blobDir, next); err != nil {
		return nil
	}
	writeLastUsed(blobDir, time.Now(), s.gen)
	next.Address = address
	return &next
}

// writeTaskFailure commits the record with stage-then-rename inside the blob
// directory, the same atomicity publishEntry gives a positive entry. The rename
// waits for a reader that holds the record open, which fails it on Windows
// (robustio).
func writeTaskFailure(blobDir string, record TaskFailure) error {
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(blobDir, "failure-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(tmpName)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if err := robustio.Rename(tmpName, filepath.Join(blobDir, taskFailureRecordFilename)); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// loadTaskFailure reads a negative entry, or nil when there is none this build
// can serve for this key. Every rejection is silent by design: an absent,
// torn, wrongly formatted, statusless or foreign-keyed record is a MISS, so the
// task simply runs again. A record that a writer is replacing is not absent:
// the read waits for the replacement (readReplacedFile).
func loadTaskFailure(blobDir, address, key string) *TaskFailure {
	data, err := readReplacedFile(filepath.Join(blobDir, taskFailureRecordFilename))
	if err != nil {
		return nil
	}
	var record TaskFailure
	if err := json.Unmarshal(data, &record); err != nil {
		return nil
	}
	if record.Format != CurrentTaskFailureFormat || record.Key != key {
		return nil
	}
	if record.Result == nil || record.Result.Status != "failed" {
		// A record that does not state a failure cannot replay one.
		return nil
	}
	if record.Attempts < 1 {
		return nil
	}
	record.Address = address
	return &record
}
