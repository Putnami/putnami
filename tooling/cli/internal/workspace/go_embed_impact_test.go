package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestGoEmbedSelectorSelectsChangedAndDeletedTargets(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "go-embed-selector-selects-changed-and-deleted-targets")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package p\n//go:embed assets/*.sql\nvar sql []byte\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !selectsTaskPath(root, "assets/removed.sql", []string{"**/*.go", "go-embed:build"}) {
		t.Fatal("deleted embedded asset lost its owning task")
	}
	if selectsTaskPath(root, "assets/unrelated.txt", []string{"**/*.go", "go-embed:build"}) {
		t.Fatal("unrelated asset selected")
	}
	if selectsTaskPath(root, "assets/removed.sql", []string{"**/*.go"}) {
		t.Fatal("plain Go glob claimed SQL")
	}
}

func TestGoEmbedSelectorSelectsSourceSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".source.txt"), []byte("package p\n//go:embed assets/*.sql\nvar sql []byte\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".source.txt", filepath.Join(root, "embed.go")); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	if !selectsTaskPath(root, "assets/removed.sql", []string{"**/*.go", "go-embed:build"}) {
		t.Fatal("deleted embedded asset lost its source-link owner")
	}
	if !selectsTaskPath(root, ".source.txt", []string{"**/*.go", "go-embed:build"}) {
		t.Fatal("source-link target edit did not select its owner")
	}
}
