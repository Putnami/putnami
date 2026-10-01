package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	doctor "go.putnami.dev/protocol/doctor"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestDoctorPreflight_ProductionBlocksUnwaived: over a project with an unwaived
// required-config gap, DoctorPreflight blocks under production with an exit-2
// ErrInvalidConfig carrying the report as ResultData — the exact contract the
// build gate and deploy tooling consume.
func TestDoctorPreflight_ProductionBlocksUnwaived(t *testing.T) {
	report, err := DoctorPreflight(doctorTestdataRoot, []*workspace.Project{proj("missing-config")}, doctor.ProfileProduction)
	if err == nil {
		t.Fatal("an unwaived missing required config must block under production")
	}
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Errorf("blocking error must classify as ErrInvalidConfig, got %v", err)
	}
	if got := protocolcli.ExitCodeForError(err); got != protocolcli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", got, protocolcli.ExitUsage)
	}
	data, ok := shared.ResultData(err).(doctor.Report)
	if !ok || data.Summary.Findings != report.Summary.Findings {
		t.Fatalf("blocking error must carry the report as ResultData: %#v", shared.ResultData(err))
	}
	// missing-config commits a required sensitive value (critical) and a required
	// non-sensitive value (high).
	if report.Summary.Critical < 1 || report.Summary.High < 1 {
		t.Fatalf("summary = %#v, want at least one critical and one high", report.Summary)
	}
}

// TestDoctorPreflight_ProductionWaivedPasses: a live workspace-root waiver over
// the only blocking finding clears the gate (nil error) while the finding stays
// in the report with provenance.
func TestDoctorPreflight_ProductionWaivedPasses(t *testing.T) {
	report, err := DoctorPreflight("testdata/doctor/waivers-live", []*workspace.Project{waiverApp()}, doctor.ProfileProduction)
	if err != nil {
		t.Fatalf("a live waiver over the only blocking finding must clear the gate: %v", err)
	}
	if report.Summary.Waived != 1 {
		t.Errorf("Summary.Waived = %d, want 1", report.Summary.Waived)
	}
	f := findingByCode(t, report.Findings, doctor.CheckMissingRequiredConfig)
	if f.WaivedBy == nil {
		t.Error("the waived finding must still be present with WaivedBy provenance")
	}
}

// TestDoctorPreflight_CleanPasses: a clean project blocks nothing under
// production.
func TestDoctorPreflight_CleanPasses(t *testing.T) {
	report, err := DoctorPreflight(doctorTestdataRoot, []*workspace.Project{proj("clean")}, doctor.ProfileProduction)
	if err != nil {
		t.Fatalf("clean project must pass: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("clean findings = %d, want 0", len(report.Findings))
	}
}

// TestDoctorPreflight_DevPermissive: the same blocking project stays advisory and
// never blocks under dev.
func TestDoctorPreflight_DevPermissive(t *testing.T) {
	report, err := DoctorPreflight(doctorTestdataRoot, []*workspace.Project{proj("missing-config")}, doctor.ProfileDev)
	if err != nil {
		t.Fatalf("dev preflight must not block: %v", err)
	}
	if report.Summary.High != 0 || report.Summary.Critical != 0 {
		t.Fatalf("dev summary must have no blocking findings: %#v", report.Summary)
	}
}

// TestDoctorPreflight_IsThinCompositionOverDoctorRun asserts DoctorPreflight adds
// no check or waiver logic of its own: over a tree with no committed waiver file
// (hence no clock dependency), its report is byte-identical to DoctorRun with any
// pinned clock, and their block/pass decisions agree. This pins the "reuse the
// engine as-is; the gate only wires it" constraint.
func TestDoctorPreflight_IsThinCompositionOverDoctorRun(t *testing.T) {
	projects := []*workspace.Project{proj("missing-config"), proj("security-defaults"), proj("clean")}
	preReport, preErr := DoctorPreflight(doctorTestdataRoot, projects, doctor.ProfileProduction)
	runReport, runErr := DoctorRun(doctorTestdataRoot, projects, doctor.ProfileProduction, doctorTestClock)

	pre, _ := json.MarshalIndent(preReport, "", "  ")
	run, _ := json.MarshalIndent(runReport, "", "  ")
	if !bytes.Equal(pre, run) {
		t.Fatalf("DoctorPreflight must match DoctorRun on a clock-independent tree:\n--- preflight:\n%s\n--- run:\n%s", pre, run)
	}
	if (preErr == nil) != (runErr == nil) {
		t.Fatalf("block/pass parity mismatch: preflight err=%v, run err=%v", preErr, runErr)
	}
}
