// The invocation RELATION: what a `finalizes` declaration resolves to on a
// plan, and the rules that decide whether it runs at all, split by invariant
// across these files.
//
// An earlier stage landed the contract (protocols/extension: the `runtime-file` kind, the
// `invocation` scope, the `sensitive` flag, `runOn: "finally"` plus its explicit
// `finalizes` relation) and the job context's `invocation` member
// (protocols/job). These files make them EXECUTE. They are the only place in the
// scheduler that knows what a finalizes relation is, and they stay deliberately
// generic: nothing here mentions a database, a container, or a DSN. A relation
// is (producer, consumer frontier, finalizer) and an invocation is (id, private
// artifact root, non-secret lease) — database test environments are one
// PROVIDER of that shape, not a second implementation of it.
//
// # The four invariants, and the file that owns each
//
// The subsystem has four invariants, each locally checkable, and they are the
// seams the files are cut along — not line count. A change to one invariant is
// then a change to one file, which is the property that makes a security- or
// concurrency-sensitive edit reviewable on its own:
//
//   - CONFINEMENT. A sensitive artifact's path and bytes never reach an event, a
//     result, a session record, telemetry, or cache traffic. It is two files,
//     because its halves fail differently: invocation_needles.go decides WHAT
//     counts as a secret, from a bounded read of the artifact, and
//     invocation_redact.go keeps that set off every publishing surface —
//     fail-closed, failing the task with `sensitive.leak_detected` rather than
//     publishing the value.
//   - RECOVERABILITY. A SIGKILL cannot run a finalizer, so the lease on disk is
//     what a later invocation reaps, before it provisions anything.
//     invocation_lease.go owns the private scratch, that lease, and the artifact
//     verification which decides whether the frontier may run.
//   - EXACTLY ONCE. The finalizer runs exactly once whenever its producer
//     STARTED — consumer failure and cancellation included — and never when the
//     producer never started. invocation_finalize.go owns the single claim, the
//     two dispatch paths that fold through it, and the settlement.
//   - CACHE IDENTITY FROM THE ACTION. A consumer's cache key folds in the
//     PRODUCING ACTION's digest (cachekey/executor.go), never the secret's content, so a
//     warm consumer is warm because its inputs are unchanged and not because a
//     secret happened to repeat.
//
// THIS file owns the runtime relation. Whether the resource needs to exist at
// all is decided at run time, by the first consumer that misses the cache
// (invocation_lazy.go, CLI ADR 0129). The plan-time half of the relation lives in
// internal/jobs/planner/invocation.go: resolving `finalizes` onto plan nodes
// (which is also where the producing-action stamp the cache identity needs is
// attached) and the one-invocation-per-step rule that keeps ownership of a step
// unambiguous. Every runtime member of invocationRelation is declared here and
// guarded by its mutex; the sibling files are the state machines that move
// them.
package scheduler

import (
	"sync"

	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// invocationRelation is one `finalizes` relation resolved onto plan nodes.
//
// The embedded planner.InvocationRelation is the plan-time half: the producer,
// the consumer frontier, the finalizer and whether the relation needs the leak
// guard. This type adds the runtime half.
//
// It owns the whole lifecycle of one invocation-scoped resource: the private
// scratch, the lease, the artifacts' confinement, and the single finalizer run.
// Its mutex guards every mutable member the producer's worker, the consumers'
// workers and the coordinator share; waiting, which only the coordinator
// touches, is the one exception.
type invocationRelation struct {
	planner.InvocationRelation

	mu sync.Mutex
	// armed records that the producer STARTED. It is the finalizer's trigger and
	// is never cleared: a producer that failed still armed cleanup.
	armed bool
	// lazy records that the producer starts only when a consumer misses the
	// cache (invocation_lazy.go). It is decided once, before the run, and never
	// changes.
	lazy bool
	// parked records that the coordinator completed the lazy producer in the
	// DAG without dispatching it, so its consumers became ready before it ran.
	parked bool
	// requested records that a consumer missed and the producer was queued.
	requested bool
	// producerDone records that the producer finished, whatever its verdict. A
	// consumer that misses reads it to decide whether it may execute now.
	producerDone bool
	// notNeeded records that the parked producer's frontier became terminal
	// without a request, and the producer got its skipped row.
	notNeeded bool
	// missed holds, by consumer key, the key a deferred consumer missed at.
	missed map[string]string
	// waiting holds the consumers that missed before the producer finished,
	// queued again once it does. Only the coordinator touches it, outside the
	// mutex.
	waiting []*ScheduledJob
	// finalized makes the finalizer's execution exactly-once across the
	// frontier-completion path and the end-of-run sweep.
	finalized bool
	// setupFailed records that the producer did not deliver its artifacts, so
	// blocked consumers get `sensitive.setup_failed` as their causal diagnostic
	// instead of a bare "dependency failed".
	setupFailed bool
	// conflict is set when the plan put one step in two relations' consumer
	// frontiers. It replaces the causal diagnostic, because "the producer did not
	// provision" would be a lie: nobody could say which producer this step's
	// resource came from.
	conflict string
	scratch  *store.InvocationScratch
	// needles are the sensitive paths and byte samples the leak guard matches.
	needles []string
	// held is the producer's live event stream, withheld until its artifacts
	// have been validated and the needles derived from them.
	//
	// A producer's events are the ONE stream the guard cannot scrub as it
	// arrives: needles come from what the producer WROTE, and nothing has been
	// written until it exits. Forwarding them live would put a producer's echo of
	// its own generated binding — or of the artifact path — on the terminal, and
	// into any renderer that persists what it displayed, before a needle exists
	// to match it. So they are spooled into the invocation's private root and
	// released, redacted, at the same moment the result guard runs. The spool is
	// outside ArtifactRoot (the only directory exposed to the subprocess), so it
	// cannot become a declared output or a consumer input.
	held *producerEventSpool
	// leakedJobs is the verdict side channel for events that have already been
	// redacted on their way to the renderer. JobResult.Events is deliberately
	// bounded, so the terminal guard cannot infer a leak by rescanning that
	// compatibility projection alone.
	leakedJobs map[string]bool
	// released switches the producer's sink back to live forwarding once the
	// needles exist. Only a retry can emit after that point, and it is scrubbed
	// as it arrives.
	released bool
}

// invocationRuntime is the scheduler's view of every relation in one plan.
//
// It is nil for every plan that declares no finalizer, which is every plan in
// this workspace today: the machinery lands with its tests before a manifest
// uses it, exactly as B2b's plan-contract checks did.
type invocationRuntime struct {
	ws          *workspace.Workspace
	relations   []*invocationRelation
	byProducer  map[string]*invocationRelation
	byConsumer  map[string]*invocationRelation
	byFinalizer map[string]*invocationRelation
	// participants maps every job key that may see the invocation locator to its
	// relation: the producer, the listed consumers, and the finalizer. Nothing
	// else in the plan receives the artifact root, in the context document or in
	// a template variable.
	participants map[string]*invocationRelation
}

// newInvocationRuntime resolves the plan's finalizes relations, returning nil
// when there are none. It also stamps each consumer's producer onto the plan
// node so cache-key derivation can fold in the producing action's digest
// without a second resolution pass.
func newInvocationRuntime(ws *workspace.Workspace, planned []*ScheduledJob) *invocationRuntime {
	resolved := planner.ResolveInvocationRelations(planned)
	if len(resolved) == 0 {
		return nil
	}
	relations := make([]*invocationRelation, 0, len(resolved))
	for _, relation := range resolved {
		relations = append(relations, &invocationRelation{InvocationRelation: *relation})
	}
	rt := &invocationRuntime{
		ws:           ws,
		relations:    relations,
		byProducer:   make(map[string]*invocationRelation, len(relations)),
		byConsumer:   make(map[string]*invocationRelation),
		byFinalizer:  make(map[string]*invocationRelation, len(relations)),
		participants: make(map[string]*invocationRelation),
	}
	for _, rel := range relations {
		rt.byProducer[rel.Producer.Key()] = rel
		rt.byFinalizer[rel.Finalizer.Key()] = rel
		rt.participants[rel.Producer.Key()] = rel
		rt.participants[rel.Finalizer.Key()] = rel
		for _, consumer := range rel.Consumers {
			// A step belongs to at most ONE invocation, which planner.ValidateInvocationFrontiers
			// rejects a plan for violating and manifest validation rejects a manifest
			// for authoring. Overwriting here would silently pick a winner: the
			// consumer would hold one relation's private artifact tree while the
			// other relation's blocking and guard state applied to nothing. Both
			// relations are refused instead, so the frontier is skipped with a cause
			// that names the collision rather than executed against a coin flip.
			if owner := rt.byConsumer[consumer.Key()]; owner != nil && owner != rel {
				reason := planner.SharedConsumerReason(consumer, &owner.InvocationRelation, &rel.InvocationRelation)
				owner.refuse(reason)
				rel.refuse(reason)
				continue
			}
			rt.byConsumer[consumer.Key()] = rel
			rt.participants[consumer.Key()] = rel
		}
	}
	return rt
}

// refuse marks the relation unusable: its frontier is blocked with reason and
// never runs against a resource whose ownership is ambiguous.
func (rel *invocationRelation) refuse(reason string) {
	rel.mu.Lock()
	rel.setupFailed = true
	rel.conflict = reason
	rel.mu.Unlock()
}

// relationFor returns the relation a job participates in, or nil.
func (rt *invocationRuntime) relationFor(job *ScheduledJob) *invocationRelation {
	if rt == nil || job == nil {
		return nil
	}
	return rt.participants[job.Key()]
}
