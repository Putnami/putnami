package specreport

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// An Attribution resolves a path once, retries one it could not resolve, and
// a new Attribution sees a link that moved since the last one.
func TestAttributionCachesOnlyResolvedPaths(t *testing.T) {
	dir := t.TempDir()
	projectA := filepath.Join(dir, "a")
	projectB := filepath.Join(dir, "b")
	for _, project := range []string{projectA, projectB} {
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "spec_test.go"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link")
	fragment := spectest.Fragment{File: filepath.Join(link, "spec_test.go")}

	attribution := NewAttribution()
	if attribution.OwnedBy(fragment, projectA) {
		t.Fatal("a fragment under a missing link is owned")
	}
	if err := os.Symlink(projectA, link); err != nil {
		t.Fatal(err)
	}
	if !attribution.OwnedBy(fragment, projectA) {
		t.Fatal("a failed resolution was kept: the link that appeared is not followed")
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(projectB, link); err != nil {
		t.Fatal(err)
	}
	if !attribution.OwnedBy(fragment, projectA) {
		t.Fatal("an attribution resolved the same path twice")
	}
	if !OwnedBy(fragment, projectB) || OwnedBy(fragment, projectA) {
		t.Fatal("a new attribution does not see the moved link")
	}
}

// The zero Attribution resolves as NewAttribution does.
func TestZeroAttributionResolves(t *testing.T) {
	project := t.TempDir()
	file := filepath.Join(project, "spec_test.go")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var attribution Attribution
	if !attribution.OwnedBy(spectest.Fragment{File: file}, project) {
		t.Fatal("the zero attribution does not own a fragment inside its project")
	}
}
