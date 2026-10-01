package agentartifacts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrCollision reports that a plan preserved at least one file this package
// could not prove it owns. It is returned INSTEAD of applying anything: a
// collision aborts the whole mutation, so the workspace is byte-identical to
// what it was before the call.
var ErrCollision = errors.New("agent artifact materialization aborted: user-owned files would be overwritten or removed")

// tempSuffix marks a staged file. It carries the pid so two processes staging
// the same workspace cannot delete each other's in-flight bytes, and it is a
// sibling of the target so the rename that publishes it is an intra-directory —
// therefore atomic, therefore same-filesystem — operation.
const tempSuffix = ".putnami-agent-tmp"

// staging is one pending write: the temp path holding the new bytes, the target
// it will be renamed onto, and the directories that had to be created for it.
type staging struct {
	temp        string
	target      string
	createdDirs []string
}

// Apply performs the mutation the plan describes, transactionally.
//
// Two phases, and the split is the guarantee:
//
//  1. STAGE — every byte of every create/update is written to a temp sibling of
//     its target. No target has changed yet. A failure here removes the staged
//     bytes and the directories staging created, leaving zero mutations.
//  2. COMMIT — each staged file is published with an atomic rename, then the
//     removals run and the directories they emptied are pruned.
//
// A crash between the two phases, or partway through phase 2, leaves a mix of
// old and new target bytes plus some temp siblings. That state is RECOVERABLE
// by rerunning: files already renamed re-plan as unchanged, files not yet
// renamed re-plan as create or update (their recorded digest is still the
// pre-run one), and the ownership state was never advanced, because it is
// written only after this function returns nil.
//
// Apply refuses a plan with collisions. The check is duplicated here rather
// than trusted from the caller because this is the function that writes.
func Apply(workspaceRoot string, artifact *Artifact, plan *Plan) error {
	if plan.HasCollisions() {
		return ErrCollision
	}

	staged := make([]staging, 0, len(plan.Entries))
	committed := false
	defer func() {
		if committed {
			return
		}
		// Unwind deepest-first so a directory is only removed after everything
		// this run put inside it is gone.
		for i := len(staged) - 1; i >= 0; i-- {
			os.Remove(staged[i].temp)
			for j := len(staged[i].createdDirs) - 1; j >= 0; j-- {
				os.Remove(staged[i].createdDirs[j])
			}
		}
	}()

	for _, entry := range plan.Entries {
		if entry.Action != ActionCreate && entry.Action != ActionUpdate {
			continue
		}
		target, err := workspacePath(workspaceRoot, entry.Path)
		if err != nil {
			return err
		}
		// Register the pending write BEFORE creating anything, so a failure at
		// any step below is still unwound by the deferred rollback.
		staged = append(staged, staging{
			temp:   fmt.Sprintf("%s%s.%d", target, tempSuffix, os.Getpid()),
			target: target,
		})
		pending := &staged[len(staged)-1]

		content, err := artifact.Read(entry.Path)
		if err != nil {
			return err
		}
		created, err := ensureDir(filepath.Dir(target))
		pending.createdDirs = created
		if err != nil {
			return fmt.Errorf("create directory for %s: %w", entry.Path, err)
		}
		if err := os.WriteFile(pending.temp, content, 0o644); err != nil {
			return fmt.Errorf("stage %s: %w", entry.Path, err)
		}
	}

	committed = true
	for i, pending := range staged {
		if err := os.Rename(pending.temp, pending.target); err != nil {
			for _, remaining := range staged[i:] {
				os.Remove(remaining.temp)
			}
			return fmt.Errorf("publish %s: %w (rerun to finish; already-published files re-plan as unchanged)",
				pending.target, err)
		}
	}

	for _, entry := range plan.Select(ActionRemove) {
		target, err := workspacePath(workspaceRoot, entry.Path)
		if err != nil {
			return err
		}
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", entry.Path, err)
		}
		pruneEmptyParents(workspaceRoot, target)
	}
	return nil
}

// ensureDir creates dir and reports the directories it actually had to create,
// shallowest first.
//
// Returning that list is what makes a staging rollback exact. Removing every
// empty ancestor instead would delete an empty directory the USER created and
// this run merely walked through — small, but it is still a mutation, and the
// whole contract here is that a failed run mutates nothing.
func ensureDir(dir string) ([]string, error) {
	var missing []string
	for current := dir; ; {
		if _, err := os.Lstat(current); err == nil {
			break
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	// missing is deepest-first; reverse it so callers unwind in creation order.
	for i, j := 0, len(missing)-1; i < j; i, j = i+1, j-1 {
		missing[i], missing[j] = missing[j], missing[i]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return missing, err
	}
	return missing, nil
}

// pruneEmptyParents removes directories left empty by a removal, walking up
// toward (but never to) the workspace root.
//
// It is best-effort and needs no emptiness check of its own: os.Remove refuses
// a non-empty directory, so the walk stops at the first directory that still
// holds anything — including anything the user put there. It also stops at the
// first non-directory, so it can never delete a file, and at the first symlink,
// so it can never delete a link's target.
func pruneEmptyParents(workspaceRoot, target string) {
	root := filepath.Clean(workspaceRoot)
	for dir := filepath.Dir(target); dir != root && len(dir) > len(root); dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}
