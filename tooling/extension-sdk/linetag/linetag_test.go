package linetag

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// repository returns a repository on branch main with one commit.
func repository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "commit.gpgsign", "false")
	git(t, dir, "config", "tag.gpgsign", "false")
	commit(t, dir, "first")
	return dir
}

// clock dates each git call one minute after the last one, so the order of
// the commits of a fixture never depends on how fast the test runs.
var clock atomic.Int64

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	date := fmt.Sprintf("@%d +0000", 1700000000+60*clock.Add(1))
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, subject string) {
	t.Helper()
	git(t, dir, "commit", "--quiet", "--allow-empty", "-m", subject)
}

func TestALineWithoutAReleaseTagHasNoLastRelease(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "-a", "go/v0.1.0", "-m", "another line")

	if release, ok, err := LastRelease(dir, "ts/v{version}"); err != nil || ok {
		t.Fatalf("LastRelease on an untagged line = %+v/%v/%v, want none and no error", release, ok, err)
	}
}

func TestTheLastReleaseNamesTheTagItsVersionAndItsCommit(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "-a", "ts/v0.1.0", "-m", "annotated")
	tagged := git(t, dir, "rev-parse", "HEAD")
	commit(t, dir, "feat(ts): add")

	release, ok, err := LastRelease(dir, "ts/v{version}")
	if err != nil || !ok {
		t.Fatalf("LastRelease = %v/%v", ok, err)
	}
	if want := (Release{Tag: "ts/v0.1.0", Version: "0.1.0", Commit: tagged}); release != want {
		t.Fatalf("LastRelease = %+v, want %+v", release, want)
	}
}

func TestATagThatIsNotAReleaseVersionIsNeverTheLastRelease(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "-a", "ts/v0.3.0", "-m", "release")
	commit(t, dir, "feat(ts): add")
	for _, stray := range []string{
		"ts/vnext-experiment",
		"ts/v0.4.0-rc.1",
		"ts/v0.4.0+build.7",
		"ts/v0.4",
		"ts/v0.4.0.1",
		"ts/v0.04.0",
		"ts/v1.x.0",
		"ts/v",
	} {
		git(t, dir, "tag", stray)
	}
	commit(t, dir, "fix(ts): repair")

	release, ok, err := LastRelease(dir, "ts/v{version}")
	if err != nil || !ok || release.Tag != "ts/v0.3.0" {
		t.Fatalf("LastRelease = %+v/%v/%v, want ts/v0.3.0", release, ok, err)
	}
}

// The merged branch holds two commits before its tag, so `git describe`
// finds the merged tag nearest whatever the commit dates.
func TestAnOlderReleaseOfAMergedBranchDoesNotReplaceALaterOne(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "-a", "ts/v0.1.0", "-m", "release")
	commit(t, dir, "feat(ts): add")
	git(t, dir, "tag", "-a", "ts/v0.2.0", "-m", "release")
	commit(t, dir, "fix(ts): repair")
	commit(t, dir, "docs(ts): explain")
	git(t, dir, "checkout", "--quiet", "-b", "hotfix", "ts/v0.1.0")
	commit(t, dir, "fix(ts): hotfix")
	commit(t, dir, "test(ts): cover the hotfix")
	git(t, dir, "tag", "-a", "ts/v0.1.1", "-m", "hotfix release")
	git(t, dir, "checkout", "--quiet", "main")
	git(t, dir, "merge", "--quiet", "--no-ff", "-m", "merge the hotfix", "hotfix")

	if nearest := git(t, dir, "describe", "--tags", "--abbrev=0", "--match", "ts/v*"); nearest != "ts/v0.1.1" {
		t.Fatalf("git describe = %s: the fixture no longer puts the merged tag nearest", nearest)
	}
	release, ok, err := LastRelease(dir, "ts/v{version}")
	if err != nil || !ok || release.Tag != "ts/v0.2.0" {
		t.Fatalf("LastRelease = %+v/%v/%v, want ts/v0.2.0", release, ok, err)
	}
}

// A feature branch that merges main reaches the release main tagged after the
// branch started, through the merge's second parent.
func TestAReleaseReachedThroughASecondParentCounts(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "ts/v0.2.0")
	git(t, dir, "checkout", "--quiet", "-b", "feature")
	commit(t, dir, "feat(ts): start")
	git(t, dir, "checkout", "--quiet", "main")
	commit(t, dir, "feat(ts): ship")
	git(t, dir, "tag", "ts/v0.3.0")
	git(t, dir, "checkout", "--quiet", "feature")
	git(t, dir, "merge", "--quiet", "--no-ff", "-m", "merge main", "main")

	release, ok, err := LastRelease(dir, "ts/v{version}")
	if err != nil || !ok || release.Tag != "ts/v0.3.0" {
		t.Fatalf("LastRelease = %+v/%v/%v, want ts/v0.3.0", release, ok, err)
	}
}

func TestAReleaseHEADCannotReachIsNotTheLastRelease(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "ts/v0.1.0")
	git(t, dir, "checkout", "--quiet", "-b", "next")
	commit(t, dir, "feat(ts): next")
	git(t, dir, "tag", "ts/v0.2.0")
	git(t, dir, "checkout", "--quiet", "main")

	release, ok, err := LastRelease(dir, "ts/v{version}")
	if err != nil || !ok || release.Tag != "ts/v0.1.0" {
		t.Fatalf("LastRelease = %+v/%v/%v, want ts/v0.1.0", release, ok, err)
	}
}

func TestATagOnATreeIsNotARelease(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "ts/v0.1.0")
	git(t, dir, "tag", "ts/v0.2.0", "HEAD^{tree}")

	release, ok, err := LastRelease(dir, "ts/v{version}")
	if err != nil || !ok || release.Tag != "ts/v0.1.0" {
		t.Fatalf("LastRelease = %+v/%v/%v, want ts/v0.1.0", release, ok, err)
	}
}

func TestAReleaseTagGitCannotReadIsAnError(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "ts/v0.1.0")
	git(t, dir, "tag", "-a", "ts/v0.2.0", "-m", "release")
	object := git(t, dir, "rev-parse", "refs/tags/ts/v0.2.0")
	if err := os.Remove(filepath.Join(dir, ".git", "objects", object[:2], object[2:])); err != nil {
		t.Fatal(err)
	}

	_, ok, err := LastRelease(dir, "ts/v{version}")
	if err == nil || ok || !strings.Contains(err.Error(), "ts/v0.2.0") {
		t.Fatalf("LastRelease with an unreadable tag = %v/%v, want an error naming ts/v0.2.0", ok, err)
	}
}

func TestTheHighestReleaseWinsWhateverItsDistance(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "ts/v0.10.0")
	commit(t, dir, "fix(ts): repair")
	git(t, dir, "tag", "ts/v0.9.12")
	git(t, dir, "tag", "ts/v0.2.0")
	commit(t, dir, "docs: explain")

	release, ok, err := LastRelease(dir, "ts/v{version}")
	if err != nil || !ok || release.Tag != "ts/v0.10.0" {
		t.Fatalf("LastRelease = %+v/%v/%v, want ts/v0.10.0", release, ok, err)
	}
}

func TestThePatternMatchesCompletely(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "release-v0.4.0-stable")
	commit(t, dir, "feat: add")
	git(t, dir, "tag", "release-v9.9.9-nightly")
	git(t, dir, "tag", "prerelease-v9.9.9-stable")
	commit(t, dir, "fix: repair")

	release, ok, err := LastRelease(dir, "release-v{version}-stable")
	if err != nil || !ok || release.Tag != "release-v0.4.0-stable" || release.Version != "0.4.0" {
		t.Fatalf("LastRelease = %+v/%v/%v, want release-v0.4.0-stable", release, ok, err)
	}
}

func TestVersionReadsOnlyAReleaseTagOfTheLine(t *testing.T) {
	t.Parallel()
	for tag, want := range map[string]string{
		"ts/v1.2.3":      "1.2.3",
		"ts/v10.20.30":   "10.20.30",
		"ts/v1.2.3-rc.1": "",
		"ts/vnext":       "",
		"ts/v01.2.3":     "",
		"go/v1.2.3":      "",
	} {
		if got, ok := Version(tag, "ts/v{version}"); got != want || ok != (want != "") {
			t.Errorf("Version(%q) = %q/%v, want %q", tag, got, ok, want)
		}
	}
	if _, ok := Version("ts/v1.2.3", "ts/v*"); ok {
		t.Error("Version with an unusable pattern succeeded")
	}
}

func TestAnUnusablePatternIsAnError(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	for _, pattern := range []string{"v*", "lib/v{version}/{version}", "-v{version}", "no-placeholder", "ts v{version}"} {
		if _, ok, err := LastRelease(dir, pattern); err == nil || ok {
			t.Errorf("LastRelease(%q) = %v/%v, want an error", pattern, ok, err)
		}
	}
}

func TestAHistoryGitCannotReadIsAnError(t *testing.T) {
	t.Parallel()
	dir := repository(t)
	git(t, dir, "tag", "ts/v0.1.0")
	commit(t, dir, "feat(ts): add")
	commit(t, dir, "fix(ts): repair")
	missing := git(t, dir, "rev-parse", "HEAD~1")
	if err := os.Remove(filepath.Join(dir, ".git", "objects", missing[:2], missing[2:])); err != nil {
		t.Fatal(err)
	}

	_, ok, err := LastRelease(dir, "ts/v{version}")
	if err == nil || ok {
		t.Fatalf("LastRelease on a history with a missing commit = %v/%v, want an error", ok, err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not name the unreadable commit %s", err, missing)
	}
}

func TestARepositoryWithoutACommitIsAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "--initial-branch=main")

	if _, ok, err := LastRelease(dir, "ts/v{version}"); err == nil || ok {
		t.Fatalf("LastRelease without a commit = %v/%v, want an error", ok, err)
	}
}

// Not parallel: it sets GIT_TRACE, which makes git write to stderr.
func TestGitTraceDoesNotChangeTheReading(t *testing.T) {
	dir := repository(t)
	t.Setenv("GIT_TRACE", "1")
	if _, ok, err := LastRelease(dir, "ts/v{version}"); err != nil || ok {
		t.Fatalf("LastRelease on an untagged line under GIT_TRACE = %v/%v, want none and no error", ok, err)
	}
	git(t, dir, "tag", "ts/v0.1.0")
	if release, ok, err := LastRelease(dir, "ts/v{version}"); err != nil || !ok || release.Tag != "ts/v0.1.0" {
		t.Fatalf("LastRelease under GIT_TRACE = %+v/%v/%v, want ts/v0.1.0", release, ok, err)
	}
}
