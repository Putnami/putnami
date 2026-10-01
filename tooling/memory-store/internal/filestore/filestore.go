// Package filestore is the file memory backend: a store directory holding
// the identity document store.json and one document per record under
// records/.
//
// Every write compares and writes under an exclusive lock on the store's lock
// file, so of two writers that start from one revision the second always
// sees the first's result: atomic replacement alone would let both land and
// the last silently win. A document is written to a temporary file, synced,
// and renamed over its name, then the directory is synced, so a reader never
// sees a torn document and a crash leaves either the old or the new one.
// Readers take no lock: each document they read is whole.
package filestore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

// LockFile is the lock file inside the store directory. It is never removed.
const LockFile = "lock"

// DefaultLockTimeout bounds the wait for the store lock.
const DefaultLockTimeout = 10 * time.Second

// Store is one store directory.
type Store struct {
	root string
	// LockTimeout bounds the wait for the store lock; a write that cannot
	// take it in time is unavailable and writes nothing.
	LockTimeout time.Duration
}

// Open names a store directory. Nothing is created until the first write.
func Open(root string) *Store {
	return &Store{root: root, LockTimeout: DefaultLockTimeout}
}

// Kind is the reference source kind of a file store.
func (s *Store) Kind() string { return "file" }

// Read returns a view of the store directory.
func (s *Store) Read(_ context.Context) (store.View, error) {
	info, err := os.Stat(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return &view{root: s.root}, nil
	}
	if err != nil {
		return nil, store.Failf(store.Unavailable, "store.unreadable", true, "read the memory store: %v", err)
	}
	if !info.IsDir() {
		return nil, store.Failf(store.Unavailable, "store.unreadable", false, "the memory store %s is not a directory", s.root)
	}
	meta, err := readMeta(s.root)
	if err != nil {
		return nil, err
	}
	return &view{root: s.root, id: meta.ID}, nil
}

// Update compares and writes one record under the store lock. A record that
// does not encode within the bound is refused before it is written.
func (s *Store) Update(ctx context.Context, id string, change store.Change) (*store.Document, string, error) {
	records := filepath.Join(s.root, store.RecordsDir)
	if err := os.MkdirAll(records, 0o755); err != nil {
		return nil, "", store.Failf(store.Unavailable, "store.unwritable", false, "create the memory store: %v", err)
	}
	lock, err := acquire(ctx, filepath.Join(s.root, LockFile), s.LockTimeout)
	if err != nil {
		return nil, "", err
	}
	defer lock.release()

	meta, err := readMeta(s.root)
	if err != nil {
		return nil, "", err
	}
	if meta.ID == "" {
		if err := refuseOrphanRecords(records); err != nil {
			return nil, "", err
		}
		if meta, err = store.NewMeta(); err != nil {
			return nil, "", store.Failf(store.Unavailable, "store.unwritable", false, "%v", err)
		}
		if err := writeAtomically(s.root, store.MetaPath, store.EncodeMeta(meta)); err != nil {
			// The identity alone names an empty store: whether or not it
			// landed, no record was written.
			return nil, "", store.Failf(store.Unavailable, "store.unwritable", false, "%v", err)
		}
	}
	current, err := readRecord(s.root, id)
	if err != nil {
		return nil, "", err
	}
	next, err := change(meta.ID, current)
	if err != nil {
		return nil, "", err
	}
	record, err := store.Encode(next)
	if err != nil {
		return nil, "", err
	}
	if err := writeAtomically(records, store.RecordFileName(id), record); err != nil {
		var landed *landedError
		if errors.As(err, &landed) {
			return nil, "", store.Failf(store.Unresolved, "store.unsynced", false, "%v", err)
		}
		return nil, "", store.Failf(store.Unavailable, "store.unwritable", false, "%v", err)
	}
	return next, meta.ID, nil
}

type view struct {
	root string
	id   string
}

func (v *view) StoreID() string { return v.id }

func (v *view) Get(_ context.Context, id string) (*store.Document, error) {
	return readRecord(v.root, id)
}

func (v *view) List(_ context.Context) ([]*store.Document, error) {
	entries, err := os.ReadDir(filepath.Join(v.root, store.RecordsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, store.Failf(store.Unavailable, "store.unreadable", true, "list the memory store: %v", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if id, ok := store.IDFromFileName(entry.Name()); ok && entry.Type().IsRegular() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	docs := make([]*store.Document, 0, len(ids))
	for _, id := range ids {
		doc, err := readRecord(v.root, id)
		if err != nil {
			return nil, err
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

// refuseOrphanRecords refuses to give an identity to a store whose records/
// already holds a record: those records were written under an identity that
// is gone, and a new one would change the source of every reference to them.
func refuseOrphanRecords(records string) error {
	entries, err := os.ReadDir(records)
	if err != nil {
		return store.Failf(store.Unavailable, "store.unreadable", true, "list the memory store: %v", err)
	}
	for _, entry := range entries {
		if _, ok := store.IDFromFileName(entry.Name()); ok && entry.Type().IsRegular() {
			return store.Failf(store.Unavailable, "store.invalid", false,
				"the memory store holds records but no %s identity document; nothing was written", store.MetaPath)
		}
	}
	return nil
}

func readMeta(root string) (store.Meta, error) {
	data, err := os.ReadFile(filepath.Join(root, store.MetaPath))
	if errors.Is(err, fs.ErrNotExist) {
		return store.Meta{}, nil
	}
	if err != nil {
		return store.Meta{}, store.Failf(store.Unavailable, "store.unreadable", true, "read the memory store identity: %v", err)
	}
	meta, err := store.DecodeMeta(data)
	if err != nil {
		return store.Meta{}, store.Failf(store.Unavailable, "store.invalid", false, "%v", err)
	}
	return meta, nil
}

// readRecord returns the document id, or nil when there is none. A record
// removed between a listing and its read is absent, not an error.
func readRecord(root, id string) (*store.Document, error) {
	data, err := os.ReadFile(filepath.Join(root, store.RecordsDir, store.RecordFileName(id)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, store.Failf(store.Unavailable, "store.unreadable", true, "read memory record %s: %v", id, err)
	}
	doc, err := store.Decode(id, data)
	if err != nil {
		return nil, store.Failf(store.Unavailable, "store.invalid", false, "%v", err)
	}
	return doc, nil
}

// syncDirectory makes a rename in a directory durable.
var syncDirectory = filelock.SyncDir

// landedError is a write whose rename happened and whose durability is in
// doubt.
type landedError struct{ err error }

func (e *landedError) Error() string { return e.err.Error() }

func (e *landedError) Unwrap() error { return e.err }

// writeAtomically writes a temporary file, syncs it, renames it over name,
// and syncs the directory. A failure before the rename leaves the old
// document; a failure to sync the directory after it is a *landedError.
func writeAtomically(dir, name string, data []byte) error {
	temp, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	tempName := temp.Name()
	fail := func(stage string, err error) error {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return fmt.Errorf("%s %s: %w", stage, name, err)
	}
	if _, err := temp.Write(data); err != nil {
		return fail("write", err)
	}
	if err := temp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("close %s: %w", name, err)
	}
	// A reader of the document holds it open without sharing delete access;
	// on Windows the rename over it fails until that reader closes it.
	if err := robustio.Rename(tempName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tempName)
		return fmt.Errorf("replace %s: %w", name, err)
	}
	if err := syncDirectory(dir); err != nil {
		return &landedError{err: fmt.Errorf("sync the directory of %s: %w", name, err)}
	}
	return nil
}
