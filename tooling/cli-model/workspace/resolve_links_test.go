package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveLinksFollowsALinkAndReportsAMissingPath(t *testing.T) {
	real := CanonicalRoot(t.TempDir())
	target := filepath.Join(real, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveLinks(target); err != nil || got != target {
		t.Fatalf("ResolveLinks(%s) = %q, %v, want the directory itself", target, got, err)
	}
	if _, err := ResolveLinks(filepath.Join(target, "missing", "tail")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ResolveLinks of a missing tail = %v, want fs.ErrNotExist", err)
	}
	link := filepath.Join(real, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, err := ResolveLinks(filepath.Join(link, ".")); err != nil || got != target {
		t.Fatalf("ResolveLinks(%s) = %q, %v, want %q", link, got, err, target)
	}
	if got := CanonicalRoot(link); got != target {
		t.Fatalf("CanonicalRoot(%s) = %q, want %q", link, got, target)
	}
}
