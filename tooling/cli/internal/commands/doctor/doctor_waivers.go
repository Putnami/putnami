package doctor

// Waiver evaluation for `putnami doctor`.
//
// A waiver is a committed, owned, expiring, reviewed acceptance of a doctor
// finding, recorded in the workspace-root doctor.waivers.json (the frozen
// go.putnami.dev/protocol/doctor.WaiverFile shape). This file is the CLI engine
// that turns that committed data into report outcomes; the wire contract and
// its strict structural parse/validate live in protocols/doctor and are
// consumed here read-only.
//
// The load-bearing invariant is FAIL CLOSED: an invalid, malformed, or expired
// waiver must NEVER silently weaken the gate. Concretely:
//
//   - only a structurally valid AND unexpired waiver suppresses a finding;
//   - an expired waiver becomes a doctor.waiver_expired finding and its target
//     stays at full severity (still blocks);
//   - a waiver naming an unknown check code becomes a doctor.waiver_unknown_code
//     finding and matches nothing;
//   - any OTHER structural defect (missing owner/reason, bad expires syntax,
//     duplicate, wrong protocolVersion, unparsable JSON) makes the whole file
//     untrustworthy, so NO waiver is applied and the run fails closed with a
//     blocking protocolcli.ErrInvalidConfig error (the same exit-2 shape
//     doctorError uses), the report attached for the failure envelope.
//
// Expiry is evaluated here, not in the protocol package: the injected clock
// (DoctorRun's now) keeps the decision deterministic and testable.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
)

// doctorWorkspaceScope is the Finding.Project value stamped on a waiver-hygiene
// finding derived from a workspace-wide waiver (empty Waiver.Project): the
// workspace root, whose canonical project id is "/". Findings must name a
// non-empty project (doctor.ValidateReport), so a workspace-wide waiver still
// needs a concrete, deterministic scope value.
const doctorWorkspaceScope = "/"

// liveWaiver is a structurally valid, unexpired waiver ready to suppress a
// matching finding. projectID is the canonical project id the waiver scopes to
// (resolved from the authored id-or-path form); an empty projectID means the
// waiver applies workspace-wide and matches any project. field, when non-empty,
// narrows the waiver to the single finding whose first evidence field matches,
// so one broad code+project waiver cannot silently suppress a sibling finding
// (e.g. a critical missing secret) the author never named.
type liveWaiver struct {
	code      doctor.CheckCode
	projectID string
	field     string
	owner     string
	reason    string
	expires   string
}

// loadWaivers reads and evaluates the committed workspace-root
// doctor.waivers.json against the injected clock. It returns the live waivers to
// apply, the waiver-hygiene findings to surface (expired / unknown-code), and a
// malformed error when the file is structurally untrustworthy.
//
// Absent file → (nil, nil, nil): no waivers, no findings, a normal run. The file
// is read exactly once at the workspace root, never per project.
//
// Fail-closed classification of the strict structural diagnostics: an
// ErrorCodeInvalidCheckCode diagnostic is NOT fatal — that one waiver can never
// match a finding, so it degrades to a doctor.waiver_unknown_code finding while
// the file's other waivers still apply. Every OTHER structural diagnostic
// (missing owner/reason, invalid-expires SYNTAX, duplicate, bad protocolVersion,
// unparsable JSON) means the file cannot be trusted at all, so loadWaivers
// applies NOTHING and returns a malformed error; the caller turns that into a
// blocking exit-2 result. A file that exists but cannot be read is treated the
// same way (fail closed), since an unreadable gate input is not a clean gate.
func loadWaivers(wsRoot string, projects []*workspace.Project, profile doctor.Profile, now time.Time) (live []liveWaiver, findings []doctor.Finding, malformed error) {
	data, err := os.ReadFile(filepath.Join(wsRoot, doctor.WaiverFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("%s could not be read (%w); refusing to run the gate without its waivers", doctor.WaiverFilename, err)
	}

	wf, diags := doctor.ParseAndValidateWaiverFile(data)

	// An invalid-check-code diagnostic degrades one waiver to a hygiene finding;
	// any other error taints the whole file. Collect the fatal subset.
	var fatal []diag.Diagnostic
	for _, d := range diag.Errors(diags) {
		if d.Code != doctor.ErrorCodeInvalidCheckCode {
			fatal = append(fatal, d)
		}
	}
	if len(fatal) > 0 || wf == nil {
		return nil, nil, malformedWaiverFileError(fatal)
	}

	for _, wv := range wf.Waivers {
		scope := waiverFindingProject(projects, wv.Project)
		switch {
		case !doctor.ValidCheckCodes[wv.Code]:
			findings = append(findings, waiverHygieneFinding(
				doctor.CheckWaiverUnknownCode, scope, profile, wv,
				fmt.Sprintf("waiver references check code %q, which is not a recognized doctor check; it matches no finding", wv.Code)))
		case waiverExpired(wv.Expires, now):
			findings = append(findings, waiverHygieneFinding(
				doctor.CheckWaiverExpired, scope, profile, wv,
				fmt.Sprintf("waiver for check code %q expired on %s; the underlying finding is no longer suppressed", wv.Code, wv.Expires)))
		default:
			live = append(live, liveWaiver{
				code:      wv.Code,
				projectID: normalizeWaiverProject(projects, wv.Project),
				field:     strings.TrimSpace(wv.Field),
				owner:     wv.Owner,
				reason:    wv.Reason,
				expires:   wv.Expires,
			})
		}
	}
	return live, findings, nil
}

// applyLiveWaivers stamps WaiverProvenance onto every finding a live waiver
// matches, in place. A waiver matches a finding when the codes are equal, the
// waiver is workspace-wide (empty projectID) or its resolved project id equals
// the finding's project, AND — when the waiver names a field — its field equals
// the finding's first evidence field. A field-less waiver still matches every
// finding of its code in scope, so an author who wants to accept just one of
// several same-code findings (say a required port, but not a required secret of
// the same code) scopes the waiver to that field and the others keep blocking.
// The waiver's authored project form was already resolved to a canonical id
// (liveWaiver.projectID) in loadWaivers, so project matching here is a pure id
// comparison. A finding is waived at most once (first matching waiver wins); a
// waived finding is never dropped — it stays in the report with its provenance
// and full severity, and is only excluded from the blocking total.
func applyLiveWaivers(findings []doctor.Finding, live []liveWaiver) {
	if len(live) == 0 {
		return
	}
	for i := range findings {
		f := &findings[i]
		if f.WaivedBy != nil {
			continue
		}
		for _, w := range live {
			if w.code != f.Code {
				continue
			}
			if w.projectID != "" && w.projectID != f.Project {
				continue
			}
			if w.field != "" && w.field != findingField(*f) {
				continue
			}
			f.WaivedBy = &doctor.WaiverProvenance{Owner: w.owner, Reason: w.reason, Expires: w.expires}
			break
		}
	}
}

// findingField returns the finding's first evidence field (a config path or
// field name), or "" when it carries none. It is the value a field-scoped
// waiver matches against.
func findingField(f doctor.Finding) string {
	if len(f.Evidence) > 0 {
		return f.Evidence[0].Field
	}
	return ""
}

// waiverExpired reports whether a waiver with the given RFC 3339 expires value
// has lapsed relative to now. A full timestamp lapses AT its instant; a
// date-only value (YYYY-MM-DD) lapses at END OF DAY UTC — it stays live through
// the whole named day and expires at the following midnight UTC. In both cases
// the waiver is expired when now is at or after the lapse instant. An
// unparsable value (unreachable after the protocol's structural validation)
// fails closed: it is treated as expired so a broken date can never grant a
// waiver.
func waiverExpired(expires string, now time.Time) bool {
	s := strings.TrimSpace(expires)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return !now.Before(t)
	}
	if d, err := time.Parse(time.DateOnly, s); err == nil {
		return !now.Before(d.AddDate(0, 0, 1))
	}
	return true
}

// normalizeWaiverProject resolves an authored waiver project (which may be a
// canonical id like "/app" or a workspace-relative path like "app") to the
// canonical project id used by Finding.Project, so a waiver matches regardless
// of which form the author used. An empty scope stays empty (workspace-wide); an
// unresolved scope is returned verbatim so it deterministically matches nothing.
func normalizeWaiverProject(projects []*workspace.Project, waiverProject string) string {
	if strings.TrimSpace(waiverProject) == "" {
		return ""
	}
	for _, p := range projects {
		if waiverProject == p.ID || waiverProject == filepath.ToSlash(p.Path) {
			return p.ID
		}
	}
	return waiverProject
}

// waiverFindingProject is the Finding.Project stamped on a waiver-hygiene
// finding: the waiver's resolved project scope, or the workspace-root scope when
// the waiver is workspace-wide.
func waiverFindingProject(projects []*workspace.Project, waiverProject string) string {
	if id := normalizeWaiverProject(projects, waiverProject); id != "" {
		return id
	}
	return doctorWorkspaceScope
}

// waiverHygieneFinding builds a doctor.waiver_expired or doctor.waiver_unknown_code
// finding for a problematic waiver. Severity is profile-graded exactly like the
// artifact-derived checks (production strict, dev/test permissive), the evidence
// points at the committed waiver file and the offending check code (a name,
// never a resolved value), and the baked remediation is stamped from the frozen
// taxonomy.
func waiverHygieneFinding(code doctor.CheckCode, project string, profile doctor.Profile, wv doctor.Waiver, message string) doctor.Finding {
	return doctor.Finding{
		Code:        code,
		Severity:    severityFor(code, profile, false),
		Profile:     profile,
		Project:     project,
		Message:     message,
		Evidence:    []doctor.Evidence{{Path: doctor.WaiverFilename, Field: string(wv.Code)}},
		Remediation: doctor.Remediation(code),
	}
}

// malformedWaiverFileError builds the fail-closed cause for a structurally
// untrustworthy waiver file. It names the file, the number of blocking defects,
// and the first one, so the reviewer sees why the gate refused to honor any
// waiver.
func malformedWaiverFileError(fatal []diag.Diagnostic) error {
	detail := "it could not be parsed"
	if len(fatal) > 0 {
		detail = fatal[0].String()
	}
	return fmt.Errorf(
		"%s is malformed (%d blocking defect(s)) and cannot be trusted; refusing to apply any waiver so the gate is not silently weakened: %s",
		doctor.WaiverFilename, len(fatal), detail)
}

// waiverMalformedError wraps the fail-closed cause as a blocking, exit-2
// configuration error carrying the report for the structured failure envelope —
// the same protocolcli.ErrInvalidConfig + WithResultData shape
// doctorError uses, so a malformed waiver file blocks even when the underlying
// findings alone would not.
func waiverMalformedError(report doctor.Report, cause error) error {
	return shared.WithResultData(protocolcli.Classify(cause, protocolcli.ErrInvalidConfig), report)
}
