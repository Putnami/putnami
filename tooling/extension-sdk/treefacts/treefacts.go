// Package treefacts answers the questions a job asks Git about the tree it
// runs in: the directories Git ignores entirely, the paths the index tracks,
// the repository state and the project's release baseline.
//
// A task declares each fact it reads as a runtime input (`trackedFiles`,
// `ignoredDirectories`, `repository`, `releaseBaseline`); the orchestrator
// folds the fact into the task's cache key and writes the same value to the
// job context's `tree` member (go.putnami.dev/protocol/job, doc/adr/0002).
// Facts answers from that member, and starts no Git. When the member does not
// cover the question, because the task did not declare the fact, the
// orchestrator could not read it, or an older orchestrator wrote no member, it
// asks Git, as jobs did before the member existed.
package treefacts

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/procstat"
)

// Facts answers the tree facts of one job. The zero value and a nil *Facts
// answer every question from Git.
type Facts struct {
	root string
	tree *job.Tree
}

// From returns the facts of the job whose context is ctx; nil ctx answers
// from Git.
func From(ctx *job.Context) *Facts {
	if ctx == nil {
		return &Facts{}
	}
	return &Facts{root: ctx.WorkspaceRoot, tree: ctx.Tree}
}

// covered returns dir relative to the workspace root in slash form, when the
// context's listings cover it: dir lies at or below the tree's directory.
func (f *Facts) covered(dir string) (string, bool) {
	if f == nil || f.tree == nil || f.root == "" {
		return "", false
	}
	rel, err := filepath.Rel(f.root, dir)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", false
	}
	directory := f.tree.Directory
	if directory != "." && rel != directory && !strings.HasPrefix(rel, directory+"/") {
		return "", false
	}
	return rel, true
}

// relativeTo returns the paths of listing at or below rel ("." holds all),
// relative to rel, in their order.
func relativeTo(listing []string, rel string) []string {
	selected := []string{}
	for _, p := range listing {
		switch {
		case rel == ".":
			selected = append(selected, p)
		case strings.HasPrefix(p, rel+"/"):
			selected = append(selected, p[len(rel)+1:])
		}
	}
	return selected
}

// IgnoredDirectories returns the directories Git ignores entirely at or below
// dir, an absolute directory, as slash paths relative to dir. Outside a work
// tree, or when Git cannot answer, nothing is ignored.
func (f *Facts) IgnoredDirectories(dir string) map[string]bool {
	dirs := map[string]bool{}
	if rel, ok := f.covered(dir); ok && f.tree.IgnoredDirectories != nil {
		for _, p := range relativeTo(f.tree.IgnoredDirectories, rel) {
			dirs[p] = true
		}
		return dirs
	}
	cmd := exec.Command(gitread.Executable(), "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	cmd.Dir = dir
	output, err := procstat.Output(cmd, procstat.Git)
	if err != nil {
		return dirs
	}
	for entry := range bytes.SplitSeq(output, []byte{0}) {
		if name := string(entry); strings.HasSuffix(name, "/") {
			dirs[strings.TrimSuffix(name, "/")] = true
		}
	}
	return dirs
}

// TrackedFiles returns the paths the Git index tracks at or below dir, an
// absolute directory, as slash paths relative to dir, sorted. The index is
// read alone, so a tracked file deleted from the work tree is listed. Outside
// every repository the list is empty. Asking Git, a failure, a missing git
// program included, is an error.
func (f *Facts) TrackedFiles(dir string) ([]string, error) {
	if rel, ok := f.covered(dir); ok && f.tree.TrackedFiles != nil {
		return relativeTo(f.tree.TrackedFiles, rel), nil
	}
	stdout, stderr, err := gitread.RunCapture(dir, "ls-files", "-z", "--cached")
	if err != nil {
		var unmanaged *gitread.UnmanagedError
		if errors.As(gitread.Unmanaged(dir), &unmanaged) && errors.Is(unmanaged.Cause, gitread.ErrNotRepository) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("list the files Git tracks in %s: %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	paths := []string{}
	for entry := range strings.SplitSeq(stdout, "\x00") {
		if entry != "" {
			paths = append(paths, entry)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// Repository returns the state of the repository holding the workspace: one
// of job.RepositoryStates.
func (f *Facts) Repository() (string, error) {
	if f != nil && f.tree != nil && f.tree.Repository != nil {
		return f.tree.Repository.State, nil
	}
	state, _, err := RepositoryState(f.workspaceRoot())
	return state, err
}

func (f *Facts) workspaceRoot() string {
	if f == nil || f.root == "" {
		return "."
	}
	return f.root
}

// ReleaseBaseline returns the project's release baseline the context carries,
// and false when it carries none. History is a fact only a task that declares
// it receives: there is no fallback here, and a job that reads history
// without the member reads it as it did before the member existed.
func (f *Facts) ReleaseBaseline() (*job.ReleaseBaseline, bool) {
	if f == nil || f.tree == nil || f.tree.ReleaseBaseline == nil {
		return nil, false
	}
	return f.tree.ReleaseBaseline, true
}

// ChangeHistory returns the history of the change since the run's impacted
// baseline the context carries, and false when it carries none: the task did
// not declare `changeHistory`, its cache is enabled, the run has no impacted
// baseline, or the orchestrator could not read it (doc/adr/0003 of the job
// protocol). As for the release baseline there is no fallback here: a job
// without the member reads history as it did before the member existed.
func (f *Facts) ChangeHistory() (*job.ChangeHistory, bool) {
	if f == nil || f.tree == nil || f.tree.ChangeHistory == nil {
		return nil, false
	}
	return f.tree.ChangeHistory, true
}

// RepositoryState is the state of the repository holding dir and the
// absolute root of its work tree: outside a work tree, then without a commit,
// then shallow, else complete. The root is empty outside a work tree. A Git
// failure is an error, never a state.
func RepositoryState(dir string) (state, root string, err error) {
	output, stderr, err := gitread.RunCapture(dir, "rev-parse", "--is-inside-work-tree", "--is-shallow-repository", "--show-toplevel")
	if err != nil {
		// Only a directory outside every repository is unmanaged: a refused
		// repository, or a git that cannot run, is a failure.
		var unmanaged *gitread.UnmanagedError
		if errors.As(gitread.Unmanaged(dir), &unmanaged) && errors.Is(unmanaged.Cause, gitread.ErrNotRepository) {
			return job.RepositoryUnmanaged, "", nil
		}
		return "", "", fmt.Errorf("read the repository state of %s: %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) == 0 || lines[0] != "true" {
		return job.RepositoryUnmanaged, "", nil
	}
	if len(lines) != 3 {
		return "", "", fmt.Errorf("read the repository state of %s: unexpected git rev-parse output %q", dir, output)
	}
	root = filepath.FromSlash(lines[2])
	if _, _, err := gitread.RunCapture(dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return "", "", fmt.Errorf("read HEAD of %s: %w", dir, err)
		}
		return job.RepositoryNoCommit, root, nil
	}
	if lines[1] == "true" {
		return job.RepositoryShallow, root, nil
	}
	return job.RepositoryComplete, root, nil
}
