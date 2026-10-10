// Package changeplan projects the planner's typed output onto the CI
// ImpactPlan wire, go.putnami.dev/protocol/ci, and through the protocol's
// ChangePlanFromImpactPlan onto the ChangePlan wire. The protocol owns both
// documents, their canonical forms, the digest and validation; this package
// owns only the safe projection and ordering of planner data. It never
// recreates impact analysis or serializes a task invocation.
package changeplan

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// Input is the checked, planner-owned data used to construct one ImpactPlan.
// Commands is the command list the planner planned, in its order.
type Input struct {
	Generator        ciproto.ChangePlanGenerator
	Commands         []string
	BaseSHA          string
	HeadSHA          string
	ChangedFiles     []string
	DirectProjects   []*workspace.Project
	ImpactedProjects []*workspace.Project
	Planned          []*jobs.ScheduledJob
	Cache            ciproto.ChangePlanCache
}

// The nouns name the document a refusal is about: BuildImpact refuses as the
// impact plan, Build and ProjectTasks as the change plan.
const (
	impactPlanNoun = "impact plan"
	changePlanNoun = "change plan"
)

// BuildImpact projects planner output into a canonical ImpactPlan. The caller
// owns revision resolution and planner invocation; this package owns only safe
// projection and ordering, and returns only a plan ciproto.ValidateImpactPlan
// accepts.
func BuildImpact(in Input) (ciproto.ImpactPlan, error) {
	plan, err := projectImpact(in, impactPlanNoun)
	if err != nil {
		return ciproto.ImpactPlan{}, err
	}
	if err := ciproto.ValidateImpactPlan(plan); err != nil {
		return ciproto.ImpactPlan{}, err
	}
	return plan, nil
}

// Build projects planner output into the canonical ChangePlan of repository
// and stamps its digest. It is the ImpactPlan projection BuildImpact makes,
// handed to ciproto.ChangePlanFromImpactPlan, so a ChangePlan and an
// ImpactPlan of one plan never describe it two ways. It returns only a plan
// ciproto.ValidateChangePlan accepts, and refuses every shared member as the
// ChangePlan's.
func Build(in Input, repository ciproto.ChangePlanRepository) (ciproto.ChangePlan, error) {
	plan, err := projectImpact(in, changePlanNoun)
	if err != nil {
		return ciproto.ChangePlan{}, err
	}
	return ciproto.ChangePlanFromImpactPlan(plan, repository)
}

// projectImpact is the one projection of planner output onto plan members:
// every list deduplicated and sorted, the transitive dependents derived, and
// the command list copied in its order. It validates nothing the protocol
// validates. noun names the document in a refusal.
func projectImpact(in Input, noun string) (ciproto.ImpactPlan, error) {
	plan := ciproto.ImpactPlan{
		Version:      ciproto.ImpactPlanVersion,
		Generator:    in.Generator,
		Commands:     append([]string{}, in.Commands...),
		BaseSHA:      strings.TrimSpace(in.BaseSHA),
		HeadSHA:      strings.TrimSpace(in.HeadSHA),
		ChangedFiles: canonicalPaths(in.ChangedFiles),
		Cache:        canonicalCache(in.Cache),
	}

	var err error
	if plan.Impact.DirectProjects, err = canonicalProjects(in.DirectProjects, noun); err != nil {
		return ciproto.ImpactPlan{}, err
	}
	if plan.Impact.Projects, err = canonicalProjects(in.ImpactedProjects, noun); err != nil {
		return ciproto.ImpactPlan{}, err
	}
	plan.Impact.TransitiveDependents = plan.Impact.DerivedTransitiveDependents()
	if plan.Tasks, err = canonicalTasks(in.Planned, noun); err != nil {
		return ciproto.ImpactPlan{}, err
	}
	return plan, nil
}

func canonicalProjects(projects []*workspace.Project, noun string) ([]ciproto.ChangePlanProject, error) {
	byID := make(map[string]ciproto.ChangePlanProject, len(projects))
	for _, p := range projects {
		if p == nil || p.ID == "" || p.Name == "" {
			return nil, fmt.Errorf("%s project identity is incomplete", noun)
		}
		project := ciproto.ChangePlanProject{ID: p.ID, Name: p.Name, Path: filepath.ToSlash(p.Path)}
		if prior, exists := byID[project.ID]; exists && prior != project {
			return nil, fmt.Errorf("%s has conflicting project identities for %q", noun, project.ID)
		}
		byID[project.ID] = project
	}
	out := make([]ciproto.ChangePlanProject, 0, len(byID))
	for _, project := range byID {
		out = append(out, project)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ProjectTasks is the one safe projection of a planned DAG onto admission
// metadata: identities, hard deadlines, declared resources, edges and
// contract digests, sorted by identity key. The portable runner request reuses
// it so the ChangePlan and the execution request can never describe the same
// plan two ways. It adds nothing to the ChangePlan digest domain.
func ProjectTasks(planned []*jobs.ScheduledJob) ([]ciproto.ChangePlanTask, error) {
	return canonicalTasks(planned, changePlanNoun)
}

func canonicalTasks(planned []*jobs.ScheduledJob, noun string) ([]ciproto.ChangePlanTask, error) {
	byKey := make(map[string]ciproto.ChangePlanTask, len(planned))
	for _, job := range planned {
		task, err := projectTask(job, noun)
		if err != nil {
			return nil, err
		}
		if prior, exists := byKey[task.Identity.Key]; exists && !equalTask(prior, task) {
			return nil, fmt.Errorf("%s has conflicting tasks for %q", noun, task.Identity.Key)
		}
		byKey[task.Identity.Key] = task
	}
	out := make([]ciproto.ChangePlanTask, 0, len(byKey))
	for _, task := range byKey {
		out = append(out, task)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity.Key < out[j].Identity.Key })
	return out, nil
}

func projectTask(job *jobs.ScheduledJob, noun string) (ciproto.ChangePlanTask, error) {
	if job == nil || job.JobDef == nil || job.Project == nil || job.Extension == nil {
		return ciproto.ChangePlanTask{}, fmt.Errorf("%s contains incomplete planned task", noun)
	}
	identity := job.TypedIdentity()
	if identity.Key == "" || identity.Project.ID == "" || identity.Task.Name == "" || identity.Provider.Extension == "" {
		return ciproto.ChangePlanTask{}, fmt.Errorf("%s contains incomplete task identity", noun)
	}
	deadline := job.JobDef.TimeoutMs
	if deadline == 0 {
		deadline = jobs.DefaultTimeoutMs
	}
	if deadline < 1 {
		return ciproto.ChangePlanTask{}, fmt.Errorf("task %s has no hard deadline", identity.Key)
	}
	weight := 1.0
	if job.JobDef.CPUWeight != nil && *job.JobDef.CPUWeight > 0 &&
		!math.IsNaN(*job.JobDef.CPUWeight) && !math.IsInf(*job.JobDef.CPUWeight, 0) {
		weight = *job.JobDef.CPUWeight
	}
	heavy := job.JobDef.Traits.Heavy
	if job.JobDef.Heavy != nil {
		heavy = *job.JobDef.Heavy
	}
	return ciproto.ChangePlanTask{
		Identity:   identity,
		DeadlineMS: deadline,
		ResourceClass: ciproto.ChangePlanResourceClass{Heavy: heavy, CPUWeight: weight,
			Reads: canonicalResources(job.JobDef.Reads), Writes: canonicalResources(job.JobDef.Writes)},
		DependsOn:      canonicalStrings(job.DependsOn),
		SerializeAfter: canonicalStrings(job.SerializeAfter),
		ContractDigest: job.JobDef.ContractDigest,
	}, nil
}

func canonicalResources(refs []extension.ResourceRef) []ciproto.ChangePlanResource {
	byKey := make(map[string]ciproto.ChangePlanResource, len(refs))
	for _, ref := range refs {
		if ref.ID == "" {
			continue
		}
		resource := ciproto.ChangePlanResource{ID: ref.ID, Scope: ref.EffectiveScope()}
		byKey[resource.Scope+"\x00"+resource.ID] = resource
	}
	out := make([]ciproto.ChangePlanResource, 0, len(byKey))
	for _, resource := range byKey {
		out = append(out, resource)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope == out[j].Scope {
			return out[i].ID < out[j].ID
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

func canonicalCache(cache ciproto.ChangePlanCache) ciproto.ChangePlanCache {
	cache.Status = strings.TrimSpace(cache.Status)
	cache.Reason = strings.TrimSpace(cache.Reason)
	entries := append([]ciproto.ChangePlanCacheEntry(nil), cache.Entries...)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].TaskKey != entries[j].TaskKey {
			return entries[i].TaskKey < entries[j].TaskKey
		}
		if entries[i].Key != entries[j].Key {
			return entries[i].Key < entries[j].Key
		}
		return !entries[i].Present && entries[j].Present
	})
	cache.Entries = make([]ciproto.ChangePlanCacheEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.TaskKey == "" || entry.Key == "" ||
			(len(cache.Entries) > 0 && cache.Entries[len(cache.Entries)-1].TaskKey == entry.TaskKey) {
			continue
		}
		cache.Entries = append(cache.Entries, entry)
	}
	return cache
}

func canonicalStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// canonicalPaths preserves every byte of a git path except the disallowed
// empty spelling. In particular it must not trim whitespace: a filename with a
// leading, trailing, or embedded newline is still an exact commit-diff path.
func canonicalPaths(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func equalTask(left, right ciproto.ChangePlanTask) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftBytes) == string(rightBytes)
}
