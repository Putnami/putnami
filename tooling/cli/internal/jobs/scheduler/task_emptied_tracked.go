package scheduler

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.putnami.dev/sdk/extension/gitread"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// trackedPaths is putnamigit.TrackedPaths. It is a variable only so a test can
// count the reads of the index the scheduler makes.
var trackedPaths = putnamigit.TrackedPaths

// trackedIndex is the run's read of the Git index (ADR 0068 point 6): the
// paths it tracks under the workspace root, and the stamp of the index file
// taken before that read. The first read is the run's tree facts'
// (cachekey.TreeFacts), so the emptied outputs and the trackedFiles fact of
// every job context share it; the index is read again only when the index
// file changed since, as a task that stages a file changes it.
type trackedIndex struct {
	mu    sync.Mutex
	read  bool
	stamp putnamigit.IndexStamp
	paths []string
	err   error
}

// trackedUnder lists the paths the index tracks at or below rel
// (putnamigit.TrackedPathsUnder), from the run's read of the index. It reads
// the index again when the index file changed since that read, or when its
// location cannot be found: a stat answers whether it changed, and only a
// change starts git.
func (s *Scheduler) trackedUnder(root, rel string) ([]string, error) {
	s.tracked.mu.Lock()
	defer s.tracked.mu.Unlock()
	if !s.tracked.read {
		var stamped bool
		s.tracked.paths, s.tracked.stamp, stamped, s.tracked.err = s.tree.TrackedRead(trackedPaths)
		s.tracked.read = stamped
	}
	if stamp, stamped := putnamigit.ReadIndexStamp(root); !s.tracked.read || !stamped || stamp != s.tracked.stamp {
		s.tracked.paths, s.tracked.err = trackedPaths(root)
		s.tracked.read, s.tracked.stamp = stamped, stamp
	}
	if s.tracked.err != nil {
		return nil, s.tracked.err
	}
	return putnamigit.TrackedPathsUnder(s.tracked.paths, rel), nil
}

// refuseTrackedEmptiedOutputs fails when an output of job that declares
// emptied holds a file the Git index tracks (ADR 0068). Such an output owns
// its path whole: an execution empties it, and a restore empties or replaces
// it, so either would delete a committed file. The scheduler asks before it
// empties or writes anything for job, once per such output, and answers from
// the run's read of the index (trackedUnder), which it repeats only when the
// index changed, so a tracked file already deleted from the worktree counts
// too. A file under a kept subpath (ceded, preserved, or the build
// stamp) is never removed, so it does not count.
//
// The error names the first such file and the Git command that takes it out
// of the output: git mv for a file in the worktree, and git rm --cached for one
// the index still tracks after it left the worktree.
//
// A required or command-output directory is emptied without the flag (ADR
// 0064, ADR 0065), and the scheduler asks nothing about it. Outside a Git work
// tree there is no index, and the outputs are emptied as before. A workspace
// with a Git entry at or above its root but no git program to read the index
// fails instead: the engine cannot tell whether the output holds a tracked
// file there.
func (s *Scheduler) refuseTrackedEmptiedOutputs(job *ScheduledJob) error {
	roots := s.outputRoots()
	if roots.Root == "" {
		return nil // emptyDeclaredOutput refuses a run without a workspace root.
	}
	for _, declared := range cachekey.EmptiedOutputDeclarations(job) {
		if !declared.Output.Emptied {
			continue
		}
		root := declared.Output.EffectiveRoot()
		base, ok := roots.OutputRoot(job, root)
		if !ok {
			return fmt.Errorf("declared output %q has unknown root %q", declared.ID, root)
		}
		rel, err := filepath.Rel(roots.Root, filepath.Join(base, filepath.FromSlash(declared.Rel)))
		if err != nil {
			return fmt.Errorf("declared output %q: %w", declared.ID, err)
		}
		rel = filepath.ToSlash(rel)
		tracked, err := s.trackedUnder(roots.Root, rel)
		var unmanaged *gitread.UnmanagedError
		if errors.As(err, &unmanaged) {
			if entry, inside := putnamigit.WorkTreeEntry(roots.Root); inside && errors.Is(err, gitread.ErrNoProgram) {
				return fmt.Errorf("declared output %q (%s): %s marks a Git work tree, but no git program is on PATH, so the engine cannot tell whether the output holds a Git-tracked file: install git",
					declared.ID, rel, entry)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("declared output %q: cannot tell whether %s holds a Git-tracked file: %w", declared.ID, rel, err)
		}
		keep := emptyingKeep(declared)
		for _, path := range tracked {
			if inner, below := cutPrefixFold(path, rel+"/"); below && keptPath(keep, inner) {
				continue
			}
			if _, statErr := os.Lstat(filepath.Join(roots.Root, filepath.FromSlash(path))); errors.Is(statErr, fs.ErrNotExist) {
				return fmt.Errorf("declared output %q (%s): %s is still tracked by Git but missing from the worktree, and an emptied output owns its path whole: run git rm --cached %s",
					declared.ID, rel, path, path)
			}
			return fmt.Errorf("declared output %q (%s) holds the Git-tracked file %s: an emptied output owns its path whole, so move the committed files out of it with git mv",
				declared.ID, rel, path)
		}
	}
	return nil
}

// cutPrefixFold is strings.CutPrefix that folds case, as
// putnamigit.TrackedPathsUnder does.
func cutPrefixFold(path, prefix string) (string, bool) {
	if len(path) < len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) {
		return path, false
	}
	return path[len(prefix):], true
}

// keptPath reports whether the output-relative slash path inner is a kept
// subpath or lies below one. It folds case, as the emptying does.
func keptPath(keep []string, inner string) bool {
	for _, kept := range keep {
		if strings.EqualFold(inner, kept) ||
			len(inner) > len(kept) && inner[len(kept)] == '/' && strings.EqualFold(inner[:len(kept)], kept) {
			return true
		}
	}
	return false
}
