package planner

import (
	"sort"
	"sync"

	jobmodel "go.putnami.dev/cli/model/jobs"
)

// DAGState tracks the DAG execution state. The planner walks it to prove a
// plan can be scheduled, and the scheduler walks the same state to dispatch
// the plan, so the two cannot disagree on what a dependency count means.
type DAGState struct {
	// remaining maps jobKey → number of unresolved dependencies
	remaining map[string]int
	// dependents maps jobKey → list of job keys that depend on it
	dependents map[string][]string
	// jobs maps jobKey → ScheduledJob
	jobs map[string]*jobmodel.ScheduledJob
	mu   sync.Mutex
}

// NewDAGState builds the state of a plan: every job with its unresolved
// scheduling predecessors counted.
func NewDAGState(planned []*jobmodel.ScheduledJob) *DAGState {
	ds := &DAGState{
		remaining:  make(map[string]int, len(planned)),
		dependents: make(map[string][]string),
		jobs:       make(map[string]*jobmodel.ScheduledJob, len(planned)),
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

// Ready returns all jobs with zero unresolved dependencies.
func (ds *DAGState) Ready() []*jobmodel.ScheduledJob {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	var ready []*jobmodel.ScheduledJob
	for key, count := range ds.remaining {
		if count == 0 {
			ready = append(ready, ds.jobs[key])
		}
	}
	return ready
}

// Complete marks a job as done and returns newly-ready dependents.
func (ds *DAGState) Complete(key string) []*jobmodel.ScheduledJob {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	delete(ds.remaining, key)

	var newReady []*jobmodel.ScheduledJob
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

// RemainingSnapshot returns the jobs that cannot yet run, plus their current
// unresolved dependency counts. It is used only when the scheduler has no
// ready or in-flight work left before every job completed.
func (ds *DAGState) RemainingSnapshot() ([]*jobmodel.ScheduledJob, map[string]int) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	keys := make([]string, 0, len(ds.remaining))
	for key := range ds.remaining {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	jobs := make([]*jobmodel.ScheduledJob, 0, len(keys))
	counts := make(map[string]int, len(keys))
	for _, key := range keys {
		if job := ds.jobs[key]; job != nil {
			jobs = append(jobs, job)
			counts[key] = ds.remaining[key]
		}
	}
	return jobs, counts
}

// Size is the number of jobs the DAG still accounts for — the plan the
// coordinator dispatches, which excludes the finalizer nodes the invocation
// runtime owns. It is read only before the workers start and from the
// coordinator, which is the goroutine that mutates it.
func (ds *DAGState) Size() int {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return len(ds.jobs)
}
