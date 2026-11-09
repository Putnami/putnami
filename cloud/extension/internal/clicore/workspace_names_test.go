package clicore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectLookupsReadTheWorkspaceNames covers a project whose putnami.json
// declares no `name`: the workspace names it from its provider. With the
// membership of the job context, every lookup answers to that name; without
// one, the lookups read putnami.json as before.
func TestProjectLookupsReadTheWorkspaceNames(t *testing.T) {
	root := t.TempDir()
	library := filepath.Join(root, "php", "samples", "library")
	api := filepath.Join(root, "apps", "api")
	writeNamesFixture(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"acme"}`)
	writeNamesFixture(t, filepath.Join(library, "putnami.json"), `{"tags":["php"]}`)
	writeNamesFixture(t, filepath.Join(api, "putnami.json"), `{"name":"apps/api"}`)
	t.Cleanup(func() { UseWorkspaceProjects(nil) })

	UseWorkspaceProjects([]WorkspaceProject{
		{Name: "putnami-example/library", Dir: library},
		{Name: "apps/api", Dir: api},
	})
	if name, err := ProjectNameAt(library); err != nil || name != "putnami-example/library" {
		t.Fatalf("ProjectNameAt(library) = %q, %v", name, err)
	}
	for _, token := range []string{"putnami-example/library", "library"} {
		dir, err := FindAppDir(root, token)
		if err != nil || !SamePath(dir, canonicalDir(library)) {
			t.Fatalf("FindAppDir(%q) = %q, %v; want %s", token, dir, err, library)
		}
	}
	if canon, err := CanonicalProjectName(root, "library"); err != nil || canon != "putnami-example/library" {
		t.Fatalf("CanonicalProjectName(library) = %q, %v", canon, err)
	}
	if name, ok := FindActiveProject(filepath.Join(library, "src"), root); !ok || name != "putnami-example/library" {
		t.Fatalf("FindActiveProject(library/src) = %q, %v", name, ok)
	}
	keys := WorkspaceProjectKeys(root)
	for _, key := range []string{"putnami-example/library", "php/samples/library", "apps/api"} {
		if !keys[key] {
			t.Errorf("WorkspaceProjectKeys misses %q: %v", key, keys)
		}
	}
	if keys["acme"] {
		t.Errorf("WorkspaceProjectKeys holds the workspace itself, which the membership does not name: %v", keys)
	}

	UseWorkspaceProjects(nil)
	if name, _ := ProjectNameAt(library); name != "" {
		t.Fatalf("without a membership ProjectNameAt(library) = %q, want the putnami.json name, none", name)
	}
	if _, err := FindAppDir(root, "putnami-example/library"); err == nil || !strings.Contains(err.Error(), "no workspace project named") {
		t.Fatalf("without a membership FindAppDir = %v, want not found", err)
	}
	if dir, err := FindAppDir(root, "api"); err != nil || !SamePath(dir, canonicalDir(api)) {
		t.Fatalf("without a membership FindAppDir(api) = %q, %v", dir, err)
	}
}

func writeNamesFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
