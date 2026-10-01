package template

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
)

// An installed template is an artifact store entry, and the store writes its
// recency sidecar into every entry it serves. Rendering the template must copy
// the template's files and none of the store's bookkeeping: a `lastused` file
// in a new project gets committed by `init`.
func TestRenderDirLeavesTheArtifactStoreBookkeepingBehind(t *testing.T) {
	store := artifactstore.New(filepath.Join(t.TempDir(), "artifacts"))
	digest := strings.Repeat("ab", 32)
	entry, err := store.Admit(digest, func(stageDir string) error {
		for rel, content := range map[string]string{
			ManifestFilename:                        `{"name":"probe"}`,
			"main.go":                               "package main\n",
			filepath.Join("config", "lastused.txt"): "payload\n",
			filepath.Join("config", "lastused"):     "nested payload\n",
		} {
			path := filepath.Join(stageDir, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("admit the template: %v", err)
	}
	store.Touch(digest)
	if _, err := os.Stat(filepath.Join(entry, "lastused")); err != nil {
		t.Fatalf("the store did not stamp the entry, so this test proves nothing: %v", err)
	}
	// The temporary names the sidecar writers rename into place.
	for _, name := range []string{"lastused-123456", ".lastused.4242"} {
		if err := os.WriteFile(filepath.Join(entry, name), []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "templates", "probe")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dirlink.Create(entry, link); err != nil {
		t.Fatalf("create the template link: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "api")
	if err := RenderDir(link, dst, RenderVars{ProjectName: "api"}); err != nil {
		t.Fatalf("RenderDir: %v", err)
	}

	var got []string
	err = filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dst, path)
		got = append(got, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"config/lastused", "config/lastused.txt", "main.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("rendered files = %v, want %v", got, want)
	}
}
