package observe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/observe/internal/gitbase"
)

// historyNow is the time the fixture histories are observed at.
var historyNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// historyClone is a clone of acme/widgets on github.com whose main branch
// holds one commit per age, the first one a root commit, oldest first, with
// origin/main on its last commit. The caller is on a feature branch one
// commit ahead, with a file it has not committed. It returns the clone and
// the commits of main.
func historyClone(t *testing.T, ages ...time.Duration) (string, []string) {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "--quiet", "--initial-branch=main")
	var shas []string
	for i, age := range ages {
		writeDemoFile(t, root, fmt.Sprintf("commit-%02d.txt", i), "commit\n")
		datedCommit(t, root, historyNow.Add(-age), fmt.Sprintf("commit %d", i))
		shas = append(shas, git(t, root, "rev-parse", "HEAD"))
	}
	git(t, root, "remote", "add", "origin", "git@github.com:acme/widgets.git")
	git(t, root, "update-ref", "refs/remotes/origin/main", shas[len(shas)-1])
	git(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	git(t, root, "checkout", "--quiet", "-b", "feature")
	writeDemoFile(t, root, "feature.txt", "feature\n")
	datedCommit(t, root, historyNow, "feature")
	writeDemoFile(t, root, "draft.txt", "not committed\n")
	return root, shas
}

func datedCommit(t *testing.T, root string, when time.Time, message string) {
	t.Helper()
	git(t, root, "add", "-A")
	cmd := gittest.Command(root, "commit", "--quiet", "-m", message)
	date := when.Format(time.RFC3339)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func days(n ...int) []time.Duration {
	out := make([]time.Duration, 0, len(n))
	for _, d := range n {
		out = append(out, time.Duration(d)*24*time.Hour)
	}
	return out
}

func historyOptions(root string) HistoryOptions {
	opts := laptopOptions(root, func(context.Context, string, string, string) (string, error) { return "4242", nil })
	return HistoryOptions{Options: opts, Window: DefaultHistoryWindow, Now: historyNow}
}

func observedSHAs(h History) []string {
	var out []string
	for _, c := range h.Commits {
		out = append(out, c.Commit.SHA)
	}
	return out
}

func historyHasCode(h History, code string) bool {
	for _, d := range h.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestHistoryObservesTheWindowOfTheDefaultBranch(t *testing.T) {
	spectest.Proves(t, feature, "recent-history", "history-observes-the-window-of-the-default-branch")
	root, shas := historyClone(t, days(20, 15, 10, 5, 1)...)
	h := RunHistory(context.Background(), historyOptions(root))
	if want := shas[2:]; !slices.Equal(observedSHAs(h), want) {
		t.Fatalf("observed %v, want %v; diagnostics %+v", observedSHAs(h), want, h.Diagnostics)
	}
	if h.Branch != "main" || h.InWindow != 3 || !h.Until.Equal(historyNow) || !h.Since.Equal(historyNow.Add(-DefaultHistoryWindow)) {
		t.Fatalf("history %s, %d in window, %s to %s", h.Branch, h.InWindow, h.Since, h.Until)
	}
	for i, c := range h.Commits {
		o := c.Observation
		if o.Subject.SHA != shas[i+2] || o.Subject.BaseSHA != shas[i+1] || o.Subject.Branch != "main" {
			t.Errorf("commit %d: subject %+v", i, o.Subject)
		}
		if o.Subject.ProjectID != "4242" || o.Subject.Repository != "acme/widgets" {
			t.Errorf("commit %d: repository %+v", i, o.Subject)
		}
		if want := fmt.Sprintf("commit-%02d.txt", i+2); len(o.ChangedFiles) != 1 || o.ChangedFiles[0].Path != want {
			t.Errorf("commit %d: changed files %+v, want %s", i, o.ChangedFiles, want)
		}
		if len(o.Reports) != 0 {
			t.Errorf("commit %d read reports %+v", i, o.Reports)
		}
		for _, d := range o.Diagnostics {
			if d.Code == "observe.report_missing" {
				t.Errorf("commit %d: %s", i, d.Message)
			}
		}
		validEvidence(t, o)
	}
	if len(h.Diagnostics) != 0 {
		t.Errorf("diagnostics %+v", h.Diagnostics)
	}

	// A window that reaches the root commit skips it: it has no parent.
	opts := historyOptions(root)
	opts.Window = 30 * 24 * time.Hour
	h = RunHistory(context.Background(), opts)
	if want := shas[1:]; !slices.Equal(observedSHAs(h), want) || h.InWindow != 5 || !historyHasCode(h, CodeHistoryParentMissing) {
		t.Fatalf("a window over the root commit observed %v, %d in window, diagnostics %+v", observedSHAs(h), h.InWindow, h.Diagnostics)
	}
}

func TestHistoryFallsBackToTheLastActivity(t *testing.T) {
	spectest.Proves(t, feature, "recent-history", "history-falls-back-to-the-last-activity")
	root, shas := historyClone(t, days(200, 100, 95, 90, 80)...)
	h := RunHistory(context.Background(), historyOptions(root))
	lastActivity := historyNow.Add(-80 * 24 * time.Hour)
	if !h.Until.Equal(lastActivity) || !h.Since.Equal(lastActivity.Add(-DefaultHistoryWindow)) {
		t.Fatalf("window %s to %s, want the 14 days that end at %s", h.Since, h.Until, lastActivity)
	}
	if want := shas[3:]; !slices.Equal(observedSHAs(h), want) {
		t.Fatalf("observed %v, want %v", observedSHAs(h), want)
	}

	// A clone that records no default branch reads the current branch and
	// says how to record it.
	git(t, root, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	h = RunHistory(context.Background(), historyOptions(root))
	if h.Branch != "feature" || !historyHasCode(h, CodeHistoryDefaultBranchMissing) || len(h.Commits) != 1 {
		t.Fatalf("without origin/HEAD: branch %q, %d commits, diagnostics %+v", h.Branch, len(h.Commits), h.Diagnostics)
	}
}

func TestHistorySamplesAtMost100CommitsEvenly(t *testing.T) {
	spectest.Proves(t, feature, "recent-history", "history-samples-at-most-100-commits-evenly")
	for _, n := range []int{1, 99, 100, 101, 150, 250, 1000} {
		commits := make([]gitbase.Commit, n)
		for i := range commits {
			commits[i] = gitbase.Commit{SHA: fmt.Sprint(i)}
		}
		sampled := SampleCommits(commits, MaxHistoryCommits)
		if want := min(n, MaxHistoryCommits); len(sampled) != want {
			t.Fatalf("%d commits: %d sampled, want %d", n, len(sampled), want)
		}
		if sampled[0].SHA != "0" || sampled[len(sampled)-1].SHA != fmt.Sprint(n-1) {
			t.Errorf("%d commits: the sample runs from %s to %s, not from the oldest to the newest", n, sampled[0].SHA, sampled[len(sampled)-1].SHA)
		}
		gaps := map[int]bool{}
		previous := -1
		for _, c := range sampled {
			var i int
			fmt.Sscan(c.SHA, &i)
			if i <= previous {
				t.Fatalf("%d commits: the sample is not ordered or repeats %d", n, i)
			}
			if previous >= 0 {
				gaps[i-previous] = true
			}
			previous = i
		}
		if len(gaps) > 2 {
			t.Errorf("%d commits: the gaps %v are not even", n, gaps)
		}
	}

	root, shas := historyClone(t, days(13, 12, 11, 10, 9, 8, 7, 6, 5)...)
	opts := historyOptions(root)
	opts.MaxCommits = 3
	h := RunHistory(context.Background(), opts)
	if want := []string{shas[1], shas[5], shas[8]}; !slices.Equal(observedSHAs(h), want) || h.InWindow != 9 {
		t.Fatalf("observed %v of %d, want %v", observedSHAs(h), h.InWindow, want)
	}
}

func TestHistoryIsBoundedInTime(t *testing.T) {
	spectest.Proves(t, feature, "recent-history", "history-is-bounded-in-time")
	root, shas := historyClone(t, days(6, 5, 4, 3, 2, 1)...)
	opts := historyOptions(root)
	opts.Budget = 150 * time.Second
	// Each reading of the clock is one minute later: the two newest commits
	// start 1 and 2 minutes in, the third would start 3 minutes in, past the
	// budget, and the oldest ones are left.
	tick := historyNow
	opts.Clock = func() time.Time {
		tick = tick.Add(time.Minute)
		return tick
	}
	h := RunHistory(context.Background(), opts)
	if want := shas[4:]; !slices.Equal(observedSHAs(h), want) {
		t.Fatalf("observed %v, want the newest %v", observedSHAs(h), want)
	}
	found := false
	for _, d := range h.Diagnostics {
		if d.Code == CodeHistoryBudgetSpent {
			found = strings.Contains(d.Message, "3 commits left unobserved")
		}
	}
	if !found {
		t.Fatalf("no budget warning counts the 3 commits left: %+v", h.Diagnostics)
	}

	// An interrupt during the run stops it and says so; the commits observed
	// before it are kept, oldest first, and the worktree is removed.
	worktrees := git(t, root, "worktree", "list", "--porcelain")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts = historyOptions(root)
	var kept []string
	opts.Each = func(c HistoryCommit) {
		kept = append(kept, c.Commit.SHA)
		if len(kept) == 2 {
			cancel()
		}
	}
	h = RunHistory(ctx, opts)
	if want := shas[4:]; !slices.Equal(observedSHAs(h), want) || !slices.Equal(kept, []string{shas[5], shas[4]}) {
		t.Fatalf("an interrupted run observed %v and handed %v, want %v newest first", observedSHAs(h), kept, want)
	}
	if !historyHasCode(h, CodeHistoryInterrupted) || historyHasCode(h, CodeHistoryBudgetSpent) {
		t.Fatalf("diagnostics %+v", h.Diagnostics)
	}
	if after := git(t, root, "worktree", "list", "--porcelain"); after != worktrees {
		t.Fatalf("the worktree outlived the interrupt:\n%s", after)
	}
}

func TestHistoryReadsABranchWhoseDatesAreOutOfOrder(t *testing.T) {
	// The third commit is dated 40 days ago, before the window, between two
	// commits in it: the walk goes past it.
	root, shas := historyClone(t, days(20, 10, 40, 5, 1)...)
	h := RunHistory(context.Background(), historyOptions(root))
	if want := []string{shas[1], shas[3], shas[4]}; !slices.Equal(observedSHAs(h), want) || h.InWindow != 3 {
		t.Fatalf("observed %v of %d, want %v", observedSHAs(h), h.InWindow, want)
	}
}

func TestHistoryRunsNoHookOfTheRepository(t *testing.T) {
	root, _ := historyClone(t, days(3, 2, 1)...)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(root, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := RunHistory(context.Background(), historyOptions(root))
	if len(h.Commits) != 2 || historyHasCode(h, CodeHistoryUnreadable) || historyHasCode(h, CodeHistoryCommitUnobserved) {
		t.Fatalf("observed %v, diagnostics %+v", observedSHAs(h), h.Diagnostics)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the post-checkout hook ran: %v", err)
	}
}

func TestHistoryRunIDsDeriveFromTheCommit(t *testing.T) {
	spectest.Proves(t, feature, "history-evidence", "history-run-ids-derive-from-the-commit")
	root, shas := historyClone(t, days(3, 2, 1)...)
	runIDs := func() []string {
		var ids []string
		for _, c := range RunHistory(context.Background(), historyOptions(root)).Commits {
			doc := validEvidence(t, c.Observation)
			if doc.Subject.SHA != c.Commit.SHA {
				t.Fatalf("the document of %s is about %s", c.Commit.SHA, doc.Subject.SHA)
			}
			ids = append(ids, doc.CI.RunID)
		}
		return ids
	}
	first, second := runIDs(), runIDs()
	want := []string{"history-" + shas[1], "history-" + shas[2]}
	if !slices.Equal(first, want) || !slices.Equal(second, want) {
		t.Fatalf("run ids %v then %v, want %v on every run", first, second, want)
	}
}

func TestHistoryLeavesTheWorkingTreeAsItWas(t *testing.T) {
	spectest.Proves(t, feature, "history-evidence", "history-leaves-the-working-tree-as-it-was")
	root, _ := historyClone(t, days(3, 2, 1)...)
	git(t, root, "add", "--intent-to-add", "draft.txt")
	state := func() string {
		return strings.Join([]string{
			git(t, root, "rev-parse", "HEAD"),
			git(t, root, "symbolic-ref", "HEAD"),
			git(t, root, "status", "--porcelain"),
			git(t, root, "worktree", "list", "--porcelain"),
		}, "\n")
	}
	before := state()
	h := RunHistory(context.Background(), historyOptions(root))
	if len(h.Commits) != 2 {
		t.Fatalf("observed %v", observedSHAs(h))
	}
	if after := state(); after != before {
		t.Fatalf("the caller's checkout changed:\n%s\nwas\n%s", after, before)
	}
	if data, err := os.ReadFile(root + "/draft.txt"); err != nil || string(data) != "not committed\n" {
		t.Fatalf("the uncommitted file reads %q, %v", data, err)
	}
}

func TestParseHistoryWindow(t *testing.T) {
	for value, want := range map[string]time.Duration{"": DefaultHistoryWindow, "36h": 36 * time.Hour, "30d": 30 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "365d": MaxHistoryWindow} {
		if got, err := ParseHistoryWindow(value); err != nil || got != want {
			t.Errorf("%q: %s, %v; want %s", value, got, err, want)
		}
	}
	for _, value := range []string{"0d", "-1d", "+3d", "14", "d", "3m", "366d", "53w", "1.5d", "99999999999999999999d"} {
		if _, err := ParseHistoryWindow(value); err == nil {
			t.Errorf("%q is accepted", value)
		}
	}
}
