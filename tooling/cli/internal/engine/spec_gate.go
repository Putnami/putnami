package engine

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/cli/model/workspace"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/specgate"
)

// The spec gate is SESSION-ATTACHED: after any real engine session that names
// a command of the verification kind, the post-session result finalizer joins
// the extension-emitted criteria projections with the observation reports
// already present in the session's job results and lets `enforce` fail the
// run through the canonical reducer. There is deliberately no verify DAG job:
// the finalizer seam already holds every job's results and artifacts, so no
// cross-job artifact transfer or planner special case is needed, and the
// expensive work — running the tests — was already parallelized by the DAG.
//
// The POLICY (what to collect, how to join, when a group blocks) lives in
// internal/specgate and protocols/features; only the attachment lives here.
// Terminal, watch, extension
// aliases, lifecycle adapters, and MCP run_jobs all receive identical
// behavior by going through Engine.Run.

// specGateOutcome carries the collected verification record from the
// finalizer (which runs inside the scheduler's terminal phase, before the
// reduction) to execute's session-persistence block (which runs after it), so
// the decided record lands beside the session report without a second
// collection pass.
type specGateOutcome struct {
	record *features.SpecVerificationRecord
}

// resultsSucceeded reports whether every result the session produced reached a
// clean verdict. It is the finalizer's view of jobs.SessionResult.Success, which
// is computed AFTER finalizers run and so cannot be consulted here: a failed
// task fails the run, and a canceled one means a signal or a fail-fast abort cut
// the session short. Skipped tasks are not failures.
func resultsSucceeded(results map[string]*scheduler.JobResult) bool {
	for _, result := range results {
		if result == nil {
			continue
		}
		if result.Status == "failed" || result.Status == "canceled" {
			return false
		}
	}
	return true
}

// specGateFinalizer returns the post-session spec-verification finalizer, or
// nil when this run must not carry it: a session that names none of
// verificationCommands, the loaded commands of the verification kind (nothing
// new was observed, so there is nothing to judge), a run with no loaded
// workspace, or a policy that resolves off everywhere — off means "do
// not collect or evaluate automatically", so the collector is not attached at
// all rather than attached and muted.
//
// The finalizer itself never computes exit status. It contributes one failed
// synthetic result per spec-owning project whose enforce-mode requirements
// did not all resolve verified, and the canonical session reducer — the only
// sanction point — turns that row into the run's verdict exactly the way it
// does every other failed task.
//
// recovery is the unplanned-attester source: the seam that lets a
// requirement attested by a dependency the selection did not plan resolve from
// that dependency's own report producer cache entry. It may be nil, which disables
// recovery and leaves every such check unresolvable — and therefore warned.
//
// producers names the selected projects a criteria-projection producer applies
// to. It may be nil, and then a session that projected nothing records
// nothing.
//
// A verification command attaches the gate whether or not its tasks declare
// the report. A verification pipeline that declares none has no report
// producer, so its projects read as consulted and silent and block under
// enforce. Attaching only on a declared producer would skip the gate in that
// workspace and let enforce pass on no evidence.
func specGateFinalizer(req *Request, ws *workspace.Workspace, planned []*scheduler.ScheduledJob,
	verificationCommands []string, recovery specgate.ObservationRecovery, producers specgate.ProjectionProducers,
	outcome *specGateOutcome) func(map[string]*scheduler.JobResult) {
	if ws == nil || outcome == nil || !slices.ContainsFunc(req.Commands, func(command string) bool {
		return slices.Contains(verificationCommands, command)
	}) {
		return nil
	}
	if !specgate.GateActive(ws) {
		return nil
	}
	return func(results map[string]*scheduler.JobResult) {
		record, err := specgate.Collect(ws, planned, results, time.Now(), recovery, producers)
		if err != nil {
			// A collection the workspace's own config makes impossible was
			// refused before jobs ran; anything left is an internal fault, and a
			// gate must not turn it into a red build nobody can act on.
			iox.Fprintf(os.Stderr, "putnami: spec verification did not run: %v\n", err)
			return
		}
		outcome.record = record
		// A session whose work already failed or was cut short keeps its own
		// verdict: available observations are still collected and persisted,
		// but no secondary "missing report" failure is added on top — the
		// original incomplete execution is authoritative.
		//
		// The WARNINGS are silenced by the same guard, and that is not
		// symmetry for its own sake. Both of them describe the run's evidence
		// SCOPE, and a truncated run has no scope worth describing: when a
		// canceled `test~test` never reports, every requirement it attests
		// reads as never observed, and the exhausted-scope message then tells
		// a reader "no test proves this yet — add a spectest.Proves call" for
		// requirements whose tests exist and simply did not run. That is worse
		// than silence: it is confident, wrong, and it sends someone to write
		// a duplicate attestation. The failing job is already on screen and is
		// the fact that explains the absence.
		if !resultsSucceeded(results) {
			return
		}
		reportSpecGateWarnings(record)
		for projectID, sanction := range specgate.Sanctions(record) {
			// The coded diagnostic reaches the machine records (session.json
			// run.failures, report.json); the terminal keeps printing the
			// error message alone.
			results[projectID+":"+specGateResultTask] = &scheduler.JobResult{
				Status: "failed",
				Error:  &scheduler.JobError{Message: sanction.Message},
				Canonical: &scheduler.TaskResult{Diagnostics: []scheduler.TaskDiagnostic{{
					Severity: "error", Code: sanction.Code, Message: sanction.Message,
				}}},
			}
		}
	}
}

// reportSpecGateWarnings prints the gate's non-blocking findings on stderr, in
// deterministic project order.
//
// It goes to stderr rather than into a synthetic result because a warning must
// not be able to change an exit code by accident: the reducer turns every
// result it is handed into a verdict, so a "warning" shaped like a result is
// one refactor away from being a failure.
func reportSpecGateWarnings(record *features.SpecVerificationRecord) {
	warnings := specgate.WarningMessages(record)
	projects := make([]string, 0, len(warnings))
	for project := range warnings {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	for _, project := range projects {
		for _, message := range warnings[project] {
			iox.Fprintf(os.Stderr, "putnami: spec verification: %s: %s\n", project, message)
		}
	}
}

// projectionProducerPlan answers specgate.ProjectionProducers with a plan of
// the criteria commands whose tasks declare the criteria projection over the
// session's selected projects, so the planner's own activation rule decides
// which projects a criteria-projection producer applies to.
// extensions is the whole discovered set, never a set the session narrowed:
// a producer the session excluded still applies to its project. skipped is
// what discovery failed to load.
type projectionProducerPlan struct {
	req        *Request
	ws         *workspace.Workspace
	projects   []*workspace.Project
	extensions []*extension.ExtensionDescription
	skipped    []extension.SkippedExtension
}

// ApplicableProjects plans the projection producers' commands over the
// selected projects with the run's command params and without its disable
// lists, and returns the selected projects whose plan holds a
// criteria-projection producer. A selected project that disables one of those
// commands itself hides whether a producer applies to it, and so does an
// extension that failed to load or a configured registry extension that is
// not loaded, disabled or not: each answer is an error. With no producer
// command, no producer applies to any project.
func (plan *projectionProducerPlan) ApplicableProjects() (map[string]bool, error) {
	if len(plan.skipped) > 0 {
		return nil, fmt.Errorf("extension %s failed to load", plan.skipped[0].Name)
	}
	if plan.req.Config != nil {
		configured := *plan.req.Config
		configured.Disable = nil
		if missing := missingRegistryExtensions(&configured, plan.extensions); len(missing) > 0 {
			return nil, fmt.Errorf("extension %s is not loaded", missing[0])
		}
	}
	commands := scheduler.SpecCriteriaProjectionCommands(plan.extensions)
	if len(commands) == 0 {
		return map[string]bool{}, nil
	}
	selected := make(map[string]bool, len(plan.projects))
	for _, project := range plan.projects {
		if project == nil {
			continue
		}
		if project.Config == nil || project.Config.Disable == nil {
			selected[project.ID] = true
			continue
		}
		// A project's own disable list hides a producer when it names a producer
		// command, alone or under one extension.
		for _, command := range commands {
			if slices.ContainsFunc(project.Config.Disable.Jobs, func(job string) bool {
				return job == command || strings.HasSuffix(job, ":"+command)
			}) {
				return nil, fmt.Errorf("project %s disables %s", project.ID, command)
			}
		}
		selected[project.ID] = true
	}
	planned, err := planner.Plan(plan.ws, commands, plan.projects, plan.extensions,
		plan.req.CommandParams, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	applicable := make(map[string]bool)
	for _, job := range planned {
		if job != nil && job.Project != nil && selected[job.Project.ID] && scheduler.ProducesSpecCriteriaProjection(job) {
			applicable[job.Project.ID] = true
		}
	}
	return applicable, nil
}

// specGateResultTask is the synthetic plan name a sanction is reported under.
// The full key is "<projectID>:specs~verify", so the reduction attributes the
// failure to the spec-owning project and the identity derives command "specs",
// matching the audit surface a reader is pointed at.
const specGateResultTask = "specs~verify"
