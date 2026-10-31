package gitread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommitsSinceKeepsMultilineBodies pins the record format: a body with
// blank lines and a footer survives whole, the range excludes the base, and the
// newest commit comes first.
func TestCommitsSinceKeepsMultilineBodies(t *testing.T) {
	repo := initGitRepo(t)
	runGit(t, repo, "checkout", "-q", "-b", "feature")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "feat(cli): add a flag")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "refactor(cli): drop the greeting\n\nWhy it goes.\n\nBREAKING CHANGE: Greet is gone.")

	commits, err := CommitsSince(repo, "main")
	if err != nil {
		t.Fatalf("CommitsSince: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("CommitsSince returned %d commits, want 2: %#v", len(commits), commits)
	}
	head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	if commits[0].SHA != head || commits[0].Subject != "refactor(cli): drop the greeting" {
		t.Fatalf("newest commit = %#v, want HEAD %s first", commits[0], head)
	}
	if commits[0].Body != "Why it goes.\n\nBREAKING CHANGE: Greet is gone." {
		t.Fatalf("body = %q, want it whole", commits[0].Body)
	}
	if commits[1].Subject != "feat(cli): add a flag" || commits[1].Body != "" {
		t.Fatalf("oldest commit = %#v, want the flag commit with no body", commits[1])
	}
}

// TestCommitsSinceRefusesABaseThatNamesNothing pins that an unknown base is an
// error: an empty range would pass every check that reads it.
func TestCommitsSinceRefusesABaseThatNamesNothing(t *testing.T) {
	repo := initGitRepo(t)
	for _, base := range []string{"origin/missing", "--all", ""} {
		if _, err := CommitsSince(repo, base); err == nil {
			t.Fatalf("CommitsSince(%q) succeeded, want an error", base)
		}
	}
}

func TestIsShallowTellsAShallowCloneFromAFullOne(t *testing.T) {
	origin := initGitRepo(t)
	runGit(t, origin, "commit", "-q", "--allow-empty", "-m", "second")
	shallow, err := IsShallow(origin)
	if err != nil || shallow {
		t.Fatalf("IsShallow(full) = %v, %v; want false", shallow, err)
	}

	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, origin, "clone", "-q", "--depth", "1", "file://"+origin, clone)
	shallow, err = IsShallow(clone)
	if err != nil || !shallow {
		t.Fatalf("IsShallow(depth 1) = %v, %v; want true", shallow, err)
	}

	if _, err := IsShallow(t.TempDir()); err == nil {
		t.Fatal("IsShallow outside a repository succeeded, want an error")
	}
}

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
