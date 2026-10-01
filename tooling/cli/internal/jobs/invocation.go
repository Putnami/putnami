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
//     PRODUCING ACTION's digest (executor.go), never the secret's content, so a
//     warm consumer is warm because its inputs are unchanged and not because a
//     secret happened to repeat.
//
// THIS file owns the relation itself and the two decisions that precede any
// execution: resolving `finalizes` onto plan nodes (which is also where the
// producing-action stamp the cache identity needs is attached), the
// one-invocation-per-step rule that keeps ownership of a step unambiguous, and
// the pruning that answers "does this resource need to exist at all" before
// anything is provisioned. Every member of invocationRelation is declared here
// and guarded by its mutex; the sibling files are the state machines that move
// them.
package jobs

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// invocationRelation is one `finalizes` relation resolved onto plan nodes.
//
// It owns the whole lifecycle of one invocation-scoped resource: the private
// scratch, the lease, the artifacts' confinement, and the single finalizer run.
// Its mutex guards every mutable member because the producer's worker, the
// consumers' workers and the coordinator all touch it.
type invocationRelation struct {
	provider  string
	producer  *ScheduledJob
	consumers []*ScheduledJob
	finalizer *ScheduledJob
	// sensitive records that the producer declares at least one sensitive
	// invocation-scoped output, so the relation needs the leak guard at all. It
	// is resolved once, off the declaration, and never changes: a relation
	// without it keeps the unwrapped event handler and pays nothing per event.
	sensitive bool

	mu sync.Mutex
	// armed records that the producer STARTED. It is the finalizer's trigger and
	// is never cleared: a producer that failed still armed cleanup.
	armed bool
	// pruned records that every consumer was already cached, so neither the
	// producer nor the finalizer has any work to do.
	pruned bool
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
	relations := resolveInvocationRelations(planned)
	if len(relations) == 0 {
		return nil
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
		rt.byProducer[rel.producer.Key()] = rel
		rt.byFinalizer[rel.finalizer.Key()] = rel
		rt.participants[rel.producer.Key()] = rel
		rt.participants[rel.finalizer.Key()] = rel
		for _, consumer := range rel.consumers {
			// A step belongs to at most ONE invocation, which validateInvocationFrontiers
			// rejects a plan for violating and manifest validation rejects a manifest
			// for authoring. Overwriting here would silently pick a winner: the
			// consumer would hold one relation's private artifact tree while the
			// other relation's blocking and guard state applied to nothing. Both
			// relations are refused instead, so the frontier is skipped with a cause
			// that names the collision rather than executed against a coin flip.
			if owner := rt.byConsumer[consumer.Key()]; owner != nil && owner != rel {
				reason := sharedConsumerReason(consumer, owner, rel)
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

// validateInvocationFrontiers is the plan-time half of the one-invocation-per-step
// rule, and the reason it is an ERROR rather than a repair: the manifest check
// makes an authored collision unpublishable, so a plan that still carries one
// means the expansion produced a shape no manifest describes. Running it would
// mean choosing which relation owns the step.
//
// It resolves the relations as a side effect, which is also what stamps each
// consumer's producing action for cache-key derivation, so Plan calls this
// instead of resolving separately.
func validateInvocationFrontiers(planned []*ScheduledJob) error {
	owner := make(map[string]*invocationRelation)
	var lines []string
	for _, rel := range resolveInvocationRelations(planned) {
		for _, consumer := range rel.consumers {
			existing, claimed := owner[consumer.Key()]
			if !claimed {
				owner[consumer.Key()] = rel
				continue
			}
			lines = append(lines, "  "+sharedConsumerReason(consumer, existing, rel))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	sort.Strings(lines)
	return fmt.Errorf("plan shares a step between two finalizes relations:\n%s", strings.Join(lines, "\n"))
}

// sharedConsumerReason names the step and BOTH relations, because either one
// alone leaves the reader guessing which other declaration they have to change.
func sharedConsumerReason(consumer *ScheduledJob, first, second *invocationRelation) string {
	return fmt.Sprintf(
		"%s is in the consumer frontier of %s (finalized by %s) and of %s (finalized by %s); "+
			"a step belongs to at most one invocation",
		consumer.Key(),
		first.producer.Key(), first.finalizer.Key(),
		second.producer.Key(), second.finalizer.Key())
}

// refuse marks the relation unusable: its frontier is blocked with reason and
// never runs against a resource whose ownership is ambiguous.
func (rel *invocationRelation) refuse(reason string) {
	rel.mu.Lock()
	rel.setupFailed = true
	rel.conflict = reason
	rel.mu.Unlock()
}

// resolveInvocationRelations turns every `runOn: finally` plan node into a
// resolved relation, dropping any whose producer or consumer frontier is not in
// the plan.
//
// A dropped relation leaves its finalizer inert rather than half-wired: the
// frontier is the contract's completion signal, and a finalizer that cannot see
// its whole frontier would run at an arbitrary moment. Pipeline expansion can
// legitimately remove a step (a plan-time `if`), so this is a normal outcome and
// not an error.
//
// The function is PURE in the plan apart from the producer stamp it attaches,
// which is idempotent, so Plan and the scheduler can both call it and cannot
// disagree.
func resolveInvocationRelations(planned []*ScheduledJob) []*invocationRelation {
	var finalizers []*ScheduledJob
	for _, job := range planned {
		if isFinalizerJob(job) {
			finalizers = append(finalizers, job)
		}
	}
	if len(finalizers) == 0 {
		return nil
	}

	byStep := make(map[string]*ScheduledJob, len(planned))
	for _, job := range planned {
		byStep[invocationStepKey(job, job.StepID())] = job
	}

	relations := make([]*invocationRelation, 0, len(finalizers))
	for _, finalizer := range finalizers {
		relation := finalizer.Step.Finalizes
		producer := byStep[invocationStepKey(finalizer, strings.TrimSpace(relation.Producer))]
		if producer == nil || producer == finalizer {
			continue
		}
		consumers := make([]*ScheduledJob, 0, len(relation.Consumers))
		complete := len(relation.Consumers) > 0
		for _, authored := range relation.Consumers {
			consumer := byStep[invocationStepKey(finalizer, strings.TrimSpace(authored))]
			if consumer == nil {
				complete = false
				break
			}
			consumers = append(consumers, consumer)
		}
		if !complete {
			continue
		}
		provider := ""
		if producer.Extension != nil {
			provider = producer.Extension.Name
		}
		for _, consumer := range consumers {
			consumer.InvocationProducer = producer
		}
		relations = append(relations, &invocationRelation{
			provider:  provider,
			producer:  producer,
			consumers: consumers,
			finalizer: finalizer,
			sensitive: declaresSensitiveArtifact(producer),
		})
	}
	if len(relations) == 0 {
		return nil
	}
	return relations
}

// declaresSensitiveArtifact reports whether the producer declares an
// invocation-scoped output marked sensitive — the only thing that can put a
// needle in the guard. A relation without one is exempt from the event guard
// entirely rather than paying a lock per event to be told the needle set is
// empty.
func declaresSensitiveArtifact(producer *ScheduledJob) bool {
	for _, output := range sortedInvocationOutputs(producer) {
		if output.Sensitive && output.PathFrom == "" {
			return true
		}
	}
	return false
}

// invocationStepKey addresses a sibling step of the same pipeline: same
// project, same extension, same command. A finalizes relation never crosses any
// of those boundaries — it names step ids inside one command's `run` list.
func invocationStepKey(sibling *ScheduledJob, stepID string) string {
	project, ext := "", ""
	if sibling.Project != nil {
		project = sibling.Project.ID
	}
	if sibling.Extension != nil {
		ext = sibling.Extension.Name
	}
	return project + "\x00" + ext + "\x00" + sibling.CommandName() + "\x00" + stepID
}

// schedulableJobs returns the plan minus its finalizer nodes.
//
// A finalizer is never dispatched by the DAG. Its trigger is producer START and
// its completion frontier is the consumers' TERMINAL states — neither of which a
// dependency DAG can express, since the DAG only knows "ran successfully".
// Leaving it in would make it ready immediately (a finalizer has no dependsOn,
// by contract) and run it before the work it tears down.
func schedulableJobs(planned []*ScheduledJob) []*ScheduledJob {
	held := 0
	for _, job := range planned {
		if isFinalizerJob(job) {
			held++
		}
	}
	if held == 0 {
		return planned
	}
	out := make([]*ScheduledJob, 0, len(planned)-held)
	for _, job := range planned {
		if !isFinalizerJob(job) {
			out = append(out, job)
		}
	}
	return out
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
func (rt *invocationRuntime) isPruned(job *ScheduledJob) bool {
	if rt == nil || job == nil {
		return false
	}
	key := job.Key()
	rel := rt.byProducer[key]
	if rel == nil {
		rel = rt.byFinalizer[key]
	}
	if rel == nil {
		return false
	}
	rel.mu.Lock()
	defer rel.mu.Unlock()
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
func (rt *invocationRuntime) prune(
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	commandParams map[string]any,
	versions RunVersions,
	cache *store.CacheManager,
	bypass CacheBypass,
	stats *CacheStats,
) {
	if rt == nil || cache == nil || bypass.All {
		return
	}
	keys, err := PrecomputeKeys(ws, planned, commandParams, versions, cache, bypass, stats)
	if err != nil {
		return
	}
	for _, rel := range rt.relations {
		allHit := len(rel.consumers) > 0
		for _, consumer := range rel.consumers {
			hash, ok := keys[consumer.Key()]
			if !ok || !cachedEntryPresent(cache, consumer, hash) {
				allHit = false
				break
			}
		}
		if !allHit {
			continue
		}
		if expression := strings.TrimSpace(rel.finalizer.Step.Finalizes.PruneIf); expression != "" {
			prune, valid := extension.EvaluateExpressionChecked(
				expression, &extension.WhenContext{Params: commandParams})
			if !valid || !prune {
				continue
			}
		}
		rel.mu.Lock()
		rel.pruned = true
		rel.mu.Unlock()
	}
}

// cachedEntryPresent reports whether the local store already holds the job's
// entry at hash, using the same address the execution path would look it up at.
func cachedEntryPresent(cache *store.CacheManager, job *ScheduledJob, hash string) bool {
	present, err := CacheEntryPresent(cache, job, hash)
	return err == nil && present
}
