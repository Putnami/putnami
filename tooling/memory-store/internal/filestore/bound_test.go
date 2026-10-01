//go:build unix

package filestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

// TestADocumentAboveTheBoundIsRefusedBeforeAnythingIsWritten: a record the
// reader would refuse is never written, and an existing one stays as it was.
func TestADocumentAboveTheBoundIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	spectest.Proves(t, feature, "readable-records", "a-record-above-the-bound-is-refused-before-writing")
	root := t.TempDir()
	s := Open(root)
	_, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return document(strings.Repeat("x", store.MaxDocumentBytes)), nil
	})
	var failure *store.Error
	if !errors.As(err, &failure) || failure.Outcome != store.Invalid || failure.Reason != "record.too_large" || failure.Retryable {
		t.Fatalf("a document above the bound: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, store.RecordPath("a"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused write left a record: %v", err)
	}

	if _, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return document("one"), nil
	}); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return document(strings.Repeat("x", store.MaxDocumentBytes)), nil
	})
	if !errors.As(err, &failure) || failure.Outcome != store.Invalid {
		t.Fatalf("a document above the bound over an existing record: %v", err)
	}
	view, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := view.Get(context.Background(), "a"); err != nil || doc == nil || doc.Content != "one" {
		t.Fatalf("the refused write changed the record: %+v (%v)", doc, err)
	}
}
