// Package changeplan projects the planner's typed output onto the CI
// ChangePlan wire, go.putnami.dev/protocol/ci. The protocol owns the document,
// its canonical form, digest and validation; this package owns only the safe
// projection and ordering of planner data. It never recreates impact analysis
// or serializes a task invocation.
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

// Input is the checked, planner-owned data used to construct one ChangePlan.
type Input struct {
	Generator        ciproto.ChangePlanGenerator
	Repository       ciproto.ChangePlanRepository
	BaseSHA          string
	HeadSHA          string
	ChangedFiles     []string
	DirectProjects   []*workspace.Project
	ImpactedProjects []*workspace.Project
	Planned          []*jobs.ScheduledJob
	Cache            ciproto.ChangePlanCache
}

// Build projects planner output into a canonical ChangePlan and stamps its
// digest. The caller owns revision resolution and planner invocation; this
// package owns only safe projection and ordering, and returns only a plan
// ciproto.ValidateChangePlan accepts.
func Build(in Input) (ciproto.ChangePlan, error) {
	plan := ciproto.ChangePlan{
		Version:      ciproto.ChangePlanVersion,
		Generator:    in.Generator,
		Repository:   in.Repository,
		BaseSHA:      strings.TrimSpace(in.BaseSHA),
		HeadSHA:      strings.TrimSpace(in.HeadSHA),
		ChangedFiles: canonicalPaths(in.ChangedFiles),
		Cache:        canonicalCache(in.Cache),
	}

	var err error
	if plan.Impact.DirectProjects, err = canonicalProjects(in.DirectProjects); err != nil {
		return ciproto.ChangePlan{}, err
	}
	if plan.Impact.Projects, err = canonicalProjects(in.ImpactedProjects); err != nil {
		return ciproto.ChangePlan{}, err
	}
	plan.Impact.TransitiveDependents = plan.Impact.DerivedTransitiveDependents()
	if plan.Tasks, err = canonicalTasks(in.Planned); err != nil {
		return ciproto.ChangePlan{}, err
	}

	digest, err := ciproto.RecomputeChangePlanDigest(plan)
	if err != nil {
		return ciproto.ChangePlan{}, err
	}
	plan.Digest = digest
	if err := ciproto.ValidateChangePlan(plan); err != nil {
		return ciproto.ChangePlan{}, err
	}
	return plan, nil
}

func canonicalProjects(projects []*workspace.Project) ([]ciproto.ChangePlanProject, error) {
	byID := make(map[string]ciproto.ChangePlanProject, len(projects))
	for _, p := range projects {
		if p == nil || p.ID == "" || p.Name == "" {
			return nil, fmt.Errorf("change plan project identity is incomplete")
		}
		project := ciproto.ChangePlanProject{ID: p.ID, Name: p.Name, Path: filepath.ToSlash(p.Path)}
		if prior, exists := byID[project.ID]; exists && prior != project {
			return nil, fmt.Errorf("change plan has conflicting project identities for %q", project.ID)
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
	return canonicalTasks(planned)
}

func canonicalTasks(planned []*jobs.ScheduledJob) ([]ciproto.ChangePlanTask, error) {
	byKey := make(map[string]ciproto.ChangePlanTask, len(planned))
	for _, job := range planned {
		task, err := projectTask(job)
		if err != nil {
			return nil, err
		}
		if prior, exists := byKey[task.Identity.Key]; exists && !equalTask(prior, task) {
			return nil, fmt.Errorf("change plan has conflicting tasks for %q", task.Identity.Key)
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

func projectTask(job *jobs.ScheduledJob) (ciproto.ChangePlanTask, error) {
	if job == nil || job.JobDef == nil || job.Project == nil || job.Extension == nil {
		return ciproto.ChangePlanTask{}, fmt.Errorf("change plan contains incomplete planned task")
	}
	identity := job.TypedIdentity()
	if identity.Key == "" || identity.Project.ID == "" || identity.Task.Name == "" || identity.Provider.Extension == "" {
		return ciproto.ChangePlanTask{}, fmt.Errorf("change plan contains incomplete task identity")
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
