package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
)

func TestIsShallowOnAFullClone(t *testing.T) {
	t.Parallel()
	shallow, err := IsShallow(initGitRepo(t))
	if err != nil {
		t.Fatalf("IsShallow: %v", err)
	}
	if shallow {
		t.Error("a freshly initialized repository reported as shallow")
	}
}

func TestTagsAtHead(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	if tags, err := TagsAtHead(dir); err != nil || len(tags) != 0 {
		t.Fatalf("TagsAtHead on an untagged commit = %v/%v, want none", tags, err)
	}
	gitDo(t, dir, "tag", "-a", "ts/v0.1.0", "-m", "a")
	gitDo(t, dir, "tag", "-a", "go/v0.1.0", "-m", "b")
	tags, err := TagsAtHead(dir)
	if err != nil {
		t.Fatalf("TagsAtHead: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("TagsAtHead = %v, want both tags", tags)
	}
}

// An untagged line is an ordinary state, not an error: it has never released.
func TestLastReachableTag(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	if _, _, ok, err := LastReachableTag(dir, "ts/v{version}"); err != nil || ok {
		t.Fatalf("LastReachableTag on an untagged line = %v/%v, want not found and no error", ok, err)
	}
	gitDo(t, dir, "tag", "-a", "ts/v0.1.0", "-m", "a")
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add")

	tag, commit, ok, err := LastReachableTag(dir, "ts/v{version}")
	if err != nil || !ok {
		t.Fatalf("LastReachableTag = %v/%v", ok, err)
	}
	if tag != "ts/v0.1.0" || len(commit) != 40 {
		t.Fatalf("tag/commit = %q/%q, want the tag and its full commit id", tag, commit)
	}
	// The other line's prefix must not match this one's tag.
	if _, _, ok, err := LastReachableTag(dir, "go/v{version}"); err != nil || ok {
		t.Fatalf("LastReachableTag(go) = %v/%v, want not found", ok, err)
	}
}

func TestLastReachableTagMatchesTheCompletePattern(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	gitDo(t, dir, "tag", "-a", "release-v0.4.0-stable", "-m", "release")
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add")
	// This tag is nearer and shares the literal prefix, but the line pattern
	// cannot render it because it lacks the required suffix.
	gitDo(t, dir, "tag", "-a", "release-v9.9.9-nightly", "-m", "other convention")
	writeCommit(t, dir, "typescript/b.ts", "fix(ts): repair")

	tag, _, ok, err := LastReachableTag(dir, "release-v{version}-stable")
	if err != nil || !ok || tag != "release-v0.4.0-stable" {
		t.Fatalf("LastReachableTag with suffix = %q/%v/%v, want release-v0.4.0-stable", tag, ok, err)
	}
}

// A line whose only tags sit on another branch has never released from HEAD:
// git describe fails there, and that failure is the untagged line.
func TestLastReachableTagIgnoresATagHEADCannotReach(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	gitDo(t, dir, "checkout", "-b", "release")
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add")
	gitDo(t, dir, "tag", "-a", "ts/v0.1.0", "-m", "a")
	gitDo(t, dir, "checkout", "main")

	if _, _, ok, err := LastReachableTag(dir, "ts/v{version}"); err != nil || ok {
		t.Fatalf("LastReachableTag with the tag on another branch = %v/%v, want not found and no error", ok, err)
	}
}

// A history git cannot read makes git describe print the untagged line's own
// message. It is still a failure, and it must say so: read as untagged, the
// line would silently version at 0.0.0.
func TestLastReachableTagReportsAHistoryGitCannotRead(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	gitDo(t, dir, "tag", "-a", "ts/v0.1.0", "-m", "a")
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add")
	writeCommit(t, dir, "typescript/b.ts", "fix(ts): repair")
	missing := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD~1"))
	if err := os.Remove(filepath.Join(dir, ".git", "objects", missing[:2], missing[2:])); err != nil {
		t.Fatal(err)
	}

	_, _, ok, err := LastReachableTag(dir, "ts/v{version}")
	if err == nil || ok {
		t.Fatalf("LastReachableTag on a history with a missing commit = %v/%v, want an error", ok, err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not name the unreadable commit %s", err, missing)
	}
}

// A describe that fails on a line HEAD has tagged, or a tag listing that fails
// behind it, is named in the error rather than read as the untagged line. The
// failures are injected by a git on PATH that refuses the named subcommands.
// Not parallel: it replaces PATH.
func TestLastReachableTagNamesAFailureOnATaggedLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The error naming is platform-independent and proven on Unix; the
		// injecting git is a #! script, which Windows cannot run from PATH.
		t.Skip("the injecting git is a POSIX shell script")
	}
	dir := initGitRepo(t)
	gitDo(t, dir, "tag", "-a", "ts/v0.1.0", "-m", "a")
	writeCommit(t, dir, "typescript/a.ts", "feat(ts): add")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		refused []string
		want    []string
	}{
		"describe fails":         {refused: []string{"describe"}, want: []string{"injected describe failure", "HEAD reaches ts/v0.1.0"}},
		"tag listing fails also": {refused: []string{"describe", "tag"}, want: []string{"injected describe failure", "injected tag failure"}},
	} {
		t.Run(name, func(t *testing.T) {
			bin := t.TempDir()
			var script strings.Builder
			script.WriteString("#!/bin/sh\n")
			for _, sub := range tc.refused {
				script.WriteString("[ \"$1\" = " + sub + " ] && { echo 'fatal: injected " + sub + " failure' >&2; exit 128; }\n")
			}
			script.WriteString("exec '" + realGit + "' \"$@\"\n")
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script.String()), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			_, _, ok, err := LastReachableTag(dir, "ts/v{version}")
			if err == nil || ok {
				t.Fatalf("LastReachableTag = %v/%v, want an error", ok, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// Set to debug git, GIT_TRACE writes to stderr; an untagged line must still
// read as untagged rather than as a failure. Not parallel: it sets GIT_TRACE.
func TestLastReachableTagUnderGitTrace(t *testing.T) {
	dir := initGitRepo(t)
	t.Setenv("GIT_TRACE", "1")
	if _, _, ok, err := LastReachableTag(dir, "ts/v{version}"); err != nil || ok {
		t.Fatalf("LastReachableTag on an untagged line under GIT_TRACE = %v/%v, want not found and no error", ok, err)
	}
}

func TestCommitsSinceReadsSubjectBodyAndPathspec(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	base, err := HeadSHA(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeCommitWithBody(t, dir, "typescript/a.ts", "feat(ts): add", "why\n\nBREAKING CHANGE: gone")
	writeCommit(t, dir, "go/a.go", "fix(go): stop")

	all, err := CommitsSince(dir, base, nil)
	if err != nil {
		t.Fatalf("CommitsSince: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("CommitsSince(whole tree) = %d commits, want 2", len(all))
	}
	if all[0].Subject != "fix(go): stop" {
		t.Errorf("first commit = %q, want the newest first", all[0].Subject)
	}
	scoped, err := CommitsSince(dir, base, []string{"typescript"})
	if err != nil {
		t.Fatalf("CommitsSince(typescript): %v", err)
	}
	if len(scoped) != 1 || scoped[0].Subject != "feat(ts): add" {
		t.Fatalf("CommitsSince(typescript) = %+v, want only the typescript commit", scoped)
	}
	if !strings.Contains(scoped[0].Body, "BREAKING CHANGE:") || len(scoped[0].SHA) != 40 {
		t.Errorf("commit = %+v, want the body and the full SHA", scoped[0])
	}

	// An empty fromCommit walks the whole history, which is the untagged line.
	whole, err := CommitsSince(dir, "", nil)
	if err != nil || len(whole) != 3 {
		t.Fatalf("CommitsSince(whole history) = %d/%v, want 3 commits", len(whole), err)
	}
}

// A rename lists both of its paths, so a file moved out of a project still
// counts as a change to it; the pathspec limits the files; a path git would
// quote keeps its bytes.
func TestCommitFilesListsBothPathsOfARename(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	writeCommit(t, dir, "typescript/é \"q\".ts", "feat(ts): add")
	if err := os.MkdirAll(filepath.Join(dir, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitDo(t, dir, "mv", "typescript/é \"q\".ts", "go/é \"q\".ts")
	gitDo(t, dir, "commit", "-m", "refactor!: move")
	head, err := HeadSHA(dir)
	if err != nil {
		t.Fatal(err)
	}

	files, err := CommitFiles(dir, head, nil)
	if err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	if !slices.Equal(files, []string{"go/é \"q\".ts", "typescript/é \"q\".ts"}) {
		t.Errorf("files = %q, want both paths of the rename", files)
	}
	scoped, err := CommitFiles(dir, head, []string{"typescript"})
	if err != nil || !slices.Equal(scoped, []string{"typescript/é \"q\".ts"}) {
		t.Errorf("CommitFiles(typescript) = %q/%v, want only the typescript path", scoped, err)
	}
	if _, err := CommitFiles(dir, "0000000000000000000000000000000000000000", nil); err == nil {
		t.Error("CommitFiles accepted a commit the repository does not have")
	}
}

func TestTreeStateReportsTheWorkingTree(t *testing.T) {
	t.Parallel()
	dir := initGitRepo(t)
	state, err := TreeState(dir)
	if err != nil {
		t.Fatalf("TreeState: %v", err)
	}
	if state.Branch != "main" || state.SHA == "" || state.Suffix == "" || state.IsDirty {
		t.Fatalf("TreeState = %+v, want a clean main checkout", state)
	}
	if state.Base != "" || state.Full != "" {
		t.Errorf("TreeState = %+v, want no version: the line is not known here", state)
	}
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := TreeState(dir)
	if err != nil || !dirty.IsDirty {
		t.Fatalf("TreeState on a dirty tree = %+v/%v", dirty, err)
	}
}

// The line tag placeholder is restated in this package rather than imported.
// This is the guard that keeps the two spellings equal.
func TestLineTagPlaceholderMatchesTheWorkspaceProtocol(t *testing.T) {
	t.Parallel()
	if lineTagPlaceholder != wsproto.LineTagPlaceholder {
		t.Fatalf("placeholder = %q, want the workspace protocol's %q", lineTagPlaceholder, wsproto.LineTagPlaceholder)
	}
	pattern := wsproto.LineTagPattern("typescript", &wsproto.LineConfig{Tag: "ts/v{version}"})
	version, ok := versionFromLineTag(wsproto.RenderLineTag(pattern, "1.2.3"), pattern)
	if !ok || version != "1.2.3" {
		t.Fatalf("versionFromLineTag = %q/%v, want the rendered version back", version, ok)
	}
}

// writeCommitWithBody writes one file and commits it with a subject and a body.
func writeCommitWithBody(t *testing.T, dir, relPath, subject, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(subject), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, dir, "add", ".")
	gitDo(t, dir, "commit", "-m", subject, "-m", body)
}

// commitAt writes one file and commits it with the given committer date, so a
// test can tell which commit's time a version suffix carries.
func commitAt(t *testing.T, dir, relPath, subject, date string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(relPath)), []byte(subject), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", subject}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+date, "GIT_AUTHOR_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
}

// The override names WHICH COMMIT a run is bound to; the tree it built stays
// HEAD's. These tests set the environment, so none of them is parallel.
func TestTreeStateFollowsTheSourceRevisionOverride(t *testing.T) {
	dir := initGitRepo(t)
	bound := commitAt(t, dir, "bound.txt", "the bound commit", "2026-01-02T03:04:05Z")
	boundShort := bound[:sourceRevisionShortLength]
	head := commitAt(t, dir, "merge.txt", "the synthetic merge", "2026-03-04T05:06:07Z")
	headShort := strings.TrimSpace(runGit(t, dir, "rev-parse", "--short", head))

	t.Setenv(SourceRevisionEnv, "")
	unset, err := TreeState(dir)
	if err != nil {
		t.Fatalf("TreeState: %v", err)
	}
	if unset.SHA != headShort || unset.Suffix != "20260304050607-"+headShort {
		t.Fatalf("TreeState without the override = %+v, want HEAD %s", unset, headShort)
	}

	// A reachable bound commit: its fixed-length prefix and git's committer
	// time. The time variable is ignored because git can answer.
	t.Setenv(SourceRevisionEnv, bound)
	t.Setenv(SourceCommitTimeEnv, "1")
	state, err := TreeState(dir)
	if err != nil {
		t.Fatalf("TreeState with the override: %v", err)
	}
	if state.SHA != boundShort || state.Suffix != "20260102030405-"+boundShort {
		t.Fatalf("TreeState = %+v, want the bound commit %s at its own time", state, boundShort)
	}
	if state.Branch != "main" || state.IsDirty {
		t.Fatalf("TreeState = %+v, want the checkout's branch and a clean tree", state)
	}

	// The dirty flag and hash are the checkout's: they report the tree that
	// was built, under the bound commit's identity.
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := TreeState(dir)
	if err != nil || !dirty.IsDirty || !strings.HasPrefix(dirty.Suffix, "20260102030405-"+boundShort+"-") {
		t.Fatalf("TreeState on a dirty tree = %+v/%v, want the bound identity plus a dirty hash", dirty, err)
	}
	if err := os.Remove(filepath.Join(dir, "dirty.txt")); err != nil {
		t.Fatal(err)
	}

	// Binding to HEAD itself keeps HEAD's time and tree state, with the fixed
	// prefix instead of git's abbreviation: one commit has one version whether
	// or not a runner's repository has it.
	t.Setenv(SourceRevisionEnv, head)
	atHead, err := TreeState(dir)
	want := &VersionInfo{SHA: head[:sourceRevisionShortLength], Branch: unset.Branch, Suffix: "20260304050607-" + head[:sourceRevisionShortLength]}
	if err != nil || !reflect.DeepEqual(atHead, want) {
		t.Fatalf("TreeState bound to HEAD = %+v/%v, want %+v", atHead, err, want)
	}
	// Whitespace is unset.
	t.Setenv(SourceRevisionEnv, "  \t")
	blank, err := TreeState(dir)
	if err != nil || !reflect.DeepEqual(blank, unset) {
		t.Fatalf("TreeState with a blank override = %+v/%v, want %+v", blank, err, unset)
	}
}

func TestTreeStateDatesAnUnreachableSourceRevisionFromTheEnvironment(t *testing.T) {
	dir := initGitRepo(t)
	const unreachable = "0123456789abcdef0123456789abcdef01234567"
	committedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t.Setenv(SourceRevisionEnv, unreachable)
	t.Setenv(SourceCommitTimeEnv, strconv.FormatInt(committedAt.Unix(), 10))
	state, err := TreeState(dir)
	if err != nil {
		t.Fatalf("TreeState: %v", err)
	}
	if state.SHA != "0123456789ab" || state.Suffix != "20260102030405-0123456789ab" || state.IsDirty || state.Branch != "main" {
		t.Fatalf("TreeState = %+v, want the first twelve characters at the given time", state)
	}

	// A SHA-256 id is a valid override; the protocol's 40-character rule is
	// the release-set's to enforce, not the suffix's.
	t.Setenv(SourceRevisionEnv, strings.Repeat("ab", 32))
	state, err = TreeState(dir)
	if err != nil || state.SHA != "abababababab" || state.Suffix != "20260102030405-abababababab" {
		t.Fatalf("TreeState with a 64-character override = %+v/%v", state, err)
	}

	// Without the time the run fails: HEAD's time is never the fallback.
	t.Setenv(SourceRevisionEnv, unreachable)
	for _, value := range []string{"", "  ", "0", "-1", "abc", "1.5"} {
		t.Setenv(SourceCommitTimeEnv, value)
		if _, err := TreeState(dir); err == nil || !strings.Contains(err.Error(), SourceCommitTimeEnv) {
			t.Fatalf("TreeState with %s=%q = %v, want an error naming the variable", SourceCommitTimeEnv, value, err)
		}
	}
}

func TestTreeStateRefusesAMalformedSourceRevision(t *testing.T) {
	dir := initGitRepo(t)
	valid := strings.Repeat("0123456789abcdef", 2) + "89abcdef"
	for _, value := range []string{
		strings.ToUpper(valid),         // uppercase is refused, not folded
		valid[:39],                     // one short of a SHA-1
		valid + "0",                    // one past a SHA-1
		strings.Repeat("ab", 31) + "a", // one short of a SHA-256
		"g" + valid[1:],                // not hex
		"HEAD",                         // a ref name, not a commit id
		"--" + valid[2:],               // a flag-shaped value never reaches git
	} {
		t.Setenv(SourceRevisionEnv, value)
		_, err := TreeState(dir)
		if err == nil || !strings.Contains(err.Error(), SourceRevisionEnv) {
			t.Fatalf("TreeState with %s=%q = %v, want an error naming the variable", SourceRevisionEnv, value, err)
		}
	}
}

func TestSourceRevisionOverrideTrimsAndValidates(t *testing.T) {
	sha1 := strings.Repeat("0123456789abcdef", 2) + "89abcdef"
	sha256 := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		value    string
		revision string
		set      bool
		fails    bool
	}{
		{value: "", set: false},
		{value: " \n", set: false},
		{value: sha1, revision: sha1, set: true},
		{value: " " + sha1 + "\n", revision: sha1, set: true},
		{value: sha256, revision: sha256, set: true},
		{value: strings.ToUpper(sha1), set: true, fails: true},
		{value: sha1[:39], set: true, fails: true},
	} {
		t.Setenv(SourceRevisionEnv, tc.value)
		revision, set, err := SourceRevisionOverride()
		if (err != nil) != tc.fails || set != tc.set || revision != tc.revision {
			t.Fatalf("SourceRevisionOverride(%q) = %q/%v/%v, want %q/%v/fails=%v", tc.value, revision, set, err, tc.revision, tc.set, tc.fails)
		}
	}
}

// BoundSourceRevision accepts the override only when the version suffix can be
// stamped from it, so a caller that records the revision cannot outlive a
// TreeState that failed. Unset, it runs no git command and never fails.
func TestBoundSourceRevisionRequiresAnEstablishedIdentity(t *testing.T) {
	dir := initGitRepo(t)
	outside := t.TempDir()
	const unreachable = "0123456789abcdef0123456789abcdef01234567"
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	t.Setenv(SourceRevisionEnv, "")
	t.Setenv(SourceCommitTimeEnv, "")
	if revision, set, err := BoundSourceRevision(outside); revision != "" || set || err != nil {
		t.Fatalf("BoundSourceRevision unset = %q/%v/%v, want nothing and no error outside git", revision, set, err)
	}
	t.Setenv(SourceRevisionEnv, head)
	if revision, set, err := BoundSourceRevision(dir); revision != head || !set || err != nil {
		t.Fatalf("BoundSourceRevision(HEAD) = %q/%v/%v, want HEAD", revision, set, err)
	}

	t.Setenv(SourceRevisionEnv, unreachable)
	for _, root := range []string{dir, outside} {
		if revision, set, err := BoundSourceRevision(root); revision != "" || !set || err == nil || !strings.Contains(err.Error(), SourceCommitTimeEnv) {
			t.Fatalf("BoundSourceRevision without a time in %s = %q/%v/%v, want an error naming %s", root, revision, set, err, SourceCommitTimeEnv)
		}
	}
	t.Setenv(SourceCommitTimeEnv, "1767323045")
	for _, root := range []string{dir, outside} {
		if revision, set, err := BoundSourceRevision(root); revision != unreachable || !set || err != nil {
			t.Fatalf("BoundSourceRevision with a time in %s = %q/%v/%v, want the override", root, revision, set, err)
		}
	}

	t.Setenv(SourceRevisionEnv, strings.ToUpper(unreachable))
	if revision, set, err := BoundSourceRevision(dir); revision != "" || !set || err == nil || !strings.Contains(err.Error(), SourceRevisionEnv) {
		t.Fatalf("BoundSourceRevision malformed = %q/%v/%v, want an error naming %s", revision, set, err, SourceRevisionEnv)
	}
}

// One bound commit publishes under one version: a runner whose repository has
// the commit and a runner whose repository does not, given the commit's time,
// stamp the same SHA and suffix. Git's own abbreviation would differ between
// the two, and grows with the repository.
func TestOneBoundCommitHasOneVersionOnEveryRunner(t *testing.T) {
	with := initGitRepo(t)
	bound := commitAt(t, with, "bound.txt", "the bound commit", "2026-01-02T03:04:05Z")
	gitDo(t, with, "config", "core.abbrev", "20")
	without := initGitRepo(t)

	t.Setenv(SourceRevisionEnv, bound)
	t.Setenv(SourceCommitTimeEnv, strconv.FormatInt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix(), 10))
	reachable, err := TreeState(with)
	if err != nil {
		t.Fatalf("TreeState where the commit is reachable: %v", err)
	}
	unreachable, err := TreeState(without)
	if err != nil {
		t.Fatalf("TreeState where the commit is not reachable: %v", err)
	}
	want := "20260102030405-" + bound[:sourceRevisionShortLength]
	if reachable.SHA != unreachable.SHA || reachable.Suffix != want || unreachable.Suffix != want {
		t.Fatalf("SHA/Suffix = %q/%q and %q/%q, want %q on both runners",
			reachable.SHA, reachable.Suffix, unreachable.SHA, unreachable.Suffix, want)
	}
}
