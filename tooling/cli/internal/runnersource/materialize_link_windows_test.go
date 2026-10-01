//go:build windows

package runnersource

import (
	"os"
	"slices"
	"strings"
	"testing"

	runner "go.putnami.dev/protocol/runner"
)

// A runner on Windows refuses a manifest symlink by name and leaves no partial
// source tree behind, rather than materializing a tree without the link.
func TestMaterializeRefusesSymlinkOnWindows(t *testing.T) {
	repo, store := sourceFixture(t)
	manifest := captureSource(t, store, repo).Manifest
	manifest.Entries = append(slices.Clone(manifest.Entries), runner.SourceEntry{Path: "zz-link", Kind: "symlink", Target: "tracked"})
	if err := runner.ValidateSourceManifest(manifest); err != nil {
		t.Fatalf("fixture manifest: %v", err)
	}

	parent := t.TempDir()
	if _, err := store.Materialize(parent, manifest); err == nil ||
		!strings.Contains(err.Error(), "source entry zz-link is a symbolic link to tracked") {
		t.Fatalf("Materialize error = %v, want a refusal naming zz-link", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("refused materialization left %v (%v)", entries, err)
	}
}
