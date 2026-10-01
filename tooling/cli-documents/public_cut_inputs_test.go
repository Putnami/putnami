package documents

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/store"
)

// Exercise the actual declaration, collector and scanner together. A marker
// in an unrelated project's new document must invalidate a warm pass before
// staging; ignored manifests and private worktree state must remain invisible.
func TestPublicCutCandidateInputsInvalidateTheRecordedVerdict(t *testing.T) {
	patterns := declaredTestFilePatterns(t, cliDocumentsProjectDir)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publicCutRunGit(t, root, "init")
	write := func(path, data string) {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", ".context/\nworktrees/\n.putnami/\n")
	write("README.md", "public\n")
	publicCutRunGit(t, root, "add", ".")
	project := filepath.Join(root, cliDocumentsProjectDir)
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	key := func() string {
		t.Helper()
		input := &store.CacheKey{ProjectRoot: project, FilePatterns: patterns}
		digest, err := input.ComputeHashUsing(store.NewCacheManager(nil))
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	before := key()
	for _, path := range []string{".context/putnami.json", "worktrees/local/putnami.json", ".putnami/cache/data"} {
		write(path, "local state")
	}
	if key() != before {
		t.Fatal("ignored local candidates changed the release key")
	}
	path := "tooling/clientgen-extension/doc/new.md"
	write(path, "see Putnami/"+"putnami#"+"123\n")
	if key() == before {
		t.Fatal("unrelated non-ignored candidate document reused the old release key")
	}
	if !store.SelectsPath(projectRelativePath(cliDocumentsProjectDir, path), patterns) {
		t.Fatal("candidate document does not select its declared reader under --impacted")
	}
	files, err := publicCutTrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if errors := evaluatePublicCut(scanPublicCut(files), nil, nil); !publicCutErrorsContain(errors, path) {
		t.Fatalf("new candidate debt did not fail the release scan: %v", errors)
	}
	var scanned []string
	for _, file := range files {
		scanned = append(scanned, filepath.Join(root, filepath.FromSlash(file.path)))
	}
	keyed, err := store.CollectKeyFiles(project, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keyed, scanned) {
		t.Fatalf("keyed candidate set differs from scanner:\nkeyed %s\nscanned %s", strings.Join(keyed, ", "), strings.Join(scanned, ", "))
	}
}
