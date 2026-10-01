package engine

import (
	"io"
	"os"

	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// PreflightGate evaluates the committed artifacts of the selected projects under
// a deployment profile and returns the deterministic report plus, when an
// unwaived blocking finding remains or the committed doctor.waivers.json is
// malformed, an exit-2 error carrying that report. It is exactly the signature
// of commands.DoctorPreflight, which is the only production implementation.
//
// It is INJECTED (Request.Preflight) rather than called directly, which is what
// a later change made. The doctor evaluator lives in
// internal/commands next to the `putnami doctor` command that also renders it,
// and the single call from here was the ONLY reason the engine — the package
// every adapter routes through — reached into the command subtree. That one
// edge forced structural workarounds elsewhere: `putnami mcp` is hosted in
// internal/cli rather than internal/commands purely to keep
// mcp → engine → commands → mcp from closing (see internal/cli/mcp_serve.go).
// Inverting it leaves the engine depending on nothing but the workspace model,
// the job layer and the protocols.
//
// A nil gate FAILS CLOSED under the production profile (see
// runProductionPreflight): an adapter that forgets to wire it gets a loud,
// exit-2 refusal, never a silently ungated production build. Nil is harmless
// under every other profile because the gate does not run there at all.
type PreflightGate func(wsRoot string, projects []*workspace.Project, profile doctor.Profile) (doctor.Report, error)

// errNoPreflightGate is what an unwired gate reports. It is classified as an
// invalid-configuration error so it maps to the same exit 2 a real block does:
// from the caller's side "this build was not gated" and "this build was gated
// and refused" must be equally fatal.
var errNoPreflightGate = protocolcli.InvalidConfigf(
	"production preflight gate is not wired: this run cannot be verified against `putnami doctor`, " +
		"so it is refused rather than executed ungated (report this — it is a CLI defect, not a workspace one)")

// runProductionPreflight is the fail-closed doctor gate run between plan and
// execute. It returns (exitCode, blocked): blocked is true only when the
// resolved profile is production AND the doctor engine found an UNWAIVED
// high/critical finding (exit 2) or the committed doctor.waivers.json is
// malformed (fail closed), in which case the caller returns exitCode WITHOUT
// executing any job.
//
// For every other profile — dev (the default) and test — the gate is a COMPLETE
// no-op: the doctor engine is never invoked, so zero-config local flows are
// untouched (an explicit acceptance criterion of the slice). It is likewise a
// no-op when --dry-run previews the plan rather than executing — which is why it
// reads previewsOnly and not Global.DryRun: the extension-alias adapter forwards
// --dry-run to the extension as a job param and still runs the job, so that run
// is gated like any other execution.
//
// The gate reuses the exported engine invocation the deploy tooling calls
// (commands.DoctorPreflight, injected as Request.Preflight), so a build and a
// deployment gate on the identical contract. On a block it renders the report
// per output mode before returning.
func runProductionPreflight(req *Request, selectedProjects []*workspace.Project) (int, bool) {
	if req.previewsOnly() {
		return ExitSuccess, false
	}
	if doctor.Profile(req.Global.EnvProfile) != doctor.ProfileProduction {
		return ExitSuccess, false
	}
	if req.Preflight == nil {
		report := doctor.Report{Profile: doctor.ProfileProduction}
		renderProductionPreflightFailure(req.stdout(), os.Stderr, req.Global.Output, report, errNoPreflightGate)
		return protocolcli.ExitCodeForError(errNoPreflightGate), true
	}

	report, gateErr := req.Preflight(req.WorkspaceRoot, selectedProjects, doctor.ProfileProduction)
	if gateErr == nil {
		// Clean, or only info/warning findings, or every blocking finding waived:
		// stay silent so the build's own output stream is never corrupted, and let
		// execution proceed.
		return ExitSuccess, false
	}
	renderProductionPreflightFailure(req.stdout(), os.Stderr, req.Global.Output, report, gateErr)
	return protocolcli.ExitCodeForError(gateErr), true
}

// renderProductionPreflightFailure renders a blocked production preflight per
// output mode, mirroring the established doctor/contracts failure pattern. In a
// structured mode (--output=json|jsonl) it writes the failure v2 envelope to
// stdout — status failure, exit 2, and the full report carried as data so an
// agent gets the machine-readable findings — and echoes the message to stderr
// for a watching human. In human mode it prints the blocking findings to stderr
// as a pre-execution abort notice. Either way the build is aborted before any
// job runs.
func renderProductionPreflightFailure(stdout, stderr io.Writer, outputFormat string, report doctor.Report, gateErr error) {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		_, _ = protocolcli.WriteResultV2(stdout, protocolcli.OutputJSONL,
			protocolcli.NewResultV2("doctor", report, gateErr))
		iox.Fprintf(stderr, "putnami: %v\n", gateErr)
		return
	}
	printPreflightReportHuman(stderr, report)
	iox.Fprintf(stderr, "putnami: %v\n", gateErr)
}

// printPreflightReportHuman prints the doctor report that blocked a production
// build: one line per unwaived finding (severity, code, project, field) plus its
// remediation-carrying message, then the tally. Waived findings do not block, so
// they are omitted from the list (their count still shows in the tally) — the
// operator sees exactly what to fix or waive.
func printPreflightReportHuman(w io.Writer, report doctor.Report) {
	iox.Fprintf(w, "\n  Production preflight (doctor · profile %s) blocked the build:\n", report.Profile)
	for _, f := range report.Findings {
		if f.WaivedBy != nil {
			continue
		}
		field := ""
		if len(f.Evidence) > 0 && f.Evidence[0].Field != "" {
			field = " " + f.Evidence[0].Field
		}
		iox.Fprintf(w, "    [%s] %s %s%s\n", f.Severity, f.Code, f.Project, field)
		iox.Fprintf(w, "      %s\n", f.Message)
	}
	s := report.Summary
	iox.Fprintf(w, "\n  %d finding(s): %d critical · %d high · %d warning · %d info (%d waived)\n",
		s.Findings, s.Critical, s.High, s.Warning, s.Info, s.Waived)
	iox.Fprintln(w, "  Fix or waive them in doctor.waivers.json, or run `putnami doctor --profile production` for detail.")
}
