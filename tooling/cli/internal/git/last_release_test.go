package git

import (
	"testing"
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
// carries the older release ts/v0.1.1.
func mergedHotfixRepo(t *testing.T) string {
	t.Helper()
	dir := initGitRepo(t)
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add")
	gitDo(t, dir, "tag", "-a", "ts/v0.1.0", "-m", "release")
	writeCommit(t, dir, "typescript/b.ts", "feat(ts): more")
	gitDo(t, dir, "tag", "-a", "ts/v0.2.0", "-m", "release")
	writeCommit(t, dir, "typescript/c.ts", "fix(ts): repair")
	gitDo(t, dir, "checkout", "-q", "-b", "hotfix", "ts/v0.1.0")
	writeCommit(t, dir, "typescript/hotfix.ts", "fix(ts): hotfix")
	gitDo(t, dir, "tag", "-a", "ts/v0.1.1", "-m", "hotfix release")
	gitDo(t, dir, "checkout", "-q", "main")
	gitDo(t, dir, "merge", "-q", "--no-ff", "-m", "merge the hotfix", "hotfix")
	return dir
}

// The version bump and the release baseline of a task key read one last
// release: the line's highest release tag on the first-parent history.
func TestTheVersionAndTheReleaseBaselineStartFromTheLastRelease(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name           string
		repo           func(*testing.T) string
		wantTag, wantV string
	}{
		{"a tag that is not a release", strayTagRepo, "ts/v0.3.0", "0.3.1"},
		{"a release of a merged branch", mergedHotfixRepo, "ts/v0.2.0", "0.2.1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
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
