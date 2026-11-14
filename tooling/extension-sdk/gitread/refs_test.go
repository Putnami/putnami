package gitread_test

import (
	"testing"

	"go.putnami.dev/sdk/extension/gitread"
)

func TestCurrentBranch_UnbornRepo(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	runGit(t, dir, "checkout", "-q", "--orphan", "unborn")

	branch, err := gitread.CurrentBranch(dir)
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "unborn" {
		t.Errorf("CurrentBranch = %q, want %q", branch, "unborn")
	}
}

// A detached HEAD has no branch to exclude, so a tier that excludes the branch
// itself must not treat the literal "HEAD" as one.
func TestCurrentBranchNameReportsNoBranchOnDetachedHEAD(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	if got := gitread.CurrentBranchName(repo); got != "main" {
		t.Fatalf("CurrentBranchName = %q, want main", got)
	}
	runGit(t, repo, "checkout", "-q", "--detach", "HEAD")
	if got := gitread.CurrentBranchName(repo); got != "" {
		t.Fatalf("CurrentBranchName on a detached HEAD = %q, want empty", got)
	}
}

func TestRefNamesBranchStripsOnlyTheRemoteSegment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		ref    string
		branch string
		want   bool
	}{
		{"origin/fix/3214", "fix/3214", true},
		{"origin/main", "other", false},
		{"main", "main", false},
		{"origin/main", "", false},
	} {
		if got := gitread.RefNamesBranch(test.ref, test.branch); got != test.want {
			t.Errorf("RefNamesBranch(%q, %q) = %t, want %t", test.ref, test.branch, got, test.want)
		}
	}
}

// The trunk is origin/HEAD, then origin/main, then origin/master, then a local
// main or master.
func TestTrunkRefPrefersTheRemoteDefaultBranch(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	if got := gitread.TrunkRef(repo); got != "main" {
		t.Fatalf("TrunkRef(local main only) = %q, want main", got)
	}
	runGit(t, repo, "update-ref", "refs/remotes/origin/master", "HEAD")
	if got := gitread.TrunkRef(repo); got != "origin/master" {
		t.Fatalf("TrunkRef(origin/master) = %q, want origin/master", got)
	}
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	if got := gitread.TrunkRef(repo); got != "origin/main" {
		t.Fatalf("TrunkRef(origin/main) = %q, want origin/main", got)
	}
	runGit(t, repo, "update-ref", "refs/remotes/origin/trunk", "HEAD")
	runGit(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	if got := gitread.TrunkRef(repo); got != "origin/trunk" {
		t.Fatalf("TrunkRef(origin/HEAD) = %q, want origin/trunk", got)
	}

	bare := t.TempDir()
	runGit(t, bare, "init", "-q")
	if got := gitread.TrunkRef(bare); got != "" {
		t.Fatalf("TrunkRef(no commit) = %q, want none", got)
	}
}

// An upstream that names the branch, or that already contains HEAD, is the
// branch's own counterpart; a base the branch moved past is not.
func TestUpstreamIsSelfCounterpart(t *testing.T) {
	t.Parallel()
	repo := initGitRepo(t)
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, repo, "checkout", "-q", "-b", "feature")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "work")

	if !gitread.UpstreamIsSelfCounterpart(repo, "origin/feature", "feature") {
		t.Error("origin/feature is not the counterpart of feature")
	}
	if gitread.UpstreamIsSelfCounterpart(repo, "origin/main", "feature") {
		t.Error("origin/main, which HEAD moved past, read as the counterpart of feature")
	}
	runGit(t, repo, "update-ref", "refs/remotes/origin/published", "HEAD")
	if !gitread.UpstreamIsSelfCounterpart(repo, "origin/published", "feature") {
		t.Error("a ref that already contains HEAD is not read as the counterpart")
	}
	if got := gitread.UpstreamRef(repo); got != "" {
		t.Errorf("UpstreamRef without a tracking branch = %q, want none", got)
	}
}
