//go:build unix

package filestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

const feature = "tooling/memory-store"

func TestRecordsWithoutAnIdentityAreNeverGivenANewOne(t *testing.T) {
	spectest.Proves(t, feature, "explicit-outage", "records-without-an-identity-are-never-given-a-new-one")
	root := t.TempDir()
	s := Open(root)
	if _, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return document("one"), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, store.MetaPath)); err != nil {
		t.Fatal(err)
	}
	ran := false
	_, _, err := s.Update(context.Background(), "b", func(string, *store.Document) (*store.Document, error) {
		ran = true
		return document("two"), nil
	})
	var failure *store.Error
	if !errors.As(err, &failure) || failure.Outcome != store.Unavailable || failure.Reason != "store.invalid" || failure.Retryable || ran {
		t.Fatalf("a write to records without an identity: %v (change ran: %v)", err, ran)
	}
	if _, err := os.Stat(filepath.Join(root, store.MetaPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused write minted an identity: %v", err)
	}
}

func TestALandedWriteWhoseDirectorySyncFailedIsUnresolved(t *testing.T) {
	spectest.Proves(t, feature, "retries-never-duplicate", "a-landed-write-whose-sync-failed-is-unresolved")
	root := t.TempDir()
	s := Open(root)
	if _, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return document("one"), nil
	}); err != nil {
		t.Fatal(err)
	}
	syncDirectory = func(string) error { return errors.New("the disk refused the sync") }
	t.Cleanup(func() { syncDirectory = filelock.SyncDir })
	_, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return document("two"), nil
	})
	var failure *store.Error
	if !errors.As(err, &failure) || failure.Outcome != store.Unresolved || failure.Reason != "store.unsynced" || failure.Retryable {
		t.Fatalf("a write whose directory sync failed: %v", err)
	}
	view, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := view.Get(context.Background(), "a"); err != nil || doc == nil || doc.Content != "two" {
		t.Fatalf("the renamed document: %+v (%v)", doc, err)
	}
}
