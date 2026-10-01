package engine

import (
	"os"
	"slices"
	"strings"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/mapgen"
)

// The workspace map is BUILD-ATTACHED, not a verb someone remembers.
//
// The map is a pure projection of committed inputs, so a manually-invoked
// generator makes it eventually consistent — it converges only when a human runs
// the command — and a per-project freshness TEST cannot fix that, because the
// generator's real inputs are workspace-wide: the project holding the test is
// never impacted by the change that invalidates its output.
//
// The artifact is EPHEMERAL CLI state under .putnami/, not a
// committed file, which removes the reason a build ever went red over it: there
// is nothing to drift, nothing to commit, and nothing a runner could "fix" into
// an uncommitted build output. So the attachment now runs unconditionally in
// every workspace (writing into the CLI's own gitignored state directory is
// harmless anywhere), CI resolves to off, and this seam never fails a session.
//
// The framework has no core-injectable job (only extension manifests declare
// plan nodes), so the reduce attaches HERE: the post-session result finalizer
// seam, the same one the unpublished-archives guard uses, so it runs after the
// session's work is done.
//
// The POLICY (which mode) lives in internal/mapgen, next to the generator it
// describes; only the attachment lives here.

// contextMapFinalizer returns the post-session finalizer that refreshes the
// workspace map, or nil when this run must not touch it.
//
// nil is returned for every run that is not a real build: a session without the
// build command, a run with no loaded workspace, and a resolved-off mode (an
// explicit PUTNAMI_CONTEXT_MAP=off, or CI — see mapgen.ResolveMode). A
// plan-only/dry-run preview never reaches execution, so a finalizer it carries
// never runs. The finalizer itself additionally refuses to act when the session
// did not succeed — a failed build's tree is not a state worth publishing as the
// workspace's map, and a map error must never be what a user debugs while their
// build is red.
func contextMapFinalizer(req *Request, ws *workspace.Workspace, selected []*workspace.Project) func(map[string]*jobs.JobResult) {
	if ws == nil || !slices.Contains(req.Commands, "build") {
		return nil
	}
	if mapgen.ResolveMode(os.Getenv) == mapgen.ModeOff {
		return nil
	}
	quiet := req.Global.Quiet
	return func(results map[string]*jobs.JobResult) {
		if !resultsSucceeded(results) {
			return
		}
		report, err := mapgen.Generate(ws, selected)
		if err != nil {
			reportContextMapError(err)
			return
		}
		reportContextMapWrite(report, quiet)
	}
}

// resultsSucceeded reports whether every result the session produced reached a
// clean verdict. It is the finalizer's view of jobs.SessionResult.Success, which
// is computed AFTER finalizers run and so cannot be consulted here: a failed
// task fails the run, and a canceled one means a signal or a fail-fast abort cut
// the session short. Skipped tasks are not failures.
func resultsSucceeded(results map[string]*jobs.JobResult) bool {
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

// reportContextMapError notes a map the build could not refresh WITHOUT failing
// the session. The map is gitignored CLI state that the MCP tool rebuilds in
// memory anyway, so an unwritable .putnami/ costs a convenience file, not a
// build: turning a green build red over it is exactly the class of red this
// issue retired. It still goes to stderr, unsuppressed by --quiet, because a
// state directory the CLI cannot write is worth knowing about.
func reportContextMapError(err error) {
	iox.Fprintf(os.Stderr, "putnami: warning: could not refresh the workspace map: %v\n", err)
}

// reportContextMapWrite notes a map the build actually rewrote. A run that
// changed nothing says nothing — the whole point is that the artifact stays
// correct without anyone noticing it.
func reportContextMapWrite(report mapgen.Report, quiet bool) {
	if quiet || len(report.Outputs) == 0 {
		return
	}
	iox.Fprintf(os.Stderr, "putnami: refreshed the workspace map: %s\n", strings.Join(report.Outputs, ", "))
}
