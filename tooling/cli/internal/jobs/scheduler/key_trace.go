package scheduler

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// KeyTraceEntry is what one cacheable job of a traced run keyed: the key the
// run precomputed for it before any task ran (PrecomputeKeys, as the remote
// negotiation computes it),
// whether the local store held its entry where the job would look it up then
// (entryPresentAtCurrentBinding), the key it computed when it started, and
// whether the local store served it.
//
// The remote negotiation decides on the precomputed key, and the
// job looks its entry up at the key it starts with, so on an unchanged run
// the two name one entry.
type KeyTraceEntry struct {
	Job  string
	Task string
	// SourceBound is whether the job's task declares cache.sourceBound: its
	// entry is found at the key bound to its project's source binding.
	SourceBound bool
	// Precomputed is "" when the precompute keyed nothing for the job.
	Precomputed string
	// PrecomputedEntry is whether the local store held the job's entry at its
	// precomputed key, bound to its project's binding for a source-bound task,
	// before any task ran.
	PrecomputedEntry bool
	// Started is "" when the job did not reach its cache lookup: it was
	// skipped, canceled, or computed no key.
	Started string
	// ServedLocally is whether the job was served from the local store's
	// entry at its start key.
	ServedLocally bool
}

// Diverged reports whether the job started at another key than the one the
// run precomputed for it, none included.
func (e KeyTraceEntry) Diverged() bool {
	return e.Started != "" && e.Precomputed != e.Started
}

// Unaddressed reports whether the job was served from the local store at its
// precomputed key while the precompute found no entry there.
func (e KeyTraceEntry) Unaddressed() bool {
	return e.ServedLocally && !e.Diverged() && !e.PrecomputedEntry
}

// Unserved reports whether the precompute found the job's entry at the key
// the job started with, and the local store did not serve the job there.
func (e KeyTraceEntry) Unserved() bool {
	return e.PrecomputedEntry && e.Started != "" && !e.Diverged() && !e.ServedLocally
}

// KeyTrace collects the key trace of every run of the process while it is
// installed.
type KeyTrace struct {
	mu      sync.Mutex
	entries []KeyTraceEntry
	prunes  []PruneTrace
}

// PruneTrace is what one traced run decided about its finalizes relations:
// how many it holds, and how many have a producer that waits for a consumer
// that misses the cache (CLI ADR 0129). Err names why the run traced no key.
type PruneTrace struct {
	Relations int
	Lazy      int
	Err       string
}

// Prunes returns the prune traces collected so far, one per traced run, and
// forgets them.
func (t *KeyTrace) Prunes() []PruneTrace {
	t.mu.Lock()
	defer t.mu.Unlock()
	prunes := t.prunes
	t.prunes = nil
	return prunes
}

var installedKeyTrace atomic.Pointer[KeyTrace]

// TraceKeys installs a key trace for the runs that start from now on, until
// the returned stop is called. A test reads what it collected with Entries
// and Prunes. It is a test seam: the shipped binary never installs one, so a
// run pays one atomic load for it. A run that bypasses the cache for every
// task is not traced.
func TraceKeys() (*KeyTrace, func()) {
	trace := &KeyTrace{}
	installedKeyTrace.Store(trace)
	return trace, func() { installedKeyTrace.CompareAndSwap(trace, nil) }
}

// Entries returns the entries collected so far, ordered by job key, and
// forgets them, so the next run is read alone.
func (t *KeyTrace) Entries() []KeyTraceEntry {
	t.mu.Lock()
	entries := t.entries
	t.entries = nil
	t.mu.Unlock()
	slices.SortFunc(entries, func(a, b KeyTraceEntry) int { return strings.Compare(a.Job, b.Job) })
	return entries
}

// keyTrace is one run's share of an installed KeyTrace.
type keyTrace struct {
	trace *KeyTrace
	prune PruneTrace
	mu    sync.Mutex
	byJob map[string]*KeyTraceEntry
	order []string
}

// startKeyTrace records the run's precomputed keys, and whether the local
// store holds each job's entry where the job would look it up, when a trace
// is installed; nil otherwise. The trace computes the keys as the remote
// negotiation does: the scheduler precomputes none itself. A run whose keys
// cannot be precomputed is recorded with the reason, so a test reads it as a
// failure, not as a run with nothing to hold.
func (s *Scheduler) startKeyTrace() *keyTrace {
	trace := installedKeyTrace.Load()
	if trace == nil || s.bypass.All {
		return nil
	}
	run := &keyTrace{trace: trace, byJob: map[string]*KeyTraceEntry{}}
	if s.invocations != nil {
		run.prune.Relations = len(s.invocations.relations)
		for _, rel := range s.invocations.relations {
			rel.mu.Lock()
			if rel.lazy {
				run.prune.Lazy++
			}
			rel.mu.Unlock()
		}
	}
	if s.cache == nil {
		run.prune.Err = "the run has no cache"
		return run
	}
	precomputed, err := cachekey.PrecomputeKeys(s.ws, s.planned, s.commandParams, s.cfg.VersionInfo, s.cache, s.bypass, cachekey.AtEntryKeys(s.lookupEntryKey))
	if err != nil {
		run.prune.Err = err.Error()
		return run
	}
	for _, key := range slices.Sorted(maps.Keys(precomputed)) {
		entry := &KeyTraceEntry{Job: key, Precomputed: precomputed[key]}
		if job := s.byKey[key]; job != nil {
			entry.Task = job.JobDef.Name
			entry.SourceBound = cachekey.TaskIsSourceBound(job)
			entry.PrecomputedEntry = s.entryPresentAtCurrentBinding(job, precomputed[key])
		}
		run.byJob[key] = entry
		run.order = append(run.order, key)
	}
	return run
}

// started records the key job computed at its start, and whether the local
// store served it there.
func (r *keyTrace) started(job *ScheduledJob, key string, servedLocally bool) {
	if r == nil || job == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.byJob[job.Key()]
	if entry == nil {
		entry = &KeyTraceEntry{Job: job.Key()}
		if job.JobDef != nil {
			entry.Task = job.JobDef.Name
		}
		r.byJob[job.Key()] = entry
		r.order = append(r.order, job.Key())
	}
	entry.Started, entry.ServedLocally = key, servedLocally
}

// finish hands the run's entries and its prune trace to the installed trace.
func (r *keyTrace) finish() {
	if r == nil {
		return
	}
	r.mu.Lock()
	entries := make([]KeyTraceEntry, 0, len(r.order))
	for _, key := range r.order {
		entries = append(entries, *r.byJob[key])
	}
	r.mu.Unlock()
	r.trace.mu.Lock()
	r.trace.entries = append(r.trace.entries, entries...)
	r.trace.prunes = append(r.trace.prunes, r.prune)
	r.trace.mu.Unlock()
}

// entryPresentAtCurrentBinding reports whether the local store holds job's
// entry at hash where the job would look it up if it started now: hash itself,
// or for a source-bound task hash bound to its project's current source
// binding (lookupEntryKey), which the run's stamp already computed. The key
// trace reads it.
func (s *Scheduler) entryPresentAtCurrentBinding(job *ScheduledJob, hash string) bool {
	present, err := cachekey.CacheEntryPresentAt(s.cache, job, hash, s.lookupEntryKey)
	return err == nil && present
}
