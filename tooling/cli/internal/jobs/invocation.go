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
// THIS file owns the runtime relation and the decision that precedes any
// execution: the pruning that answers "does this resource need to exist at all"
// before anything is provisioned. The plan-time half of the relation lives in
// internal/jobs/planner/invocation.go: resolving `finalizes` onto plan nodes
// (which is also where the producing-action stamp the cache identity needs is
// attached) and the one-invocation-per-step rule that keeps ownership of a step
// unambiguous. Every runtime member of invocationRelation is declared here and
// guarded by its mutex; the sibling files are the state machines that move
// them.
package jobs

import (
	"strings"
	"sync"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
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
// Its mutex guards every mutable member because the producer's worker, the
// consumers' workers and the coordinator all touch it.
type invocationRelation struct {
	planner.InvocationRelation

	mu sync.Mutex
	// armed records that the producer STARTED. It is the finalizer's trigger and
	// is never cleared: a producer that failed still armed cleanup.
	armed bool
	// pruned records that every consumer was already cached, so neither the
	// producer nor the finalizer has any work to do.
	pruned bool
	// recheck records that pruned rests on a consumer key an earlier task's
	// writes may move, so the producer confirms it before it is skipped
	// (isPruned).
	recheck bool
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

// isPruned reports whether the job is the PRODUCER or the FINALIZER of a
// relation whose whole consumer frontier was already cached.
//
// A CONSUMER is deliberately not matched, and that asymmetry is the whole point
// of the predicate. Pruning is decided BY the consumers: the relation is pruned
// precisely because every consumer's entry is already in the local store, so a
// consumer that reached this check must be allowed to fall through to
// lookupRestoreOrClaim and be SERVED that entry — its stored verdict replayed
// and its declared outputs materialized. Matching through the participants map
// (producer + consumers + finalizer) instead reported every consumer of every
// relation "skipped" on each warm run: the cached verdict was never served and
// the declared outputs were never restored, for exactly the runs the pruning
// exists to make cheap.
//
// A producer about to start confirms, once, a pruning decided on a consumer key
// that may have moved since (prune): present answers for each consumer from the
// key it computes now, and the first absent entry withdraws the pruning, so the
// producer runs and its finalizer with it. present runs without the relation's
// lock: it takes the run's lock, which the coordinator holds while it takes
// every relation's (pendingFinalizers, settleFinalizers), so a relation's lock
// is never held while the run's is awaited. Nothing reads pruned meanwhile:
// only the producer's own start claims the confirmation, and the finalizer
// waits for the producer to arm it.
func (rt *invocationRuntime) isPruned(job *ScheduledJob, present func(consumer *ScheduledJob) bool) bool {
	if rt == nil || job == nil {
		return false
	}
	key := job.Key()
	rel := rt.byProducer[key]
	producer := rel != nil
	if rel == nil {
		rel = rt.byFinalizer[key]
	}
	if rel == nil {
		return false
	}
	rel.mu.Lock()
	confirm := producer && rel.pruned && rel.recheck
	if confirm {
		rel.recheck = false
	}
	pruned := rel.pruned
	rel.mu.Unlock()
	if !confirm {
		return pruned
	}

	confirmed := true
	for _, consumer := range rel.Consumers {
		if !present(consumer) {
			confirmed = false
			break
		}
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if !confirmed {
		rel.pruned = false
	}
	return rel.pruned
}

// prune marks the relations whose consumers are ALL already served from the
// local store, so neither the producer nor its finalizer runs.
//
// Negotiation before materialization is the contract's wording and the reason
// this runs before the first task opens: the producer is uncacheable by
// construction (a task with an invocation-scoped output must set cache:false),
// so if it were allowed to start first it would provision a resource nobody was
// ever going to read.
//
// A consumer whose entry lives only in the REMOTE cache is deliberately treated
// as a miss here. Remote hits are discovered lazily, per task, during execution
// (RemoteCache.Restore); asking the provider up front would turn every relation
// into a synchronous round trip. Provisioning a resource nobody reads is a
// wasted setup, not a correctness failure — the finalizer still tears it down.
//
// A manifest can condition pruning on command params through finalizes.pruneIf.
// This is the provider-neutral escape hatch for a producer whose action is not
// solely in service of its consumer frontier for a particular request. False
// and malformed expressions keep the lifecycle: an optimization may waste work
// when uncertain, but it must never swallow an imperative side effect.
//
// A consumer key a planned write may move (cachekey.PlanKeyMovement) is only an
// estimate here: the consumer will look its entry up at the key it computes
// when it starts. Such a relation is pruned only when every job that can move
// that key finishes before the producer starts (cachekey.SettlesBefore), and the
// producer confirms the decision then, from the keys the consumers compute at
// that moment (isPruned). Otherwise it is not pruned: a consumer that
// missed its entry would run against a resource nobody provisioned.
func (rt *invocationRuntime) prune(
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	commandParams map[string]any,
	versions cachekey.RunVersions,
	cache *store.CacheManager,
	bypass cachekey.CacheBypass,
	stats *cachekey.CacheStats,
) {
	if rt == nil || cache == nil || bypass.All {
		return
	}
	keys, err := cachekey.PrecomputeKeys(ws, planned, commandParams, versions, cache, bypass, stats)
	if err != nil {
		return
	}
	var movement *cachekey.KeyMovement
	var byKey map[string]*ScheduledJob
	for _, rel := range rt.relations {
		allHit := len(rel.Consumers) > 0
		for _, consumer := range rel.Consumers {
			hash, ok := keys[consumer.Key()]
			if !ok || !cachedEntryPresent(cache, consumer, hash) {
				allHit = false
				break
			}
		}
		if !allHit {
			continue
		}
		if expression := strings.TrimSpace(rel.Finalizer.Step.Finalizes.PruneIf); expression != "" {
			prune, valid := extension.EvaluateExpressionChecked(
				expression, &extension.WhenContext{Params: commandParams})
			if !valid || !prune {
				continue
			}
		}
		if movement == nil {
			decided := cachekey.PlanKeyMovement(cachekey.KeyInputs{Workspace: ws, CommandParams: commandParams}, planned)
			movement = &decided
			byKey = cachekey.JobsByKey(planned)
		}
		recheck, settled := false, true
		for _, consumer := range rel.Consumers {
			if !movement.Moving[consumer.Key()] {
				continue
			}
			recheck = true
			if !cachekey.SettlesBefore(rel.Producer, consumer, *movement, byKey) {
				settled = false
				break
			}
		}
		if !settled {
			continue
		}
		rel.mu.Lock()
		rel.pruned = true
		rel.recheck = recheck
		rel.mu.Unlock()
	}
}

// cachedEntryPresent reports whether the local store already holds the job's
// entry at hash, using the same address the execution path would look it up at.
func cachedEntryPresent(cache *store.CacheManager, job *ScheduledJob, hash string) bool {
	present, err := cachekey.CacheEntryPresent(cache, job, hash)
	return err == nil && present
}
