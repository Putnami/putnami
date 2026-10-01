package engine

import (
	modeljobs "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// withholdRequestedServeSteps applies withholdServeSteps when the request asks
// for it, and returns the plan unchanged otherwise.
func withholdRequestedServeSteps(req *Request, planned []*jobs.ScheduledJob, selected []*workspace.Project) (kept, withheld []*jobs.ScheduledJob) {
	if !req.WithholdServeSteps {
		return planned, nil
	}
	return withholdServeSteps(planned, selected)
}

// attachWithheldServeSteps hands a serve preparation's withheld steps, and the
// workspace they were planned against, back on its result.
func attachWithheldServeSteps(result SessionResult, req *Request, ws *workspace.Workspace, withheld []*jobs.ScheduledJob) SessionResult {
	if req.WithholdServeSteps {
		result.Workspace, result.Withheld = ws, withheld
	}
	return result
}

// servePreparationWithoutFiniteSteps reports a serve preparation whose
// pipelines declare nothing but their serve steps, with its successful result:
// the scheduler is not started for an empty plan.
func servePreparationWithoutFiniteSteps(req *Request, planned []*jobs.ScheduledJob, selected []*workspace.Project, ws *workspace.Workspace, withheld []*jobs.ScheduledJob) (SessionResult, bool) {
	if !req.WithholdServeSteps || len(planned) > 0 {
		return SessionResult{}, false
	}
	return attachWithheldServeSteps(SessionResult{ExitCode: ExitSuccess, Projects: selected}, req, ws, withheld), true
}

// forcesServeWatch reports whether a run is promoted to watch mode. Serve
// commands always watch, except an iteration of an outer watch loop, which
// already owns the replan policy, and a serve preparation whose serve steps were
// left to the supervisor that asked for it (Request.WithholdServeSteps): what
// remains of that run is finite.
func forcesServeWatch(req *Request, isServeMode bool) bool {
	return isServeMode && !req.Global.Watch && !req.watchIteration && !req.WithholdServeSteps
}

// withholdServeSteps splits a planned `serve` run into the jobs the scheduler
// runs and the long-running serve steps a supervisor starts itself
// (Request.WithholdServeSteps).
//
// A job is withheld only when every one of these holds, so nothing finite can
// be taken out of the run by accident:
//
//   - it belongs to a SELECTED project: a dependency's `^generate` step, which
//     the serve pipeline schedules on a project nobody asked to serve, still runs;
//   - its command is `serve`;
//   - its task is uncacheable, which is what every serve step declares ("a
//     cache hit cannot reproduce a live process or a bound port");
//   - no other planned job depends on it or is serialized after it: it is the
//     terminal step of its pipeline.
//
// The kept plan stays a closed graph: no kept job can name a withheld one,
// by the last rule.
func withholdServeSteps(planned []*jobs.ScheduledJob, selected []*workspace.Project) (kept, withheld []*jobs.ScheduledJob) {
	selectedIDs := make(map[string]bool, len(selected))
	for _, project := range selected {
		if project != nil {
			selectedIDs[project.ID] = true
		}
	}
	predecessors := make(map[string]bool)
	for _, job := range planned {
		if job == nil {
			continue
		}
		for _, key := range job.SchedulingPredecessors() {
			predecessors[key] = true
		}
	}
	kept = make([]*jobs.ScheduledJob, 0, len(planned))
	for _, job := range planned {
		if isWithheldServeStep(job, selectedIDs, predecessors) {
			withheld = append(withheld, job)
			continue
		}
		kept = append(kept, job)
	}
	return kept, withheld
}

func isWithheldServeStep(job *jobs.ScheduledJob, selectedIDs, predecessors map[string]bool) bool {
	if job == nil || job.Project == nil || job.JobDef == nil {
		return false
	}
	return selectedIDs[job.Project.ID] &&
		modeljobs.JobCommandName(job) == "serve" &&
		!job.JobDef.Cache &&
		!predecessors[job.Key()]
}
