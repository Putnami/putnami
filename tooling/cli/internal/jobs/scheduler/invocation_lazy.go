// A setup task starts when its first consumer misses the cache (CLI ADR 0129).
//
// A lazy relation's producer is PARKED when it becomes ready: the coordinator
// completes it in the DAG, so its consumers become ready, and dispatches
// nothing. A consumer that hits the cache is served and never needs the
// resource. A consumer that misses while the producer has not finished
// returns DEFERRED before it claims the key; the coordinator then queues the
// producer, once, and queues every deferred consumer again when the producer
// finishes. A parked producer whose whole frontier became terminal without a
// request ends with a skipped row, and its finalizer, never armed, is settled
// skipped as well.
//
// The decision is taken at run time, from the key each consumer computes when
// it starts: nothing is predicted before the first task, unlike the plan-time
// pruning this replaces (ADR 0113 §8 measured it).
//
// Every member this file moves is a field of invocationRelation guarded by its
// mutex, except waiting, which only the coordinator touches.
package scheduler

import (
	"strings"
	"sync"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
)

// notNeededMessage is the skipped row of a parked producer no consumer asked
// for.
const notNeededMessage = "not needed: every consumer of this invocation-scoped resource was served from cache, or did not run"

// decideLazy marks the relations whose producer may wait for a consumer that
// misses. A relation stays eager when:
//
//   - its finalizer's pruneIf is false or invalid under the params the
//     producer receives (its flags' defaults, the workspace options and the
//     command line, merged as for its job context): an optimization never
//     swallows an imperative side effect, such as a teardown the user asked
//     for, and a param no layer sets stays invalid;
//   - the plan gave one of its consumers to another relation too (refuse), so
//     its frontier is blocked anyway;
//   - a job outside its consumer frontier waits for the producer in the DAG:
//     that job would wait for a producer no consumer requests.
//
// It runs once, before the first task, and computes no key.
func (rt *invocationRuntime) decideLazy(planned []*ScheduledJob, params func(job *ScheduledJob) extension.ParamMap) {
	if rt == nil {
		return
	}
	outsiders := make(map[*invocationRelation]bool)
	for _, job := range planned {
		for _, pred := range job.SchedulingPredecessors() {
			rel := rt.byProducer[pred]
			if rel == nil || rt.byConsumer[job.Key()] == rel {
				continue
			}
			outsiders[rel] = true
		}
	}
	for _, rel := range rt.relations {
		if outsiders[rel] {
			continue
		}
		if expression := strings.TrimSpace(rel.Finalizer.Step.Finalizes.PruneIf); expression != "" {
			lazy, valid := extension.EvaluateExpressionChecked(expression, &extension.WhenContext{Params: params(rel.Producer)})
			if !valid || !lazy {
				continue
			}
		}
		rel.mu.Lock()
		rel.lazy = rel.conflict == ""
		rel.mu.Unlock()
	}
}

// park parks job when it is the producer of a lazy relation, and reports
// whether it did. The coordinator then completes it in the DAG instead of
// dispatching it.
func (rt *invocationRuntime) park(job *ScheduledJob) bool {
	if rt == nil || job == nil {
		return false
	}
	rel := rt.byProducer[job.Key()]
	if rel == nil {
		return false
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if !rel.lazy || rel.parked {
		return false
	}
	rel.parked = true
	return true
}

// lazyMiss is what a consumer of a parked relation carries into its cache
// lookup: whether a miss must defer to the producer instead of claiming the
// entry and executing, and the key a previous opening missed at, which the
// lookup does not ask the stores about again.
type lazyMiss struct {
	deferMiss bool
	missed    string
}

// lazyOpen answers the lazyMiss of job, decided once as the job opens: a
// consumer of a parked relation whose producer has not finished defers on a
// miss. A worker calls it.
func (rt *invocationRuntime) lazyOpen(job *ScheduledJob) lazyMiss {
	if rt == nil || job == nil {
		return lazyMiss{}
	}
	rel := rt.byConsumer[job.Key()]
	if rel == nil {
		return lazyMiss{}
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
	return lazyMiss{deferMiss: rel.parked && !rel.producerDone, missed: rel.missed[job.Key()]}
}

// recordMiss records the key at which a deferred consumer missed, so its
// second opening, once the producer finished, claims that key without asking
// the stores again, and the run counts one miss for it.
func (rt *invocationRuntime) recordMiss(job *ScheduledJob, hash string) {
	rel := rt.byConsumer[job.Key()]
	if rel == nil || hash == "" {
		return
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if rel.missed == nil {
		rel.missed = make(map[string]string)
	}
	rel.missed[job.Key()] = hash
}

// parkedProducer reports whether key is the producer of a lazy relation, parked
// in the DAG, whether a consumer requested it or not: the grant of a protected
// job does not wait for it. Its consumers on that job's path are ancestors too
// and are still required; a requested producer runs before them, and one that
// failed blocks them with sensitive.setup_failed, so the job is refused
// through them.
func (rt *invocationRuntime) parkedProducer(key string) bool {
	if rt == nil {
		return false
	}
	rel := rt.byProducer[key]
	if rel == nil {
		return false
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
	return rel.parked
}

// finishParked records that job, when it is a parked producer, finished, and
// returns the consumers that deferred to it, to be queued again. A parked
// producer was completed in the DAG when it was parked, so the caller must not
// complete it again: parked reports that.
func (rt *invocationRuntime) finishParked(job *ScheduledJob) (waiting []*ScheduledJob, parked bool) {
	if rt == nil || job == nil {
		return nil, false
	}
	rel := rt.byProducer[job.Key()]
	if rel == nil {
		return nil, false
	}
	rel.mu.Lock()
	parked = rel.parked
	rel.producerDone = parked
	rel.mu.Unlock()
	if !parked {
		return nil, false
	}
	waiting = rel.waiting
	rel.waiting = nil
	return waiting, true
}

// requestProducer records that consumer missed the cache before its parked producer
// finished. It returns the producer when this is the first request, so the
// coordinator queues it, and the consumer itself when the producer has
// finished meanwhile, so the coordinator queues it again at once.
func (rt *invocationRuntime) requestProducer(consumer *ScheduledJob) (producer, again *ScheduledJob) {
	rel := rt.byConsumer[consumer.Key()]
	if rel == nil {
		return nil, consumer
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if rel.producerDone {
		return nil, consumer
	}
	rel.waiting = append(rel.waiting, consumer)
	if rel.requested {
		return nil, nil
	}
	rel.requested = true
	return rel.Producer, nil
}

// parkReady parks every lazy producer among ready and completes it in the
// DAG. It returns the jobs left to queue and the dependents the parked
// producers released, which the caller admits through queueReady.
func (s *Scheduler) parkReady(ready []*ScheduledJob, state *planner.DAGState) (queue, released []*ScheduledJob) {
	queue = ready[:0:0]
	for _, job := range ready {
		if s.invocations.park(job) {
			s.verbWaves.ready(job)
			released = append(released, state.Complete(job.Key())...)
			continue
		}
		queue = append(queue, job)
	}
	return queue, released
}

// deferToProducers takes the consumers that missed the cache before their
// producer finished: it queues each producer on its first request and returns
// the consumers whose producer finished meanwhile, to be queued again.
func (s *Scheduler) deferToProducers(pending, deferred []*ScheduledJob) (queued, again []*ScheduledJob) {
	queued = pending
	grew := false
	for _, consumer := range deferred {
		producer, ready := s.invocations.requestProducer(consumer)
		if ready != nil {
			again = append(again, ready)
		}
		if producer == nil {
			continue
		}
		s.metrics.markReady(producer.Key())
		s.pendingBatch(producer)
		queued = append(queued, producer)
		grew = true
	}
	if grew {
		sortPendingLongestFirst(queued)
	}
	return queued, again
}

// settleUnneededProducers ends every parked producer that no consumer
// requested once its whole frontier is terminal: a skipped row, and no
// process. Its finalizer was never armed, so closeInvocations settles it
// skipped as well.
func (s *Scheduler) settleUnneededProducers(results map[string]*JobResult, mu *sync.Mutex, totalCompleted *int) {
	if s.invocations == nil {
		return
	}
	for _, rel := range s.invocations.relations {
		rel.mu.Lock()
		candidate := rel.parked && !rel.requested && !rel.notNeeded
		rel.mu.Unlock()
		if !candidate {
			continue
		}
		mu.Lock()
		terminal := rel.frontierTerminal(results)
		mu.Unlock()
		if !terminal {
			continue
		}
		rel.mu.Lock()
		rel.notNeeded = true
		rel.mu.Unlock()
		skipped := &JobResult{Status: "skipped", Error: &JobError{Message: notNeededMessage}, TaskWall: time.Duration(0)}
		mu.Lock()
		results[rel.Producer.Key()] = skipped
		mu.Unlock()
		s.renderer.JobStart(rel.Producer)
		s.renderer.JobComplete(rel.Producer, skipped)
		s.emitSessionJobEnd(rel.Producer, skipped)
		*totalCompleted++
	}
}
