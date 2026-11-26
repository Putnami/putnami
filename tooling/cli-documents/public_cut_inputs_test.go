package documents

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/cli/internal/store"
)

// publicCutInputsRepo is a repository that ignores local state, with README.md
// staged. write stores a file in it.
func publicCutInputsRepo(t *testing.T) (root string, write func(path, data string)) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publicCutRunGit(t, root, "init")
	write = func(path, data string) {
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
	return root, write
}

func TestPublicCutInputsRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		root, _ := publicCutInputsRepo(t)
		return root
	})
}

// publicCutCandidatePatterns is the only declaration that keys what the
// scanner reads. The project does not declare it, since it selects the project
// for every change of the repository; the whole-tree gates run without a cache
// instead (requireWholeTreeGates). The test below keeps the two enumerations
// equal, so a run that does key on the whole tree keys what the scanner reads.
var publicCutCandidatePatterns = []string{"git:**"}

// Exercise the whole-tree pattern, the collector and the scanner together. A
// marker in an unrelated project's new document must move the key before
// staging; ignored manifests and private worktree state must remain invisible.
func TestPublicCutCandidateInputsInvalidateTheRecordedVerdict(t *testing.T) {
	patterns := publicCutCandidatePatterns
	root, write := publicCutInputsRepo(t)
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
		t.Fatal("the whole-tree pattern does not select a candidate document")
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
	keyed, err := store.CollectKeyFiles(project, patterns, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keyed, scanned) {
		t.Fatalf("keyed candidate set differs from scanner:\nkeyed %s\nscanned %s", strings.Join(keyed, ", "), strings.Join(scanned, ", "))
	}
}
