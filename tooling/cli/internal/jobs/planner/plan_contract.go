// Plan-time enforcement of the v3 task contract.
//
// protocols/extension states three invariants over a task's declaration
// (task_contract.go). Two of them are decidable only once a PLAN exists:
//
//   - ONE OWNER PER OUTPUT is manifest-local in ValidateOutputOwnership, which
//     compares root-relative refs inside a single manifest. The cross-extension,
//     cross-project half is explicitly the planner's, and it compares refs whose
//     roots have been resolved to concrete directories — which is how a
//     workspace-rooted path that lands inside a project is caught.
//   - HONEST EFFECTS is checked against the manifest's own cache policy at
//     validation time. The PLAN knows the effective one: a pipeline step may
//     re-enable caching on a task that declares an external effect, and only the
//     plan node carries that merged answer.
//
// Both checks are keyed on `declares`, so they are VACUOUS for every v2-adapted
// manifest (no declaration, no verdict) until B3 populates the data. That is
// deliberate: the machinery lands with its tests before the data arrives, so
// populating a manifest cannot smuggle an unowned output past the planner.
package planner

import (
	"fmt"
	"sort"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// validatePlanContract rejects a plan that contradicts the v3 task contract.
// Ordering is plan order, then sorted output id, so the diagnostics of two runs
// over one workspace are byte-identical.
func validatePlanContract(planned []*jobmodel.ScheduledJob, ws *workspace.Workspace) error {
	if len(planned) == 0 {
		return nil
	}
	if lines := overlappingOutputLines(planned, ws, maxDAGDiagnosticLines); len(lines) > 0 {
		return fmt.Errorf("plan contains overlapping declared outputs — every output has exactly one owner:\n%s",
			strings.Join(lines, "\n"))
	}
	if lines := conflictingEffectLines(planned, maxDAGDiagnosticLines); len(lines) > 0 {
		return fmt.Errorf("plan contains conflicting task effects:\n%s", strings.Join(lines, "\n"))
	}
	return nil
}

// overlappingOutputLines reports every pair of plan nodes whose declared
// outputs claim the same filesystem region, naming BOTH owners. Only refs that
// share a resolved root are compared, so the pairwise scan stays proportional
// to the collisions that are actually possible.
func overlappingOutputLines(planned []*jobmodel.ScheduledJob, ws *workspace.Workspace, limit int) []string {
	owners := cachekey.DeclaredOutputOwners(planned, ws)
	if len(owners) < 2 {
		return nil
	}

	byRoot := make(map[string][]cachekey.DeclaredOutputOwner, 4)
	order := make([]string, 0, 4)
	for _, owner := range owners {
		if _, seen := byRoot[owner.Ref.Root]; !seen {
			order = append(order, owner.Ref.Root)
		}
		byRoot[owner.Ref.Root] = append(byRoot[owner.Ref.Root], owner)
	}

	var lines []string
	total := 0
	for _, root := range order {
		group := byRoot[root]
		for i := 1; i < len(group); i++ {
			for j := 0; j < i; j++ {
				later, earlier := group[i], group[j]
				if later.Job == earlier.Job && later.ID == earlier.ID {
					continue
				}
				// ONE manifest task scheduled under several commands is one
				// OWNER, not several: `build~generate` and `test~generate` are
				// the same declaration executing for the same project, and the
				// planner already serializes them via serializeWriteResources.
				// Without this exemption every multi-command plan over a
				// declared task rejects itself. Two
				// DIFFERENT manifest tasks claiming one path remain an error.
				if later.ID == earlier.ID && sameManifestTask(later.Job, earlier.Job) {
					continue
				}
				// A carve-out cedes a subpath to ANOTHER task, which works
				// because the pipeline orders the two. Inside ONE task nothing
				// orders its outputs against each other on restore, so its own
				// cede does not divide its own declaration — the same rule the
				// manifest-local check applies (ValidateOutputOwnership).
				laterRef, earlierRef := later.Ref, earlier.Ref
				if sameManifestTask(later.Job, earlier.Job) {
					laterRef, earlierRef = laterRef.WithoutCarveOuts(), earlierRef.WithoutCarveOuts()
				}
				if !extension.OutputsOverlap(laterRef, earlierRef) {
					continue
				}
				total++
				if len(lines) < limit {
					lines = append(lines, fmt.Sprintf("  %s output %q (%s) overlaps %s output %q (%s)",
						later.Job.Key(), later.ID, describeOutputRef(later.Ref),
						earlier.Job.Key(), earlier.ID, describeOutputRef(earlier.Ref)))
				}
			}
		}
	}
	if total > len(lines) {
		lines = append(lines, fmt.Sprintf("  ... and %d more overlapping outputs", total-len(lines)))
	}
	return lines
}

// sameManifestTask reports whether two plan nodes execute the SAME manifest
// task for the SAME project — one declaration scheduled more than once (e.g.
// a generate task shared by build and test), never two distinct owners. The
// manifest task is identified by extension name plus the pipeline step's task
// reference (the declaration lives on the manifest task, not the plan node);
// non-pipeline nodes compare by job definition name.
func sameManifestTask(a, b *jobmodel.ScheduledJob) bool {
	if a == nil || b == nil || a.Project == nil || b.Project == nil {
		return false
	}
	if a.Project.ID != b.Project.ID {
		return false
	}
	if a.Extension == nil || b.Extension == nil || a.Extension.Name != b.Extension.Name {
		return false
	}
	return manifestTaskName(a) != "" && manifestTaskName(a) == manifestTaskName(b)
}

// manifestTaskName names the manifest task a plan node executes.
func manifestTaskName(job *jobmodel.ScheduledJob) string {
	if job.Step != nil && job.Step.Task != "" {
		return job.Step.Task
	}
	if job.JobDef != nil {
		return job.JobDef.Name
	}
	return ""
}

func describeOutputRef(ref extension.OutputRef) string {
	if ref.FromPort != "" {
		return "port " + ref.FromPort + " under " + ref.Root
	}
	return ref.Path
}

// conflictingEffectLines reports every plan node that would be served from
// cache while declaring an effect a cache hit cannot reproduce. The manifest
// check sees only the task's own policy; the plan node carries the effective
// one (step override included), which is the answer that decides whether the
// registry push, the cloud mutation or the process actually happens.
func conflictingEffectLines(planned []*jobmodel.ScheduledJob, limit int) []string {
	var lines []string
	total := 0
	for _, job := range planned {
		declaration := jobmodel.TaskDeclarationOf(job)
		if declaration == nil || len(declaration.Effects) == 0 {
			continue
		}
		if !cachekey.IsCacheEnabled(job, cachekey.CacheBypass{}) {
			continue
		}
		for _, effect := range sortedEffects(declaration.Effects) {
			if !extension.IsExternalTaskEffect(effect) {
				continue
			}
			total++
			if len(lines) < limit {
				lines = append(lines, fmt.Sprintf("  %s declares the external effect %q, which a cache hit would skip; such a task must set cache.enabled to false",
					job.Key(), effect))
			}
		}
	}
	if total > len(lines) {
		lines = append(lines, fmt.Sprintf("  ... and %d more conflicting effects", total-len(lines)))
	}
	return lines
}

// sortedEffects returns the declared effects in canonical order without
// mutating the manifest's slice, so the diagnostics do not depend on whether
// the declaration traveled through a normalizing load path.
func sortedEffects(effects []string) []string {
	out := append([]string(nil), effects...)
	sort.Strings(out)
	return out
}
