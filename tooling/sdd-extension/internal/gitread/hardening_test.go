package gitread

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests below pin the argument hardening this package's doc comment claims.
// Core exercises the same guards across tooling/cli/internal/git's own suite,
// which is not copied wholesale; these keep the guards provable HERE, because a
// widened read does not fail loudly — it silently produces a source binding
// over the wrong file set, and a binding is what evidence freshness compares.

func TestTreeEntryAtRefusesArgumentsItCannotPassSafely(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	sha := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))

	if _, _, err := TreeEntryAt(repo, "HEAD", "README.md"); err == nil {
		t.Fatal("a symbolic ref was accepted where an immutable commit ID is required")
	}
	for _, repoPath := range []string{"../escape", "/absolute", "a/../b", "a//b", ".", "nul\x00path"} {
		if _, _, err := TreeEntryAt(repo, sha, repoPath); err == nil {
			t.Errorf("unsafe repository path %q was accepted", repoPath)
		}
	}
	entry, exists, err := TreeEntryAt(repo, sha, "absent.md")
	if err != nil || exists || entry.ObjectID != "" {
		t.Fatalf("absent path = %+v, %t, %v; want the zero entry and no error", entry, exists, err)
	}
	root, exists, err := TreeEntryAt(repo, sha, "")
	if err != nil || !exists || root.Type != "tree" || root.Mode != "040000" {
		t.Fatalf("root entry = %+v, %t, %v", root, exists, err)
	}
}

func TestTreeEntriesListsDirectMembersSortedAndRefusesBadObjectIDs(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	for _, relative := range []string{"pkg/b.txt", "pkg/a.txt", "pkg/nested/deep.txt"} {
		filename := filepath.Join(repo, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(relative), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "add", "pkg")
	runGit(t, repo, "commit", "-m", "add package")
	sha := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))

	pkg, exists, err := TreeEntryAt(repo, sha, "pkg")
	if err != nil || !exists {
		t.Fatalf("TreeEntryAt(pkg) = %t, %v", exists, err)
	}
	entries, err := TreeEntries(repo, pkg.ObjectID)
	if err != nil {
		t.Fatal(err)
	}
	// Direct members only: the nested file must not appear, only its directory.
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Path)
	}
	if strings.Join(got, ",") != "a.txt,b.txt,nested" {
		t.Fatalf("tree entries = %v, want the direct members in sorted order", got)
	}
	for _, objectID := range []string{"", "zzz", "not-hex-at-all", strings.Repeat("a", 65)} {
		if _, err := TreeEntries(repo, objectID); err == nil {
			t.Errorf("invalid tree object ID %q was accepted", objectID)
		}
	}
}

func TestReadTreeBlobStaysBoundedAndBlobOnly(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "payload.txt"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "payload.txt")
	runGit(t, repo, "commit", "-m", "add payload")
	sha := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	entry, _, err := TreeEntryAt(repo, sha, "payload.txt")
	if err != nil {
		t.Fatal(err)
	}

	data, err := ReadTreeBlob(repo, entry.ObjectID, 10)
	if err != nil || string(data) != "0123456789" {
		t.Fatalf("exact-limit read = %q, %v", data, err)
	}
	if _, err := ReadTreeBlob(repo, entry.ObjectID, 9); !errors.Is(err, ErrTreeObjectTooLarge) {
		t.Fatalf("over-limit read error = %v, want ErrTreeObjectTooLarge", err)
	}
	// A tree is not a blob, and the caller must not receive its serialization.
	if _, err := ReadTreeBlob(repo, sha, 4096); err == nil {
		t.Fatal("a commit object was read as a blob")
	}
	for _, objectID := range []string{"", "nothex"} {
		if _, err := ReadTreeBlob(repo, objectID, 4096); err == nil {
			t.Errorf("invalid object ID %q was accepted", objectID)
		}
	}
	if _, err := ReadTreeBlob(repo, entry.ObjectID, -1); err == nil {
		t.Fatal("a negative read limit was accepted")
	}
}

func TestCommitSourceBindingRefusesRootsItCannotBind(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	if _, err := CommitSourceBinding(repo, "-c", "project"); err == nil {
		t.Fatal("an option-shaped revision was accepted")
	}
	if _, err := CommitSourceBinding(repo, "HEAD", "../escape"); err == nil {
		t.Fatal("a project root outside the tree was accepted")
	}
	if _, err := CommitSourceBinding(repo, "HEAD", "absent"); err == nil {
		t.Fatal("an absent project root produced a binding")
	}
	// README.md exists but is a blob, so it is not a bindable root.
	if _, err := CommitSourceBinding(repo, "HEAD", "README.md"); err == nil {
		t.Fatal("a blob was accepted as a project root")
	}
}

func TestProjectSourceBindingRefusesRootsOutsideTheRepository(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	if _, err := ProjectSourceBinding(repo, t.TempDir()); err == nil {
		t.Fatal("a project root outside the repository produced a binding")
	}
	if _, err := ProjectSourceBinding(repo, filepath.Join(repo, "absent")); err == nil {
		t.Fatal("an absent project root produced a binding")
	}
}
