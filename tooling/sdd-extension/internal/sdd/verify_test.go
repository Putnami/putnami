package sdd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// verifyWorkspace is one spec-owning project whose spec states two textual
// requirements: `issue` joins an authored criterion, `record` joins a
// criterionless requirement, and `ghost` joins nothing.
func verifyWorkspace(t *testing.T, projectOptions map[string]map[string]any) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"verify-test","includes":["app"]}`)
	writeFixtureFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"billing-app","type":"application"}`)
	writeFixtureFile(t, filepath.Join(root, "app", featureproto.ManifestFilename), `{
  "protocolVersion": 2,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoicing",
    "type": "feature",
    "name": "Invoicing",
    "outcome": "Customers receive invoices",
    "owner": "billing",
    "target": "coded",
    "requirements": [
      {"id": "issue", "stage": "coded", "evidenceKinds": ["attestation"],
       "verification": {"kind": "acceptance", "checks": ["issue-check"]}},
      {"id": "record", "stage": "coded", "evidenceKinds": ["artifact"]}
    ]
  }]
}`)
	writeFixtureFile(t, filepath.Join(root, "app", "specs", "invoicing.json"), `{
  "protocolVersion": 1,
  "feature": "billing/invoicing",
  "outcomes": ["Customers receive invoices"],
  "requirements": [
    {"id": "issue", "text": "An invoice is issued."},
    {"id": "record", "text": "Invoices are recorded."},
    {"id": "ghost", "text": "Nothing declares this."}
  ]
}`)
	project := appProject("billing-app", "app")
	project.Config = &wsproto.ProjectConfig{Name: "billing-app", Type: "application", Options: projectOptions}
	return fixtureWorkspace("verify-test", root, project)
}

func enforceOptions() map[string]map[string]any {
	return map[string]map[string]any{"sdd": {"verification": map[string]any{"specs": "enforce"}}}
}

func writeVerifySession(t *testing.T, root, sessionID, record string) {
	t.Helper()
	dir := filepath.Join(root, ".putnami", "sessions", sessionID)
	writeFixtureFile(t, filepath.Join(dir, featureproto.SpecVerificationRecordFilename), record)
	latest := filepath.Join(root, ".putnami", "sessions", "latest")
	_ = os.Remove(latest)
	if err := os.Symlink(dir, latest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func verifySessionRecord(sessionID string, state featureproto.RequirementVerificationState, blocked bool) string {
	return `{
  "protocolVersion": 1,
  "sessionId": "` + sessionID + `",
  "generatedAt": "2026-08-23T12:00:00Z",
  "groups": [{
    "project": "/app",
    "feature": "billing/invoicing",
    "spec": "app/specs/invoicing.json",
    "mode": "enforce",
    "modeSource": "project",
    "automaticEvaluation": true,
    "blocked": ` + boolLiteral(blocked) + `,
    "requirements": [
      {"requirement": "ghost", "state": "unmapped"},
      {"requirement": "issue", "state": "` + string(state) + `",
       "checks": [{"check": "issue-check", "state": "satisfied", "reason": "check-passed", "path": "invoice_test.go"}]},
      {"requirement": "record", "state": "unexecutable"}
    ],
    "counts": {"specRequirements": 3, "executable": 1, "verified": 1, "unmapped": 1, "unexecutable": 1, "missing": 0, "stale": 0, "contradicted": 0}
  }]
}`
}

func boolLiteral(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// TestBuildSpecVerifyResultWithoutASessionFailsClosed pins the no-session
// audit: states come from the one evaluator over zero observations, so the
// executable requirement is missing, the rest keep their join states, and an
// enforce project blocks while report never does.
func TestBuildSpecVerifyResultWithoutASessionFailsClosed(t *testing.T) {
	ws := verifyWorkspace(t, enforceOptions())
	report, verdict := BuildSpecVerifyResult(ws, Selection{}, "")
	if verdict == nil {
		t.Fatal("enforce with nothing proven verified cleanly")
	}
	if report.Recorded || report.Session != "" {
		t.Errorf("report claims a session: %+v", report)
	}
	states := map[string]featureproto.RequirementVerificationState{}
	for _, requirement := range report.Groups[0].Requirements {
		states[requirement.Requirement] = requirement.State
	}
	want := map[string]featureproto.RequirementVerificationState{
		"issue": featureproto.RequirementMissing, "record": featureproto.RequirementUnexecutable,
		"ghost": featureproto.RequirementUnmapped,
	}
	for id, state := range want {
		if states[id] != state {
			t.Errorf("%s = %s, want %s", id, states[id], state)
		}
	}
	if len(report.Blocked) != 1 || report.Blocked[0] != "/app" {
		t.Errorf("blocked = %v", report.Blocked)
	}

	relaxed, verdict := BuildSpecVerifyResult(verifyWorkspace(t, nil), Selection{}, "")
	if verdict != nil {
		t.Fatalf("the default report mode failed: %v", verdict)
	}
	if relaxed.Groups[0].Mode != featureproto.VerificationModeReport || len(relaxed.Blocked) != 0 {
		t.Errorf("default mode group = %+v blocked %v", relaxed.Groups[0], relaxed.Blocked)
	}
}

func TestBuildSpecVerifyResultReplaysTheRecordedVerdict(t *testing.T) {
	ws := verifyWorkspace(t, enforceOptions())
	writeVerifySession(t, ws.Root, "20260823-120000-abcdef", verifySessionRecord("20260823-120000-abcdef", featureproto.RequirementVerified, false))

	report, verdict := BuildSpecVerifyResult(ws, Selection{}, "")
	if verdict != nil {
		t.Fatalf("a clean recorded verdict failed: %v", verdict)
	}
	if !report.Recorded || report.Session != "20260823-120000-abcdef" {
		t.Fatalf("report did not replay the latest session: %+v", report)
	}
	group := report.Groups[0]
	if group.Requirements[1].Requirement != "issue" || group.Requirements[1].State != featureproto.RequirementVerified {
		t.Errorf("replayed states = %+v", group.Requirements)
	}
	if group.Requirements[1].Checks[0].Path != "invoice_test.go" {
		t.Errorf("replayed provenance = %+v", group.Requirements[1].Checks)
	}

	// A recorded blocked decision is reproduced, not recomputed.
	writeVerifySession(t, ws.Root, "20260823-130000-abcdef", verifySessionRecord("20260823-130000-abcdef", featureproto.RequirementContradicted, true))
	blocked, verdict := BuildSpecVerifyResult(ws, Selection{}, "20260823-130000-abcdef")
	if verdict == nil || len(blocked.Blocked) != 1 {
		t.Fatalf("a recorded blocked group did not fail the audit: %v / %+v", verdict, blocked.Blocked)
	}
	if !strings.Contains(verdict.Error(), "/app") {
		t.Errorf("verdict %q does not name the blocked project", verdict)
	}
}

func TestBuildSpecVerifyResultRefusesABrokenOrForeignRecord(t *testing.T) {
	ws := verifyWorkspace(t, nil)
	if _, verdict := BuildSpecVerifyResult(ws, Selection{}, "20990101-000000-ffffff"); verdict == nil {
		t.Error("an explicit missing session verified cleanly")
	}
	writeVerifySession(t, ws.Root, "20260823-120000-abcdef", verifySessionRecord("some-other-session", featureproto.RequirementVerified, false))
	if _, verdict := BuildSpecVerifyResult(ws, Selection{}, "20260823-120000-abcdef"); verdict == nil {
		t.Error("a record claiming another session was accepted")
	}
	if _, verdict := BuildSpecVerifyResult(ws, Selection{}, "../escape"); verdict == nil {
		t.Error("a path-shaped session id was accepted")
	}
}

func TestBuildSpecVerifyResultUnderOffReportsWithoutEvaluating(t *testing.T) {
	off := map[string]map[string]any{"sdd": {"verification": map[string]any{"specs": "off"}}}
	report, verdict := BuildSpecVerifyResult(verifyWorkspace(t, off), Selection{}, "")
	if verdict != nil {
		t.Fatalf("off failed: %v", verdict)
	}
	group := report.Groups[0]
	if group.AutomaticEvaluation || group.Blocked || len(group.Requirements) != 0 {
		t.Errorf("off group = %+v", group)
	}
	if group.Mode != featureproto.VerificationModeOff || group.ModeSource != featureproto.VerificationModeSourceProject {
		t.Errorf("off provenance = (%s, %s)", group.Mode, group.ModeSource)
	}
}

func TestBuildSpecVerifyResultKeepsStructuralErrorsBlocking(t *testing.T) {
	ws := verifyWorkspace(t, nil)
	writeFixtureFile(t, filepath.Join(ws.Root, "app", "specs", "broken.json"), `{
  "protocolVersion": 1,
  "feature": "billing/unauthored",
  "outcomes": ["An outcome"],
  "requirements": []
}`)
	report, verdict := BuildSpecVerifyResult(ws, Selection{}, "")
	if verdict == nil {
		t.Fatal("a spec detailing an unauthored feature verified cleanly")
	}
	if !diag.HasErrors(report.Diagnostics) {
		t.Errorf("structural findings missing: %+v", report.Diagnostics)
	}
}
