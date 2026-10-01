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

func reportModeOptions() map[string]map[string]any {
	return map[string]map[string]any{"sdd": {"verification": map[string]any{"specs": "report"}}}
}

// committedBaseline writes the specs.baseline.json of the fixture's /app
// project, in its directory.
func committedBaseline(t *testing.T, root, contents string) {
	t.Helper()
	writeFixtureFile(t, appBaselinePath(root), contents)
}

// appBaselinePath is where the fixture's /app project commits its floor.
func appBaselinePath(root string) string {
	return filepath.Join(root, "app", featureproto.SpecsBaselineFilename)
}

// appIssueBaseline is the committed floor the verify fixture derives under
// enforce for its one spec-owning project: its one executable requirement.
const appIssueBaseline = `{
  "$schema": "https://putnami.dev/schemas/putnami-specs-baseline.json",
  "protocolVersion": 2,
  "executableRequirements": ["billing/invoicing#issue"]
}`

// workspaceIssueBaseline is the same floor in the older workspace-root
// document, which names its project.
const workspaceIssueBaseline = `{
  "$schema": "https://putnami.dev/schemas/putnami-specs-baseline.json",
  "protocolVersion": 1,
  "projects": [
    {"project": "/app", "mode": "enforce", "executableRequirements": ["billing/invoicing#issue"]}
  ]
}`

// TestBuildSpecsBaselineResultDerivesAndWritesTheFloor pins the derivation and
// the write path: under enforce the floor is the spec-owning project with
// exactly its executable (feature#requirement) identities — `record` has no
// verification criterion and `ghost` joins nothing, so neither may appear —
// and --update writes the canonical bytes so a second run reports up to date.
func TestBuildSpecsBaselineResultDerivesAndWritesTheFloor(t *testing.T) {
	ws := verifyWorkspace(t, enforceOptions())

	report, err := BuildSpecsBaselineResult(ws, false)
	if err != nil {
		t.Fatalf("read-only baseline derivation failed: %v", err)
	}
	if report.Current == nil || len(report.Current.Projects) != 1 {
		t.Fatalf("current floor = %+v, want the one enforced project", report.Current)
	}
	project := report.Current.Projects[0]
	if project.Project != "/app" || project.Mode != featureproto.VerificationModeEnforce {
		t.Fatalf("floor project = %+v", project)
	}
	if len(project.ExecutableRequirements) != 1 || project.ExecutableRequirements[0] != "billing/invoicing#issue" {
		t.Fatalf("floor requirements = %v, want exactly the criterion-backed one", project.ExecutableRequirements)
	}
	if !report.Changed || report.Updated {
		t.Fatalf("changed = %v updated = %v, want out of date and untouched without --update", report.Changed, report.Updated)
	}
	if len(report.Paths) != 1 || report.Paths[0] != "app/"+featureproto.SpecsBaselineFilename {
		t.Fatalf("paths = %v, want the one file in the enforced project's directory", report.Paths)
	}
	if _, err := os.Stat(appBaselinePath(ws.Root)); !os.IsNotExist(err) {
		t.Fatal("the read-only run wrote the committed baseline")
	}

	written, err := BuildSpecsBaselineResult(ws, true)
	if err != nil {
		t.Fatalf("--update failed: %v", err)
	}
	if !written.Updated || !written.Changed {
		t.Fatalf("update report = %+v, want an acknowledged rewrite", written)
	}
	data, err := os.ReadFile(appBaselinePath(ws.Root))
	if err != nil {
		t.Fatal(err)
	}
	if parsed, findings := featureproto.ParseAndValidateProjectSpecsBaseline(data); parsed == nil || diag.HasErrors(findings) {
		t.Fatalf("the written baseline fails its own strict reader: %v\n%s", findings, data)
	}

	again, err := BuildSpecsBaselineResult(ws, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed || again.Updated {
		t.Fatalf("second run = %+v, want up to date with no rewrite", again)
	}
}

// TestBuildSpecsBaselineResultRoundTripsAnEmptyFloorEntry: an enforced spec
// owner with no criterion-backed requirement yet is a legal floor entry with
// an empty requirement list, and the file --update writes for it must satisfy
// the strict reader — the writer that bricks its own consumer is the one
// failure mode `specs baseline` may never have.
func TestBuildSpecsBaselineResultRoundTripsAnEmptyFloorEntry(t *testing.T) {
	ws := verifyWorkspace(t, enforceOptions())
	// Drop the one authored criterion: the owner stays enforced, the floor
	// entry goes empty.
	writeFixtureFile(t, filepath.Join(ws.Root, "app", featureproto.ManifestFilename), `{
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
      {"id": "issue", "stage": "coded", "evidenceKinds": ["attestation"]},
      {"id": "record", "stage": "coded", "evidenceKinds": ["artifact"]}
    ]
  }]
}`)

	written, err := BuildSpecsBaselineResult(ws, true)
	if err != nil {
		t.Fatalf("--update failed: %v", err)
	}
	if written.Current == nil || len(written.Current.Projects) != 1 ||
		len(written.Current.Projects[0].ExecutableRequirements) != 0 {
		t.Fatalf("floor = %+v, want the one enforced project with an empty requirement list", written.Current)
	}
	data, err := os.ReadFile(appBaselinePath(ws.Root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"executableRequirements": []`) {
		t.Fatalf("written baseline does not encode the empty list as []:\n%s", data)
	}
	if parsed, findings := featureproto.ParseAndValidateProjectSpecsBaseline(data); parsed == nil || diag.HasErrors(findings) {
		t.Fatalf("the written baseline fails its own strict reader: %v\n%s", findings, data)
	}
	if _, err := BuildSpecsRatchetResult(ws); err != nil {
		t.Fatalf("the ratchet refuses the floor its own writer recorded: %v", err)
	}
}

// TestBuildSpecsBaselineResultRemovesTheFileOfAProjectThatLeftEnforce: the
// deliberate way to lower the floor is `--update`, and for a project that
// left enforce it deletes that project's file. The ratchet then passes, and
// the deletion is the reviewed edit the diff carries.
func TestBuildSpecsBaselineResultRemovesTheFileOfAProjectThatLeftEnforce(t *testing.T) {
	ws := verifyWorkspace(t, reportModeOptions())
	committedBaseline(t, ws.Root, appIssueBaseline)

	report, err := BuildSpecsBaselineResult(ws, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "app/" + featureproto.SpecsBaselineFilename
	if !report.Changed || len(report.Removed) != 1 || report.Removed[0] != want {
		t.Fatalf("report = %+v, want %s named for removal", report, want)
	}
	if _, err := os.Stat(appBaselinePath(ws.Root)); err != nil {
		t.Fatalf("the read-only run touched the committed baseline: %v", err)
	}

	if _, err := BuildSpecsBaselineResult(ws, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(appBaselinePath(ws.Root)); !os.IsNotExist(err) {
		t.Fatalf("--update kept the file of a project that left enforce: %v", err)
	}
	if _, err := BuildSpecsRatchetResult(ws); err != nil {
		t.Fatalf("the ratchet refuses the floor --update lowered: %v", err)
	}
}

// TestSpecsBaselineMovesAWorkspaceRootFloorIntoTheProject covers the older
// layout: one workspace-root file recording every project. The ratchet keeps
// enforcing it and warns, and `--update` moves the entry into the project's
// directory and deletes the root file.
func TestSpecsBaselineMovesAWorkspaceRootFloorIntoTheProject(t *testing.T) {
	ws := verifyWorkspace(t, reportModeOptions())
	rootFile := filepath.Join(ws.Root, featureproto.SpecsBaselineFilename)
	writeFixtureFile(t, rootFile, workspaceIssueBaseline)

	report, err := BuildSpecsRatchetResult(ws)
	if err == nil || report.Summary.RegressedProjects != 1 {
		t.Fatalf("verdict = %v summary = %+v, want the root file still enforced", err, report.Summary)
	}
	if !hasDiagnostic(report.Diagnostics, featureproto.WarningCodeMisplacedBaseline, "/app") {
		t.Fatalf("diagnostics = %+v, want the warning naming the project to move", report.Diagnostics)
	}

	ws = verifyWorkspace(t, enforceOptions())
	rootFile = filepath.Join(ws.Root, featureproto.SpecsBaselineFilename)
	writeFixtureFile(t, rootFile, workspaceIssueBaseline)
	moved, err := BuildSpecsBaselineResult(ws, true)
	if err != nil {
		t.Fatal(err)
	}
	if !moved.Updated || len(moved.Removed) != 1 || moved.Removed[0] != featureproto.SpecsBaselineFilename {
		t.Fatalf("report = %+v, want the root file removed", moved)
	}
	if _, err := os.Stat(rootFile); !os.IsNotExist(err) {
		t.Fatalf("--update kept the workspace-root file: %v", err)
	}
	held, err := BuildSpecsRatchetResult(ws)
	if err != nil {
		t.Fatalf("the moved floor failed: %v", err)
	}
	if hasDiagnostic(held.Diagnostics, featureproto.WarningCodeMisplacedBaseline, "") {
		t.Fatalf("the moved floor still draws the warning: %+v", held.Diagnostics)
	}
}

// TestBuildSpecsRatchetResultRefusesAMisplacedEntry: a project's directory
// holds a project baseline only, the workspace root holds one only when a
// project sits there, and one project is recorded once. Each mistake would let
// a change lower the floor through a file its reviewer does not expect to hold
// it, so each fails closed.
func TestBuildSpecsRatchetResultRefusesAMisplacedEntry(t *testing.T) {
	cases := map[string]func(t *testing.T, root string){
		"workspace document in a project": func(t *testing.T, root string) {
			committedBaseline(t, root, workspaceIssueBaseline)
		},
		"project baseline at a root with no project": func(t *testing.T, root string) {
			writeFixtureFile(t, filepath.Join(root, featureproto.SpecsBaselineFilename), appIssueBaseline)
		},
		"recorded twice": func(t *testing.T, root string) {
			committedBaseline(t, root, appIssueBaseline)
			writeFixtureFile(t, filepath.Join(root, featureproto.SpecsBaselineFilename), workspaceIssueBaseline)
		},
	}
	for name, commit := range cases {
		t.Run(name, func(t *testing.T) {
			ws := verifyWorkspace(t, enforceOptions())
			commit(t, ws.Root)
			report, err := BuildSpecsRatchetResult(ws)
			if err == nil || !strings.Contains(err.Error(), "unreadable") {
				t.Fatalf("verdict = %v, want the refusal", err)
			}
			if !diag.HasErrors(report.Diagnostics) {
				t.Fatalf("diagnostics = %+v, want an error", report.Diagnostics)
			}
		})
	}
}

// TestBuildSpecsBaselineResultUnderReportDerivesAnEmptyFloor: a report project
// is not part of the enforced floor, so the derived baseline is empty — the
// ratchet protects commitments, and report never made one.
func TestBuildSpecsBaselineResultUnderReportDerivesAnEmptyFloor(t *testing.T) {
	ws := verifyWorkspace(t, reportModeOptions())
	report, err := BuildSpecsBaselineResult(ws, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Current == nil || len(report.Current.Projects) != 0 {
		t.Fatalf("floor under report = %+v, want no enforced project", report.Current)
	}
}

// TestBuildSpecsBaselineResultLeavesAnUnresolvableOwnerOut: a spec whose
// owning directory is not a resolvable workspace member cannot commit to the
// floor. The fallback scope must never be baked into the committed artifact —
// there it would fail the ratchet spuriously the day the project becomes
// resolvable — so the derivation warns and leaves the owner out.
func TestBuildSpecsBaselineResultLeavesAnUnresolvableOwnerOut(t *testing.T) {
	// A workspace-rooted manifest and spec: discovery binds them to no member,
	// so specOwnerPolicy resolves the fallback scope while the workspace level
	// says enforce.
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"floor-test","includes":[]}`)
	writeFixtureFile(t, filepath.Join(root, featureproto.ManifestFilename), `{
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
       "verification": {"kind": "acceptance", "checks": ["issue-check"]}}
    ]
  }]
}`)
	writeFixtureFile(t, filepath.Join(root, "specs", "invoicing.json"), `{
  "protocolVersion": 1,
  "feature": "billing/invoicing",
  "outcomes": ["Customers receive invoices"],
  "requirements": [
    {"id": "issue", "text": "An invoice is issued."}
  ]
}`)
	ws := workspace.NewWorkspace(root,
		&wsproto.Config{Name: "floor-test", Options: enforceOptions()}, nil)

	report, err := BuildSpecsBaselineResult(ws, false)
	if err != nil {
		t.Fatalf("an unresolvable owner failed the derivation: %v", err)
	}
	if report.Current == nil || len(report.Current.Projects) != 0 {
		t.Fatalf("floor = %+v, want no entry for the fallback scope", report.Current)
	}
	found := false
	for _, finding := range report.Diagnostics {
		if finding.Code == featureproto.WarningCodeUnresolvedFeatureAuthority && finding.Severity == diag.Warning {
			found = true
			if !strings.Contains(finding.Message, "billing/invoicing") ||
				!strings.Contains(finding.Message, featureproto.SpecsBaselineFilename) {
				t.Fatalf("warning names neither the feature nor the artifact: %+v", finding)
			}
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want the unresolvable-owner warning", report.Diagnostics)
	}
}

// TestBuildSpecsRatchetResultAdoptsThenHoldsTheFloor pins the two green paths:
// an absent baseline is initial adoption (a warning nudge, never a failure),
// and a committed baseline matching the derived floor is silence.
func TestBuildSpecsRatchetResultAdoptsThenHoldsTheFloor(t *testing.T) {
	ws := verifyWorkspace(t, enforceOptions())

	report, err := BuildSpecsRatchetResult(ws)
	if err != nil {
		t.Fatalf("adoption failed: %v", err)
	}
	if report.BaselinePresent {
		t.Fatal("an absent baseline reported as present")
	}
	if report.Summary.GrownProjects != 1 {
		t.Fatalf("summary = %+v, want the enforced project counted as growth", report.Summary)
	}
	nudge := findRatchetWarning(report.Diagnostics)
	if nudge == nil || !strings.Contains(nudge.Message, "specs baseline --update") {
		t.Fatalf("diagnostics = %+v, want the adoption nudge naming the command", report.Diagnostics)
	}

	if _, err := BuildSpecsBaselineResult(ws, true); err != nil {
		t.Fatal(err)
	}
	held, err := BuildSpecsRatchetResult(ws)
	if err != nil {
		t.Fatalf("a matching floor failed: %v", err)
	}
	if !held.BaselinePresent || held.Summary.RegressedProjects != 0 || held.Summary.LostRequirements != 0 {
		t.Fatalf("held report = %+v %+v", held.BaselinePresent, held.Summary)
	}
	if found := findRatchetWarning(held.Diagnostics); found != nil {
		t.Fatalf("a matching floor drew a nudge: %+v", found)
	}
}

// TestBuildSpecsRatchetResultFailsOnShrink is the epic's guarantee: a project
// the committed baseline records as enforced that regressed to report fails
// validate-workspace, and the error names the committed baseline as the
// reviewed policy change.
func TestBuildSpecsRatchetResultFailsOnShrink(t *testing.T) {
	ws := verifyWorkspace(t, reportModeOptions())
	committedBaseline(t, ws.Root, appIssueBaseline)

	report, err := BuildSpecsRatchetResult(ws)
	if err == nil {
		t.Fatal("a recorded project regressed out of enforce and the ratchet passed")
	}
	if ResultData(err) == nil {
		t.Fatalf("the failing verdict carries no report payload: %v", err)
	}
	if report.Summary.RegressedProjects != 1 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	found := false
	for _, finding := range report.Diagnostics {
		if finding.Code == featureproto.ErrorCodeRatchetRegression && finding.Severity == diag.Error {
			found = true
			if !strings.Contains(finding.Message, featureproto.SpecsBaselineFilename) {
				t.Fatalf("finding does not name the reviewed policy artifact: %+v", finding)
			}
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want the regression error", report.Diagnostics)
	}
}

// TestBuildSpecsRatchetResultNudgesOnGrowth: a floor grown beyond the
// committed baseline passes with one warning to raise it — growth is
// desirable and never forced.
func TestBuildSpecsRatchetResultNudgesOnGrowth(t *testing.T) {
	ws := verifyWorkspace(t, enforceOptions())
	committedBaseline(t, ws.Root,
		`{"$schema":"https://putnami.dev/schemas/putnami-specs-baseline.json","protocolVersion":2,"executableRequirements":[]}`)

	report, err := BuildSpecsRatchetResult(ws)
	if err != nil {
		t.Fatalf("growth failed the ratchet: %v", err)
	}
	if report.Summary.GrownRequirements != 1 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	nudge := findRatchetWarning(report.Diagnostics)
	if nudge == nil || !strings.Contains(nudge.Message, "grew beyond the committed baseline") {
		t.Fatalf("diagnostics = %+v, want the growth nudge", report.Diagnostics)
	}
}

// TestBuildSpecsRatchetResultRefusesABrokenBaseline: a committed baseline the
// strict reader rejects fails closed rather than comparing against a guess.
func TestBuildSpecsRatchetResultRefusesABrokenBaseline(t *testing.T) {
	ws := verifyWorkspace(t, enforceOptions())
	committedBaseline(t, ws.Root,
		`{"protocolVersion":2,"executableRequirements":["no-feature-part"]}`)

	report, err := BuildSpecsRatchetResult(ws)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("verdict = %v, want the unreadable-baseline refusal", err)
	}
	if !report.BaselinePresent || !diag.HasErrors(report.Diagnostics) {
		t.Fatalf("report = %+v", report)
	}
}

// hasDiagnostic reports whether a diagnostic with code mentions text.
func hasDiagnostic(diagnostics []diag.Diagnostic, code, text string) bool {
	for _, finding := range diagnostics {
		if finding.Code == code && strings.Contains(finding.Message, text) {
			return true
		}
	}
	return false
}

// findRatchetWarning returns the first growth-nudge diagnostic.
func findRatchetWarning(diagnostics []diag.Diagnostic) *diag.Diagnostic {
	for i, finding := range diagnostics {
		if finding.Code == featureproto.WarningCodeRatchetGrowth && finding.Severity == diag.Warning {
			return &diagnostics[i]
		}
	}
	return nil
}
