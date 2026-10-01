// Package store is the local collaboration provider's file store: one JSON
// document holding every task, proposal, review and idempotency key of one
// store directory.
//
// Every write is a read-modify-write under an exclusive lock on the store's
// lock file, and lands by atomic rename, so concurrent writers serialize, a
// comparison against a revision and the write that follows it are one step,
// and a reader never sees a torn document. A writer waits for the lock at
// most Store.LockTimeout, then fails with ErrBusy having written nothing.
// Readers take no lock: the rename is their consistency.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/robustio"
)

// FormatVersion is the version of the state document this package reads and
// writes. A document of another version is refused, never reinterpreted.
const FormatVersion = 1

const (
	stateFile = "state.json"
	lockFile  = "lock"
)

// State is the whole store.
type State struct {
	Version int `json:"version"`
	// StoreID qualifies every reference this store issues: its source is
	// "local:<StoreID>", so references from two stores never collide.
	StoreID   string            `json:"storeId"`
	Next      Counters          `json:"next"`
	Tasks     []collab.Task     `json:"tasks"`
	Proposals []collab.Proposal `json:"proposals"`
	Reviews   []Review          `json:"reviews"`
	// Keys maps the SHA-256 of an operation and an idempotency key it was
	// given to the write it produced.
	Keys map[string]Key `json:"keys"`
}

// Counters allocate identifiers; they only grow.
type Counters struct {
	Task     int `json:"task"`
	Proposal int `json:"proposal"`
	Review   int `json:"review"`
}

// Review is a published review, its body, and the proposal it belongs to.
type Review struct {
	Proposal string        `json:"proposal"`
	Body     string        `json:"body"`
	Review   collab.Review `json:"review"`
}

// Key records the write an idempotency key produced and a digest of the
// request that produced it.
type Key struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

// Source is the reference source of this store.
func (s *State) Source() string {
	if s.StoreID == "" {
		return ""
	}
	return "local:" + s.StoreID
}

// ErrUnsupportedFormat refuses a state document of an unknown version.
var ErrUnsupportedFormat = errors.New("the store was written by an unsupported version of the local provider")

// DefaultLockTimeout bounds the wait for the store lock.
const DefaultLockTimeout = 10 * time.Second

// ErrBusy is a write that could not take the store lock in time because
// another writer held it. Nothing was written.
var ErrBusy = errors.New("another writer held the local store lock")

// Store is one store directory.
type Store struct {
	root string
	// LockTimeout bounds the wait for the store lock.
	LockTimeout time.Duration
}

// Open names a store directory. Nothing is created until the first write.
func Open(root string) *Store {
	return &Store{root: root, LockTimeout: DefaultLockTimeout}
}

// Root is the store directory.
func (s *Store) Root() string { return s.root }

// Read returns the current state. A store that was never written is empty
// and has no StoreID yet.
func (s *Store) Read() (*State, error) {
	data, err := os.ReadFile(filepath.Join(s.root, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return emptyState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the local store: %w", err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("the local store %s is not a valid state document: %w", filepath.Join(s.root, stateFile), err)
	}
	if state.Version != FormatVersion {
		return nil, fmt.Errorf("%w (format %d, this provider reads %d)", ErrUnsupportedFormat, state.Version, FormatVersion)
	}
	if state.Keys == nil {
		state.Keys = map[string]Key{}
	}
	return &state, nil
}

func emptyState() *State {
	return &State{Version: FormatVersion, Keys: map[string]Key{}}
}

// WriteError is a failed Update. Landed reports whether the new state may be
// on disk: false means nothing was written, true that the rename happened and
// only its durability is in doubt.
type WriteError struct {
	Landed bool
	Err    error
}

func (e *WriteError) Error() string { return e.Err.Error() }

func (e *WriteError) Unwrap() error { return e.Err }

// Update applies change to the current state under the store's exclusive
// lock and writes the result. When change returns an error nothing is
// written and that error is returned as is. A lock another writer holds
// longer than LockTimeout fails the update with ErrBusy. A store written for
// the first time receives its StoreID before change runs, so change can
// issue references.
func (s *Store) Update(change func(*State) error) (*State, error) {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, &WriteError{Err: fmt.Errorf("create the local store: %w", err)}
	}
	lock, err := acquire(filepath.Join(s.root, lockFile), s.LockTimeout)
	if err != nil {
		return nil, &WriteError{Err: fmt.Errorf("lock the local store: %w", err)}
	}
	defer lock.release()

	state, err := s.Read()
	if err != nil {
		return nil, &WriteError{Err: err}
	}
	if state.StoreID == "" {
		id, err := newStoreID()
		if err != nil {
			return nil, &WriteError{Err: err}
		}
		state.StoreID = id
	}
	if err := change(state); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, &WriteError{Err: fmt.Errorf("encode the local store: %w", err)}
	}
	if err := writeAtomically(s.root, stateFile, append(encoded, '\n')); err != nil {
		return nil, err
	}
	return state, nil
}

// writeAtomically writes a temporary file, syncs it, and renames it over
// name. A failure before the rename leaves the old state; a failure to sync
// the directory after it is reported as landed.
func writeAtomically(dir, name string, data []byte) error {
	temp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return &WriteError{Err: fmt.Errorf("write the local store: %w", err)}
	}
	tempName := temp.Name()
	cleanup := func() { _ = os.Remove(tempName) }
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		cleanup()
		return &WriteError{Err: fmt.Errorf("write the local store: %w", err)}
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		cleanup()
		return &WriteError{Err: fmt.Errorf("sync the local store: %w", err)}
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return &WriteError{Err: fmt.Errorf("close the local store: %w", err)}
	}
	// A reader of the state file holds it open without sharing delete access;
	// on Windows the rename over it fails until that reader closes it.
	if err := robustio.Rename(tempName, filepath.Join(dir, name)); err != nil {
		cleanup()
		return &WriteError{Err: fmt.Errorf("replace the local store: %w", err)}
	}
	if err := filelock.SyncDir(dir); err != nil {
		return &WriteError{Landed: true, Err: fmt.Errorf("sync the local store directory: %w", err)}
	}
	return nil
}

func newStoreID() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", &WriteError{Err: fmt.Errorf("allocate a store id: %w", err)}
	}
	return hex.EncodeToString(raw[:]), nil
}
