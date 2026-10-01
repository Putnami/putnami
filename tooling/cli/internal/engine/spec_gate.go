package engine

import (
	"os"
	"slices"
	"sort"
	"time"

	"go.putnami.dev/cli/model/workspace"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/specgate"
)

// The spec gate is SESSION-ATTACHED: after any real
// engine session containing `test`, the post-session result finalizer joins
// the extension-emitted criteria projections with the observation reports
// already present in the session's job results and lets `enforce` fail the
// run through the canonical reducer. There is deliberately no verify DAG job:
// the finalizer seam already holds every job's results and artifacts, so no
// cross-job artifact transfer or planner special case is needed, and the
// expensive work — running the tests — was already parallelized by the DAG.
//
// The POLICY (what to collect, how to join, when a group blocks) lives in
// internal/specgate and protocols/features; only the attachment lives here,
// the same split context_map.go established. Terminal, watch, extension
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

// specGateFinalizer returns the post-session spec-verification finalizer, or
// nil when this run must not carry it: a session without the test command
// (nothing new was observed, so there is nothing to judge), a run with no
// loaded workspace, or a policy that resolves off everywhere — off means "do
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
// that dependency's own test cache entry. It may be nil, which disables
// recovery and leaves every such check unresolvable — and therefore warned.
func specGateFinalizer(req *Request, ws *workspace.Workspace, planned []*jobs.ScheduledJob,
	recovery specgate.ObservationRecovery, outcome *specGateOutcome) func(map[string]*jobs.JobResult) {
	if ws == nil || outcome == nil || !slices.Contains(req.Commands, "test") {
		return nil
	}
	if !specgate.GateActive(ws) {
		return nil
	}
	return func(results map[string]*jobs.JobResult) {
		record, err := specgate.Collect(ws, planned, results, time.Now(), recovery)
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
		for projectID, message := range specgate.SanctionMessages(record) {
			results[projectID+":"+specGateResultTask] = &jobs.JobResult{
				Status: "failed",
				Error:  &jobs.JobError{Message: message},
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

// specGateResultTask is the synthetic plan name a sanction is reported under.
// The full key is "<projectID>:specs~verify", so the reduction attributes the
// failure to the spec-owning project and the identity derives command "specs",
// matching the audit surface a reader is pointed at.
const specGateResultTask = "specs~verify"
