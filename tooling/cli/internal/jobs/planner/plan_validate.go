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

const maxDAGDiagnosticLines = 8

// ValidatePlanDAG catches missing dependencies and cycles before execution.
// The scheduler can only run jobs whose dependency count reaches zero; without
// this guard an unschedulable plan would finish immediately and every project
// would look merely skipped.
func ValidatePlanDAG(planned []*jobmodel.ScheduledJob) error {
	if len(planned) == 0 {
		return nil
	}

	jobsByKey := JobsByPlanKey(planned)
	if ambiguous := ambiguousKeyLines(planned, maxDAGDiagnosticLines); len(ambiguous) > 0 {
		// Two planned jobs sharing one key is an ambiguous provider: the DAG
		// map, the result map and every cache lookup would silently collapse
		// them last-wins, which is why this is a plan-time error.
		return fmt.Errorf("plan contains ambiguous providers — two jobs share one identity:\n%s", strings.Join(ambiguous, "\n"))
	}
	if missing := missingDependencyLines(planned, jobsByKey, maxDAGDiagnosticLines); len(missing) > 0 {
		return fmt.Errorf("dependency graph has unresolved job dependencies:\n%s", strings.Join(missing, "\n"))
	}

	state := NewDAGState(planned)
	queue := state.ready()
	visited := 0
	for len(queue) > 0 {
		job := queue[0]
		queue = queue[1:]
		visited++
		queue = append(queue, state.complete(job.Key())...)
	}

	if visited == len(planned) {
		return nil
	}

	_, remaining := state.remainingSnapshot()
	lines := blockedDependencyLines(planned, remaining, maxDAGDiagnosticLines)
	return fmt.Errorf("dependency graph cannot be scheduled; possible dependency cycle:\n%s", strings.Join(lines, "\n"))
}

// ambiguousKeyLines reports every plan key claimed by more than one job,
// naming both providers so the manifest owner knows which pair collides.
func ambiguousKeyLines(planned []*jobmodel.ScheduledJob, limit int) []string {
	first := make(map[string]*jobmodel.ScheduledJob, len(planned))
	var lines []string
	for _, job := range planned {
		key := job.Key()
		prior, ok := first[key]
		if !ok {
			first[key] = job
			continue
		}
		if len(lines) < limit {
			lines = append(lines, fmt.Sprintf("  %s: provided by %s and %s",
				key, plannedProviderName(prior), plannedProviderName(job)))
		}
	}
	return lines
}

func plannedProviderName(job *jobmodel.ScheduledJob) string {
	if job.Extension != nil && job.Extension.Name != "" {
		return job.Extension.Name
	}
	return "(no extension)"
}

func JobsByPlanKey(planned []*jobmodel.ScheduledJob) map[string]*jobmodel.ScheduledJob {
	out := make(map[string]*jobmodel.ScheduledJob, len(planned))
	for _, job := range planned {
		out[job.Key()] = job
	}
	return out
}

func missingDependencyLines(planned []*jobmodel.ScheduledJob, jobsByKey map[string]*jobmodel.ScheduledJob, limit int) []string {
	var lines []string
	missingTotal := 0
	for _, job := range planned {
		for _, dep := range job.SchedulingPredecessors() {
			if _, ok := jobsByKey[dep]; ok {
				continue
			}
			missingTotal++
			if len(lines) < limit {
				lines = append(lines, fmt.Sprintf("  %s depends on missing %s", job.Key(), dep))
			}
		}
	}
	if missingTotal > len(lines) {
		lines = append(lines, fmt.Sprintf("  ... and %d more unresolved dependencies", missingTotal-len(lines)))
	}
	return lines
}

func blockedDependencyLines(planned []*jobmodel.ScheduledJob, remaining map[string]int, limit int) []string {
	allKeys := make(map[string]bool, len(planned))
	for _, job := range planned {
		allKeys[job.Key()] = true
	}
	remainingSet := make(map[string]bool, len(remaining))
	for key := range remaining {
		remainingSet[key] = true
	}

	var lines []string
	blockedTotal := 0
	for _, job := range planned {
		if !remainingSet[job.Key()] {
			continue
		}
		blockedTotal++
		if len(lines) >= limit {
			continue
		}
		blockers := pendingDeps(job, allKeys, remainingSet)
		if len(blockers) == 0 {
			lines = append(lines, fmt.Sprintf("  %s is still pending", job.Key()))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s waits for %s", job.Key(), strings.Join(blockers, ", ")))
	}
	if blockedTotal > len(lines) {
		lines = append(lines, fmt.Sprintf("  ... and %d more blocked jobs", blockedTotal-len(lines)))
	}
	return lines
}

func pendingDeps(job *jobmodel.ScheduledJob, allKeys, remainingSet map[string]bool) []string {
	var blockers []string
	for _, dep := range job.SchedulingPredecessors() {
		if remainingSet[dep] {
			blockers = append(blockers, dep)
			continue
		}
		if !allKeys[dep] {
			blockers = append(blockers, "missing "+dep)
		}
	}
	sort.Strings(blockers)
	const maxBlockers = 4
	if len(blockers) > maxBlockers {
		truncated := append([]string(nil), blockers[:maxBlockers]...)
		truncated = append(truncated, fmt.Sprintf("... and %d more", len(blockers)-maxBlockers))
		return truncated
	}
	return blockers
}

func StuckDAGMessage(planned []*jobmodel.ScheduledJob, remaining map[string]int) string {
	lines := blockedDependencyLines(planned, remaining, maxDAGDiagnosticLines)
	if len(lines) == 0 {
		return "dependency graph cannot make progress"
	}
	return "dependency graph cannot make progress; remaining jobs are blocked:\n" + strings.Join(lines, "\n")
}

// ValidateBatchProjectLimits fails the plan when a job's batch cap option holds
// anything but a positive integer. Checking at plan time turns a typo in
// putnami.workspace.json into an error before any task runs, instead of a
// grouping nobody asked for. The scheduler still refuses to batch such a job
// if a value reaches it by another route.
//
// It reads the cap exactly as Scheduler.readyBatch does: BatchProjectLimit over
// DeliveredJobParams with JobConfigDefaults. The one input that differs is the
// run's execution-only extension parameters, which the engine overlays after
// planning; they are framework globals, never a batch cap.
func ValidateBatchProjectLimits(planned []*jobmodel.ScheduledJob, ws *workspace.Workspace, commandParams extension.ParamMap) error {
	for _, job := range planned {
		if job == nil || job.JobDef == nil || job.JobDef.Batchable == nil ||
			job.JobDef.Batchable.MaxProjectsParam == "" || job.Extension == nil || job.Project == nil {
			continue
		}
		params := cachekey.DeliveredJobParams(job, commandParams, cachekey.JobConfigDefaults(ws, job))
		if _, _, err := cachekey.BatchProjectLimit(job.JobDef.Batchable, params); err != nil {
			return fmt.Errorf("invalid batch size for %s: %w", job.Key(), err)
		}
	}
	return nil
}
