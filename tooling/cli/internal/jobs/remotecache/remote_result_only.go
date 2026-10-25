package remotecache

import (
	"context"
	"log/slog"
	"slices"
	"time"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
)

// dependedOnJobs returns the key of every job that some planned job must wait
// for: its functional dependencies and its write-serialization predecessors.
func dependedOnJobs(planned []*jobmodel.ScheduledJob) map[string]bool {
	out := make(map[string]bool)
	for _, job := range planned {
		if job == nil {
			continue
		}
		for _, predecessor := range job.SchedulingPredecessors() {
			out[predecessor] = true
		}
	}
	return out
}

// TaskFilesNeeded reports whether this run reads the files of job's cache
// entry. It is true unless the provider echoed the result-only capability and
// the plan proves that no reader exists; see this file's header for the rule.
func (r *RemoteCache) TaskFilesNeeded(job *jobmodel.ScheduledJob) bool {
	if r == nil || job == nil || r.dependedOn == nil {
		return true
	}
	mode, ok := r.resultOnlyMode()
	if !ok {
		return true
	}
	switch mode {
	case cache.ModeMinimal:
	case cache.ModeToplevel:
		if requestedByRun(job) {
			return true
		}
	default:
		return true
	}
	return r.dependedOn[job.Key()] || declaresPostRunReport(job) || len(cachekey.PolicedOutputs(job)) > 0
}

// requestedByRun reports whether job delivers for a project the run's
// selection names. A job without a selection, and a job that runs once for the
// whole selection, count as requested.
func requestedByRun(job *jobmodel.ScheduledJob) bool {
	if job.Selection == nil || job.Project == nil || len(job.SelectedProjects) > 0 {
		return true
	}
	return slices.Contains(job.Selection.ProjectIDs, job.Project.ID)
}

// declaresPostRunReport reports whether job declares a report that a reader
// consults on disk after the scheduler finishes: the feature verification
// report and the executable criteria projection, which the spec gate reads
// from the command output directory. A command-output file whose path the
// task reports at run time may be either, so it counts too. Every other
// post-run reader uses the replayed result, which a status-only hit carries.
func declaresPostRunReport(job *jobmodel.ScheduledJob) bool {
	declaration := jobmodel.TaskDeclarationOf(job)
	if declaration == nil {
		return false
	}
	for _, output := range declaration.Outputs {
		if output.EffectiveRoot() != extensionproto.OutputRootCommandOutput || output.Kind != extensionproto.OutputKindFile {
			continue
		}
		switch output.Path {
		case "", features.VerificationReportFilename, features.SpecCriteriaProjectionFilename:
			return true
		}
	}
	return false
}

// RestoreTaskResult asks the provider for the result of a declared-capture
// task whose files this run does not read, and records a trusted hit as a
// local result-only entry. It returns (nil, ReuseNone) for a miss, a hint, a
// payload this build cannot interpret, or any failure, and the caller then
// executes the task.
//
// It takes no lease. A lease keeps sibling sessions from downloading one
// entry twice, and a result-only restore downloads nothing; waiting on a
// sibling that downloads the full entry would only delay a status.
func (r *RemoteCache) RestoreTaskResult(
	ctx context.Context,
	hash string,
	job *jobmodel.ScheduledJob,
	cm *store.CacheManager,
) (*store.ResultOnlyTaskEntry, jobmodel.ReuseKind) {
	if r == nil || cm == nil || hash == "" {
		return nil, jobmodel.ReuseNone
	}
	sess, ok := r.ensureProvider(ctx, nil, cm)
	if !ok {
		r.recordUnavailable()
		return nil, jobmodel.ReuseNone
	}
	if !sess.RestoreResultOnly() {
		return nil, jobmodel.ReuseNone
	}
	restore, err := sess.Restore(ctx, &cache.RestoreParams{Key: store.RemoteTaskEntryKey(hash), ResultOnly: true})
	r.recordLookup(restoreFailure(restore, err))
	if err != nil {
		//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
		slog.Warn("remote cache: provider restore failed; building locally",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return nil, jobmodel.ReuseNone
	}
	if restore.Status != cache.RestoreHit || restore.Result == nil || restore.Manifest == nil {
		return nil, jobmodel.ReuseNone
	}
	if !r.Trust.Accepts(restore.Channel) {
		// A hint can warm blobs but never return a green result, and a
		// result-only restore fetched no blob to warm.
		return nil, jobmodel.ReuseNone
	}
	start := time.Now()
	recorded, err := cm.PublishResultOnlyTaskEntry(store.RemoteTaskEntryHit{
		Key:      hash,
		Result:   cachekey.EntryResultFromActionResult(restore.Result),
		Metadata: taskEntryMetadata(job, restore.Result.DurationMs),
		Manifest: restore.Manifest,
	})
	if err != nil {
		//nolint:gosec // G706 false positive: dynamic values are structured slog attributes, not a format string.
		slog.Debug("remote cache: result-only hit is not interpretable; building locally",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return nil, jobmodel.ReuseNone
	}
	var durationMs int64
	if recorded.Metadata != nil {
		durationMs = recorded.Metadata.DurationMs
	}
	r.recordRestoredHit(hash, cachekey.ActionResultFromEntryResult(recorded.Result, durationMs, 0, r.actionEvents),
		nil, durationMs, 0, start, time.Now())
	return recorded, jobmodel.ReuseRemoteCache
}
