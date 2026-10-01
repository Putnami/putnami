package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// waiverApp is the single-project fixture the waiver roots share: an "app"
// project whose committed config schema requires server.port with no config
// source, so each waivers-* root yields exactly one
// doctor.missing_required_config finding (high under production) to waive.
func waiverApp() *workspace.Project { return &workspace.Project{ID: "/app", Path: "app"} }

// atClock parses an RFC3339 instant into a pinned test clock, failing on a bad
// literal so a typo can never silently shift an expiry boundary. Every waiver
// test pins its clock through this helper — never real time.Now().
func atClock(t *testing.T, ts string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("bad test clock %q: %v", ts, err)
	}
	return parsed
}

// findingByCode returns the single finding with code, failing when the count is
// not exactly one.
func findingByCode(t *testing.T, findings []doctor.Finding, code doctor.CheckCode) doctor.Finding {
	t.Helper()
	got := findingsByCode(findings)[code]
	if len(got) != 1 {
		t.Fatalf("want exactly one %s finding, got %d: %#v", code, len(got), findings)
	}
	return got[0]
}

// countCode returns how many findings carry code.
func countCode(findings []doctor.Finding, code doctor.CheckCode) int {
	return len(findingsByCode(findings)[code])
}

// assertValidReport pins cross-contract parity: every report the waiver engine
// emits (waived findings, hygiene findings, and their Summary — including
// Summary.Waived) must round-trip through the frozen doctor.ValidateReport, so
// the CLI producer never drifts from the protocol the TS/Go consumers validate.
func assertValidReport(t *testing.T, report doctor.Report) {
	t.Helper()
	if diags := doctor.ValidateReport(&report); diag.HasErrors(diags) {
		t.Fatalf("report failed the frozen doctor.ValidateReport: %v", diags)
	}
}

// TestDoctorWaivers_LiveSuppresses: a live, structurally valid waiver over the
// only blocking finding suppresses it (records provenance, counts it in
// Summary.Waived, clears the gate) without dropping it from the report. It also
// exercises path-form waiver scope ("app" → "/app").
func TestDoctorWaivers_LiveSuppresses(t *testing.T) {
	report, err := DoctorRun("testdata/doctor/waivers-live", []*workspace.Project{waiverApp()}, doctor.ProfileProduction, atClock(t, "2027-01-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("a live waiver over the only blocking finding must clear the gate (exit 0), got: %v", err)
	}
	assertValidReport(t, report)

	f := findingByCode(t, report.Findings, doctor.CheckMissingRequiredConfig)
	if f.WaivedBy == nil {
		t.Fatal("waived finding must carry WaivedBy provenance and still be present in the report")
	}
	if f.WaivedBy.Owner != "platform-team" || f.WaivedBy.Reason == "" || f.WaivedBy.Expires != "2099-01-01" {
		t.Errorf("provenance = %#v, want owner platform-team / non-empty reason / expires 2099-01-01", f.WaivedBy)
	}
	if f.Severity != doctor.SeverityHigh {
		t.Errorf("a waived finding keeps its full severity, got %s", f.Severity)
	}
	if report.Summary.Waived != 1 {
		t.Errorf("Summary.Waived = %d, want 1", report.Summary.Waived)
	}
	if report.Summary.High != 1 {
		t.Errorf("Summary.High = %d, want 1 (waived findings stay in the severity tally)", report.Summary.High)
	}
	if n := countCode(report.Findings, doctor.CheckWaiverExpired) + countCode(report.Findings, doctor.CheckWaiverUnknownCode); n != 0 {
		t.Errorf("a live waiver must not emit a hygiene finding, got %d", n)
	}
}

// TestDoctorWaivers_ExpiredStillBlocks: an expired waiver never suppresses its
// finding (fail closed) and surfaces a doctor.waiver_expired hygiene finding of
// its own; the underlying finding stays at full severity and still blocks.
func TestDoctorWaivers_ExpiredStillBlocks(t *testing.T) {
	report, err := DoctorRun("testdata/doctor/waivers-expired", []*workspace.Project{waiverApp()}, doctor.ProfileProduction, atClock(t, "2027-01-01T00:00:00Z"))
	if err == nil {
		t.Fatal("an expired waiver must not suppress its finding; the gate must still block (exit 2)")
	}
	if got := protocolcli.ExitCodeForError(err); got != protocolcli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", got, protocolcli.ExitUsage)
	}
	assertValidReport(t, report)

	f := findingByCode(t, report.Findings, doctor.CheckMissingRequiredConfig)
	if f.WaivedBy != nil {
		t.Fatalf("an expired waiver must NOT waive: %#v", f.WaivedBy)
	}
	exp := findingByCode(t, report.Findings, doctor.CheckWaiverExpired)
	if exp.Project != "/app" {
		t.Errorf("waiver_expired project = %q, want /app", exp.Project)
	}
	if exp.Severity != doctor.SeverityHigh {
		t.Errorf("waiver_expired severity = %s, want high under production", exp.Severity)
	}
	if exp.Evidence[0].Path != doctor.WaiverFilename {
		t.Errorf("waiver_expired evidence path = %q, want %q", exp.Evidence[0].Path, doctor.WaiverFilename)
	}
	if exp.Remediation != doctor.Remediation(doctor.CheckWaiverExpired) {
		t.Error("waiver_expired must stamp the baked remediation")
	}
	if report.Summary.Waived != 0 {
		t.Errorf("Summary.Waived = %d, want 0", report.Summary.Waived)
	}
}

// TestDoctorWaivers_UnknownCode: a waiver naming a code outside the taxonomy
// becomes a doctor.waiver_unknown_code finding and matches nothing, so the real
// finding it never covered still blocks.
func TestDoctorWaivers_UnknownCode(t *testing.T) {
	report, err := DoctorRun("testdata/doctor/waivers-unknown", []*workspace.Project{waiverApp()}, doctor.ProfileProduction, atClock(t, "2027-01-01T00:00:00Z"))
	if err == nil {
		t.Fatal("an unknown-code waiver suppresses nothing; the underlying finding must still block")
	}
	assertValidReport(t, report)

	unknown := findingByCode(t, report.Findings, doctor.CheckWaiverUnknownCode)
	if unknown.Evidence[0].Field != "doctor.not_a_real_check" {
		t.Errorf("unknown-code evidence field = %q, want the bogus code doctor.not_a_real_check", unknown.Evidence[0].Field)
	}
	if unknown.Project != "/app" {
		t.Errorf("unknown-code project = %q, want /app", unknown.Project)
	}
	if unknown.Remediation != doctor.Remediation(doctor.CheckWaiverUnknownCode) {
		t.Error("waiver_unknown_code must stamp the baked remediation")
	}
	if f := findingByCode(t, report.Findings, doctor.CheckMissingRequiredConfig); f.WaivedBy != nil {
		t.Fatal("an unknown-code waiver must not waive a real finding")
	}
	if report.Summary.Waived != 0 {
		t.Errorf("Summary.Waived = %d, want 0", report.Summary.Waived)
	}
}

// TestDoctorWaivers_FieldScopedSuppressesOnlyItsField: a waiver that names a
// field suppresses only the finding for that field, so a sibling finding of the
// SAME code (here a critical missing secret) the author never named keeps
// blocking. This is the fail-closed fix for over-broad (code, project) waivers.
func TestDoctorWaivers_FieldScopedSuppressesOnlyItsField(t *testing.T) {
	report, err := DoctorRun("testdata/doctor/waivers-field-scoped", []*workspace.Project{waiverApp()}, doctor.ProfileProduction, atClock(t, "2027-01-01T00:00:00Z"))
	if err == nil {
		t.Fatal("a server.port-scoped waiver must not clear the sibling critical database.password; the gate must still block")
	}
	if got := protocolcli.ExitCodeForError(err); got != protocolcli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", got, protocolcli.ExitUsage)
	}
	assertValidReport(t, report)

	byField := map[string]doctor.Finding{}
	for _, f := range findingsByCode(report.Findings)[doctor.CheckMissingRequiredConfig] {
		byField[f.Evidence[0].Field] = f
	}
	if port, ok := byField["server.port"]; !ok || port.WaivedBy == nil {
		t.Fatalf("server.port must be waived by its field-scoped waiver: %#v", port)
	}
	if pw, ok := byField["database.password"]; !ok || pw.WaivedBy != nil {
		t.Fatalf("database.password must NOT be waived by a server.port-scoped waiver: %#v", pw)
	}
	if report.Summary.Waived != 1 {
		t.Errorf("Summary.Waived = %d, want 1", report.Summary.Waived)
	}
	if report.Summary.Critical != 1 {
		t.Errorf("Summary.Critical = %d, want 1 (the unwaived sensitive missing config still blocks)", report.Summary.Critical)
	}
}

// TestDoctorWaivers_FieldlessWaivesEverySameCodeFinding: a waiver with no field
// keeps the deliberate broad behavior — it accepts every finding of its code in
// the project — so the existing (code, project) form is unchanged.
func TestDoctorWaivers_FieldlessWaivesEverySameCodeFinding(t *testing.T) {
	report, err := DoctorRun("testdata/doctor/waivers-broad-code", []*workspace.Project{waiverApp()}, doctor.ProfileProduction, atClock(t, "2027-01-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("a field-less waiver accepts every missing-config finding, clearing the gate: %v", err)
	}
	assertValidReport(t, report)
	for _, f := range findingsByCode(report.Findings)[doctor.CheckMissingRequiredConfig] {
		if f.WaivedBy == nil {
			t.Errorf("field-less waiver must suppress %s", f.Evidence[0].Field)
		}
	}
	if report.Summary.Waived != 2 {
		t.Errorf("Summary.Waived = %d, want 2", report.Summary.Waived)
	}
}

// TestDoctorWaivers_MalformedFailsClosed: a waiver file with a non-check-code
// structural defect (here, a missing owner) is untrustworthy, so NO waiver is
// applied and the run fails closed with a blocking ErrInvalidConfig — even under
// dev, where the underlying findings alone would not block. This is the
// load-bearing safety invariant: a broken file never grants a silent exception.
func TestDoctorWaivers_MalformedFailsClosed(t *testing.T) {
	for _, profile := range []doctor.Profile{doctor.ProfileProduction, doctor.ProfileDev} {
		report, err := DoctorRun("testdata/doctor/waivers-malformed", []*workspace.Project{waiverApp()}, profile, atClock(t, "2027-01-01T00:00:00Z"))
		if err == nil {
			t.Fatalf("profile %s: a malformed waiver file must fail closed (exit 2), even when findings alone would not block", profile)
		}
		if !errors.Is(err, protocolcli.ErrInvalidConfig) {
			t.Errorf("profile %s: malformed waiver error must classify as ErrInvalidConfig, got %v", profile, err)
		}
		if got := protocolcli.ExitCodeForError(err); got != protocolcli.ExitUsage {
			t.Errorf("profile %s: exit code = %d, want %d", profile, got, protocolcli.ExitUsage)
		}
		if !strings.Contains(err.Error(), doctor.WaiverFilename) {
			t.Errorf("profile %s: error message must name %s, got %q", profile, doctor.WaiverFilename, err.Error())
		}
		// The report rides along on the error for the structured failure envelope.
		if _, ok := shared.ResultData(err).(doctor.Report); !ok {
			t.Errorf("profile %s: malformed error must carry the report as ResultData, got %#v", profile, shared.ResultData(err))
		}
		// Fail closed: nothing waived, and no hygiene finding derived from the
		// tainted file.
		if report.Summary.Waived != 0 {
			t.Errorf("profile %s: a broken file must not waive anything, Waived = %d", profile, report.Summary.Waived)
		}
		for _, f := range report.Findings {
			if f.WaivedBy != nil {
				t.Errorf("profile %s: no finding may be waived by a broken file: %#v", profile, f)
			}
			if f.Code == doctor.CheckWaiverExpired || f.Code == doctor.CheckWaiverUnknownCode {
				t.Errorf("profile %s: a broken file must not derive hygiene findings: %#v", profile, f)
			}
		}
	}
}

// TestDoctorWaivers_UnreadableFailsClosed covers the other half of the gate's
// fail-closed rule. A waiver file that PARSES badly is malformed; one that
// cannot be READ at all makes the gate unavailable — it cannot know which
// findings an operator already accepted. loadWaivers refuses in that case
// ("refusing to run the gate without its waivers"), and nothing proved it: an
// I/O error silently degrading to "no waivers" leaves the run gated on a
// picture the operator never authored.
//
// The unreadable file is a DIRECTORY at the waiver path rather than a
// chmod-ed file, so the test behaves the same when the suite runs as root.
func TestDoctorWaivers_UnreadableFailsClosed(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "doctor-gate", "production-blocks-unwaived-and-fails-closed")

	for _, profile := range []doctor.Profile{doctor.ProfileProduction, doctor.ProfileDev} {
		wsRoot := t.TempDir()
		if err := os.Mkdir(filepath.Join(wsRoot, doctor.WaiverFilename), 0o755); err != nil {
			t.Fatal(err)
		}

		report, err := DoctorRun(wsRoot, []*workspace.Project{waiverApp()}, profile, atClock(t, "2027-01-01T00:00:00Z"))
		if err == nil {
			t.Fatalf("profile %s: an unreadable %s must fail closed, not degrade to running with no waivers", profile, doctor.WaiverFilename)
		}
		if !errors.Is(err, protocolcli.ErrInvalidConfig) {
			t.Errorf("profile %s: unreadable waiver error must classify as ErrInvalidConfig, got %v", profile, err)
		}
		if !strings.Contains(err.Error(), doctor.WaiverFilename) {
			t.Errorf("profile %s: error must name %s, got %q", profile, doctor.WaiverFilename, err.Error())
		}
		if report.Summary.Waived != 0 {
			t.Errorf("profile %s: an unreadable file must not waive anything, Waived = %d", profile, report.Summary.Waived)
		}
	}
}

// TestDoctorWaivers_DateOnlyBoundary pins the end-of-day UTC rule for a
// date-only expires (YYYY-MM-DD): the waiver stays live through the whole named
// day and lapses at the following midnight UTC. It also exercises workspace-wide
// scope (empty project matches /app) and the workspace-scope value on the
// resulting hygiene finding.
func TestDoctorWaivers_DateOnlyBoundary(t *testing.T) {
	tests := []struct {
		name   string
		clock  string
		waived bool
	}{
		{"end-of-day still live", "2027-06-15T23:59:59Z", true},
		{"next midnight lapses", "2027-06-16T00:00:00Z", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := DoctorRun("testdata/doctor/waivers-dateonly", []*workspace.Project{waiverApp()}, doctor.ProfileProduction, atClock(t, tt.clock))
			assertValidReport(t, report)
			f := findingByCode(t, report.Findings, doctor.CheckMissingRequiredConfig)
			if tt.waived {
				if f.WaivedBy == nil {
					t.Fatal("date-only waiver must stay live through end-of-day UTC")
				}
				if err != nil {
					t.Errorf("a live waiver clears the gate, got err: %v", err)
				}
				if report.Summary.Waived != 1 {
					t.Errorf("Summary.Waived = %d, want 1", report.Summary.Waived)
				}
				if n := countCode(report.Findings, doctor.CheckWaiverExpired); n != 0 {
					t.Errorf("a live date-only waiver must not emit waiver_expired, got %d", n)
				}
				return
			}
			if f.WaivedBy != nil {
				t.Fatal("date-only waiver must lapse at the following midnight UTC")
			}
			if err == nil {
				t.Error("a lapsed waiver must leave the gate blocking")
			}
			exp := findingByCode(t, report.Findings, doctor.CheckWaiverExpired)
			if exp.Project != doctorWorkspaceScope {
				t.Errorf("workspace-wide waiver_expired project = %q, want %q", exp.Project, doctorWorkspaceScope)
			}
		})
	}
}

// TestWaiverExpired pins the clock-comparison rule directly for both RFC3339
// timestamps (lapse AT the instant) and date-only values (lapse at end-of-day
// UTC = the following midnight), plus the fail-closed treatment of an
// unparsable value.
func TestWaiverExpired(t *testing.T) {
	tests := []struct {
		name    string
		expires string
		now     string
		expired bool
	}{
		{"timestamp future", "2027-12-31T00:00:00Z", "2027-06-15T12:00:00Z", false},
		{"timestamp past", "2027-01-01T00:00:00Z", "2027-06-15T12:00:00Z", true},
		{"timestamp exact instant lapses", "2027-06-15T12:00:00Z", "2027-06-15T12:00:00Z", true},
		{"timestamp one second before", "2027-06-15T12:00:01Z", "2027-06-15T12:00:00Z", false},
		{"date-only midday is live", "2027-06-15", "2027-06-15T12:00:00Z", false},
		{"date-only end of day is live", "2027-06-15", "2027-06-15T23:59:59Z", false},
		{"date-only next midnight lapses", "2027-06-15", "2027-06-16T00:00:00Z", true},
		{"unparsable fails closed", "not-a-date", "2027-06-15T12:00:00Z", true},
	}
	for _, tt := range tests {
		if got := waiverExpired(tt.expires, atClock(t, tt.now)); got != tt.expired {
			t.Errorf("%s: waiverExpired(%q, %q) = %v, want %v", tt.name, tt.expires, tt.now, got, tt.expired)
		}
	}
}

// TestDoctorWaivers_Deterministic asserts a waiver run is byte-stable across
// repeated evaluations of the same committed tree, profile, and injected clock:
// waiver matching, the hygiene finding, and the Summary are a pure function of
// those inputs — no map iteration or wall-clock leaks in.
func TestDoctorWaivers_Deterministic(t *testing.T) {
	clock := atClock(t, "2027-01-01T00:00:00Z")
	first, _ := DoctorRun("testdata/doctor/waivers-expired", []*workspace.Project{waiverApp()}, doctor.ProfileProduction, clock)
	want, err := json.MarshalIndent(first, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := range 20 {
		got, _ := DoctorRun("testdata/doctor/waivers-expired", []*workspace.Project{waiverApp()}, doctor.ProfileProduction, clock)
		data, _ := json.MarshalIndent(got, "", "  ")
		if !bytes.Equal(data, want) {
			t.Fatalf("iteration %d not byte-stable:\n--- got:\n%s\n--- want:\n%s", i, data, want)
		}
	}
}
