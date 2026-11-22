package gitbase

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Commit is one commit of a first-parent history.
type Commit struct {
	// SHA is the commit id.
	SHA string `json:"sha"`
	// Parent is its first parent, "" for a root commit or a commit on the
	// shallow boundary of the clone.
	Parent string `json:"parent,omitempty"`
	// Time is its committer date.
	Time time.Time `json:"date"`
}

// Root returns the top directory of the repository that holds dir.
func Root(ctx context.Context, dir string) (string, error) {
	return git{ctx: ctx, dir: dir}.output("rev-parse", "--show-toplevel")
}

// LastCommit returns the commit ref names, with its first parent and date.
func LastCommit(ctx context.Context, dir, ref string) (Commit, error) {
	g := git{ctx: ctx, dir: dir}
	sha, err := g.commit(ref)
	if err != nil {
		return Commit{}, err
	}
	commits, err := parseLog(g, "log", "-1", logFormat, "--end-of-options", sha)
	if err != nil {
		return Commit{}, err
	}
	if len(commits) != 1 {
		return Commit{}, fmt.Errorf("git log: %s names no commit", ref)
	}
	return commits[0], nil
}

// FirstParentCommits returns the first-parent commits of ref whose committer
// date is after since and not after until, oldest first.
func FirstParentCommits(ctx context.Context, dir, ref string, since, until time.Time) ([]Commit, error) {
	g := git{ctx: ctx, dir: dir}
	sha, err := g.commit(ref)
	if err != nil {
		return nil, err
	}
	// --since would stop the walk at the first commit dated before the
	// window, and lose the commits after it on a branch whose dates are out
	// of order: the walk reads the whole first-parent chain up to until, and
	// the exact bounds are applied below. Git compares dates to the second,
	// so --until is one second wider.
	commits, err := parseLog(g, "log", "--first-parent", logFormat,
		"--until="+gitDate(until.Add(time.Second)), "--end-of-options", sha)
	if err != nil {
		return nil, err
	}
	kept := make([]Commit, 0, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		if c := commits[i]; c.Time.After(since) && !c.Time.After(until) {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

// logFormat prints a commit as its id, its parents and its committer date in
// seconds, separated by NUL bytes.
const logFormat = "--format=%H%x00%P%x00%ct"

func parseLog(g git, args ...string) ([]Commit, error) {
	out, err := g.output(args...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 3 {
			return nil, fmt.Errorf("git log: unexpected line %q", line)
		}
		seconds, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("git log: unexpected date %q", fields[2])
		}
		parent, _, _ := strings.Cut(fields[1], " ")
		commits = append(commits, Commit{SHA: fields[0], Parent: parent, Time: time.Unix(seconds, 0).UTC()})
	}
	return commits, nil
}

func gitDate(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// worktreeEnv keeps a checkout of an old commit from running anything the
// repository holds: no hook, no submodule checkout, and no large file
// fetched through Git LFS.
var worktreeEnv = []string{
	"GIT_LFS_SKIP_SMUDGE=1",
	"GIT_CONFIG_COUNT=3",
	"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=" + os.DevNull,
	"GIT_CONFIG_KEY_1=submodule.recurse", "GIT_CONFIG_VALUE_1=false",
	"GIT_CONFIG_KEY_2=filter.lfs.required", "GIT_CONFIG_VALUE_2=false",
}

// AddWorktree checks sha out, detached, in a new worktree at path of the
// repository that holds dir. The caller's working tree, index and HEAD stay
// as they were.
func AddWorktree(ctx context.Context, dir, path, sha string) error {
	_, err := git{ctx: ctx, dir: dir, env: worktreeEnv}.raw("worktree", "add", "--quiet", "--detach", "--end-of-options", path, sha)
	return err
}

// CheckoutDetached checks sha out, detached, in the worktree at path.
func CheckoutDetached(ctx context.Context, path, sha string) error {
	_, err := git{ctx: ctx, dir: path, env: worktreeEnv}.raw("checkout", "--quiet", "--force", "--detach", sha, "--")
	return err
}

// RemoveWorktree removes the worktree at path from the repository that holds
// dir.
func RemoveWorktree(ctx context.Context, dir, path string) error {
	g := git{ctx: ctx, dir: dir}
	_, err := g.raw("worktree", "remove", "--force", "--end-of-options", path)
	if err != nil {
		_, _ = g.raw("worktree", "prune")
	}
	return err
}
