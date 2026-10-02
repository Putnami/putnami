package git

import (
	"slices"
	"strings"
	"testing"
)

// RevList lists the parents the commit objects record, newest first, up to
// its limit. A replace ref present at the call adds no parent, and an argument
// that is not a full lowercase commit id never reaches git.
func TestRevListReadsTheRecordedParentsOnly(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	first := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	commitNewFile(t, dir, "second.txt")
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	orphan := strings.TrimSpace(runGit(t, dir, "commit-tree", strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD^{tree}")), "-m", "orphan"))
	runGit(t, dir, "replace", "--graft", head, orphan)

	commits, err := RevList(dir, head, 10)
	if err != nil || !slices.Equal(commits, []string{head, first}) {
		t.Fatalf("RevList = %v, %v; want %v", commits, err, []string{head, first})
	}
	commits, err = RevList(dir, head, 1)
	if err != nil || !slices.Equal(commits, []string{head}) {
		t.Fatalf("RevList limited to 1 = %v, %v; want %v", commits, err, []string{head})
	}
	for _, revision := range []string{"HEAD", strings.ToUpper(head), head[:12], "--all", ""} {
		if commits, err := RevList(dir, revision, 10); err == nil {
			t.Errorf("RevList(%q) = %v; want a refusal", revision, commits)
		}
	}
	if commits, err := RevList(dir, head, 0); err == nil {
		t.Errorf("RevList with no limit = %v; want a refusal", commits)
	}
	if commits, err := RevList(dir, strings.Repeat("0", 40), 10); err == nil {
		t.Errorf("RevList of a commit the repository lacks = %v; want an error", commits)
	}
}
