package refstore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

// TestADocumentAboveTheBoundNeverReachesTheBranch: a record the reader would
// refuse is never committed, so the branch does not move.
func TestADocumentAboveTheBoundNeverReachesTheBranch(t *testing.T) {
	spectest.Proves(t, feature, "readable-records", "a-record-above-the-bound-is-refused-before-writing")
	isolated(t)
	path := filepath.Join(t.TempDir(), "memory.git")
	s := Open(Config{Path: path})
	if _, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return record("a", "one"), nil
	}); err != nil {
		t.Fatal(err)
	}
	gitDir := "--git-dir=" + path
	before := run(t, nil, gitDir, "rev-parse", "refs/heads/"+DefaultBranch)
	_, _, err := s.Update(context.Background(), "a", func(string, *store.Document) (*store.Document, error) {
		return record("a", strings.Repeat("x", store.MaxDocumentBytes)), nil
	})
	var failure *store.Error
	if !errors.As(err, &failure) || failure.Outcome != store.Invalid || failure.Reason != "record.too_large" || failure.Retryable {
		t.Fatalf("a document above the bound: %v", err)
	}
	if after := run(t, nil, gitDir, "rev-parse", "refs/heads/"+DefaultBranch); after != before {
		t.Fatalf("the refused write moved the branch from %s to %s", before, after)
	}
}
