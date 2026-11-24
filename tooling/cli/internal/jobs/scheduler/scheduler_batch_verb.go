package scheduler

import (
	"strings"
	"sync"

	extensionproto "go.putnami.dev/protocol/extension"
)

// A delegated verb runs as one tool command (ADR 0019 of tooling/stacks).
//
// A batchable task whose cache is off is a delegated tool's task: the tool
// keeps its own cache, and one call of the tool over every project of the verb
// is what makes a warm run as fast as the tool alone. Its tasks do not become
// ready together: each waits for its own project's graph step, and those end
// at different times. Grouping only the peers that are ready at one moment, as
// cached batches do, would start one tool command per wave of graph steps.
//
// So a delegated task waits for its peers. A peer is a task of the same verb:
// the same extension, task and tool. A task is of the verb's FIRST WAVE when
// no task of its verb is among its transitive prerequisites; the first wave
// is what one command can run, since a later task waits for an earlier one's
// result. A ready first-wave task waits while a first-wave peer is not ready
// yet, and the coordinator dispatches other work meanwhile. It never waits
// when nothing else that ends is in flight: then no completion could make a
// peer ready (takeAdmissiblePendingGroup). A serve runs until interrupted, so
// it does not count.
//
// No deadlock follows from the wait. A first-wave peer depends on no task of
// its verb, so its readiness never needs the waiting task to run; and a wait
// never outlives the work in flight that ends, so two verbs waiting on each
// other's tasks run once that work is done.

// delegatedVerb answers the verb a job's wait reads: the extension, the task
// and the tool, for a batchable job whose cache is off. A cached batchable job
// never waits for its peers, and answers false.
func delegatedVerb(job *ScheduledJob) (string, bool) {
	if job == nil || job.JobDef == nil || job.Extension == nil ||
		job.JobDef.Batchable == nil || job.JobDef.Cache {
		return "", false
	}
	tool := strings.TrimSpace(job.JobDef.Batchable.Tool)
	if tool == "" {
		return "", false
	}
	return job.Extension.Name + "\x00" + job.JobDef.Name + "\x00" + tool, true
}

// verbWaves tracks, for each delegated verb, the first-wave tasks that are not
// ready yet. Only the coordinator mutates it; the lock keeps a test or a
// future reader safe.
type verbWaves struct {
	mu sync.Mutex
	// verbs maps a first-wave job's key to its verb.
	verbs map[string]string
	// unready maps a verb to the keys of its first-wave jobs not ready yet.
	unready map[string]map[string]bool
	// serving counts the dispatched jobs that serve a workload until
	// interrupted: their completion never comes, so a wait must not count on
	// it.
	serving int
}

// track records jobs dispatched (delta 1) or completed (delta -1), counting
// those that serve a workload.
func (w *verbWaves) track(jobs []*ScheduledJob, delta int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, job := range jobs {
		if job != nil && job.JobDef != nil && job.JobDef.Traits.Workload == extensionproto.WorkloadServe {
			w.serving += delta
		}
	}
}

// serves is the number of dispatched jobs that serve a workload.
func (w *verbWaves) serves() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.serving
}

// newVerbWaves derives the first wave of each delegated verb of the plan. A
// plan with no delegated batchable job answers nil, and costs one pass.
func newVerbWaves(jobs []*ScheduledJob) *verbWaves {
	byKey := make(map[string]*ScheduledJob, len(jobs))
	delegated := false
	for _, job := range jobs {
		byKey[job.Key()] = job
		if _, ok := delegatedVerb(job); ok {
			delegated = true
		}
	}
	if !delegated {
		return nil
	}
	// ancestors memoizes, per job key, the delegated verbs among the job's
	// transitive prerequisites.
	ancestors := make(map[string]map[string]bool, len(jobs))
	var verbsAbove func(key string) map[string]bool
	verbsAbove = func(key string) map[string]bool {
		if seen, ok := ancestors[key]; ok {
			return seen
		}
		// A cycle never reaches the scheduler (ValidatePlanDAG refuses it);
		// the placeholder only keeps a corrupt plan from recursing forever.
		ancestors[key] = nil
		var out map[string]bool
		job := byKey[key]
		if job == nil {
			return nil
		}
		for _, pred := range job.SchedulingPredecessors() {
			above := verbsAbove(pred)
			verb, isVerb := delegatedVerb(byKey[pred])
			if len(above) == 0 && !isVerb {
				continue
			}
			if out == nil {
				out = make(map[string]bool, len(above)+1)
			}
			for v := range above {
				out[v] = true
			}
			if isVerb {
				out[verb] = true
			}
		}
		ancestors[key] = out
		return out
	}
	w := &verbWaves{verbs: map[string]string{}, unready: map[string]map[string]bool{}}
	for _, job := range jobs {
		verb, ok := delegatedVerb(job)
		if !ok || verbsAbove(job.Key())[verb] {
			continue
		}
		key := job.Key()
		w.verbs[key] = verb
		if w.unready[verb] == nil {
			w.unready[verb] = map[string]bool{}
		}
		w.unready[verb][key] = true
	}
	return w
}

// ready records that a job left the unready state: it was queued, or skipped.
func (w *verbWaves) ready(job *ScheduledJob) {
	if w == nil || job == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	key := job.Key()
	if verb, ok := w.verbs[key]; ok {
		delete(w.unready[verb], key)
	}
}

// waits reports whether a ready job waits for a first-wave peer of its verb
// that is not ready yet.
func (w *verbWaves) waits(job *ScheduledJob) bool {
	if w == nil || job == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	verb, ok := w.verbs[job.Key()]
	if !ok {
		return false
	}
	for key := range w.unready[verb] {
		if key != job.Key() {
			return true
		}
	}
	return false
}

// withoutWaiting answers the pending queue with its first job that does not
// wait for its verb's peers moved to the front, and whether there is one; the
// rest keeps its order. Only a job that batches at all waits: one the batch key
// keeps alone, such as a cache-verification run, runs at once.
func (s *Scheduler) withoutWaiting(pending []*ScheduledJob) ([]*ScheduledJob, bool) {
	for i, job := range pending {
		if s.verbWaves != nil {
			if key, _ := s.pendingBatch(job); key != "" && s.verbWaves.waits(job) {
				continue
			}
		}
		if i == 0 {
			return pending, true
		}
		reordered := make([]*ScheduledJob, 0, len(pending))
		reordered = append(reordered, job)
		reordered = append(reordered, pending[:i]...)
		return append(reordered, pending[i+1:]...), true
	}
	return pending, false
}
