package cachekey

import (
	"context"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// RemoteCache is the remote build cache a scheduler run negotiates with,
// restores from and shares with. internal/jobs/remotecache implements it. The
// scheduler names only this interface, so it does not import the remote cache
// client, and the keys it hands the cache are the keys this package computes.
//
// Every method is best-effort: a failure logs and leaves the run local, so the
// remote cache can speed a build up but never break it. A nil RemoteCache is a
// local-only run. A nil *remotecache.RemoteCache stored in this interface is
// not a nil interface, so the caller passes a nil interface instead.
type RemoteCache interface {
	// Negotiate hands the build's key set to the provider before any job runs.
	Negotiate(ctx context.Context, ws *workspace.Workspace, planned []*jobmodel.ScheduledJob, commandParams extension.ParamMap, versions RunVersions, cm *store.CacheManager, bypass CacheBypass)
	// Prefetch is the scheduler's pre-execution hook.
	Prefetch(ctx context.Context, planned []*jobmodel.ScheduledJob, cm *store.CacheManager)
	// TaskFilesNeeded reports whether this run reads the files of job's cache
	// entry.
	TaskFilesNeeded(job *jobmodel.ScheduledJob) bool
	// RestoreTaskEntry restores a declared-capture miss from the provider into
	// the local store and returns the entry it published.
	RestoreTaskEntry(ctx context.Context, hash string, job *jobmodel.ScheduledJob, cm *store.CacheManager) (*store.TaskEntry, jobmodel.ReuseKind)
	// RestoreTaskResult restores only the result of a declared-capture miss
	// whose files this run does not read.
	RestoreTaskResult(ctx context.Context, hash string, job *jobmodel.ScheduledJob, cm *store.CacheManager) (*store.ResultOnlyTaskEntry, jobmodel.ReuseKind)
	// UploadTaskEntry shares a freshly published task-owned entry with the
	// provider.
	UploadTaskEntry(ctx context.Context, hash string, job *jobmodel.ScheduledJob, cm *store.CacheManager)
	// ObjectCacheJobEnv returns the environment a job subprocess needs to reach
	// the provider's object cache. It never enters a cache key.
	ObjectCacheJobEnv() []string
	// CacheStats returns the accumulator the run's cache activity is recorded
	// in, or nil when none is set.
	CacheStats() *CacheStats
	// SetCacheStats makes stats that accumulator.
	SetCacheStats(stats *CacheStats)
	// DrainUploads waits for the background uploads at the end of the run.
	DrainUploads()
	// Stop is the scheduler's end-of-run hook.
	Stop()
}
