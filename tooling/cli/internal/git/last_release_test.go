package git

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// strayTagRepo holds release ts/v0.3.0, then tags nearer HEAD that carry the
// line's prefix and are not releases.
func strayTagRepo(t *testing.T) string {
	t.Helper()
	dir := initGitRepo(t)
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add")
	gitDo(t, dir, "tag", "-a", "ts/v0.3.0", "-m", "release")
	writeCommit(t, dir, "typescript/b.ts", "fix(ts): repair")
	gitDo(t, dir, "tag", "ts/vnext-experiment")
	gitDo(t, dir, "tag", "ts/v0.4.0-rc.1")
	writeCommit(t, dir, "typescript/c.ts", "fix(ts): repair again")
	return dir
}

// mergedHotfixRepo holds release ts/v0.2.0 on main and merges a branch that
// carries the older release ts/v0.1.1. The branch holds two commits before its
// tag, so `git describe` finds that tag nearest whatever the commit dates.
func mergedHotfixRepo(t *testing.T) string {
	t.Helper()
	dir := initGitRepo(t)
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add")
	gitDo(t, dir, "tag", "-a", "ts/v0.1.0", "-m", "release")
	writeCommit(t, dir, "typescript/b.ts", "feat(ts): more")
	gitDo(t, dir, "tag", "-a", "ts/v0.2.0", "-m", "release")
	writeCommit(t, dir, "typescript/c.ts", "fix(ts): repair")
	writeCommit(t, dir, "typescript/d.ts", "docs(ts): explain")
	gitDo(t, dir, "checkout", "-q", "-b", "hotfix", "ts/v0.1.0")
	writeCommit(t, dir, "typescript/hotfix.ts", "fix(ts): hotfix")
	writeCommit(t, dir, "typescript/hotfix.test.ts", "test(ts): cover the hotfix")
	gitDo(t, dir, "tag", "-a", "ts/v0.1.1", "-m", "hotfix release")
	gitDo(t, dir, "checkout", "-q", "main")
	gitDo(t, dir, "merge", "-q", "--no-ff", "-m", "merge the hotfix", "hotfix")
	if nearest := strings.TrimSpace(runGit(t, dir, "describe", "--tags", "--abbrev=0", "--match", "ts/v*")); nearest != "ts/v0.1.1" {
		t.Fatalf("git describe = %s: the fixture no longer puts the merged tag nearest", nearest)
	}
	return dir
}

// mergedMainRepo is a feature branch that merged main after main released
// ts/v0.3.0: the release is reached through the merge's second parent.
func mergedMainRepo(t *testing.T) string {
	t.Helper()
	dir := initGitRepo(t)
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add")
	gitDo(t, dir, "tag", "-a", "ts/v0.2.0", "-m", "release")
	gitDo(t, dir, "checkout", "-q", "-b", "feature")
	writeCommit(t, dir, "typescript/feature.ts", "feat(ts): start")
	gitDo(t, dir, "checkout", "-q", "main")
	writeCommit(t, dir, "typescript/b.ts", "feat(ts): ship")
	gitDo(t, dir, "tag", "-a", "ts/v0.3.0", "-m", "release")
	gitDo(t, dir, "checkout", "-q", "feature")
	gitDo(t, dir, "merge", "-q", "--no-ff", "-m", "merge main", "main")
	return dir
}

// The version bump and the release baseline of a task key read one last
// release: the line's highest release tag that HEAD reaches.
func TestTheVersionAndTheReleaseBaselineStartFromTheLastRelease(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		check          string
		repo           func(*testing.T) string
		wantTag, wantV string
	}{
		{"a-tag-that-is-not-a-release-is-no-baseline", strayTagRepo, "ts/v0.3.0", "0.3.1"},
		{"an-older-release-of-a-merged-branch-is-no-baseline", mergedHotfixRepo, "ts/v0.2.0", "0.2.1"},
		{"a-release-reached-through-a-merge-of-main-is-the-baseline", mergedMainRepo, "ts/v0.3.0", "0.3.1"},
	} {
		t.Run(testCase.check, func(t *testing.T) {
			t.Parallel()
			spectest.Proves(t, "cli/version-from-git", "last-release-is-the-baseline", testCase.check)
			dir := testCase.repo(t)

			info, err := GetVersionInfo(dir, LineSpec{
				ScopePath: "typescript", TagPattern: "ts/v{version}", Pathspecs: []string{"typescript"}})
			if err != nil {
				t.Fatalf("GetVersionInfo: %v", err)
			}
			if info.Base != testCase.wantV {
				t.Errorf("Base = %q, want %q", info.Base, testCase.wantV)
			}
			baseline, err := ReadReleaseBaseline(dir, "ts/v{version}")
			if err != nil {
				t.Fatalf("ReadReleaseBaseline: %v", err)
			}
			if baseline.State != ReleaseBaselineTagged || baseline.Tag != testCase.wantTag {
				t.Errorf("baseline = %+v, want tag %s", baseline, testCase.wantTag)
			}
		})
	}
}
