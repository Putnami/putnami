//go:build unix

package filestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

func document(content string) *store.Document {
	return &store.Document{
		Format: store.FormatVersion, ID: "a", Kind: collab.MemoryKindNote, Content: content, Sequence: 1,
		Provenance: collab.Provenance{RecordedAt: "2026-09-24T08:00:00Z"}, UpdatedAt: "2026-09-24T08:00:00Z",
	}
}

func TestAHeldLockMakesAWriteUnavailable(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	s.LockTimeout = 50 * time.Millisecond
	if _, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return document("one"), nil
	}); err != nil {
		t.Fatal(err)
	}

	fd, err := syscall.Open(filepath.Join(root, LockFile), syscall.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ran := false
	_, _, err = s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		ran = true
		return document("two"), nil
	})
	var failure *store.Error
	if !errors.As(err, &failure) || failure.Outcome != store.Unavailable || !failure.Retryable || failure.Reason != "store.busy" || ran {
		t.Fatalf("a write under a held lock: %v (change ran: %v)", err, ran)
	}
	_ = syscall.Flock(fd, syscall.LOCK_UN)
	_ = syscall.Close(fd)

	written, id, err := s.Update(context.Background(), "a", func(_ string, current *store.Document) (*store.Document, error) {
		if current == nil || current.Content != "one" {
			t.Fatalf("the refused write changed the record: %+v", current)
		}
		return document("two"), nil
	})
	if err != nil || written.Content != "two" || id == "" {
		t.Fatalf("after the lock was released: %v %+v", err, written)
	}
}

func TestAChangeThatRefusesWritesNothing(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	refusal := errors.New("refused")
	if _, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return nil, refusal
	}); !errors.Is(err, refusal) {
		t.Fatalf("the refusal was not returned: %v", err)
	}
	view, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	docs, err := view.List(context.Background())
	if err != nil || len(docs) != 0 {
		t.Fatalf("a refused change wrote %v (%v)", docs, err)
	}
	if view.StoreID() == "" {
		t.Fatal("the store identity is written before the first change runs")
	}
}

func TestAReadOfAMissingStoreIsEmpty(t *testing.T) {
	view, err := Open(filepath.Join(t.TempDir(), "absent")).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := view.Get(context.Background(), "a")
	if err != nil || doc != nil || view.StoreID() != "" {
		t.Fatalf("a missing store: %v %v %q", doc, err, view.StoreID())
	}
	docs, err := view.List(context.Background())
	if err != nil || docs != nil {
		t.Fatalf("a missing store lists %v (%v)", docs, err)
	}
}

func TestAnInterruptedWriteLeavesTheOldDocument(t *testing.T) {
	root := t.TempDir()
	s := Open(root)
	if _, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return document("one"), nil
	}); err != nil {
		t.Fatal(err)
	}
	// A writer died after writing its temporary file and before renaming it.
	records := filepath.Join(root, store.RecordsDir)
	if err := os.WriteFile(filepath.Join(records, ".a.json.4242.tmp"), []byte(`{"format":1,"id":"a","content":"to`), 0o644); err != nil {
		t.Fatal(err)
	}
	view, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	docs, err := view.List(context.Background())
	if err != nil || len(docs) != 1 || docs[0].Content != "one" {
		t.Fatalf("after an interrupted write: %+v (%v)", docs, err)
	}
	if err := writeAtomically(filepath.Join(root, "absent"), "x.json", []byte("x")); err == nil {
		t.Fatal("a write into a missing directory succeeded")
	}
}
