package jobs

import (
	"sort"
	"sync"
)

// jobDone pairs a completed job with its result.
type jobDone struct {
	job    *ScheduledJob
	result *JobResult
}

// jobGroup is one scheduler dispatch. A singleton preserves the historical
// execution path; multiple jobs are an opportunistic same-key batch formed
// only from work that was already ready.
type jobGroup struct {
	jobs        []*ScheduledJob
	reservation resourceReservation
}

// jobGroupDone returns every original per-project result from one dispatch.
// The coordinator still completes DAG nodes, renderer rows, and session rows
// individually so batching never changes public job accounting.
type jobGroupDone struct {
	jobs        []jobDone
	reservation resourceReservation
}

// dagState tracks the DAG execution state.
type dagState struct {
	// remaining maps jobKey → number of unresolved dependencies
	remaining map[string]int
	// dependents maps jobKey → list of job keys that depend on it
	dependents map[string][]string
	// jobs maps jobKey → ScheduledJob
	jobs map[string]*ScheduledJob
	mu   sync.Mutex
}

func newDAGState(planned []*ScheduledJob) *dagState {
	ds := &dagState{
		remaining:  make(map[string]int, len(planned)),
		dependents: make(map[string][]string),
		jobs:       make(map[string]*ScheduledJob, len(planned)),
	}

	for _, job := range planned {
		key := job.Key()
		ds.jobs[key] = job
		preds := job.SchedulingPredecessors()
		ds.remaining[key] = len(preds)
		for _, depKey := range preds {
			ds.dependents[depKey] = append(ds.dependents[depKey], key)
		}
	}

	return ds
}

// ready returns all jobs with zero unresolved dependencies.
func (ds *dagState) ready() []*ScheduledJob {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	var ready []*ScheduledJob
	for key, count := range ds.remaining {
		if count == 0 {
			ready = append(ready, ds.jobs[key])
		}
	}
	return ready
}

// complete marks a job as done and returns newly-ready dependents.
func (ds *dagState) complete(key string) []*ScheduledJob {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	delete(ds.remaining, key)

	var newReady []*ScheduledJob
	for _, depKey := range ds.dependents[key] {
		if _, exists := ds.remaining[depKey]; !exists {
			continue // already completed or removed
		}
		ds.remaining[depKey]--
		if ds.remaining[depKey] == 0 {
			newReady = append(newReady, ds.jobs[depKey])
		}
	}

	return newReady
}

// remainingSnapshot returns the jobs that cannot yet run, plus their current
// unresolved dependency counts. It is used only when the scheduler has no
// ready or in-flight work left before every job completed.
func (ds *dagState) remainingSnapshot() ([]*ScheduledJob, map[string]int) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	keys := make([]string, 0, len(ds.remaining))
	for key := range ds.remaining {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	jobs := make([]*ScheduledJob, 0, len(keys))
	counts := make(map[string]int, len(keys))
	for _, key := range keys {
		if job := ds.jobs[key]; job != nil {
			jobs = append(jobs, job)
			counts[key] = ds.remaining[key]
		}
	}
	return jobs, counts
}

// size is the number of jobs the DAG still accounts for — the plan the
// coordinator dispatches, which excludes the finalizer nodes the invocation
// runtime owns. It is read only before the workers start and from the
// coordinator, which is the goroutine that mutates it.
func (ds *dagState) size() int {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return len(ds.jobs)
}
