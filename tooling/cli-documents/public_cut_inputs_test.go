package documents

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/repositorychecks"
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

// TestPublicCutCandidateInputsInvalidateTheRecordedVerdict exercises the
// declaration of @putnami/repository-checks, the CLI's collector and the
// scanner together. A marker
// in an unrelated project's new document must invalidate a warm pass before
// staging; ignored manifests and private worktree state must remain invisible.
func TestPublicCutCandidateInputsInvalidateTheRecordedVerdict(t *testing.T) {
	patterns := declaredTestFilePatterns(t, repositorychecks.ProjectDir)
	root, write := publicCutInputsRepo(t)
	project := filepath.Join(root, filepath.FromSlash(repositorychecks.ProjectDir))
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
	if !store.SelectsPath(projectRelativePath(repositorychecks.ProjectDir, path), patterns) {
		t.Fatal("candidate document does not select its declared reader under --impacted")
	}
	cut, err := repositorychecks.ReadCandidateCut(root)
	if err != nil {
		t.Fatal(err)
	}
	if !publicCutErrorsContain(cut.UnreviewedFindings(), path) {
		t.Fatalf("new candidate debt did not fail the release scan: %v", cut.UnreviewedFindings())
	}
	var scanned []string
	for _, file := range cut.Paths() {
		scanned = append(scanned, filepath.Join(root, filepath.FromSlash(file)))
	}
	keyed, err := store.CollectKeyFiles(project, patterns, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keyed, scanned) {
		t.Fatalf("keyed candidate set differs from scanner:\nkeyed %s\nscanned %s", strings.Join(keyed, ", "), strings.Join(scanned, ", "))
	}
}

// publicCutCandidateRepo is a repository that stages unusual names, deletes
// one of them from the work tree, and holds an untracked candidate and an
// ignored file. It returns the root and the candidate paths in sorted order.
func publicCutCandidateRepo(t *testing.T) (root string, want []string) {
	t.Helper()
	root = t.TempDir()
	publicCutRunGit(t, root, "init", "-q")
	tracked := []string{".gitignore", "name with spaces.txt", "name\nwith-newline.txt", "tracked-deleted.txt"}
	want = []string{".gitignore", "candidate.txt", "name with spaces.txt", "name\nwith-newline.txt"}
	if runtime.GOOS == "windows" {
		// A Windows file name cannot contain a newline; the other unusual
		// names still run.
		tracked = slices.DeleteFunc(tracked, func(name string) bool { return strings.Contains(name, "\n") })
		want = slices.DeleteFunc(want, func(name string) bool { return strings.Contains(name, "\n") })
	}
	for _, name := range tracked {
		contents := []byte("tracked\n")
		if name == ".gitignore" {
			contents = []byte("ignored.txt\n")
		}
		if err := os.WriteFile(filepath.Join(root, name), contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicCutRunGit(t, root, append([]string{"add", "--"}, tracked...)...)
	if err := os.Remove(filepath.Join(root, "tracked-deleted.txt")); err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)
	return root, want
}

func TestPublicCutCandidateRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		root, _ := publicCutCandidateRepo(t)
		return root
	})
}

func TestPublicCutTrackedFilesIncludeNonIgnoredCandidatesAndHandleUnusualNames(t *testing.T) {
	t.Parallel()
	root, want := publicCutCandidateRepo(t)

	cut, err := repositorychecks.ReadCandidateCut(root)
	if err != nil {
		t.Fatal(err)
	}
	got := cut.Paths()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("candidate paths = %q, want %q (non-ignored included, ignored/deleted excluded, names preserved)", got, want)
	}
}

func publicCutErrorsContain(errors []string, fragment string) bool {
	for _, err := range errors {
		if strings.Contains(err, fragment) {
			return true
		}
	}
	return false
}

func publicCutRunGit(t *testing.T, root string, args ...string) {
	t.Helper()
	gittest.Run(t, root, args...)
}
