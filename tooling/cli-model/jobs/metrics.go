package jobs

import "sort"

// PlanMetrics summarizes the shape of a planned scheduling DAG. It is rendered
// by the --plan / --dry-run output and used by tests to lock the effect of
// plan-time pruning and write-resource serialization.
type PlanMetrics struct {
	Jobs      int            // total scheduled jobs (DAG nodes)
	Edges     int            // total scheduling edges across all jobs
	Projects  int            // distinct projects with at least one job
	ByCommand map[string]int // job count per root command (build, test, ...)
}

// ComputePlanMetrics derives metrics from a planned job list. Edges count the
// same predecessors the scheduler gates readiness on: functional dependencies
// plus write-serialization edges.
func ComputePlanMetrics(planned []*ScheduledJob) PlanMetrics {
	m := PlanMetrics{ByCommand: make(map[string]int)}
	projects := make(map[string]struct{})
	for _, j := range planned {
		m.Jobs++
		m.Edges += len(j.SchedulingPredecessors())
		if j.Project != nil {
			projects[j.Project.ID] = struct{}{}
		}
		if cmd := j.CommandName(); cmd != "" {
			m.ByCommand[cmd]++
		}
	}
	m.Projects = len(projects)
	return m
}

// CommandsSorted returns the command names in ByCommand in stable alphabetical
// order so rendered output and tests are deterministic.
func (m PlanMetrics) CommandsSorted() []string {
	cmds := make([]string, 0, len(m.ByCommand))
	for c := range m.ByCommand {
		cmds = append(cmds, c)
	}
	sort.Strings(cmds)
	return cmds
}
