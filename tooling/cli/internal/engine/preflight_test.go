package engine

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	doctor "go.putnami.dev/protocol/doctor"
	doctorcmd "go.putnami.dev/tooling/cli/internal/commands/doctor"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// gateFixtureSchema is a committed config schema whose only field is a REQUIRED,
// SENSITIVE value with no committed source, so under the production profile it
// surfaces as a single critical doctor.missing_required_config finding — the
// "unwaived critical finding" the gate must block on.
const gateFixtureSchema = `{
  "appName": "go.putnami.dev/example/gate",
  "version": "0.1.0",
  "configs": [
    {
      "path": "database",
      "fields": [
        { "name": "password", "type": "string", "required": true, "sensitive": true }
      ]
    }
  ]
}`

// gateLiveWaiver waives the critical finding for the fixture project with a live
// (far-future) expiry, so the same tree passes the gate.
const gateLiveWaiver = `{
  "protocolVersion": 1,
  "waivers": [
    {
      "code": "doctor.missing_required_config",
      "project": "svc",
      "owner": "platform-team",
      "reason": "database.password is injected by the platform at deploy time; OPS-1234",
      "expires": "2099-01-01"
    }
  ]
}`

// gateMalformedWaiver is structurally invalid (no owner). Per
// TestDoctorWaivers_MalformedFailsClosed the engine fails CLOSED on it under
// EVERY profile — but only if the engine actually runs.
const gateMalformedWaiver = `{
  "protocolVersion": 1,
  "waivers": [
    {
      "code": "doctor.missing_required_config",
      "project": "svc",
      "reason": "no owner field: structurally invalid",
      "expires": "2099-01-01"
    }
  ]
}`

// writeGatePreflightFixture creates a one-project workspace rooted at a temp dir:
// the "svc" project commits gateFixtureSchema (a critical missing-config gap) and
// no production config source. waivers, when non-empty, is written as the
// workspace-root doctor.waivers.json. It returns the workspace root and the
// selected project set the gate evaluates.
func writeGatePreflightFixture(t *testing.T, waivers string) (string, []*workspace.Project) {
	t.Helper()
	wsRoot := t.TempDir()
	schemaDir := filepath.Join(wsRoot, "svc", "schema")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(schemaDir, "config.json"), []byte(gateFixtureSchema), 0o644); err != nil {
		t.Fatal(err)
	}
	if waivers != "" {
		if err := os.WriteFile(filepath.Join(wsRoot, doctor.WaiverFilename), []byte(waivers), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return wsRoot, []*workspace.Project{{ID: "/svc", Name: "svc", Path: "svc"}}
}

// TestRunProductionPreflight_BlocksUnwaivedCritical: a production build over a
// workspace with an unwaived critical finding is blocked with exit 2 BEFORE any
// job runs (runProductionPreflight is called between plan and execute, and a
// blocked result short-circuits execution).
func TestRunProductionPreflight_BlocksUnwaivedCritical(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "doctor-gate", "production-blocks-unwaived-and-fails-closed")
	wsRoot, projects := writeGatePreflightFixture(t, "")
	req := &Request{WorkspaceRoot: wsRoot, Commands: []string{"build"}, Preflight: doctorcmd.DoctorPreflight}
	req.Global.EnvProfile = "production"
	req.Global.Output = "jsonl" // route the report to the injected stdout seam
	req.Stdout = io.Discard

	code, blocked := runProductionPreflight(req, projects)
	if !blocked {
		t.Fatal("an unwaived critical finding must block the production build")
	}
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
}

// TestRunProductionPreflight_WaivedPasses: the SAME workspace passes once the
// finding is waived by a live workspace-root waiver.
func TestRunProductionPreflight_WaivedPasses(t *testing.T) {
	t.Parallel()
	wsRoot, projects := writeGatePreflightFixture(t, gateLiveWaiver)
	req := &Request{WorkspaceRoot: wsRoot, Commands: []string{"build"}, Preflight: doctorcmd.DoctorPreflight}
	req.Global.EnvProfile = "production"

	code, blocked := runProductionPreflight(req, projects)
	if blocked {
		t.Fatalf("a live waiver must clear the gate; got blocked with code %d", code)
	}
	if code != ExitSuccess {
		t.Fatalf("exit code = %d, want %d", code, ExitSuccess)
	}
}

// TestRunProductionPreflight_DevAndTestNeverInvokeGate is the zero-config-local
// guarantee: the gate is a COMPLETE no-op for dev/test. The workspace carries a
// MALFORMED waiver file that would fail CLOSED under every profile *if the engine
// ran* — so a dev/test build that stays unblocked over it proves the engine was
// never invoked. TestRunProductionPreflight_ProductionMalformedWaiverFailsClosed
// is the control that the same file does block under production.
func TestRunProductionPreflight_DevAndTestNeverInvokeGate(t *testing.T) {
	t.Parallel()
	wsRoot, projects := writeGatePreflightFixture(t, gateMalformedWaiver)

	// "" is the pre-resolution default; "dev" is the resolved default; "test" is
	// the CI profile. None may gate.
	for _, profile := range []string{"", "dev", "test"} {
		req := &Request{WorkspaceRoot: wsRoot, Commands: []string{"build"}, Preflight: doctorcmd.DoctorPreflight}
		req.Global.EnvProfile = profile
		code, blocked := runProductionPreflight(req, projects)
		if blocked {
			t.Fatalf("profile %q must never invoke the gate (a malformed waiver would fail closed if it did); got blocked code %d", profile, code)
		}
		if code != ExitSuccess {
			t.Fatalf("profile %q exit code = %d, want %d", profile, code, ExitSuccess)
		}
	}
}

// TestRunProductionPreflight_ProductionMalformedWaiverFailsClosed is the control
// for the dev/test no-op test: under production the SAME malformed waiver file
// DOES fail closed and block, so the dev/test pass above is a genuine
// "engine never ran" signal.
func TestRunProductionPreflight_ProductionMalformedWaiverFailsClosed(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "doctor-gate", "production-blocks-unwaived-and-fails-closed")
	wsRoot, projects := writeGatePreflightFixture(t, gateMalformedWaiver)
	req := &Request{WorkspaceRoot: wsRoot, Commands: []string{"build"}, Preflight: doctorcmd.DoctorPreflight}
	req.Global.EnvProfile = "production"
	req.Global.Output = "jsonl"
	req.Stdout = io.Discard

	code, blocked := runProductionPreflight(req, projects)
	if !blocked {
		t.Fatal("a malformed waiver file must fail closed and block under production")
	}
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
}

// TestRunProductionPreflight_DryRunNeverGates: --dry-run is a plan preview, never
// an execution, so it never gates even under production over a blocking tree.
func TestRunProductionPreflight_DryRunNeverGates(t *testing.T) {
	t.Parallel()
	wsRoot, projects := writeGatePreflightFixture(t, "")
	req := &Request{WorkspaceRoot: wsRoot, Commands: []string{"build"}, Preflight: doctorcmd.DoctorPreflight}
	req.Global.EnvProfile = "production"
	req.Global.DryRun = true

	code, blocked := runProductionPreflight(req, projects)
	if blocked {
		t.Fatalf("--dry-run must not gate; got blocked code %d", code)
	}
	if code != ExitSuccess {
		t.Fatalf("exit code = %d, want %d", code, ExitSuccess)
	}
}

// TestRenderProductionPreflightFailure_StructuredCarriesReport asserts the
// structured (JSONL) render emits a single failure envelope on stdout that
// carries the doctor report as data and maps to exit 2 — the machine-readable
// contract an agent or deploy pipeline consumes.
func TestRenderProductionPreflightFailure_StructuredCarriesReport(t *testing.T) {
	t.Parallel()
	wsRoot, projects := writeGatePreflightFixture(t, "")
	report, gateErr := doctorcmd.DoctorPreflight(wsRoot, projects, doctor.ProfileProduction)
	if gateErr == nil {
		t.Fatal("fixture must block under production")
	}

	var buf strings.Builder
	renderProductionPreflightFailure(&buf, io.Discard, "jsonl", report, gateErr)
	out := buf.String()
	if n := strings.Count(strings.TrimSpace(out), "\n"); n != 0 {
		t.Fatalf("structured render must be a single JSONL line, got %d newlines:\n%s", n, out)
	}
	var env struct {
		Command  string        `json:"command"`
		Status   string        `json:"status"`
		ExitCode int           `json:"exitCode"`
		Data     doctor.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("parse envelope: %v\noutput: %s", err, out)
	}
	if env.Status != protocolcli.StatusFailure || env.ExitCode != protocolcli.ExitUsage {
		t.Fatalf("envelope = %+v, want failure/exit 2", env)
	}
	if env.Data.Summary.Findings != report.Summary.Findings || env.Data.Summary.Critical < 1 {
		t.Fatalf("envelope data must carry the report with the critical finding: %+v", env.Data)
	}
}

// TestRenderProductionPreflightFailure_HumanListsBlockingFindings asserts the
// human render names the blocking finding (code + field) on the error writer, so
// an operator sees exactly what to fix or waive.
func TestRenderProductionPreflightFailure_HumanListsBlockingFindings(t *testing.T) {
	t.Parallel()
	wsRoot, projects := writeGatePreflightFixture(t, "")
	report, gateErr := doctorcmd.DoctorPreflight(wsRoot, projects, doctor.ProfileProduction)
	if gateErr == nil {
		t.Fatal("fixture must block under production")
	}

	var errBuf strings.Builder
	renderProductionPreflightFailure(io.Discard, &errBuf, "", report, gateErr)
	got := errBuf.String()
	if !strings.Contains(got, string(doctor.CheckMissingRequiredConfig)) {
		t.Errorf("human render must name the blocking check code, got:\n%s", got)
	}
	if !strings.Contains(got, "database.password") {
		t.Errorf("human render must name the blocking field, got:\n%s", got)
	}
	if !strings.Contains(got, doctor.WaiverFilename) {
		t.Errorf("human render must point at %s, got:\n%s", doctor.WaiverFilename, got)
	}
}

// TestRunProductionPreflight_NilGateFailsClosed is the safety property of
// making the gate injectable. The gate is now INJECTED, so "the adapter forgot
// to wire it" is a reachable state — and it must not be the state in which a
// production build silently runs ungated. A nil gate blocks with the same exit 2
// a real finding does, over a workspace that is otherwise CLEAN, so the refusal
// cannot be mistaken for a finding.
func TestRunProductionPreflight_NilGateFailsClosed(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "doctor-gate", "production-blocks-unwaived-and-fails-closed")
	wsRoot := t.TempDir()
	projects := []*workspace.Project{{ID: "/svc", Name: "svc", Path: "svc"}}

	req := &Request{WorkspaceRoot: wsRoot, Commands: []string{"build"}}
	req.Global.EnvProfile = "production"
	req.Global.Output = "jsonl" // route the report to the injected stdout seam

	var buf strings.Builder
	req.Stdout = &buf
	code, blocked := runProductionPreflight(req, projects)
	out := buf.String()
	if !blocked {
		t.Fatal("an unwired preflight gate must block a production run, never pass it through ungated")
	}
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(out, "not wired") {
		t.Errorf("the failure must say the gate was not wired, so it is not read as a workspace finding:\n%s", out)
	}
}

// TestRunProductionPreflight_NilGateIsInertOutsideProduction: the fail-closed
// rule above applies ONLY where the gate runs. dev and test never invoke it, so
// an adapter that legitimately passes nil (none does today) still gets the
// zero-config local behavior.
func TestRunProductionPreflight_NilGateIsInertOutsideProduction(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projects := []*workspace.Project{{ID: "/svc", Name: "svc", Path: "svc"}}

	for _, profile := range []string{"", "dev", "test"} {
		req := &Request{WorkspaceRoot: wsRoot, Commands: []string{"build"}}
		req.Global.EnvProfile = profile
		code, blocked := runProductionPreflight(req, projects)
		if blocked {
			t.Fatalf("profile %q must not be gated at all; got blocked code %d", profile, code)
		}
	}
}
