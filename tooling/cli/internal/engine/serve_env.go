package engine

import (
	"io"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// serveWorkloadSteps returns, by job key, the plan's serve-workload steps:
// the long-running steps `--serve-env` reaches and the job runner announces
// the process group of. A step is one only when every one of these holds, so
// nothing finite can receive the supervisor's environment by accident:
//
//   - it belongs to a SELECTED project: a dependency's `^generate` step, which
//     the serve pipeline schedules on a project nobody asked to serve, is not;
//   - its command's resolved traits declare the serve workload;
//   - its task is uncacheable, which is what every serve step declares ("a
//     cache hit cannot reproduce a live process or a bound port");
//   - no other planned job depends on it or is serialized after it: it is the
//     terminal step of its pipeline.
//
// A finite step of a serve pipeline (config merge, generate, describe) is
// therefore never one. Nil when the plan holds none.
func serveWorkloadSteps(planned []*scheduler.ScheduledJob, selected []*workspace.Project) map[string]bool {
	selectedIDs := make(map[string]bool, len(selected))
	for _, project := range selected {
		if project != nil {
			selectedIDs[project.ID] = true
		}
	}
	predecessors := make(map[string]bool)
	for _, job := range planned {
		if job != nil {
			for _, key := range job.SchedulingPredecessors() {
				predecessors[key] = true
			}
		}
	}
	var steps map[string]bool
	for _, job := range planned {
		if job == nil || job.Project == nil || job.JobDef == nil ||
			!selectedIDs[job.Project.ID] ||
			job.JobDef.Traits.Workload != extension.WorkloadServe ||
			job.JobDef.Cache ||
			predecessors[job.Key()] {
			continue
		}
		if steps == nil {
			steps = make(map[string]bool)
		}
		steps[job.Key()] = true
	}
	return steps
}

// refuseInvalidServeEnv is the usage check of `--serve-env`, made once the
// plan is final and before anything reads it. It writes the refusal to stderr
// and reports true when the run is refused:
//
//   - it is hosted, or portable on either side of the seam: a hosted runner
//     has no supervisor on its stdin, and a portable request carries no
//     process environment to the machine that runs it;
//   - no planned step is a serve-workload step (serveWorkloadSteps): the
//     environment would reach nothing;
//   - a serve-workload step's task declares one of the variables in its own
//     manifest env, which the job runner applies after, and which would
//     therefore silently replace, the supervisor's value.
//
// The refusal names the project, the task and the variable, never a value.
func refuseInvalidServeEnv(req *Request, planned []*scheduler.ScheduledJob, steps map[string]bool, stderr io.Writer) bool {
	if req.Global.ServeEnv == "" && len(req.ServeProcessEnv) == 0 {
		return false
	}
	refuse := func(format string, args ...any) bool {
		iox.Fprintf(stderr, "putnami: "+format+"\n", args...)
		return true
	}
	if runcredential.Hosted() {
		return refuse("--serve-env is refused in a hosted run: a hosted runner has no supervisor to read the environment from")
	}
	if req.Portable != nil || req.runnerProvider != nil {
		return refuse("--serve-env is refused in a portable run: the environment does not travel to the machine that runs the request")
	}
	if len(steps) == 0 {
		return refuse("--serve-env needs a serve-workload step, and the plan holds none: run a command whose traits declare the serve workload, such as serve")
	}
	names := make(map[string]string, len(req.ServeProcessEnv))
	for _, entry := range req.ServeProcessEnv {
		name, _, _ := strings.Cut(entry, "=")
		names[envkeys.Host.Canonical(name)] = name
	}
	for _, job := range planned {
		if job == nil || job.JobDef == nil || !steps[job.Key()] {
			continue
		}
		declared := make([]string, 0, len(job.JobDef.Env))
		for name := range job.JobDef.Env {
			declared = append(declared, name)
		}
		sort.Strings(declared)
		for _, name := range declared {
			if variable, conflicts := names[envkeys.Host.Canonical(name)]; conflicts {
				return refuse("--serve-env sets %s, which the task %s of project %s declares in its own env and would replace; remove it from the document or from the task", variable, job.JobDef.Name, job.Project.ID)
			}
		}
	}
	return false
}
