package engine

import (
	"io"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// serveWorkloadSteps returns, by job key, the plan's serve-workload steps:
// the long-running steps `--serve-env` reaches and the job runner announces
// the process group of. It is the rule withholdServeSteps splits a plan by: a
// selected project's uncacheable serve-workload step that no other planned
// job depends on. A finite step of a serve pipeline (config merge, generate,
// describe), a dependency's step and a cacheable task are never one. Nil when
// the plan holds none.
func serveWorkloadSteps(planned []*scheduler.ScheduledJob, selected []*workspace.Project) map[string]bool {
	_, steps := withholdServeSteps(planned, selected)
	if len(steps) == 0 {
		return nil
	}
	keys := make(map[string]bool, len(steps))
	for _, job := range steps {
		keys[job.Key()] = true
	}
	return keys
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
