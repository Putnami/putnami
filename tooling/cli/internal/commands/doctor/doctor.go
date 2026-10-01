// Package doctor: `putnami doctor`.
//
// `putnami doctor` is a read-only production-readiness preflight. It walks the
// selected projects, reads only their committed artifacts (schema/*.json,
// conf/.env*.yaml, and the project's own putnami.json), and reports the problems
// derivable from them as a deterministic doctor.Report under a deployment
// profile. It never scans runtime state, touches the network, or mutates the
// tree.
//
// Findings fall in two families. Production readiness — the original checks —
// grades by profile and can block. Repository hygiene (committed-artifact
// stability and the schema commit regime) is advisory: it reports that a
// committed generated file is coupled to workspace state instead of its own
// project's declared inputs, which breaks builds elsewhere but never the
// workload, so it never blocks a profile. Workstation prerequisites —
// line endings of the checkout and, on Windows, long paths — are advisory too;
// they are the one family that reads the checkout and the host rather than
// committed artifacts, so only the command runs them, never the build gate.
//
// The wire contract — the CheckCode taxonomy, Severity/Profile enums, Finding,
// Report, Summary, and baked remediations — is the frozen, cross-epic
// go.putnami.dev/protocol/doctor package, consumed here read-only. This command
// is only the evaluator: it maps committed-artifact facts onto the frozen codes
// and grades them by profile.
//
// Profile grades severity: under production an unsafe state is high (or critical
// for a missing sensitive value) and fails with exit 2 via
// protocolcli.ErrInvalidConfig; under dev/test the same state stays info/warning
// and exits 0. The machine-readable report travels in the structured Result
// envelope's data, mirroring `contracts check`.
package doctor

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// DoctorCommand is the `putnami doctor` entrypoint: it resolves the deployment
// profile and the target project set, evaluates the committed artifacts, and
// renders the report. High/critical findings (only reachable under production)
// return an exit-2 error with the report attached for the failure envelope.
func DoctorCommand(wsRoot string, args []string, globalProjects, envProfile, outputFormat string) error {
	profile := normalizeProfile(envProfile)

	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return fmt.Errorf("load workspace: %w", err)
	}
	projects, err := selectDoctorProjects(ws, args, globalProjects)
	if err != nil {
		return err
	}

	workstation := checkWorkstation(profile, systemWorkstationProbe(ws.Root))
	report, runErr := doctorRun(ws.Root, projects, profile, time.Now(), workstation)
	return renderDoctor(iox.Stdout(), outputFormat, report, runErr)
}

// normalizeProfile validates the resolved profile against the frozen doctor
// enum and falls back to the permissive dev profile when it is empty or
// unrecognized. The profile is already validated during flag resolution
// (resolveProfile), so this is a defensive normalization, not the primary gate.
func normalizeProfile(envProfile string) doctor.Profile {
	p := doctor.Profile(strings.TrimSpace(envProfile))
	if !doctor.ValidProfiles[p] {
		return doctor.ProfileDev
	}
	return p
}

// DoctorRun evaluates the committed artifacts of every project under profile and
// returns the deterministic report plus, when the report contains a blocking
// (high/critical) finding, an exit-2 error carrying the report for the failure
// envelope. Projects are evaluated in ID order and the aggregate findings are
// sorted by a stable key, so the report is a pure function of the committed
// tree, the profile, and the injected clock.
//
// now is the injected clock waiver expiry is measured against (DoctorCommand
// passes time.Now()); pinning it makes expiry deterministic and testable. The
// committed workspace-root doctor.waivers.json is read once here: a live,
// structurally valid waiver suppresses its matching finding (recording
// provenance and counting it in Summary.Waived) without dropping it from the
// report, an expired or unknown-code waiver becomes a hygiene finding of its
// own, and a malformed waiver file fails closed via a blocking error rather
// than silently weakening the gate.
func DoctorRun(wsRoot string, projects []*workspace.Project, profile doctor.Profile, now time.Time) (doctor.Report, error) {
	return doctorRun(wsRoot, projects, profile, now, nil)
}

// doctorRun is DoctorRun plus findings evaluated outside the committed tree
// (the workstation checks), which go through the same waivers, ordering, and
// summary as the artifact findings.
func doctorRun(wsRoot string, projects []*workspace.Project, profile doctor.Profile, now time.Time, extra []doctor.Finding) (doctor.Report, error) {
	ordered := append([]*workspace.Project(nil), projects...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	var findings []doctor.Finding
	for _, proj := range ordered {
		findings = append(findings, evaluateProject(doctorProject{
			absDir:          filepath.Join(wsRoot, proj.Path),
			relPath:         filepath.ToSlash(proj.Path),
			id:              proj.ID,
			kind:            proj.Type,
			generatedClient: proj.GeneratedClient != nil,
			profile:         profile,
		})...)
	}
	findings = append(findings, extra...)

	// Waivers are read once at the workspace root. loadWaivers returns the live
	// waivers to apply, the waiver-hygiene findings (expired / unknown-code) to
	// surface, and — when the file is structurally untrustworthy — a malformed
	// error. Fail-closed: on malformedErr no waiver is applied, so a broken file
	// can never grant a silent exception to the gate.
	live, waiverFindings, malformedErr := loadWaivers(wsRoot, ordered, profile, now)
	findings = append(findings, waiverFindings...)
	if malformedErr == nil {
		applyLiveWaivers(findings, live)
	}
	sortFindings(findings)

	report := doctor.Report{
		ProtocolVersion: doctor.ProtocolVersion,
		Profile:         profile,
		Findings:        findings,
		Summary:         summarizeFindings(findings),
	}
	if malformedErr != nil {
		return report, waiverMalformedError(report, malformedErr)
	}
	return report, doctorError(report)
}

// sortFindings orders findings by a stable composite key — project, check code,
// first evidence path, first evidence field, then message — so the report never
// depends on project map iteration order or check invocation order.
func sortFindings(findings []doctor.Finding) {
	sort.Slice(findings, func(i, j int) bool {
		return findingSortKey(findings[i]) < findingSortKey(findings[j])
	})
}

// findingSortKey builds the total-order key used by sortFindings. The NUL
// separators keep field boundaries unambiguous so no two distinct findings
// collide onto the same key by concatenation.
func findingSortKey(f doctor.Finding) string {
	var evPath, evField string
	if len(f.Evidence) > 0 {
		evPath = f.Evidence[0].Path
		evField = f.Evidence[0].Field
	}
	return strings.Join([]string{f.Project, string(f.Code), evPath, evField, f.Message}, "\x00")
}

// summarizeFindings tallies findings per severity for the report Summary. The
// per-severity counts include waived findings (they stay in the report at full
// severity), while Waived counts the subset carrying WaiverProvenance — the same
// tally the frozen doctor.ValidateReport recomputes, so the summary round-trips.
func summarizeFindings(findings []doctor.Finding) doctor.Summary {
	s := doctor.Summary{Findings: len(findings)}
	for _, f := range findings {
		switch f.Severity {
		case doctor.SeverityInfo:
			s.Info++
		case doctor.SeverityWarning:
			s.Warning++
		case doctor.SeverityHigh:
			s.High++
		case doctor.SeverityCritical:
			s.Critical++
		}
		if f.WaivedBy != nil {
			s.Waived++
		}
	}
	return s
}

// doctorError maps a report to the command result: a report with at least one
// UNWAIVED high or critical finding is an exit-2 configuration error with the
// report attached for the structured failure envelope; anything else (clean,
// only info/warning findings, or blocking findings that a live waiver
// suppressed) is a nil error and exit 0. A waived high/critical stays in the
// report at full severity (and in Summary.High/Critical) but no longer blocks,
// so blocking is counted from the findings rather than the per-severity summary.
func doctorError(report doctor.Report) error {
	blocking := 0
	for _, f := range report.Findings {
		if f.WaivedBy != nil {
			continue
		}
		if f.Severity == doctor.SeverityHigh || f.Severity == doctor.SeverityCritical {
			blocking++
		}
	}
	if blocking == 0 {
		return nil
	}
	err := protocolcli.Classify(
		fmt.Errorf("doctor found %d production-readiness issue(s) that block the %s profile", blocking, report.Profile),
		protocolcli.ErrInvalidConfig)
	return shared.WithResultData(err, report)
}

// selectDoctorProjects resolves the projects doctor evaluates. An empty, "*", or
// "[impacted]" selector evaluates every workspace project (doctor is a read-only
// advisory scan, so the whole workspace is safe to walk); otherwise the
// comma-separated selector list is resolved to specific projects, and an
// unresolvable entry is a not-found error.
func selectDoctorProjects(ws *workspace.Workspace, args []string, globalProjects string) ([]*workspace.Project, error) {
	selector, err := shared.ParseProjectFlag(args)
	if err != nil {
		return nil, err
	}
	if selector == "" {
		selector = strings.TrimSpace(globalProjects)
	}
	if selector == "" || selector == "*" || selector == "[impacted]" {
		return append([]*workspace.Project(nil), ws.Projects...), nil
	}

	var out []*workspace.Project
	seen := map[string]bool{}
	for _, sel := range strings.Split(selector, ",") {
		sel = strings.TrimSpace(sel)
		if sel == "" {
			continue
		}
		proj := shared.ResolveProjectSelector(ws, sel)
		if proj == nil {
			return nil, cmderr.NotFoundf("project not found: %s", sel)
		}
		if !seen[proj.ID] {
			seen[proj.ID] = true
			out = append(out, proj)
		}
	}
	if len(out) == 0 {
		return nil, cmderr.NotFoundf("no projects matched: %s", selector)
	}
	return out, nil
}

// renderDoctor writes the doctor result to out. In structured mode it emits the
// success v2 envelope when there is no blocking finding and emits nothing when
// there is (the dispatcher writes the failure envelope with the report attached
// via WithResultData). In human mode it prints a summary either way. runErr is
// returned unchanged so the dispatcher maps the process exit code.
//
// The writer is a parameter rather than os.Stdout so the envelope this command
// actually emits is what its test reads. A test that rebuilt the document beside
// this function proved only that the test could build one — which is how
// doctor's envelope kept asserting the retired v1 shape after a later change moved
// the command to v2.
func renderDoctor(out io.Writer, outputFormat string, report doctor.Report, runErr error) error {
	if protocolcli.OutputMode(outputFormat).IsStructured() {
		if runErr == nil {
			_, _ = protocolcli.WriteResultV2(out, protocolcli.OutputJSONL,
				protocolcli.NewResultV2("doctor", report, nil))
		}
		return runErr
	}
	printDoctorHuman(out, report)
	return runErr
}

// printDoctorHuman prints a readable summary of a doctor report: the evaluated
// profile, one entry per finding (severity, code, project, field, message, and
// the fix), and a tally.
func printDoctorHuman(w io.Writer, report doctor.Report) {
	iox.Fprintf(w, "\n  Doctor · profile %s\n", report.Profile)
	if len(report.Findings) == 0 {
		iox.Fprintln(w, "  No production-readiness issues found.")
		iox.Fprintln(w)
		return
	}
	for _, f := range report.Findings {
		field := ""
		if len(f.Evidence) > 0 && f.Evidence[0].Field != "" {
			field = " " + f.Evidence[0].Field
		}
		iox.Fprintf(w, "    [%s] %s %s%s\n", f.Severity, f.Code, f.Project, field)
		iox.Fprintf(w, "      %s\n", f.Message)
		if f.Remediation != "" {
			iox.Fprintf(w, "      fix: %s\n", f.Remediation)
		}
	}
	s := report.Summary
	iox.Fprintf(w, "\n  %d finding(s): %d critical · %d high · %d warning · %d info\n\n",
		s.Findings, s.Critical, s.High, s.Warning, s.Info)
}
