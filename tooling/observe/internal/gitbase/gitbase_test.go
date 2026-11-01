package gitbase

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/ci"
)

const feature = "tooling/observe"

// fixture is an origin repository whose main has mainCommits commits after
// the fork point, and whose feature branch adds, modifies, deletes and renames
// files in two commits after it.
type fixture struct {
	origin    string
	forkPoint string
	mainTip   string
	featTip   string
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// The fixture's own commits and fetches start no automatic maintenance
	// either: a detached repack would still write in a temporary directory
	// the test is removing.
	cmd.Env = append(gitEnv(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=maintenance.auto", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=gc.auto", "GIT_CONFIG_VALUE_1=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T, historyCommits, mainCommits int) fixture {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin")
	run(t, t.TempDir(), "init", "--quiet", "--initial-branch=main", origin)
	run(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	write(t, origin, "keep.txt", "keep\n")
	write(t, origin, "old/name.txt", "a file that moves\nwith enough lines\nto be detected\nas a rename\n")
	write(t, origin, "gone.txt", "gone\n")
	run(t, origin, "add", "-A")
	run(t, origin, "commit", "--quiet", "-m", "root")
	for i := range historyCommits {
		write(t, origin, "history.txt", strings.Repeat("h", i+1))
		run(t, origin, "add", "-A")
		run(t, origin, "commit", "--quiet", "-m", "history")
	}
	f := fixture{origin: origin, forkPoint: run(t, origin, "rev-parse", "HEAD")}
	run(t, origin, "checkout", "--quiet", "-b", "feature")
	write(t, origin, "keep.txt", "kept and changed\n")
	write(t, origin, "src/new.txt", "new\n")
	run(t, origin, "add", "-A")
	run(t, origin, "commit", "--quiet", "-m", "feature one")
	if err := os.MkdirAll(filepath.Join(origin, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, origin, "mv", "old/name.txt", "new/name.txt")
	run(t, origin, "rm", "--quiet", "gone.txt")
	run(t, origin, "commit", "--quiet", "-m", "feature two")
	f.featTip = run(t, origin, "rev-parse", "HEAD")
	run(t, origin, "checkout", "--quiet", "main")
	for i := range mainCommits {
		write(t, origin, "main.txt", strings.Repeat("m", i+1))
		run(t, origin, "add", "-A")
		run(t, origin, "commit", "--quiet", "-m", "main")
	}
	f.mainTip = run(t, origin, "rev-parse", "HEAD")
	return f
}

// clone checks the feature branch out depth commits deep, 0 for the whole
// history, the way a CI checkout does.
func (f fixture) clone(t *testing.T, depth int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "checkout")
	args := []string{"clone", "--quiet", "--branch", "feature", "--single-branch", "--no-tags"}
	if depth > 0 {
		args = append(args, "--depth", strconv.Itoa(depth))
	}
	run(t, t.TempDir(), append(args, "file://"+f.origin, dir)...)
	return dir
}

var wantChanged = []ChangedFile{
	{Path: "gone.txt", Status: StatusDeleted},
	{Path: "keep.txt", Status: StatusModified},
	{Path: "new/name.txt", Status: StatusRenamed, PreviousPath: "old/name.txt"},
	{Path: "src/new.txt", Status: StatusAdded},
}

func codes(r Result) []string {
	if len(r.Diagnostics) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.Diagnostics))
	for _, d := range r.Diagnostics {
		out = append(out, d.Code)
	}
	return out
}

// traceGit records, in a trace2 event file, the processes every git command
// of the test starts, and returns the file.
func traceGit(t *testing.T) string {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "trace2.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	return trace
}

// noMaintenance fails when a traced git command started automatic
// maintenance, which goes on writing in the checkout after observe exits.
func noMaintenance(t *testing.T, trace string) {
	t.Helper()
	file, err := os.Open(trace)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	fetches := 0
	for scanner.Scan() {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || len(event.Argv) < 2 {
			continue
		}
		switch {
		case event.Event == "start" && event.Argv[1] == "fetch":
			fetches++
		case event.Event == "child_start" && event.Argv[0] == "git" && (event.Argv[1] == "maintenance" || event.Argv[1] == "gc"):
			t.Fatalf("a git command started %q", strings.Join(event.Argv, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if fetches == 0 {
		t.Fatal("the trace records no fetch")
	}
}

// state is what a run must leave as it was: the working tree, the index and
// HEAD.
func state(t *testing.T, dir string) string {
	t.Helper()
	return run(t, dir, "status", "--porcelain=v2", "--ignored", "--branch", "--untracked-files=all") + "\n" +
		run(t, dir, "rev-parse", "HEAD") + "\n" + run(t, dir, "ls-files", "--stage")
}

func TestAnExplicitBaseWins(t *testing.T) {
	spectest.Proves(t, feature, "diff-base", "an-explicit-base-wins")
	f := newFixture(t, 3, 2)
	dir := f.clone(t, 0)
	run(t, dir, "fetch", "--quiet", "origin", "+refs/heads/main:refs/remotes/origin/main")

	r := Resolve(context.Background(), Request{Dir: dir, Head: "HEAD", Base: &ci.Base{Ref: "HEAD~1", Source: ci.BaseFromFlag}, Fetch: true})
	if len(r.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v", r.Diagnostics)
	}
	if want := run(t, dir, "rev-parse", "HEAD~1"); r.BaseSHA != want {
		t.Fatalf("base %s, want HEAD~1 %s", r.BaseSHA, want)
	}
	want := []ChangedFile{
		{Path: "gone.txt", Status: StatusDeleted},
		{Path: "new/name.txt", Status: StatusRenamed, PreviousPath: "old/name.txt"},
	}
	if !reflect.DeepEqual(r.ChangedFiles, want) {
		t.Fatalf("changed %+v, want %+v", r.ChangedFiles, want)
	}

	r = Resolve(context.Background(), Request{Dir: dir, Head: "HEAD", Base: &ci.Base{SHA: f.forkPoint, Source: ci.BaseFromFlag}, Fetch: true})
	if r.BaseSHA != f.forkPoint || !reflect.DeepEqual(r.ChangedFiles, wantChanged) {
		t.Fatalf("a commit base gives %s %+v", r.BaseSHA, r.ChangedFiles)
	}
}

func TestChangedFilesAreMeasuredFromTheMergeBase(t *testing.T) {
	spectest.Proves(t, feature, "diff-base", "changed-files-are-measured-from-the-merge-base")
	f := newFixture(t, 3, 4)
	dir := f.clone(t, 0)

	// main moved on after the fork: its new commits are not the change's.
	r := Resolve(context.Background(), Request{Dir: dir, Head: "HEAD", Base: &ci.Base{Ref: "main", Source: ci.BaseFromPullRequest}, Fetch: true})
	if len(r.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v", r.Diagnostics)
	}
	if r.HeadSHA != f.featTip || r.BaseSHA != f.forkPoint {
		t.Fatalf("head %s base %s, want %s and the fork point %s", r.HeadSHA, r.BaseSHA, f.featTip, f.forkPoint)
	}
	if !reflect.DeepEqual(r.ChangedFiles, wantChanged) {
		t.Fatalf("changed %+v, want %+v", r.ChangedFiles, wantChanged)
	}
	if r.Root != run(t, dir, "rev-parse", "--show-toplevel") {
		t.Fatalf("root %s", r.Root)
	}
}

func TestAShallowCloneDeepensToTheMergeBase(t *testing.T) {
	spectest.Proves(t, feature, "diff-base", "a-shallow-clone-deepens-to-the-merge-base")
	spectest.Proves(t, feature, "caller-directory-writes", "observe-writes-only-git-history-in-the-caller-directory")
	f := newFixture(t, 3, 100)
	for name, base := range map[string]*ci.Base{
		"a pull request target branch": {Ref: "main", Source: ci.BaseFromPullRequest},
		"a merge request diff base":    {SHA: f.mainTip, Ref: "main", Source: ci.BaseFromMergeRequest},
		"a base commit only":           {SHA: f.mainTip, Source: ci.BaseFromMergeRequest},
	} {
		t.Run(name, func(t *testing.T) {
			dir := f.clone(t, 1)
			before := state(t, dir)
			trace := traceGit(t)
			r := Resolve(context.Background(), Request{Dir: dir, Head: "HEAD", Base: base, Fetch: true})
			noMaintenance(t, trace)
			if len(r.Diagnostics) != 0 {
				t.Fatalf("diagnostics %v", r.Diagnostics)
			}
			if r.BaseSHA != f.forkPoint {
				t.Fatalf("base %s, want the fork point %s", r.BaseSHA, f.forkPoint)
			}
			if r.Deepened == 0 {
				t.Fatal("the shallow clone reached the merge base without deepening")
			}
			if !reflect.DeepEqual(r.ChangedFiles, wantChanged) {
				t.Fatalf("changed %+v, want %+v", r.ChangedFiles, wantChanged)
			}
			if after := state(t, dir); after != before {
				t.Fatalf("the run changed the checkout:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

func TestAPullRequestMergeCommitIsMeasuredAsBuilt(t *testing.T) {
	spectest.Proves(t, feature, "diff-base", "a-pull-request-merge-commit-is-measured-as-built")
	f := newFixture(t, 3, 100)
	// GitHub builds a pull request on refs/pull/<n>/merge, a merge of the
	// feature into main, and checks it out one commit deep.
	run(t, f.origin, "checkout", "--quiet", "--detach", "main")
	run(t, f.origin, "merge", "--quiet", "--no-ff", "-m", "merge", "feature")
	merge := run(t, f.origin, "rev-parse", "HEAD")
	run(t, f.origin, "update-ref", "refs/pull/1/merge", merge)
	run(t, f.origin, "checkout", "--quiet", "main")
	dir := filepath.Join(t.TempDir(), "checkout")
	run(t, t.TempDir(), "init", "--quiet", dir)
	run(t, dir, "remote", "add", "origin", "file://"+f.origin)
	run(t, dir, "fetch", "--quiet", "--no-tags", "--depth=1", "origin", "+refs/pull/1/merge:refs/remotes/pull/1/merge")
	run(t, dir, "checkout", "--quiet", "--detach", "refs/remotes/pull/1/merge")

	trace := traceGit(t)
	r := Resolve(context.Background(), Request{Dir: dir, Head: "HEAD", Base: &ci.Base{Ref: "main", SHA: f.mainTip, Source: ci.BaseFromPullRequest}, Fetch: true})
	noMaintenance(t, trace)
	if len(r.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v", r.Diagnostics)
	}
	if r.HeadSHA != merge || r.BaseSHA != f.mainTip {
		t.Fatalf("head %s base %s, want the merge commit %s and main %s", r.HeadSHA, r.BaseSHA, merge, f.mainTip)
	}
	if !reflect.DeepEqual(r.ChangedFiles, wantChanged) {
		t.Fatalf("changed %+v, want the feature's own changes %+v", r.ChangedFiles, wantChanged)
	}
	if diff := run(t, dir, "diff", "--no-renames", "--name-only", r.BaseSHA, r.HeadSHA); diff != "gone.txt\nkeep.txt\nnew/name.txt\nold/name.txt\nsrc/new.txt" {
		t.Fatalf("the diff from the base to the head is %q", diff)
	}
}

func TestNoFetchReadsOnlyTheLocalHistory(t *testing.T) {
	spectest.Proves(t, feature, "caller-directory-writes", "observe-writes-only-git-history-in-the-caller-directory")
	f := newFixture(t, 3, 2)
	dir := f.clone(t, 1)

	r := Resolve(context.Background(), Request{Dir: dir, Head: "HEAD", Base: &ci.Base{Ref: "main", Source: ci.BaseFromPullRequest}, Fetch: false})
	if got := codes(r); !reflect.DeepEqual(got, []string{CodeBaseUnresolved}) {
		t.Fatalf("codes %v", got)
	}
	run(t, dir, "fetch", "--quiet", "--depth=1", "origin", "+refs/heads/main:refs/remotes/origin/main")
	shallow, err := os.ReadFile(filepath.Join(dir, ".git", "shallow"))
	if err != nil {
		t.Fatal(err)
	}
	objects := run(t, dir, "count-objects", "-v")
	r = Resolve(context.Background(), Request{Dir: dir, Head: "HEAD", Base: &ci.Base{Ref: "main", Source: ci.BaseFromPullRequest}, Fetch: false})
	if got := codes(r); !reflect.DeepEqual(got, []string{CodeMergeBaseUnreachable}) {
		t.Fatalf("codes %v", got)
	}
	if r.ChangedFiles != nil {
		t.Fatalf("changed files %+v without a merge base", r.ChangedFiles)
	}
	after, _ := os.ReadFile(filepath.Join(dir, ".git", "shallow"))
	if string(after) != string(shallow) || run(t, dir, "count-objects", "-v") != objects {
		t.Fatal("--no-fetch wrote Git history")
	}
}

func TestAnUnresolvedBaseIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "diff-base", "an-unresolved-base-is-a-diagnostic")
	f := newFixture(t, 1, 1)
	marker := filepath.Join(t.TempDir(), "ran")
	for name, tc := range map[string]struct {
		dir  func(t *testing.T) string
		req  Request
		code string
	}{
		"a branch origin does not have": {
			dir:  func(t *testing.T) string { return f.clone(t, 1) },
			req:  Request{Head: "HEAD", Base: &ci.Base{Ref: "missing", Source: ci.BaseFromPullRequest}, Fetch: true},
			code: CodeBaseUnresolved,
		},
		"a commit origin does not have": {
			dir:  func(t *testing.T) string { return f.clone(t, 1) },
			req:  Request{Head: "HEAD", Base: &ci.Base{SHA: strings.Repeat("ab", 20), Source: ci.BaseFromFlag}, Fetch: true},
			code: CodeBaseUnresolved,
		},
		"an option for a base": {
			dir:  func(t *testing.T) string { return f.clone(t, 0) },
			req:  Request{Head: "HEAD", Base: &ci.Base{Ref: "--upload-pack=touch /tmp/x", Source: ci.BaseFromFlag}, Fetch: true},
			code: CodeBaseUnresolved,
		},
		"an option for a base commit": {
			dir:  func(t *testing.T) string { return f.clone(t, 1) },
			req:  Request{Head: "HEAD", Base: &ci.Base{SHA: "--upload-pack=touch " + marker, Ref: "main", Source: ci.BaseFromMergeRequest}, Fetch: true},
			code: CodeBaseUnresolved,
		},
		"an unknown head": {
			dir:  func(t *testing.T) string { return f.clone(t, 0) },
			req:  Request{Head: "nope", Base: &ci.Base{Ref: "main", Source: ci.BaseFromFlag}, Fetch: true},
			code: CodeHeadUnresolved,
		},
		"a directory outside a repository": {
			dir:  func(t *testing.T) string { return t.TempDir() },
			req:  Request{Head: "HEAD", Base: &ci.Base{Ref: "main", Source: ci.BaseFromFlag}, Fetch: true},
			code: CodeNotARepository,
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := tc.req
			req.Dir = tc.dir(t)
			r := Resolve(context.Background(), req)
			if got := codes(r); !reflect.DeepEqual(got, []string{tc.code}) {
				t.Fatalf("codes %v, want %s", got, tc.code)
			}
			if r.BaseSHA != "" || r.ChangedFiles != nil {
				t.Fatalf("an unresolved base gave %s %+v", r.BaseSHA, r.ChangedFiles)
			}
		})
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a base commit reached git as an option")
	}
}

func TestUnrelatedHistoriesAreADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "diff-base", "an-unresolved-base-is-a-diagnostic")
	f := newFixture(t, 1, 1)
	dir := f.clone(t, 0)
	run(t, dir, "checkout", "--quiet", "--orphan", "other")
	run(t, dir, "commit", "--quiet", "-m", "unrelated")
	r := Resolve(context.Background(), Request{Dir: dir, Head: "HEAD", Base: &ci.Base{Ref: "feature", Source: ci.BaseFromFlag}, Fetch: true})
	if got := codes(r); !reflect.DeepEqual(got, []string{CodeMergeBaseUnreachable}) {
		t.Fatalf("codes %v", got)
	}
}

func TestNoBaseResolvesTheHeadOnly(t *testing.T) {
	spectest.Proves(t, feature, "diff-base", "an-unresolved-base-is-a-diagnostic")
	f := newFixture(t, 1, 1)
	r := Resolve(context.Background(), Request{Dir: f.clone(t, 1), Head: "HEAD", Fetch: true})
	if r.HeadSHA != f.featTip || r.BaseSHA != "" || r.ChangedFiles != nil || len(r.Diagnostics) != 0 {
		t.Fatalf("result %+v", r)
	}
}

func TestParseNameStatus(t *testing.T) {
	spectest.Proves(t, feature, "diff-base", "changed-files-are-measured-from-the-merge-base")
	files, err := parseNameStatus([]byte("A\x00a b.txt\x00T\x00link\x00C075\x00src.txt\x00copy.txt\x00"))
	if err != nil {
		t.Fatal(err)
	}
	want := []ChangedFile{
		{Path: "a b.txt", Status: StatusAdded},
		{Path: "link", Status: StatusModified},
		{Path: "copy.txt", Status: StatusAdded},
	}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files %+v", files)
	}
	for _, bad := range []string{"R100\x00only-one\x00", "X\x00a\x00", "M\x00"} {
		if _, err := parseNameStatus([]byte(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}
