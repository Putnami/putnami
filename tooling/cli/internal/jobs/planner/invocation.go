package planner

import (
	"fmt"
	"sort"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// InvocationRelation is one `finalizes` relation resolved onto plan nodes: the
// plan-time half of an invocation relation. The scheduler's own relation embeds
// it and adds the runtime state that moves it through an invocation.
type InvocationRelation struct {
	Provider  string
	Producer  *jobmodel.ScheduledJob
	Consumers []*jobmodel.ScheduledJob
	Finalizer *jobmodel.ScheduledJob
	// Sensitive records that the producer declares at least one sensitive
	// invocation-scoped output, so the relation needs the leak guard at all. It
	// is resolved once, off the declaration, and never changes: a relation
	// without it keeps the unwrapped event handler and pays nothing per event.
	Sensitive bool
}

// ValidateInvocationFrontiers is the plan-time half of the one-invocation-per-step
// rule, and the reason it is an ERROR rather than a repair: the manifest check
// makes an authored collision unpublishable, so a plan that still carries one
// means the expansion produced a shape no manifest describes. Running it would
// mean choosing which relation owns the step.
//
// It resolves the relations as a side effect, which is also what stamps each
// consumer's producing action for cache-key derivation, so Plan calls this
// instead of resolving separately.
func ValidateInvocationFrontiers(planned []*jobmodel.ScheduledJob) error {
	owner := make(map[string]*InvocationRelation)
	var lines []string
	for _, rel := range ResolveInvocationRelations(planned) {
		for _, consumer := range rel.Consumers {
			existing, claimed := owner[consumer.Key()]
			if !claimed {
				owner[consumer.Key()] = rel
				continue
			}
			lines = append(lines, "  "+SharedConsumerReason(consumer, existing, rel))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	sort.Strings(lines)
	return fmt.Errorf("plan shares a step between two finalizes relations:\n%s", strings.Join(lines, "\n"))
}

// SharedConsumerReason names the step and BOTH relations, because either one
// alone leaves the reader guessing which other declaration they have to change.
func SharedConsumerReason(consumer *jobmodel.ScheduledJob, first, second *InvocationRelation) string {
	return fmt.Sprintf(
		"%s is in the consumer frontier of %s (finalized by %s) and of %s (finalized by %s); "+
			"a step belongs to at most one invocation",
		consumer.Key(),
		first.Producer.Key(), first.Finalizer.Key(),
		second.Producer.Key(), second.Finalizer.Key())
}

// ResolveInvocationRelations turns every `runOn: finally` plan node into a
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
func ResolveInvocationRelations(planned []*jobmodel.ScheduledJob) []*InvocationRelation {
	var finalizers []*jobmodel.ScheduledJob
	for _, job := range planned {
		if jobmodel.IsFinalizerJob(job) {
			finalizers = append(finalizers, job)
		}
	}
	if len(finalizers) == 0 {
		return nil
	}

	byStep := make(map[string]*jobmodel.ScheduledJob, len(planned))
	for _, job := range planned {
		byStep[invocationStepKey(job, job.StepID())] = job
	}

	relations := make([]*InvocationRelation, 0, len(finalizers))
	for _, finalizer := range finalizers {
		relation := finalizer.Step.Finalizes
		producer := byStep[invocationStepKey(finalizer, strings.TrimSpace(relation.Producer))]
		if producer == nil || producer == finalizer {
			continue
		}
		consumers := make([]*jobmodel.ScheduledJob, 0, len(relation.Consumers))
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
		relations = append(relations, &InvocationRelation{
			Provider:  provider,
			Producer:  producer,
			Consumers: consumers,
			Finalizer: finalizer,
			Sensitive: declaresSensitiveArtifact(producer),
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
func declaresSensitiveArtifact(producer *jobmodel.ScheduledJob) bool {
	for _, output := range SortedInvocationOutputs(producer) {
		if output.Sensitive && output.PathFrom == "" {
			return true
		}
	}
	return false
}

// SortedInvocationOutputs returns the job's invocation-scoped declared outputs
// in canonical id order, so preparation, verification and hardening are
// order-independent.
func SortedInvocationOutputs(job *jobmodel.ScheduledJob) []extension.DeclaredOutput {
	declaration := jobmodel.TaskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	ids := make([]string, 0, len(declaration.Outputs))
	for id, output := range declaration.Outputs {
		if output.IsInvocationScoped() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]extension.DeclaredOutput, 0, len(ids))
	for _, id := range ids {
		out = append(out, declaration.Outputs[id])
	}
	return out
}

// invocationStepKey addresses a sibling step of the same pipeline: same
// project, same extension, same command. A finalizes relation never crosses any
// of those boundaries — it names step ids inside one command's `run` list.
func invocationStepKey(sibling *jobmodel.ScheduledJob, stepID string) string {
	project, ext := "", ""
	if sibling.Project != nil {
		project = sibling.Project.ID
	}
	if sibling.Extension != nil {
		ext = sibling.Extension.Name
	}
	return project + "\x00" + ext + "\x00" + sibling.CommandName() + "\x00" + stepID
}

// SchedulableJobs returns the plan minus its finalizer nodes.
//
// A finalizer is never dispatched by the DAG. Its trigger is producer START and
// its completion frontier is the consumers' TERMINAL states — neither of which a
// dependency DAG can express, since the DAG only knows "ran successfully".
// Leaving it in would make it ready immediately (a finalizer has no dependsOn,
// by contract) and run it before the work it tears down.
func SchedulableJobs(planned []*jobmodel.ScheduledJob) []*jobmodel.ScheduledJob {
	held := 0
	for _, job := range planned {
		if jobmodel.IsFinalizerJob(job) {
			held++
		}
	}
	if held == 0 {
		return planned
	}
	out := make([]*jobmodel.ScheduledJob, 0, len(planned)-held)
	for _, job := range planned {
		if !jobmodel.IsFinalizerJob(job) {
			out = append(out, job)
		}
	}
	return out
}
