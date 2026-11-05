package gitread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeBaseIsTheCommitTheBranchStartedFrom pins that the merge base
// ignores the commits the base gained since the branch started, and that a
// base that names nothing is an error.
func TestMergeBaseIsTheCommitTheBranchStartedFrom(t *testing.T) {
	repo := initGitRepo(t)
	start := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	runGit(t, repo, "checkout", "-q", "-b", "feature")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "feat: on the branch")
	runGit(t, repo, "checkout", "-q", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "feat: on main")
	runGit(t, repo, "checkout", "-q", "feature")

	got, err := MergeBase(repo, "main")
	if err != nil || got != start {
		t.Fatalf("MergeBase = %q, %v, want %s", got, err, start)
	}
	for _, base := range []string{"origin/missing", "--all", ""} {
		if _, err := MergeBase(repo, base); err == nil {
			t.Fatalf("MergeBase(%q) succeeded, want an error", base)
		}
	}

	// A base with no local branch is read as its origin counterpart.
	runGit(t, repo, "update-ref", "refs/remotes/origin/trunk", "main")
	if got, err := MergeBase(repo, "trunk"); err != nil || got != start {
		t.Fatalf("MergeBase(trunk) = %q, %v, want %s through origin/trunk", got, err, start)
	}

	// Unrelated histories share no commit.
	runGit(t, repo, "checkout", "-q", "--orphan", "unrelated")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "feat: elsewhere")
	if _, err := MergeBase(repo, "main"); err == nil || !strings.Contains(err.Error(), "share no commit") {
		t.Fatalf("MergeBase across unrelated histories = %v, want an error that says they share no commit", err)
	}
}

// TestRepositoryLocationNamesTheTopAndThePrefix pins both answers at the top
// of a repository and in a subdirectory.
func TestRepositoryLocationNamesTheTopAndThePrefix(t *testing.T) {
	repo, err := filepath.EvalSymlinks(initGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	top, prefix, err := RepositoryLocation(repo)
	if err != nil || top != repo || prefix != "" {
		t.Fatalf("RepositoryLocation(top) = %q, %q, %v, want %q and no prefix", top, prefix, err, repo)
	}
	sub := filepath.Join(repo, "ws", "inner")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	top, prefix, err = RepositoryLocation(sub)
	if err != nil || top != repo || prefix != "ws/inner/" {
		t.Fatalf("RepositoryLocation(sub) = %q, %q, %v, want %q and ws/inner/", top, prefix, err, repo)
	}
	if _, _, err := RepositoryLocation(t.TempDir()); err == nil {
		t.Fatal("RepositoryLocation outside a repository succeeded")
	}
}
