package version

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

// toolingNotes is the changelog releaseRepo's tooling line renders today.
func toolingNotes(commits []string) string {
	return renderChangelogAt("tooling", "0.0.1", toolingLine(commits).Commits, time.Now())
}

func TestTagCommitsTheChangelogThenTags(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "release-commit-then-tag", "changelog-is-in-the-tagged-commit")
	dir, commits := releaseRepo(t)
	host := releaseHost(commits)
	notes := toolingNotes(commits)

	got := runTag(t, dir, host, "--scope", "tooling", "--yes")
	if got.code != 0 || got.stderr != "" || !strings.HasSuffix(got.stdout, "  Created tooling/v0.0.1 on the release commit\n") {
		t.Fatalf("got %+v, want the release", got)
	}
	if !strings.Contains(got.stdout, "chore(release): tooling 0.0.1") {
		t.Errorf("stdout %q does not carry git's commit line", got.stdout)
	}
	if asked := host.asked(); len(asked) != 1 || asked[0] != "tooling" {
		t.Errorf("the host was asked for %q, want tooling once", asked)
	}
	release := git(t, dir, "rev-parse", "HEAD")
	if parent := git(t, dir, "rev-parse", "HEAD~1"); parent != commits[1] {
		t.Errorf("the release commit's parent is %s, want the released HEAD %s", parent, commits[1])
	}
	if message := git(t, dir, "log", "-1", "--format=%B"); message != "chore(release): tooling 0.0.1\n\nCo-Authored-By: Release Human <human@example.invalid>" {
		t.Errorf("release commit message %q", message)
	}
	if tagged := git(t, dir, "rev-parse", "tooling/v0.0.1^{commit}"); tagged != release {
		t.Errorf("the tag names %s, want the release commit %s", tagged, release)
	}
	if kind := git(t, dir, "cat-file", "-t", "tooling/v0.0.1"); kind != "tag" {
		t.Errorf("the tag is a %s, want an annotated tag", kind)
	}
	if contents := git(t, dir, "tag", "-l", "--format=%(contents)", "tooling/v0.0.1"); contents != strings.TrimSpace(notes) {
		t.Errorf("tag message %q, want the changelog %q", contents, notes)
	}
	if committed := git(t, dir, "show", "HEAD:tooling/CHANGELOG.md"); committed != strings.TrimSpace(notes) {
		t.Errorf("committed changelog %q, want %q", committed, notes)
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("the release left %q in the tree", status)
	}
}

func TestTagReleasesTheRootLineIntoTheRootChangelog(t *testing.T) {
	t.Parallel()
	dir, _ := releaseRepo(t)
	writeFile(t, dir, "CHANGELOG.md", "## 0.0.1 — 2026-01-01\n\n### Fixes\n\n- fix: older (1111111)\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "docs: the changelog")
	head := git(t, dir, "rev-parse", "HEAD")
	root := wire.VersionLine{Line: "", TagPattern: "v{version}", Version: "0.0.2-x",
		Release: &wire.LineRelease{Version: "0.0.2", Tag: "v0.0.2"},
		Commits: []wire.LineCommit{{SHA: head, Subject: "docs: the changelog"}}}
	host := &fakeHost{result: wire.VersionLinesResult{SourceRevision: head, Lines: []wire.VersionLine{root}}}

	got := runTag(t, dir, host, "--yes")
	if got.code != 0 || !strings.HasSuffix(got.stdout, "  Created v0.0.2 on the release commit\n") {
		t.Fatalf("got %+v, want the release of the root line", got)
	}
	if subject := git(t, dir, "log", "-1", "--format=%s"); subject != "chore(release): (workspace root) 0.0.2" {
		t.Errorf("release subject %q", subject)
	}
	changelog := git(t, dir, "show", "HEAD:CHANGELOG.md")
	if !strings.HasPrefix(changelog, "## 0.0.2 — ") || !strings.HasSuffix(changelog, "- fix: older (1111111)") {
		t.Errorf("changelog %q, want the new release above the older one", changelog)
	}
}

func TestTagPushesTheCommitAndTheTag(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "release-commit-then-tag", "push-sends-the-commit-and-the-tag")
	dir, commits := releaseRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, dir, "init", "-q", "--bare", origin)
	git(t, dir, "remote", "add", "origin", origin)
	git(t, dir, "push", "-q", "origin", "main")

	got := runTag(t, dir, releaseHost(commits), "--scope", "tooling", "--yes", "--push")
	if got.code != 0 || !strings.HasSuffix(got.stdout, "  Created tooling/v0.0.1 on the release commit\n  Pushed HEAD and tooling/v0.0.1\n") {
		t.Fatalf("got %+v, want the release pushed", got)
	}
	release := git(t, dir, "rev-parse", "HEAD")
	if pushed := git(t, origin, "rev-parse", "main"); pushed != release {
		t.Errorf("origin's main is %s, want the release commit %s", pushed, release)
	}
	if pushed := git(t, origin, "rev-parse", "tooling/v0.0.1^{commit}"); pushed != release {
		t.Errorf("origin's tag names %s, want the release commit %s", pushed, release)
	}
}

func TestTagDryRunPrintsTheProposalAndTheChangelog(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "release-is-reviewable-first", "dry-run-prints-the-proposal-and-the-changelog")
	dir, commits := releaseRepo(t)
	want := "  Would release tooling 0.0.1 as tooling/v0.0.1\n" + toolingNotes(commits) + "\n"

	if got := runTag(t, dir, releaseHost(commits), "--scope", "tooling", "--dry-run"); got != (outcome{stdout: want}) {
		t.Errorf("--dry-run got %+v, want stdout %q", got, want)
	}
	var stdout, stderr strings.Builder
	forwarded := Command{Root: dir, DryRun: true, Host: releaseHost(commits), Stdout: &stdout, Stderr: &stderr}
	if code := forwarded.Tag(t.Context(), []string{"--scope", "tooling"}); code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Errorf("a forwarded dry run exited %d with %q and %q, want stdout %q", code, stdout.String(), stderr.String(), want)
	}
	// A dirty tree is reported only for a release that would write.
	writeFile(t, dir, "tooling/cli/main.go", "package changed\n")
	if got := runTag(t, dir, releaseHost(commits), "--dry-run"); got != (outcome{stdout: want}) {
		t.Errorf("a dry run on a dirty tree got %+v, want stdout %q", got, want)
	}
	git(t, dir, "checkout", "--", ".")
	assertNothingWritten(t, dir, commits[1])
}

func TestTagWithoutYesWritesNothing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "release-is-reviewable-first", "without-yes-nothing-is-written")
	dir, commits := releaseRepo(t)
	got := runTag(t, dir, releaseHost(commits), "--scope", "tooling", "--push")
	if want := (outcome{stdout: "  Release tooling 0.0.1 as tooling/v0.0.1? Re-run with --yes to confirm.\n"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	assertNothingWritten(t, dir, commits[1])
}

func TestTagExplicitTagReplacesTheProposal(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "release-is-reviewable-first", "an-explicit-tag-replaces-the-proposal")
	dir, commits := releaseRepo(t)
	got := runTag(t, dir, releaseHost(commits), "tooling/v2.3.4", "--scope", "tooling", "--dry-run")
	want := "  Would release tooling 2.3.4 as tooling/v2.3.4\n" +
		renderChangelogAt("tooling", "2.3.4", toolingLine(commits).Commits, time.Now()) + "\n"
	if got != (outcome{stdout: want}) {
		t.Errorf("got %+v, want stdout %q", got, want)
	}
}

func TestTagRefusesARecordedGraph(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "recorded-graph-reads-only", "tag-refuses-a-recorded-graph")
	dir, commits := releaseRepo(t)
	host := releaseHost(commits)
	host.result.RecordedGraph = &wire.RecordedGraph{ObservedAt: "2026-01-02T03:04:05Z", RefreshError: "go list failed"}
	got := runTag(t, dir, host, "--scope", "tooling", "--yes")
	want := outcome{code: 1, stderr: "version: putnami.support.json reads the dependency graph, which could not be refreshed: go list failed\n"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	assertNothingWritten(t, dir, commits[1])
}

// TestTagRefusals pins each refusal of a release: exit 2, the reason on
// stderr, nothing on stdout, and nothing written.
func TestTagRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, check string
		// setup prepares the repository and the host's answer, and returns
		// the arguments and the tags the repository holds.
		setup func(t *testing.T, dir string, commits []string, host *fakeHost) ([]string, []string)
		// stderr is the refusal for releaseRepo's commits.
		stderr func(commits []string) string
	}{
		{
			name: "several lines need a scope", check: "several-lines-need-a-scope",
			setup: func(t *testing.T, dir string, commits []string, host *fakeHost) ([]string, []string) {
				root := wire.VersionLine{Line: "", TagPattern: "v{version}", Release: &wire.LineRelease{Version: "0.0.1", Tag: "v0.0.1"}}
				host.result.Lines = append([]wire.VersionLine{root}, host.result.Lines...)
				return []string{"--yes"}, nil
			},
			stderr: func([]string) string {
				return "this workspace declares 2 version lines ((workspace root), tooling); name one with --scope\n"
			},
		},
		{
			name: "a tagged HEAD", check: "tagged-head-is-refused",
			setup: func(t *testing.T, dir string, commits []string, host *fakeHost) ([]string, []string) {
				git(t, dir, "tag", "-a", "tooling/v0.0.1", "-m", "release")
				line := &host.result.Lines[0]
				line.Tagged, line.Tag, line.Version, line.Release = true, "tooling/v0.0.1", "0.0.1", nil
				return []string{"--scope", "tooling", "--yes"}, []string{"tooling/v0.0.1"}
			},
			stderr: func([]string) string { return "HEAD is already tagged tooling/v0.0.1\n" },
		},
		{
			name: "an off-pattern tag", check: "an-off-pattern-tag-is-refused",
			setup: func(t *testing.T, dir string, commits []string, host *fakeHost) ([]string, []string) {
				return []string{"v9", "--scope", "tooling", "--yes"}, nil
			},
			stderr: func([]string) string {
				return `tag "v9" is not a release tag of line tooling: the pattern "tooling/v{version}" with a MAJOR.MINOR.PATCH version` + "\n"
			},
		},
		{
			name: "a run bound to another commit", check: "a-run-bound-to-another-commit-is-refused",
			setup: func(t *testing.T, dir string, commits []string, host *fakeHost) ([]string, []string) {
				host.result.SourceRevision = commits[0]
				return []string{"--scope", "tooling", "--yes"}, nil
			},
			stderr: func(commits []string) string {
				return "version tag releases the checked-out commit " + commits[1] +
					", but PUTNAMI_SOURCE_REVISION binds this run to " + commits[0] + "; unset it to tag HEAD\n"
			},
		},
		{
			name: "a host that proposes no release", check: "a-line-without-a-proposed-release-is-refused",
			setup: func(t *testing.T, dir string, commits []string, host *fakeHost) ([]string, []string) {
				host.result.Lines[0].Release = nil
				return []string{"--scope", "tooling", "--yes"}, nil
			},
			stderr: func([]string) string {
				return "the host proposes no release for line tooling; name the tag to release\n"
			},
		},
		{
			name: "an existing tag", check: "existing-tag-is-refused",
			setup: func(t *testing.T, dir string, commits []string, host *fakeHost) ([]string, []string) {
				git(t, dir, "tag", "-a", "tooling/v0.0.1", "-m", "release", commits[0])
				return []string{"--scope", "tooling", "--yes"}, []string{"tooling/v0.0.1"}
			},
			stderr: func([]string) string { return "git tag already exists: tooling/v0.0.1\n" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spectest.Proves(t, feature, "release-refusals", tc.check)
			dir, commits := releaseRepo(t)
			host := releaseHost(commits)
			args, tags := tc.setup(t, dir, commits, host)
			want := tc.stderr(commits)
			if got := runTag(t, dir, host, args...); got != (outcome{code: 2, stderr: want}) {
				t.Errorf("got %+v, want exit 2 with %q", got, want)
			}
			assertNothingWritten(t, dir, commits[1], tags...)
		})
	}
}

// TestTagReleasesAnExplicitTagWhenTheHostProposesNone pins that the missing
// proposal refuses only the release that needs it: an explicit tag names the
// version itself.
func TestTagReleasesAnExplicitTagWhenTheHostProposesNone(t *testing.T) {
	t.Parallel()
	dir, commits := releaseRepo(t)
	host := releaseHost(commits)
	host.result.Lines[0].Release = nil
	got := runTag(t, dir, host, "tooling/v2.3.4", "--scope", "tooling", "--yes")
	if got.code != 0 || got.stderr != "" || !strings.HasSuffix(got.stdout, "  Created tooling/v2.3.4 on the release commit\n") {
		t.Errorf("got %+v, want the release of the explicit tag", got)
	}
}

func TestTagRefusesADirtyTree(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "release-refusals", "dirty-tree-is-refused")
	dir, commits := releaseRepo(t)
	writeFile(t, dir, "notes.txt", "untracked\n")
	got := runTag(t, dir, releaseHost(commits), "--scope", "tooling", "--yes")
	if want := (outcome{code: 2, stderr: "git tree is not clean; commit or discard changes before tagging\n"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if head := git(t, dir, "rev-parse", "HEAD"); head != commits[1] {
		t.Errorf("HEAD moved to %s", head)
	}
	if _, err := os.Stat(filepath.Join(dir, "tooling", "CHANGELOG.md")); !os.IsNotExist(err) {
		t.Errorf("a refused release wrote the changelog: %v", err)
	}
}

// TestTagReportsAGitFailure pins that a git command that fails ends the
// release with exit 1 and git's own error: a commit hook that refuses, and a
// push without an origin.
func TestTagReportsAGitFailure(t *testing.T) {
	t.Parallel()
	dir, commits := releaseRepo(t)
	hooks := git(t, dir, "config", "core.hooksPath")
	writeFile(t, hooks, "pre-commit", "#!/bin/sh\necho 'the hook refuses' >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(hooks, "pre-commit"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := runTag(t, dir, releaseHost(commits), "--scope", "tooling", "--yes")
	if got.code != 1 || !strings.HasPrefix(got.stderr, "git commit: ") || !strings.Contains(got.stderr, "the hook refuses") {
		t.Errorf("a refused commit got %+v, want exit 1 with git's error", got)
	}
	if err := os.Remove(filepath.Join(hooks, "pre-commit")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "reset", "-q", "--hard")

	got = runTag(t, dir, releaseHost(commits), "--scope", "tooling", "--yes", "--push")
	if got.code != 1 || !strings.HasPrefix(got.stderr, "git push: ") || !strings.Contains(got.stdout, "  Created tooling/v0.0.1 on the release commit\n") {
		t.Errorf("a push without an origin got %+v, want the release created and exit 1 with git's error", got)
	}
}

// TestTagReportsARepositoryItCannotRead pins that a release outside a git
// repository fails with exit 1 before it writes anything.
func TestTagReportsARepositoryItCannotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := runTag(t, dir, releaseHost([]string{"0123456789abcdef0123456789abcdef01234567", "89abcdef0123456789abcdef0123456789abcdef"}), "--scope", "tooling", "--yes")
	if got.code != 1 || !strings.HasPrefix(got.stderr, "version tag: read HEAD: git rev-parse: ") {
		t.Errorf("got %+v, want exit 1 naming HEAD", got)
	}
}
