package gitread

import (
	"path/filepath"
	"strings"
	"testing"
)

// The record format keeps a body with blank lines and a footer whole, the
// range excludes the base, and the newest commit comes first.
func TestCommitsSinceKeepsMultilineBodies(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	runGit(t, repo, "checkout", "-q", "-b", "feature")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "feat(cli): add a flag")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "refactor(cli): drop the greeting\n\nWhy it goes.\n\nBREAKING CHANGE: Greet is gone.")

	commits, err := CommitsSince(repo, "main", nil)
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

// Pathspecs bound the commits read, and an empty fromCommit walks the whole
// history.
func TestCommitsSinceBoundsByPathspecAndWalksTheWholeHistory(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	base, err := HeadSHA(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add", "why\n\nBREAKING CHANGE: gone")
	writeCommit(t, dir, "go/a.go", "fix(go): stop", "")

	scoped, err := CommitsSince(dir, base, []string{"typescript"})
	if err != nil {
		t.Fatalf("CommitsSince(typescript): %v", err)
	}
	if len(scoped) != 1 || scoped[0].Subject != "feat(ts): add" {
		t.Fatalf("CommitsSince(typescript) = %+v, want only the typescript commit", scoped)
	}

	whole, err := CommitsSince(dir, "", nil)
	if err != nil || len(whole) != 3 {
		t.Fatalf("CommitsSince(whole history) = %d/%v, want 3 commits", len(whole), err)
	}
}

// A base that names nothing is an error: an empty range would pass every check
// that reads it.
func TestCommitsSinceRefusesABaseThatNamesNothing(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	for _, base := range []string{"origin/missing", "--all"} {
		if _, err := CommitsSince(repo, base, nil); err == nil {
			t.Fatalf("CommitsSince(%q) succeeded, want an error", base)
		}
	}
}

func TestIsShallowTellsAShallowCloneFromAFullOne(t *testing.T) {
	t.Parallel()
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
}
