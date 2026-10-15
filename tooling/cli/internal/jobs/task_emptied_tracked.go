package jobs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// trackedUnder is putnamigit.TrackedUnder. It is a variable only so a test can
// count the questions the scheduler asks.
var trackedUnder = putnamigit.TrackedUnder

// refuseTrackedEmptiedOutputs fails when an output of job that declares
// emptied holds a file the Git index tracks (ADR 0068). Such an output owns
// its path whole: an execution empties it, and a restore empties or replaces
// it, so either would delete a committed file. The scheduler asks before it
// empties or writes anything for job, once per such output. The answer comes
// from the index alone, so a tracked file already deleted from the worktree
// counts too. A file under a kept subpath (ceded, preserved, or the build
// stamp) is never removed, so it does not count.
//
// A required or command-output directory is emptied without the flag (ADR
// 0064, ADR 0065), and the scheduler asks nothing about it. Outside a Git work
// tree there is no index, and the outputs are emptied as before.
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
		tracked, err := trackedUnder(roots.Root, rel)
		var unmanaged *putnamigit.UnmanagedError
		if errors.As(err, &unmanaged) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("declared output %q: cannot tell whether %s holds a Git-tracked file: %w", declared.ID, rel, err)
		}
		keep := emptyingKeep(declared)
		for _, path := range tracked {
			if inner, below := strings.CutPrefix(path, rel+"/"); below && keptPath(keep, inner) {
				continue
			}
			return fmt.Errorf("declared output %q (%s) holds the Git-tracked file %s: an emptied output owns its path whole, so move the committed files out of it",
				declared.ID, rel, path)
		}
	}
	return nil
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
