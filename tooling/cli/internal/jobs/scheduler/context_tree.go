package scheduler

import (
	"log/slog"
	"path/filepath"

	extproto "go.putnami.dev/protocol/extension"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// jobTree returns the tree facts job's task declares, each the value its key
// holds (cachekey.TreeFacts, the run's one read), or nil when it declares none
// (protocols/job ADR 0002). A fact Git cannot answer is left out: the task's
// key failed on it, so the task runs uncached, and the job asks Git itself.
func jobTree(facts *cachekey.TreeFacts, ws *workspace.Workspace, job *ScheduledJob) *protocoljob.Tree {
	if facts == nil {
		return nil
	}
	directory := cachekey.TreeDirectory(job)
	tree := &protocoljob.Tree{Directory: directory}
	carries := false
	if cachekey.DeclaresRuntimeInput(job, extproto.RuntimeInputRepository) {
		if state, _, err := facts.Repository(); err == nil {
			tree.Repository, carries = &protocoljob.TreeRepository{State: state}, true
		}
	}
	if cachekey.DeclaresRuntimeInput(job, extproto.RuntimeInputTrackedFiles) {
		if paths, err := facts.TrackedUnder(directory); err == nil {
			tree.TrackedFiles, carries = paths, true
		}
	}
	if cachekey.DeclaresRuntimeInput(job, extproto.RuntimeInputIgnoredDirectories) {
		if paths, err := facts.IgnoredUnder(directory); err == nil {
			tree.IgnoredDirectories, carries = paths, true
		}
	}
	if cachekey.DeclaresRuntimeInput(job, extproto.RuntimeInputReleaseBaseline) && job.Project != nil {
		dir := filepath.Join(ws.Root, job.Project.Path)
		pattern := cachekey.LineTagPattern(ws, job.Project.Line)
		if baseline, err := facts.ReleaseBaseline(dir, pattern); err == nil {
			fact := &protocoljob.ReleaseBaseline{
				State:    baseline.State,
				Pattern:  baseline.Pattern,
				HasTags:  baseline.HasTags,
				Tag:      baseline.Tag,
				Breaking: baseline.Breaking,
			}
			// The files are extracted only for a task that asks for them. An
			// extraction that fails keeps the key, which holds the baseline
			// and not the files, and leaves the whole fact out, so the job
			// reads Git (protocols/job ADR 0002 point 6); it is logged.
			files := cachekey.DeclaresRuntimeInput(job, extproto.RuntimeInputReleaseBaselineFiles)
			extracted, err := "", error(nil)
			if files {
				extracted, err = facts.BaselineDirectory(dir, pattern, baseline)
			}
			if err == nil {
				fact.Directory = extracted
				tree.ReleaseBaseline, carries = fact, true
			} else {
				slog.Warn("release baseline files not extracted; the job reads Git instead",
					"project", job.Project.Name, "tag", baseline.Tag, "error", err)
			}
		}
	}
	if !carries {
		return nil
	}
	return tree
}
