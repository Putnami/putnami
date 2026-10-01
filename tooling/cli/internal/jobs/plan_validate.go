package jobs

import (
	"fmt"
	"sort"
	"strings"
)

const maxDAGDiagnosticLines = 8

// validatePlanDAG catches missing dependencies and cycles before execution.
// The scheduler can only run jobs whose dependency count reaches zero; without
// this guard an unschedulable plan would finish immediately and every project
// would look merely skipped.
func validatePlanDAG(planned []*ScheduledJob) error {
	if len(planned) == 0 {
		return nil
	}

	jobsByKey := jobsByPlanKey(planned)
	if ambiguous := ambiguousKeyLines(planned, maxDAGDiagnosticLines); len(ambiguous) > 0 {
		// Two planned jobs sharing one key is an ambiguous provider: the DAG
		// map, the result map and every cache lookup would silently collapse
		// them last-wins, which is why this is a plan-time error.
		return fmt.Errorf("plan contains ambiguous providers — two jobs share one identity:\n%s", strings.Join(ambiguous, "\n"))
	}
	if missing := missingDependencyLines(planned, jobsByKey, maxDAGDiagnosticLines); len(missing) > 0 {
		return fmt.Errorf("dependency graph has unresolved job dependencies:\n%s", strings.Join(missing, "\n"))
	}

	state := newDAGState(planned)
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
func ambiguousKeyLines(planned []*ScheduledJob, limit int) []string {
	first := make(map[string]*ScheduledJob, len(planned))
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

func plannedProviderName(job *ScheduledJob) string {
	if job.Extension != nil && job.Extension.Name != "" {
		return job.Extension.Name
	}
	return "(no extension)"
}

func jobsByPlanKey(planned []*ScheduledJob) map[string]*ScheduledJob {
	out := make(map[string]*ScheduledJob, len(planned))
	for _, job := range planned {
		out[job.Key()] = job
	}
	return out
}

func missingDependencyLines(planned []*ScheduledJob, jobsByKey map[string]*ScheduledJob, limit int) []string {
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

func blockedDependencyLines(planned []*ScheduledJob, remaining map[string]int, limit int) []string {
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

func pendingDeps(job *ScheduledJob, allKeys, remainingSet map[string]bool) []string {
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

func stuckDAGMessage(planned []*ScheduledJob, remaining map[string]int) string {
	lines := blockedDependencyLines(planned, remaining, maxDAGDiagnosticLines)
	if len(lines) == 0 {
		return "dependency graph cannot make progress"
	}
	return "dependency graph cannot make progress; remaining jobs are blocked:\n" + strings.Join(lines, "\n")
}
