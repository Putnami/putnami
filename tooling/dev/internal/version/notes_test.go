package version

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
)

// otherFiles is what version notes prints for the last release of notesRepo.
const otherFiles = "  The last release also modified these files. Update the ones this release needs:\n" +
	"    CHANGELOG.md\n    tooling/plan.json\n"

// runNotes runs `version notes args` against host in root.
func runNotes(t *testing.T, root string, host Lines, args ...string) outcome {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := Command{Root: root, Host: host, Stdout: &stdout, Stderr: &stderr}
	code := command.Notes(t.Context(), args)
	return outcome{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// notesRepo is releaseRepo with two lines, on the branch release/notes.
//
// tooling's last release, 0.1.0, is a release commit: it adds the line's
// changelog and one file, and modifies the go line's changelog, two files that
// remain and one that a later commit deletes. go's last release, 0.9.0, is a
// tag on a commit that changes no changelog. It returns the host that answers
// both lines, bound to HEAD.
func notesRepo(t *testing.T) (string, *fakeHost) {
	t.Helper()
	dir, commits := releaseRepo(t)
	git(t, dir, "tag", "-a", "go/v0.9.0", "-m", "go", commits[1])
	write := func(message string, files map[string]string) string {
		for path, content := range files {
			writeFile(t, dir, path, content)
		}
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", message)
		return git(t, dir, "rev-parse", "HEAD")
	}
	write("docs: the release files", map[string]string{
		"CHANGELOG.md": "# Changelog\n", "go/CHANGELOG.md": "# Changelog — go\n\nEvery release.\n",
		"tooling/plan.json": "{}\n", "tooling/gone.json": "{}\n",
	})
	write("chore(release): Example 0.1.0", map[string]string{
		"CHANGELOG.md": "# Changelog\n\n## 0.1.0\n", "go/CHANGELOG.md": "# Changelog — go\n\nEvery go release.\n",
		"tooling/plan.json": "[]\n", "tooling/gone.json": "[]\n", "tooling/added.json": "{}\n",
		"tooling/CHANGELOG.md": "# Changelog — tooling\n\n## 0.1.0 — 2026-01-01\n\n- fix: older (1111111)\n",
	})
	git(t, dir, "tag", "-a", "tooling/v0.1.0", "-m", "tooling")
	if err := os.Remove(filepath.Join(dir, "tooling", "gone.json")); err != nil {
		t.Fatal(err)
	}
	head := write("feat(go): a thing", map[string]string{"go/a.go": "package a\n"})
	git(t, dir, "switch", "-q", "-c", "release/notes")
	return dir, &fakeHost{result: wire.VersionLinesResult{SourceRevision: head, Lines: []wire.VersionLine{
		{Line: "go", TagPattern: "go/v{version}", Version: "0.9.1-x", Release: &wire.LineRelease{Version: "0.9.1", Tag: "go/v0.9.1"},
			Commits: []wire.LineCommit{{SHA: head, Subject: "feat(go): a thing"}}},
		{Line: "tooling", TagPattern: "tooling/v{version}", Version: "0.1.1-x", Release: &wire.LineRelease{Version: "0.1.1", Tag: "tooling/v0.1.1"},
			Commits: []wire.LineCommit{{SHA: head, Subject: "fix(cli): the rest"}}},
	}}}
}

func TestNotesRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		dir, _ := notesRepo(t)
		return dir
	})
}

// rendered is the notes version notes renders today for a line of host.
func rendered(host *fakeHost, index int, version string) string {
	line := host.result.Lines[index]
	return renderChangelogAt(line.Line, version, line.Commits, time.Now())
}

func TestNotesCommitEveryChangelogAtOnce(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "notes-write-every-line-in-one-commit", "one-commit-holds-every-changelog")
	spectest.Proves(t, feature, "notes-write-every-line-in-one-commit", "notes-tag-and-push-nothing")
	spectest.Proves(t, feature, "notes-write-every-line-in-one-commit", "notes-name-the-other-files-of-the-last-release")
	dir, host := notesRepo(t)
	base, tags := git(t, dir, "rev-parse", "HEAD"), git(t, dir, "tag", "--list")
	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, dir, "init", "-q", "--bare", origin)
	git(t, dir, "remote", "add", "origin", origin)

	// 0.10.0 is above 0.9.0: versions compare by number.
	got := runNotes(t, dir, host, "tooling@0.2.0", "/go/@0.10.0", "--yes")
	if got.code != 0 || got.stderr != "" || !strings.HasSuffix(got.stdout, "  Committed the release notes on release/notes\n"+otherFiles) {
		t.Fatalf("got %+v, want the commit, then the other files", got)
	}
	if asked := host.asked(); len(asked) != 1 || asked[0] != "" {
		t.Errorf("the host was asked for %q, want every line once", asked)
	}
	if parent := git(t, dir, "rev-parse", "HEAD~1"); parent != base {
		t.Errorf("the notes commit's parent is %s, want %s", parent, base)
	}
	if message := git(t, dir, "log", "-1", "--format=%B"); message != "chore(release): prepare the release notes\n\n"+
		"- tooling 0.2.0\n- go 0.10.0\n\nCo-Authored-By: Release Human <human@example.invalid>" {
		t.Errorf("commit message %q", message)
	}
	if changed := git(t, dir, "show", "--format=", "--name-only", "HEAD"); changed != "go/CHANGELOG.md\ntooling/CHANGELOG.md" {
		t.Errorf("the commit changes %q, want both changelogs alone", changed)
	}
	for path, want := range map[string]string{
		"go/CHANGELOG.md":      "# Changelog — go\n\nEvery go release.\n\n" + rendered(host, 0, "0.10.0"),
		"tooling/CHANGELOG.md": "# Changelog — tooling\n\n" + rendered(host, 1, "0.2.0") + "\n## 0.1.0 — 2026-01-01\n\n- fix: older (1111111)\n",
	} {
		if body, err := os.ReadFile(filepath.Join(dir, path)); err != nil || string(body) != want {
			t.Errorf("%s = %q (%v), want %q", path, body, err, want)
		}
	}
	if status := git(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("the notes left %q in the tree", status)
	}
	if now := git(t, dir, "tag", "--list"); now != tags {
		t.Errorf("tags %q, want %q", now, tags)
	}
	if refs := git(t, origin, "for-each-ref"); refs != "" {
		t.Errorf("origin received %q", refs)
	}
}

// The root line is named /, and its first release creates no tag either. A line
// whose last release is no release commit, or that never released, names no
// other file.
func TestNotesOfTheRootLine(t *testing.T) {
	t.Parallel()
	dir, commits := releaseRepo(t)
	git(t, dir, "switch", "-q", "-c", "release/notes")
	host := &fakeHost{result: wire.VersionLinesResult{SourceRevision: commits[1], Lines: []wire.VersionLine{
		{Line: "", TagPattern: "v{version}", Commits: []wire.LineCommit{{SHA: commits[1], Subject: "fix(cli): stop the leak"}}}}}}
	if got := runNotes(t, dir, host, "/@1.0.0", "--yes"); got.code != 0 || !strings.HasSuffix(got.stdout, "  Committed the release notes on release/notes\n") {
		t.Fatalf("got %+v, want the commit and no other file", got)
	}
	if message := git(t, dir, "log", "-1", "--format=%b"); !strings.HasPrefix(message, "- (workspace root) 1.0.0\n") {
		t.Errorf("commit body %q", message)
	}
	if body := git(t, dir, "show", "HEAD:CHANGELOG.md"); !strings.HasPrefix(body, "# Changelog\n\n## 1.0.0 — ") {
		t.Errorf("changelog %q", body)
	}
}

func TestNotesDryRunPrintsTheResult(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "notes-are-reviewable-first", "notes-dry-run-prints-the-result")
	dir, host := notesRepo(t)
	head, tags := git(t, dir, "rev-parse", "HEAD"), strings.Fields(git(t, dir, "tag", "--list"))
	want := "  Would write tooling 0.2.0 into tooling/CHANGELOG.md\n" + rendered(host, 1, "0.2.0") + "\n" +
		"  Would write go 1.0.0 into go/CHANGELOG.md\n" + rendered(host, 0, "1.0.0") + "\n" + otherFiles
	if got := runNotes(t, dir, host, "tooling@0.2.0", "go@1.0.0", "--dry-run", "--yes"); got != (outcome{stdout: want}) {
		t.Errorf("--dry-run got %+v, want stdout %q", got, want)
	}
	// Only a run that would write needs a clean tree and a branch.
	git(t, dir, "switch", "-q", "main")
	writeFile(t, dir, "notes.txt", "untracked\n")
	var stdout, stderr strings.Builder
	forwarded := Command{Root: dir, DryRun: true, Host: host, Stdout: &stdout, Stderr: &stderr}
	if code := forwarded.Notes(t.Context(), []string{"tooling@0.2.0", "go@1.0.0"}); code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Errorf("a forwarded dry run exited %d with %q and %q, want stdout %q", code, stdout.String(), stderr.String(), want)
	}
	if err := os.Remove(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	assertNothingWritten(t, dir, head, tags...)
}

func TestNotesWithoutYesWriteNothing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "notes-are-reviewable-first", "notes-without-yes-write-nothing")
	dir, host := notesRepo(t)
	head, tags := git(t, dir, "rev-parse", "HEAD"), strings.Fields(git(t, dir, "tag", "--list"))
	want := outcome{stdout: "  Write these release notes in one commit on release/notes? Re-run with --yes to confirm.\n" +
		"    tooling 0.2.0\n    go 1.0.0\n"}
	if got := runNotes(t, dir, host, "tooling@0.2.0", "go@1.0.0"); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	assertNothingWritten(t, dir, head, tags...)
}

// TestNotesRefusals pins each refusal: its exit code, the reason on stderr,
// nothing on stdout, and nothing written. The refused line is the second one, so
// the first one is not written either.
func TestNotesRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, check string
		// args are the arguments after go@1.0.0 and --yes, and alone replaces
		// all of them.
		args, alone []string
		// setup changes the repository, or the host's answer.
		setup func(t *testing.T, dir string, host *fakeHost)
		// code is the exit code, 2 when absent. stderr starts the refusal.
		code   int
		stderr string
	}{
		{name: "no line", check: "notes-need-lines-and-versions", alone: []string{"--yes"}, stderr: notesUsage + "\n"},
		{name: "a line without a version", check: "notes-need-lines-and-versions", args: []string{"tooling"}, stderr: `"tooling" is not <line>@<version>; ` + notesUsage + "\n"},
		{name: "a line with an empty version", check: "notes-need-lines-and-versions", args: []string{"tooling@"}, stderr: `"tooling@" is not <line>@<version>; `},
		{name: "a version without a line", check: "notes-need-lines-and-versions", args: []string{"@0.2.0"}, stderr: `"@0.2.0" is not <line>@<version>; `},
		{name: "an unknown flag", check: "notes-need-lines-and-versions", args: []string{"--all@0.2.0"}, stderr: `"--all@0.2.0" is not <line>@<version>; `},
		{name: "a push", check: "notes-need-lines-and-versions", args: []string{"--push"}, stderr: notesUsage + "\n"},
		{name: "a scope", check: "notes-need-lines-and-versions", args: []string{"--scope", "go"}, stderr: notesUsage + "\n"},
		{name: "a scope without a value", check: "notes-need-lines-and-versions", args: []string{"--scope"}, stderr: "--scope requires a value\n"},
		{name: "a line named twice", check: "notes-need-lines-and-versions", args: []string{"/go@2.0.0"}, stderr: "line go is named twice\n"},
		{name: "an unknown line", check: "notes-refuse-an-unknown-line", args: []string{"rust@0.2.0"}, stderr: "no version line rust; this workspace declares go, tooling\n"},
		{name: "a pre-release", check: "notes-refuse-a-version-that-is-no-release", args: []string{"tooling@0.2.0-rc.1"}, stderr: `version "0.2.0-rc.1" of line tooling is not MAJOR.MINOR.PATCH` + "\n"},
		{name: "two numbers", check: "notes-refuse-a-version-that-is-no-release", args: []string{"tooling@0.2"}, stderr: `version "0.2" of line tooling is not MAJOR.MINOR.PATCH` + "\n"},
		{name: "a v prefix", check: "notes-refuse-a-version-that-is-no-release", args: []string{"tooling@v0.2.0"}, stderr: `version "v0.2.0" of line tooling is not MAJOR.MINOR.PATCH` + "\n"},
		{name: "the last release", check: "notes-refuse-a-version-that-does-not-increase", args: []string{"tooling@0.1.0"}, stderr: "version 0.1.0 of line tooling is not above its last release 0.1.0\n"},
		{name: "below the last release", check: "notes-refuse-a-version-that-does-not-increase", args: []string{"tooling@0.0.12"}, stderr: "version 0.0.12 of line tooling is not above its last release 0.1.0\n"},
		{
			name: "a tag HEAD does not reach", check: "notes-refuse-an-existing-tag", args: []string{"tooling@0.2.0"},
			setup: func(t *testing.T, dir string, _ *fakeHost) {
				git(t, dir, "tag", "-a", "tooling/v0.2.0", "-m", "apart", git(t, dir, "commit-tree", "HEAD^{tree}", "-m", "apart"))
			},
			stderr: "git tag already exists: tooling/v0.2.0\n",
		},
		{
			name: "a tagged HEAD", check: "notes-refuse-a-tagged-head", args: []string{"tooling@0.3.0"},
			setup: func(t *testing.T, dir string, host *fakeHost) {
				host.result.Lines[1].Tagged, host.result.Lines[1].Tag = true, "tooling/v0.2.0"
			},
			stderr: "HEAD is already tagged tooling/v0.2.0\n",
		},
		{
			name: "a version the changelog holds", check: "notes-refuse-a-version-already-noted", args: []string{"tooling@0.2.0"},
			setup: func(t *testing.T, dir string, host *fakeHost) {
				writeFile(t, dir, "tooling/CHANGELOG.md", "# Changelog — tooling\n\n## 0.2.0 — unreleased\n")
				git(t, dir, "commit", "-q", "-a", "-m", "docs: the notes")
				host.result.SourceRevision = git(t, dir, "rev-parse", "HEAD")
			},
			stderr: "tooling/CHANGELOG.md already holds a section for 0.2.0\n",
		},
		{
			name: "a run bound to another commit", check: "notes-refuse-a-run-bound-to-another-commit", args: []string{"tooling@0.2.0"},
			setup:  func(t *testing.T, dir string, host *fakeHost) { host.result.SourceRevision = strings.Repeat("0", 40) },
			stderr: "version notes releases the checked-out commit ",
		},
		{
			name: "a dirty tree", check: "notes-refuse-a-dirty-tree", args: []string{"tooling@0.2.0"},
			setup:  func(t *testing.T, dir string, _ *fakeHost) { writeFile(t, dir, "notes.txt", "untracked\n") },
			stderr: "git tree is not clean; commit or discard changes before writing the release notes\n",
		},
		{
			name: "the trunk of a clone without a remote", check: "notes-refuse-the-default-branch", args: []string{"tooling@0.2.0"},
			setup:  func(t *testing.T, dir string, _ *fakeHost) { git(t, dir, "switch", "-q", "main") },
			stderr: "main is the default branch; create a branch for the release notes with git switch -c <branch>\n",
		},
		{
			name: "the branch origin/HEAD names", check: "notes-refuse-the-default-branch", args: []string{"tooling@0.2.0"},
			setup: func(t *testing.T, dir string, _ *fakeHost) {
				git(t, dir, "update-ref", "refs/remotes/origin/release/notes", "HEAD")
				git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/release/notes")
			},
			stderr: "release/notes is the default branch; ",
		},
		{
			name: "a detached HEAD", check: "notes-refuse-a-detached-head", args: []string{"tooling@0.2.0"},
			setup:  func(t *testing.T, dir string, _ *fakeHost) { git(t, dir, "switch", "-q", "--detach") },
			stderr: "HEAD is detached; create a branch for the release notes with git switch -c <branch>\n",
		},
		{
			name: "a recorded graph", check: "notes-refuse-a-recorded-graph", args: []string{"tooling@0.2.0"}, code: 1,
			setup: func(t *testing.T, dir string, host *fakeHost) {
				host.result.RecordedGraph = &wire.RecordedGraph{ObservedAt: "2026-01-02T03:04:05Z", RefreshError: "go list failed"}
			},
			stderr: "version: putnami.support.json reads the dependency graph, which could not be refreshed: go list failed\n",
		},
		{
			name: "a tag git cannot read", check: "notes-report-a-git-failure", args: []string{"tooling@0.2.0"}, code: 1,
			setup: func(t *testing.T, dir string, _ *fakeHost) {
				writeFile(t, dir, ".git/refs/tags/tooling/v0.1.1", strings.Repeat("1", 40)+"\n")
			},
			stderr: "read the last release: ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spectest.Proves(t, feature, "notes-refusals", tc.check)
			dir, host := notesRepo(t)
			if tc.setup != nil {
				tc.setup(t, dir, host)
			}
			head, status := git(t, dir, "rev-parse", "HEAD"), git(t, dir, "status", "--porcelain")
			args := append([]string{"go@1.0.0", "--yes"}, tc.args...)
			if tc.alone != nil {
				args = tc.alone
			}
			code := tc.code
			if code == 0 {
				code = 2
			}
			if got := runNotes(t, dir, host, args...); got.code != code || got.stdout != "" || !strings.HasPrefix(got.stderr, tc.stderr) {
				t.Errorf("got %+v, want exit %d with %q alone", got, code, tc.stderr)
			}
			if now := git(t, dir, "rev-parse", "HEAD"); now != head {
				t.Errorf("HEAD moved to %s, want %s", now, head)
			}
			if now := git(t, dir, "status", "--porcelain"); now != status {
				t.Errorf("the tree changed: %q, want %q", now, status)
			}
		})
	}
}

// TestNotesReportAFailedCommit pins that a commit git refuses ends the command
// with exit 1 and git's own error, before it says the notes are committed.
func TestNotesReportAFailedCommit(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "notes-refusals", "notes-report-a-git-failure")
	dir, host := notesRepo(t)
	hook := filepath.Join(git(t, dir, "config", "core.hooksPath"), "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'the hook refuses' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := runNotes(t, dir, host, "tooling@0.2.0", "--yes")
	if got.code != 1 || got.stdout != "" || !strings.HasPrefix(got.stderr, "git commit: ") || !strings.Contains(got.stderr, "the hook refuses") {
		t.Errorf("got %+v, want exit 1 with git's error", got)
	}
}

// TestNotesReportAChangelogTheyCannotRead pins that a changelog that is no file
// is refused before any changelog is written.
func TestNotesReportAChangelogTheyCannotRead(t *testing.T) {
	t.Parallel()
	dir, host := notesRepo(t)
	if err := os.Remove(filepath.Join(dir, "tooling", "CHANGELOG.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "tooling/CHANGELOG.md/entry.md", "an entry\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "docs: a directory")
	head := git(t, dir, "rev-parse", "HEAD")
	host.result.SourceRevision = head
	if got := runNotes(t, dir, host, "go@1.0.0", "tooling@0.2.0", "--yes"); got.code != 1 || !strings.HasPrefix(got.stderr, "read tooling/CHANGELOG.md: ") {
		t.Errorf("got %+v, want exit 1 with the read error", got)
	}
	assertNothingWritten(t, dir, head, strings.Fields(git(t, dir, "tag", "--list"))...)
}

// A release sees the versions of a merge commit's change against its first
// parent: the files of the pull request the release commit is.
func TestNotesReadAReleaseThatIsAMergeCommit(t *testing.T) {
	t.Parallel()
	dir, host := notesRepo(t)
	git(t, dir, "switch", "-q", "main")
	git(t, dir, "switch", "-q", "-c", "other")
	writeFile(t, dir, "tooling/CHANGELOG.md", "# Changelog — tooling\n\n## 0.2.0\n")
	writeFile(t, dir, "go/a.go", "package b\n")
	git(t, dir, "commit", "-q", "-a", "-m", "chore(release): 0.2.0")
	git(t, dir, "switch", "-q", "release/notes")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge the release", "other")
	git(t, dir, "tag", "-a", "tooling/v0.2.0", "-m", "tooling")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "docs: later")
	host.result.SourceRevision = git(t, dir, "rev-parse", "HEAD")
	got := runNotes(t, dir, host, "tooling@0.3.0", "--dry-run")
	if got.code != 0 || !strings.HasSuffix(got.stdout, "Update the ones this release needs:\n    go/a.go\n") {
		t.Errorf("got %+v, want go/a.go named alone", got)
	}
}
