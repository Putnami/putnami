package runnerprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	runner "go.putnami.dev/protocol/runner"
)

// AttemptRecord is the durable, client-side state of one submission. It lives
// under .putnami/runner/attempts/<submission>.json so that a CLI which dies
// mid-run — killed, crashed, or interrupted between an acknowledgement and its
// receipt — leaves behind exactly what the next process needs to resolve the
// same work instead of running it again: the idempotency key, the attempt
// reference once acknowledged, the last cursor observed, and the terminal
// outcome once known. It is the transport's ledger; the canonical verdict is
// the imported session it eventually names.
//
// The record carries observations only. Nothing in it is a task-cache input,
// and nothing in it can turn a remote failure, a missing bundle or a failed
// import into a local success.
type AttemptRecord struct {
	// Version is AttemptRecordVersion.
	Version int `json:"version"`
	// Submission is the request's idempotency key, the record's file name.
	Submission string `json:"submission"`
	// Provider is the extension name the submission went through.
	Provider string `json:"provider"`
	// InputDigest is the execution-input digest of the bound request.
	InputDigest string `json:"inputDigest"`
	// SourceDigest is the source-manifest digest of the captured snapshot.
	SourceDigest string `json:"sourceDigest"`
	// Commands are the commands the request carried, for listings.
	Commands []string `json:"commands"`
	// Attempt is the provider's attempt reference, empty until acknowledged.
	Attempt string `json:"attempt,omitempty"`
	// State is StateSubmitting until the provider acknowledged the submission,
	// then the last provider lifecycle state observed.
	State string `json:"state"`
	// Cursor is the last output cursor forwarded to the user; a reconnect
	// resumes after it and drops anything at or below it.
	Cursor int64 `json:"cursor"`
	// ExitCode is the executing engine's exit code once the provider reported
	// it, absent before.
	ExitCode *int `json:"exitCode,omitempty"`
	// SessionID is the imported gate session, empty until the import succeeded
	// or when the remote engine refused before recording one.
	SessionID string `json:"sessionId,omitempty"`
	// Imported reports that the bundle was imported (SessionID may still be
	// empty for a refusal that recorded no session).
	Imported bool `json:"imported,omitempty"`
	// Error is the last retrieval or import failure, verbatim, kept beside the
	// remote verdict it could not bring home. Cleared on success.
	Error string `json:"error,omitempty"`
	// SubmittedAt is when this process first wrote the record, RFC 3339 UTC.
	SubmittedAt string `json:"submittedAt"`
	// UpdatedAt is when the record last changed, RFC 3339 UTC.
	UpdatedAt string `json:"updatedAt"`
}

// AttemptRecordVersion is the record document version.
const AttemptRecordVersion = 1

// StateSubmitting is the client-only state between writing the record and
// receiving the provider's acknowledgement. A record in this state with no
// attempt reference is exactly the lost-acknowledgement case: the next process
// looks the key up before it submits anything.
const StateSubmitting = "submitting"

// attemptsDirName is the workspace-relative directory of attempt records.
const attemptsDirName = ".putnami/runner/attempts"

// maxAttemptRecordBytes bounds one record file.
const maxAttemptRecordBytes = 64 << 10

// ErrAttemptNotFound reports a reference no attempt record names.
var ErrAttemptNotFound = errors.New("no attempt record names this reference")

// AttemptStore reads and writes attempt records for one workspace.
type AttemptStore struct {
	dir string
}

// NewAttemptStore returns the store under the workspace's .putnami directory.
func NewAttemptStore(wsRoot string) *AttemptStore {
	return &AttemptStore{dir: filepath.Join(wsRoot, filepath.FromSlash(attemptsDirName))}
}

// Dir is the records directory.
func (s *AttemptStore) Dir() string { return s.dir }

// Settled reports whether the provider's last observed state can no longer
// change: a terminal provider state. A submitting record is never settled.
func (r *AttemptRecord) Settled() bool {
	return runner.TerminalState(r.State)
}

// Write publishes the record atomically: staged beside its final name, synced
// and renamed into place, so a reader never sees a torn record and a crash
// between two writes leaves the previous one intact.
func (s *AttemptStore) Write(record *AttemptRecord) error {
	if record == nil || !validSubmissionKey(record.Submission) {
		return fmt.Errorf("attempt record needs a valid submission key")
	}
	record.Version = AttemptRecordVersion
	record.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if record.SubmittedAt == "" {
		record.SubmittedAt = record.UpdatedAt
	}
	if record.Commands == nil {
		record.Commands = []string{}
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	final := filepath.Join(s.dir, record.Submission+".json")
	temp, err := os.CreateTemp(s.dir, "."+record.Submission+"-*.tmp")
	if err != nil {
		return err
	}
	_, writeErr := temp.Write(append(data, '\n'))
	syncErr := temp.Sync()
	closeErr := temp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(temp.Name())
		return err
	}
	if err := os.Rename(temp.Name(), final); err != nil {
		_ = os.Remove(temp.Name())
		return err
	}
	return nil
}

// Read loads one record by submission key.
func (s *AttemptStore) Read(submission string) (*AttemptRecord, error) {
	if !validSubmissionKey(submission) {
		return nil, fmt.Errorf("%w: %q is not a submission key", ErrAttemptNotFound, submission)
	}
	return readAttemptRecord(filepath.Join(s.dir, submission+".json"))
}

// List returns every readable record, newest submission first. Unreadable
// files are skipped: a torn or foreign file must not hide the others.
func (s *AttemptStore) List() ([]*AttemptRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []*AttemptRecord
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || !validSubmissionKey(strings.TrimSuffix(name, ".json")) {
			continue
		}
		record, err := readAttemptRecord(filepath.Join(s.dir, name))
		if err != nil {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].SubmittedAt != records[j].SubmittedAt {
			return records[i].SubmittedAt > records[j].SubmittedAt
		}
		return records[i].Submission > records[j].Submission
	})
	return records, nil
}

// Find resolves a reference: an attempt reference the provider issued, or the
// submission key. Both are opaque to the user; both name one record.
func (s *AttemptStore) Find(ref string) (*AttemptRecord, error) {
	if ref == "" {
		return nil, fmt.Errorf("%w: empty reference", ErrAttemptNotFound)
	}
	records, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Submission == ref || (record.Attempt != "" && record.Attempt == ref) {
			return record, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrAttemptNotFound, ref)
}

// Pending returns the one record that still describes work in flight for the
// same inputs through the same provider: a submission whose acknowledgement
// may have been lost, or an attempt the provider has not brought to a terminal
// state. A settled record never blocks a new submission — running the same
// gate again after it finished is an intentional retry and gets a new attempt.
func (s *AttemptStore) Pending(provider, inputDigest string) (*AttemptRecord, error) {
	records, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Provider == provider && record.InputDigest == inputDigest && !record.Settled() {
			return record, nil
		}
	}
	return nil, nil
}

// Prune removes the oldest records that no longer describe anything to
// resolve — settled and imported, or settled with the verdict already brought
// home — beyond keep of them. A record still in flight, or one whose remote
// verdict was never imported, is never pruned: it is the only handle on work
// the provider may still hold.
func (s *AttemptStore) Prune(keep int) error {
	if keep <= 0 {
		return nil
	}
	records, err := s.List()
	if err != nil {
		return err
	}
	retained := 0
	for _, record := range records {
		if !record.Settled() || !record.Imported {
			continue
		}
		retained++
		if retained <= keep {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, record.Submission+".json")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func readAttemptRecord(path string) (*AttemptRecord, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrAttemptNotFound, filepath.Base(path))
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxAttemptRecordBytes {
		return nil, fmt.Errorf("attempt record %s is not a bounded regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record AttemptRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("attempt record %s: %w", path, err)
	}
	if record.Version != AttemptRecordVersion || record.Submission != strings.TrimSuffix(filepath.Base(path), ".json") {
		return nil, fmt.Errorf("attempt record %s has version %d or names another submission", path, record.Version)
	}
	if record.State != StateSubmitting && !runner.KnownState(record.State) {
		return nil, fmt.Errorf("attempt record %s carries unknown state %q", path, record.State)
	}
	return &record, nil
}

func validSubmissionKey(key string) bool {
	if len(key) != 32 {
		return false
	}
	for i := 0; i < len(key); i++ {
		b := key[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}
