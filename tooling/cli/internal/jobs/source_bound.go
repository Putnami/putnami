package jobs

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
//   - WRITE. prepareTask reads the binding the execution is about to be
//     stamped with, from the stamp on disk, the one the subprocess reads.
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
	// The stamp writers hold versionFilesMu, and writeVersionStamp is not an
	// atomic replace, so the read takes it too.
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
