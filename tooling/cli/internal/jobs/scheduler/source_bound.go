package scheduler

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// A source-bound task (cache.sourceBound) writes outputs that embed the
// source binding of its own project: the source-v1 digest the scheduler stamps
// into <project>/.gen/version.json. Go describe is the canonical one: its
// committed feature evidence names the binding of the tree it described.
//
// The binding covers every project file, tests and specs included, so it is
// NOT a key input. Keying it would move the key of the task, and of every
// dependent whose key folds it, on an edit none of them reads. Instead the
// binding is part of the entry's STORE key (cachekey.SourceBoundEntryKey), which
// every store operation addresses while the cache key stays what dependents
// fold:
//
//   - WRITE. prepareTask brings the stamp to the current tree
//     (restampSourceBinding), then reads the binding the execution is about
//     to be stamped with, from the stamp on disk, the one the subprocess reads.
//     closeTask puts it on the result, and finalizeExecutedJob stores and
//     uploads the entry at the cache key bound to it
//     (cachekey.ExecutedEntryKey). A follower adopts its leader's binding with
//     the result. An unmanaged workspace is stamped with the empty binding.
//   - READ. lookupDeclaredEntry looks up, leases, waits for and fetches the
//     entry at the cache key bound to the project's current binding
//     (lookupEntryKey). An entry is therefore found only at the binding its
//     outputs embed, and a lease at one binding never holds a waiter at
//     another.
//
// Entries stay immutable: an edit that moves only the binding stores a second
// entry beside the first, and a tree that returns to the first binding finds
// the first entry again. The direction of every doubt is execution: a binding
// that cannot be computed inside a repository gives no key, so the task
// executes, and its stamp names no binding either, so it stores nothing. The
// failure cache stays keyed by the cache key alone, so it neither records nor
// replays a source-bound failure (task_failure.go).

// restampSourceBinding brings the build stamp of job's project to the tree as
// it stands, before a source-bound execution reads it. refreshVersionFile
// writes the stamp once per project per session, for the first task of the
// project that executes. A later task can rewrite a project file, a lint fix
// for example, and every execution drops its project's binding memo
// (finalizeExecutedJob). Without this write the subprocess would read the
// earlier binding, and its evidence and its entry would name a tree the run
// has left. The write keeps a stamp whose identity already matches, so it
// changes nothing when no source moved.
//
// The write replaces the stamp whole (replaceVersionStamp): a task that reads
// the stamp meanwhile reads the earlier stamp or this one. A source-bound task
// writes it while it holds its project's source-bound lock
// (lockSourceBoundProjects) and, from its preparation through its capture, the
// project's task-output key exclusively (preparationWritesProjectTree,
// taskReadLockNames), so no other source-bound task of the project, in this
// session or another, rewrites the stamp between this write and the capture
// of this task's entry.
func (s *Scheduler) restampSourceBinding(job *ScheduledJob) {
	if s.ws == nil || job == nil || job.Project == nil {
		return
	}
	s.versionFilesMu.Lock()
	started := time.Now()
	spawned := generateVersionFilesAtWithSourceBindings(
		s.ws, []*ScheduledJob{job}, s.cfg.VersionInfo, s.versionBuildTime, true, s.capabilitySourceBindings,
	)
	s.versionFilesMu.Unlock()
	s.cacheStats.RecordLocalBindings(started, time.Now(), spawned)
	s.cache.InvalidateFileHashesUnder([]string{
		filepath.Join(s.ws.Root, job.Project.Path, filepath.FromSlash(versionStampRelPath)),
	})
}

// stampedSourceBinding returns the binding of job's own project that the build
// stamp on disk carries, which is the one an execution started now reads. The
// second result is false when the stamp names no usable binding: no stamp, a
// torn stamp, no entry for the project, or a failed binding inside a
// repository (SourceBinding empty with the unavailable marker unset).
func (s *Scheduler) stampedSourceBinding(job *ScheduledJob) (string, bool) {
	if s.ws == nil || job == nil || job.Project == nil {
		return "", false
	}
	path := filepath.Join(s.ws.Root, job.Project.Path, filepath.FromSlash(versionStampRelPath))
	// A stamp write replaces the file whole, so the read never sees a torn
	// stamp. It takes versionFilesMu, which every stamp writer of this
	// session holds, so it reads after a write in progress.
	s.versionFilesMu.Lock()
	data, err := os.ReadFile(path)
	s.versionFilesMu.Unlock()
	if err != nil {
		return "", false
	}
	var info VersionInfo
	if json.Unmarshal(data, &info) != nil {
		return "", false
	}
	for _, stamp := range info.CapabilityPackages {
		if stamp.Package != job.Project.Name {
			continue
		}
		switch {
		case stamp.SourceBindingUnavailable && stamp.SourceBinding == "":
			return "", true
		case !stamp.SourceBindingUnavailable && stamp.SourceBinding != "":
			return stamp.SourceBinding, true
		}
		return "", false
	}
	return "", false
}

// currentSourceBinding returns the binding of job's own project for the tree
// as it stands, from the session memo the stamp writer reads. An unmanaged
// workspace has the empty binding. The second result is false when the binding
// cannot be computed inside a repository.
func (s *Scheduler) currentSourceBinding(job *ScheduledJob) (string, bool) {
	if s.ws == nil || job == nil || job.Project == nil || s.capabilitySourceBindings == nil {
		return "", false
	}
	started := time.Now()
	s.versionFilesMu.Lock()
	result, spawned := s.capabilitySourceBindings.sourceBinding(s.ws.Root, filepath.Join(s.ws.Root, job.Project.Path))
	s.versionFilesMu.Unlock()
	s.cacheStats.RecordLocalBindings(started, time.Now(), spawned)
	if result.unmanaged {
		return "", true
	}
	if result.err != nil || result.binding == "" {
		return "", false
	}
	return result.binding, true
}

// executedEntryKey returns the store key an execution's entry is stored and
// uploaded at (cachekey.ExecutedEntryKey). Under --debug it says when a
// source-bound execution has none, because its stamp named no usable binding
// and the task stores nothing.
func (s *Scheduler) executedEntryKey(job *ScheduledJob, cacheEnabled bool, key string, result *JobResult) string {
	entryKey := cachekey.ExecutedEntryKey(job, key, result)
	if entryKey == "" && key != "" && cacheEnabled && s.cfg.Debug {
		// The CLI installs no debug-level slog handler (storeDeclaredCapture).
		slog.Info("source-bound task was stamped with no usable source binding; storing no entry", "job", job.Key())
	}
	return entryKey
}

// lookupEntryKey returns the store key that job's entry is looked up, leased
// and fetched under: key itself for a task that is not source-bound, and key
// bound to the project's current binding for a source-bound one. The second
// result is false when that binding cannot be computed inside a repository:
// the task then has no entry to read.
func (s *Scheduler) lookupEntryKey(job *ScheduledJob, key string) (string, bool) {
	if !cachekey.TaskIsSourceBound(job) {
		return key, true
	}
	binding, ok := s.currentSourceBinding(job)
	if !ok {
		if s.cfg.Debug {
			// The CLI installs no debug-level slog handler (storeDeclaredCapture).
			slog.Info("source-bound task has no source binding; executing without a cache lookup", "job", job.Key())
		}
		return "", false
	}
	return cachekey.SourceBoundEntryKey(key, binding), true
}
