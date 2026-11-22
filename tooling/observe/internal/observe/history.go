package observe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/observe/internal/ci"
	"go.putnami.dev/tooling/observe/internal/gitbase"
)

// Bounds of putnami observe --history.
const (
	// DefaultHistoryWindow is the window --history observes without a
	// duration.
	DefaultHistoryWindow = 14 * 24 * time.Hour
	// MaxHistoryWindow is the longest window --history takes.
	MaxHistoryWindow = 365 * 24 * time.Hour
	// MaxHistoryCommits caps the commits one run observes.
	MaxHistoryCommits = 100
	// HistoryBudget bounds the time one run spends observing commits.
	HistoryBudget = 10 * time.Minute
	// HistoryRunPrefix starts the run id of a commit's evidence document.
	HistoryRunPrefix = "history-"
)

// Diagnostic codes of putnami observe --history.
const (
	CodeHistoryBudgetSpent          = "observe.history_budget_spent"
	CodeHistoryUnreadable           = "observe.history_unreadable"
	CodeHistoryDefaultBranchMissing = "observe.history_default_branch_missing"
	CodeHistoryParentMissing        = "observe.history_parent_missing"
	CodeHistoryCommitUnobserved     = "observe.history_commit_unobserved"
)

// ParseHistoryWindow reads the duration --history takes: <n>h, <n>d or <n>w,
// from 1 hour to MaxHistoryWindow; "" is DefaultHistoryWindow.
func ParseHistoryWindow(value string) (time.Duration, error) {
	if value == "" {
		return DefaultHistoryWindow, nil
	}
	units := map[byte]time.Duration{'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	unit, ok := units[value[len(value)-1]]
	n, err := strconv.Atoi(value[:len(value)-1])
	if !ok || err != nil || n < 1 || strings.HasPrefix(value, "+") {
		return 0, fmt.Errorf("--history takes a duration such as 36h, 14d or 2w, got %q", value)
	}
	if time.Duration(n) > MaxHistoryWindow/unit {
		return 0, fmt.Errorf("--history takes at most 365d, got %q", value)
	}
	return time.Duration(n) * unit, nil
}

// HistoryOptions are the inputs of one --history run.
type HistoryOptions struct {
	// Options are the options of each commit's observation: its directory,
	// producer, decision directories, detectors and project id lookup. The
	// environment, the base, the head, the reports and the fetch are set per
	// commit.
	Options
	// Window is the length of the observed window.
	Window time.Duration
	// Now is when the run started.
	Now time.Time
	// Budget bounds the time spent observing; zero is HistoryBudget.
	Budget time.Duration
	// MaxCommits caps the observed commits; zero is MaxHistoryCommits.
	MaxCommits int
	// Clock reads the time the budget is measured with; nil is time.Now.
	Clock func() time.Time
}

// History is what one --history run observed.
type History struct {
	// Branch is the branch the commits are on.
	Branch string `json:"branch"`
	// Since and Until bound the window: a commit dated after Since and not
	// after Until is in it.
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	// InWindow counts the first-parent commits of the window.
	InWindow int `json:"inWindow"`
	// Commits are the observed commits, oldest first.
	Commits     []HistoryCommit   `json:"commits"`
	Diagnostics []diag.Diagnostic `json:"diagnostics"`
}

// HistoryCommit is one observed commit.
type HistoryCommit struct {
	Commit      gitbase.Commit `json:"commit"`
	Observation Observation    `json:"observation"`
}

// RunHistory observes the recent commits of the default branch, each in a
// temporary detached worktree measured against its first parent.
func RunHistory(ctx context.Context, opts HistoryOptions) History {
	h := History{Commits: []HistoryCommit{}, Diagnostics: []diag.Diagnostic{}}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	started := clock()
	budget := opts.Budget
	if budget <= 0 {
		budget = HistoryBudget
	}
	limit := opts.MaxCommits
	if limit <= 0 {
		limit = MaxHistoryCommits
	}
	root, err := gitbase.Root(ctx, opts.Dir)
	if err != nil {
		h.warn(CodeHistoryUnreadable, "%s is not inside a Git repository: %v", opts.Dir, err)
		return h
	}
	ref, branch := historyBranch(ctx, root, &h)
	last, err := gitbase.LastCommit(ctx, root, ref)
	if err != nil {
		h.warn(CodeHistoryUnreadable, "the branch %s names no commit: %v", ref, err)
		return h
	}
	h.Branch = branch
	h.Until = opts.Now.UTC().Truncate(time.Second)
	if last.Time.Before(h.Until.Add(-opts.Window)) {
		h.Until = last.Time
	}
	h.Since = h.Until.Add(-opts.Window)
	commits, err := gitbase.FirstParentCommits(ctx, root, ref, h.Since, h.Until)
	if err != nil {
		h.warn(CodeHistoryUnreadable, "the history of %s cannot be read: %v", ref, err)
		return h
	}
	h.InWindow = len(commits)
	withParent := commits[:0:0]
	for _, c := range commits {
		if c.Parent != "" {
			withParent = append(withParent, c)
		}
	}
	if skipped := len(commits) - len(withParent); skipped > 0 {
		h.Diagnostics = append(h.Diagnostics, diag.Diagnostic{Severity: diag.Info, Code: CodeHistoryParentMissing, Field: "history",
			Message: fmt.Sprintf("%s of the window %s no parent in this clone and %s skipped: a root commit, or the edge of a shallow clone", count(skipped, "commit"), hasHave(skipped), isAre(skipped))})
	}
	sampled := SampleCommits(withParent, limit)
	if len(sampled) == 0 {
		return h
	}
	tree, cleanup, err := historyWorktree(ctx, root, sampled[0].SHA)
	if err != nil {
		h.warn(CodeHistoryUnreadable, "no worktree could check out %s: %v", sampled[0].SHA, err)
		return h
	}
	defer cleanup()
	commitOpts := historyCommitOptions(opts.Options, tree)
	for i, c := range sampled {
		if ctx.Err() != nil || clock().Sub(started) >= budget {
			h.warn(CodeHistoryBudgetSpent, "the run spent its %s budget: %s left unobserved; observe a shorter window with --history <duration>", budget, count(len(sampled)-i, "commit"))
			break
		}
		if i > 0 {
			if err := gitbase.CheckoutDetached(ctx, tree, c.SHA); err != nil {
				h.warn(CodeHistoryCommitUnobserved, "the commit %s cannot be checked out: %v", short(c.SHA), err)
				continue
			}
		}
		h.Commits = append(h.Commits, HistoryCommit{Commit: c, Observation: observeCommit(ctx, commitOpts, c, branch)})
	}
	return h
}

// SampleCommits keeps at most limit commits, evenly spread from the first to
// the last.
func SampleCommits(commits []gitbase.Commit, limit int) []gitbase.Commit {
	if len(commits) <= limit {
		return commits
	}
	if limit == 1 {
		return commits[len(commits)-1:]
	}
	out := make([]gitbase.Commit, 0, limit)
	span, steps := len(commits)-1, limit-1
	for i := range limit {
		out = append(out, commits[(i*span+steps/2)/steps])
	}
	return out
}

// historyBranch returns the revision and the name of the branch --history
// reads: the origin remote's default branch, else the current branch.
func historyBranch(ctx context.Context, root string, h *History) (ref, name string) {
	if branch, err := gitbase.DefaultBranch(ctx, root); err == nil && branch != "" {
		return branch, strings.TrimPrefix(branch, "origin/")
	}
	current := gitbase.CurrentBranch(ctx, root)
	label := current
	if label == "" {
		label = "the detached HEAD"
	}
	h.warn(CodeHistoryDefaultBranchMissing,
		"the clone records no default branch of origin, so %s is observed instead: run `git remote set-head origin --auto` to read the default branch", label)
	return "HEAD", current
}

// historyWorktree checks sha out in a temporary detached worktree named
// after the repository, so a repository without an origin remote keeps its
// name, and returns the worktree and the function that removes it.
func historyWorktree(ctx context.Context, root, sha string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "putnami-history-")
	if err != nil {
		return "", nil, err
	}
	tree := filepath.Join(dir, filepath.Base(root))
	if err := gitbase.AddWorktree(ctx, root, tree, sha); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return tree, func() {
		// The removal outlives a canceled run.
		_ = gitbase.RemoveWorktree(context.WithoutCancel(ctx), root, tree)
		os.RemoveAll(dir)
	}, nil
}

// historyCommitOptions are the options every commit is observed with: a run
// outside a CI that reads no report, fetches nothing and asks the code host
// for the project id once.
func historyCommitOptions(opts Options, tree string) Options {
	opts.Dir = tree
	opts.Env = func(string) string { return "" }
	opts.ReadFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	opts.Fetch = false
	opts.NoReports = true
	opts.JUnit, opts.Coverage, opts.SBOM, opts.BaseSBOM, opts.Architecture = nil, nil, nil, nil, nil
	opts.MoonGraph, opts.PantsGraph, opts.BazelGraph = "", "", ""
	if lookup := opts.ProjectID; lookup != nil {
		type answer struct {
			id  string
			err error
		}
		var mu sync.Mutex
		answers := map[string]answer{}
		opts.ProjectID = func(ctx context.Context, provider, host, path string) (string, error) {
			key := provider + "\x00" + host + "\x00" + path
			mu.Lock()
			defer mu.Unlock()
			if a, ok := answers[key]; ok {
				return a.id, a.err
			}
			id, err := lookup(ctx, provider, host, path)
			answers[key] = answer{id, err}
			return id, err
		}
	}
	return opts
}

// observeCommit observes one commit against its first parent, on branch.
func observeCommit(ctx context.Context, opts Options, c gitbase.Commit, branch string) Observation {
	opts.Overrides = ci.Overrides{Base: c.Parent, Head: c.SHA}
	o := Run(ctx, opts)
	o.Subject.Branch = branch
	o.CI.RunID = HistoryRunID(c.SHA)
	return o
}

// HistoryRunID is the run id of a commit's evidence document: the evidence
// reporter derives its submission id from it, so every upload of a commit
// carries the same one.
func HistoryRunID(sha string) string {
	return HistoryRunPrefix + sha
}

func (h *History) warn(code, format string, args ...any) {
	h.Diagnostics = append(h.Diagnostics, diag.Warningf(code, "history", format, args...))
}

func hasHave(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
