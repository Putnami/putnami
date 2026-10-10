package ci

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// ImpactPlanVersion is the only ImpactPlan wire version. A plan that declares
// another version is refused.
const ImpactPlanVersion = 1

// ImpactPlan is the impacted plan of one base..head commit range for one
// ordered command list, as `putnami impact-plan` emits it: the changed files,
// the project closure they reach, and every task the engine plans for those
// commands over that closure. An extension reads it to build its own admission
// document without importing the CLI; ChangePlanFromImpactPlan projects it
// onto a ChangePlan.
//
// Like a ChangePlan, it carries identities and declared scheduling metadata
// only: never source bytes, task arguments, working directories, environment
// values, output paths, or invocation locators. It names no repository and
// carries no digest: the document a consumer derives from it, such as a
// ChangePlan, holds both. Cache is advisory.
type ImpactPlan struct {
	// Version is the wire version, always ImpactPlanVersion.
	Version int `json:"version"`
	// Generator is the implementation that built the plan.
	Generator ChangePlanGenerator `json:"generator"`
	// Commands is the command list the plan covers, in the order it was
	// requested, without duplicates. Tasks also holds the tasks of the
	// companion commands a requested command's extension declares in
	// alsoRuns, such as a validate-workspace that validate also runs.
	Commands []string `json:"commands"`
	// BaseSHA is the full commit ID the range starts from, an ancestor of
	// HeadSHA.
	BaseSHA string `json:"baseSHA"`
	// HeadSHA is the full commit ID of the checked-out revision the plan
	// covers.
	HeadSHA string `json:"headSHA"`
	// ChangedFiles is the base..head commit diff, every path byte for byte,
	// sorted and without duplicates. A rename contributes both paths.
	ChangedFiles []string `json:"changedFiles"`
	// Impact is the project closure the changed files reach.
	Impact ChangePlanImpact `json:"impact"`
	// Tasks is every task planned for Commands over Impact, sorted by
	// identity key.
	Tasks []ChangePlanTask `json:"tasks"`
	// Cache is the advisory local cache summary.
	Cache ChangePlanCache `json:"cache"`
}

// ValidateImpactPlan is the check a consumer runs before it uses an impact
// plan. It refuses an unknown version, an incomplete generator, a command list
// that is empty, holds a duplicate, or holds a name that is empty or contains
// a comma or whitespace, a revision that is not a full lowercase commit ID, a
// list that is unsorted or holds a duplicate, transitive dependents that
// differ from the derived ones, and an inconsistent cache summary.
func ValidateImpactPlan(plan ImpactPlan) error {
	const noun = "impact plan"
	if err := validateImpactPlanIdentity(plan); err != nil {
		return err
	}
	if err := validatePlanGenerator(noun, plan.Generator); err != nil {
		return err
	}
	if !canonicalCommandList(plan.Commands) {
		return fmt.Errorf("impact plan commands are not canonical")
	}
	return impactPlanBody(plan).validate(noun)
}

// ChangePlanFromImpactPlan projects plan onto the ChangePlan of the same range
// for repository. It copies every member the two documents share into new
// lists, derives TransitiveDependents from the closure, and stamps the digest,
// so the result neither aliases plan nor depends on how plan spells an empty
// list or its transitive dependents. It refuses a plan of another version or
// without a canonical command list, and returns only a ChangePlan
// ValidateChangePlan accepts: any other defect is refused as the ChangePlan's.
//
// A ChangePlan does not name its commands. The caller decides which command
// list the ChangePlan admits by the plan it passes.
func ChangePlanFromImpactPlan(plan ImpactPlan, repository ChangePlanRepository) (ChangePlan, error) {
	if err := validateImpactPlanIdentity(plan); err != nil {
		return ChangePlan{}, err
	}
	if !canonicalCommandList(plan.Commands) {
		return ChangePlan{}, fmt.Errorf("impact plan commands are not canonical")
	}
	impact := ChangePlanImpact{
		DirectProjects: cloneList(plan.Impact.DirectProjects),
		Projects:       cloneList(plan.Impact.Projects),
	}
	impact.TransitiveDependents = impact.DerivedTransitiveDependents()
	cache := plan.Cache
	cache.Entries = slices.Clone(cache.Entries)
	change := ChangePlan{
		Version:      ChangePlanVersion,
		Generator:    plan.Generator,
		Repository:   repository,
		BaseSHA:      plan.BaseSHA,
		HeadSHA:      plan.HeadSHA,
		ChangedFiles: cloneList(plan.ChangedFiles),
		Impact:       impact,
		Tasks:        cloneTasks(plan.Tasks),
		Cache:        cache,
	}
	digest, err := RecomputeChangePlanDigest(change)
	if err != nil {
		return ChangePlan{}, err
	}
	change.Digest = digest
	if err := ValidateChangePlan(change); err != nil {
		return ChangePlan{}, err
	}
	return change, nil
}

func validateImpactPlanIdentity(plan ImpactPlan) error {
	if plan.Version != ImpactPlanVersion {
		return fmt.Errorf("unsupported impact plan version %d", plan.Version)
	}
	return nil
}

func impactPlanBody(plan ImpactPlan) planBody {
	return planBody{BaseSHA: plan.BaseSHA, HeadSHA: plan.HeadSHA, ChangedFiles: plan.ChangedFiles,
		Impact: plan.Impact, Tasks: plan.Tasks, Cache: plan.Cache}
}

// canonicalCommandList reports whether commands is a non-empty list of
// distinct names, each non-empty and free of commas and whitespace. Order is
// the requested order and is not checked.
func canonicalCommandList(commands []string) bool {
	if len(commands) == 0 {
		return false
	}
	separator := func(r rune) bool { return r == ',' || unicode.IsSpace(r) }
	seen := make(map[string]bool, len(commands))
	for _, command := range commands {
		if command == "" || seen[command] || strings.ContainsFunc(command, separator) {
			return false
		}
		seen[command] = true
	}
	return true
}

// cloneList returns a new, non-nil list holding values.
func cloneList[T any](values []T) []T {
	return append(make([]T, 0, len(values)), values...)
}

// cloneTasks returns a new, non-nil list of tasks whose lists are copies too.
func cloneTasks(tasks []ChangePlanTask) []ChangePlanTask {
	out := make([]ChangePlanTask, 0, len(tasks))
	for _, task := range tasks {
		task.DependsOn = slices.Clone(task.DependsOn)
		task.SerializeAfter = slices.Clone(task.SerializeAfter)
		task.ResourceClass.Reads = slices.Clone(task.ResourceClass.Reads)
		task.ResourceClass.Writes = slices.Clone(task.ResourceClass.Writes)
		out = append(out, task)
	}
	return out
}
