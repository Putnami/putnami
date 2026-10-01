package sdd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

// scaleToZeroRegistry is the shape the issue's example uses: one checkable
// decision and one review-only decision, so every assertion below exercises
// both halves of the contract at once.
const scaleToZeroRegistry = `{
  "$schema": "https://putnami.dev/schemas/putnami-decisions.json",
  "protocolVersion": 1,
  "decisions": [
    {
      "id": "D-001",
      "statement": "serverless workloads scale to zero",
      "settled": "2026-09-03",
      "settledBy": "fdumay",
      "check": {
        "kind": "json-value",
        "files": ["**/infra/requirements.json"],
        "pointer": "/scaling/minInstances",
        "rule": "equals",
        "value": 0,
        "whenMissing": "satisfied"
      }
    },
    {
      "id": "D-002",
      "statement": "every pull request names what it deletes",
      "settled": "2026-09-03",
      "settledBy": "fdumay",
      "reviewOnly": true
    }
  ]
}`

func decisionsWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// Symlinks are resolved once so a macOS /var -> /private/var temp root does
	// not make every workspace-relative path unrecognizable.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func committedDecisions(t *testing.T, root, contents string) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(root, featureproto.DecisionsFilename), contents)
}

// TestDecisionsAbsentRegistryIsAdoption pins the one rule that lets the format
// ship before any repository adopts it: no decisions.json is not a failure, it
// is a workspace that has settled nothing yet — the same rule as an absent
// specs.baseline.json.
func TestDecisionsAbsentRegistryIsAdoption(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "settled-decisions-are-enforced", "an-absent-registry-is-adoption")
	root := decisionsWorkspace(t)
	report, err := BuildDecisionsResult(fixtureWorkspace("w", root))
	if err != nil {
		t.Fatalf("an absent registry failed the task: %v", err)
	}
	if report.RegistryPresent || report.Summary.Decisions != 0 || len(report.Findings) != 0 {
		t.Fatalf("report = %+v, want an empty adoption report", report)
	}
}

// TestDecisionsViolationNamesTheDecision is the whole point of the decision
// gate: a file that re-decides a settled value fails, and the failure carries the id, the
// statement and the settled date so a reader can tell a bug from a deliberate
// reversal.
func TestDecisionsViolationNamesTheDecision(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "settled-decisions-are-enforced", "a-violated-decision-fails-naming-it")
	root := decisionsWorkspace(t)
	committedDecisions(t, root, scaleToZeroRegistry)
	writeFixtureFile(t, filepath.Join(root, "sites", "site", "infra", "requirements.json"),
		`{"protocolVersion":2,"scaling":{"minInstances":1}}`)
	// A second matched file that holds the settled value stays silent, so the
	// findings are about the violating file and not about the glob.
	writeFixtureFile(t, filepath.Join(root, "services", "api", "infra", "requirements.json"),
		`{"protocolVersion":2,"scaling":{"minInstances":0}}`)
	// An unmatched file is never read, whatever it declares.
	writeFixtureFile(t, filepath.Join(root, "services", "api", "deploy.json"),
		`{"scaling":{"minInstances":9}}`)

	report, err := BuildDecisionsResult(fixtureWorkspace("w", root))
	if err == nil {
		t.Fatal("a violated decision did not fail the task")
	}
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one", report.Findings)
	}
	finding := report.Findings[0]
	if finding.Decision != "D-001" || finding.Path != "sites/site/infra/requirements.json" {
		t.Fatalf("finding = %+v, want D-001 anchored on the violating file", finding)
	}
	want := "decision D-001 \"serverless workloads scale to zero\" is violated by sites/site/infra/requirements.json: scaling.minInstances = 1\n" +
		"Settled 2026-09-03 by fdumay. To change it, change the decision in decisions.json, not the code."
	if finding.Message != want {
		t.Fatalf("message =\n%s\nwant\n%s", finding.Message, want)
	}
	if report.Summary.Decisions != 2 || report.Summary.Enforced != 1 || report.Summary.ReviewOnly != 1 {
		t.Fatalf("summary = %+v, want two decisions, one enforced and one review-only", report.Summary)
	}
	if report.Summary.FilesChecked != 2 || report.Summary.Violations != 1 {
		t.Fatalf("summary = %+v, want the two matched files and one violation", report.Summary)
	}
	if !strings.Contains(err.Error(), "D-001") {
		t.Fatalf("task error %q does not name the violated decision", err)
	}
}

// TestDecisionsProjectRegistryJudgesOnlyItsDirectory pins the scope of a
// project registry: its globs and its adr link are relative to the project
// directory, the same file outside the project is never judged, and a
// violation names the registry to edit.
func TestDecisionsProjectRegistryJudgesOnlyItsDirectory(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "settled-decisions-are-enforced", "a-project-registry-judges-only-its-directory")
	root := decisionsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "services", "api", featureproto.DecisionsFilename), `{"protocolVersion":1,"decisions":[
		{"id":"API-1","statement":"the api scales to zero","settled":"2026-09-03","settledBy":"fdumay",
		 "adr":"doc/adr/0001-scale.md",
		 "check":{"kind":"json-value","files":["infra/requirements.json"],"pointer":"/scaling/minInstances","rule":"equals","value":0}}]}`)
	writeFixtureFile(t, filepath.Join(root, "services", "api", "doc", "adr", "0001-scale.md"), "# ADR\n")
	writeFixtureFile(t, filepath.Join(root, "services", "api", "infra", "requirements.json"), `{"scaling":{"minInstances":2}}`)
	// The same relative path in a sibling project and at the root is outside
	// the registry's directory, so it is never read.
	writeFixtureFile(t, filepath.Join(root, "services", "api-admin", "infra", "requirements.json"), `{"scaling":{"minInstances":5}}`)
	writeFixtureFile(t, filepath.Join(root, "infra", "requirements.json"), `{"scaling":{"minInstances":5}}`)
	// A registry outside any project directory governs nothing and is not read.
	writeFixtureFile(t, filepath.Join(root, "services", featureproto.DecisionsFilename), `{"protocolVersion":1,`)

	ws := fixtureWorkspace("w", root, appProject("api", "services/api"), appProject("api-admin", "services/api-admin"))
	report, err := BuildDecisionsResult(ws)
	if err == nil {
		t.Fatal("a violated project decision did not fail the task")
	}
	if strings.Join(report.Registries, ",") != "services/api/decisions.json" {
		t.Fatalf("registries = %v, want only the project registry", report.Registries)
	}
	if report.Summary.FilesChecked != 1 || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want one file checked and one finding", report)
	}
	finding := report.Findings[0]
	if finding.Registry != "services/api/decisions.json" || finding.Path != "services/api/infra/requirements.json" {
		t.Fatalf("finding = %+v, want the project file and its registry", finding)
	}
	if !strings.Contains(finding.Message, "change the decision in services/api/decisions.json") {
		t.Fatalf("message %q does not name the registry to edit", finding.Message)
	}
}

// TestDecisionsIDsAreUniqueAcrossRegistries keeps a citation unambiguous: an
// id settled in the root registry cannot be settled again in a project one.
func TestDecisionsIDsAreUniqueAcrossRegistries(t *testing.T) {
	root := decisionsWorkspace(t)
	committedDecisions(t, root, scaleToZeroRegistry)
	writeFixtureFile(t, filepath.Join(root, "api", featureproto.DecisionsFilename), `{"protocolVersion":1,"decisions":[
		{"id":"D-002","statement":"another rule","settled":"2026-09-03","settledBy":"fdumay","reviewOnly":true}]}`)
	report, err := BuildDecisionsResult(fixtureWorkspace("w", root, appProject("api", "api")))
	if err == nil {
		t.Fatal("an id settled in two registries passed")
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Field != "api/decisions.json#D-002.id" ||
		!strings.Contains(report.Diagnostics[0].Message, "already settled in decisions.json") {
		t.Fatalf("diagnostics = %+v, want one naming both registries", report.Diagnostics)
	}
}

// TestDecisionsSatisfiedRegistryPasses keeps the gate from being vacuously red
// and pins the two ways a check passes: the value is held, or no file matches
// at all.
func TestDecisionsSatisfiedRegistryPasses(t *testing.T) {
	root := decisionsWorkspace(t)
	committedDecisions(t, root, scaleToZeroRegistry)
	writeFixtureFile(t, filepath.Join(root, "services", "api", "infra", "requirements.json"),
		`{"protocolVersion":2,"scaling":{"minInstances":0}}`)
	report, err := BuildDecisionsResult(fixtureWorkspace("w", root))
	if err != nil {
		t.Fatalf("a held decision failed the task: %v", err)
	}
	if report.Summary.Violations != 0 || report.Summary.FilesChecked != 1 {
		t.Fatalf("summary = %+v", report.Summary)
	}

	// A repository whose globs match nothing cannot violate the decision. That
	// is what lets one format serve many repositories.
	empty := decisionsWorkspace(t)
	committedDecisions(t, empty, scaleToZeroRegistry)
	emptyReport, err := BuildDecisionsResult(fixtureWorkspace("w", empty))
	if err != nil {
		t.Fatalf("a registry with no matching file failed the task: %v", err)
	}
	if emptyReport.Summary.FilesChecked != 0 || emptyReport.Summary.Violations != 0 {
		t.Fatalf("summary = %+v, want nothing checked and nothing violated", emptyReport.Summary)
	}
}

// TestDecisionsMalformedMatchedFileFailsClosed keeps a decision from being
// defeated by breaking the document its check reads.
func TestDecisionsMalformedMatchedFileFailsClosed(t *testing.T) {
	root := decisionsWorkspace(t)
	committedDecisions(t, root, scaleToZeroRegistry)
	writeFixtureFile(t, filepath.Join(root, "infra", "requirements.json"), `{"scaling":`)

	report, err := BuildDecisionsResult(fixtureWorkspace("w", root))
	if err == nil {
		t.Fatal("a matched file that is not JSON was treated as satisfied")
	}
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "not valid JSON") {
		t.Fatalf("findings = %+v, want one violation naming the parse failure", report.Findings)
	}
}

// TestDecisionsGeneratedAndVendoredTreesAreNeverRead pins the walk's ignore
// rules. A verdict must be a function of the committed tree: a copy of a
// matched file under .gen would make the same registry answer differently
// before and after a build.
func TestDecisionsGeneratedAndVendoredTreesAreNeverRead(t *testing.T) {
	root := decisionsWorkspace(t)
	committedDecisions(t, root, scaleToZeroRegistry)
	for _, excluded := range []string{".gen", ".git", "node_modules", "vendor"} {
		writeFixtureFile(t, filepath.Join(root, "app", excluded, "infra", "requirements.json"),
			`{"scaling":{"minInstances":1}}`)
	}
	report, err := BuildDecisionsResult(fixtureWorkspace("w", root))
	if err != nil {
		t.Fatalf("an excluded tree failed the task: %v", err)
	}
	if report.Summary.FilesChecked != 0 {
		t.Fatalf("the walk read %d excluded file(s): %+v", report.Summary.FilesChecked, report.Findings)
	}
}

// TestDecisionsSymlinkedMatchIsNotRead keeps the verdict a function of the
// committed bytes. A symlink's target may sit outside the worktree entirely.
func TestDecisionsSymlinkedMatchIsNotRead(t *testing.T) {
	root := decisionsWorkspace(t)
	committedDecisions(t, root, scaleToZeroRegistry)
	outside := filepath.Join(decisionsWorkspace(t), "elsewhere.json")
	writeFixtureFile(t, outside, `{"scaling":{"minInstances":1}}`)
	if err := os.MkdirAll(filepath.Join(root, "app", "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "app", "infra", "requirements.json")); err != nil {
		t.Skipf("this filesystem refuses symlinks: %v", err)
	}
	report, err := BuildDecisionsResult(fixtureWorkspace("w", root))
	if err != nil {
		t.Fatalf("a symlinked match failed the task: %v", err)
	}
	if report.Summary.FilesChecked != 0 {
		t.Fatalf("the walk followed a symlink: %+v", report)
	}
}

// TestDecisionsRegistryFailsClosed is D7 at the task boundary: a registry that
// cannot be read WHOLE fails, and nothing is checked against a half-understood
// policy.
func TestDecisionsRegistryFailsClosed(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "settled-decisions-are-enforced", "an-unusable-registry-fails-closed")
	for name, contents := range map[string]string{
		"not json":       `{"protocolVersion":1,`,
		"no version":     `{"decisions":[]}`,
		"unknown member": `{"protocolVersion":1,"decisions":[],"mode":"report"}`,
		"duplicate id": `{"protocolVersion":1,"decisions":[
			{"id":"D-1","statement":"a","settled":"2026-01-02","settledBy":"x","reviewOnly":true},
			{"id":"D-1","statement":"b","settled":"2026-01-02","settledBy":"x","reviewOnly":true}]}`,
		"loose date": `{"protocolVersion":1,"decisions":[
			{"id":"D-1","statement":"a","settled":"2026-1-2","settledBy":"x","reviewOnly":true}]}`,
		"neither check nor mark": `{"protocolVersion":1,"decisions":[
			{"id":"D-1","statement":"a","settled":"2026-01-02","settledBy":"x"}]}`,
		"unknown kind": `{"protocolVersion":1,"decisions":[
			{"id":"D-1","statement":"a","settled":"2026-01-02","settledBy":"x",
			 "check":{"kind":"yaml-value","files":["a.json"],"pointer":"/a","rule":"equals","value":0}}]}`,
		"no globs": `{"protocolVersion":1,"decisions":[
			{"id":"D-1","statement":"a","settled":"2026-01-02","settledBy":"x",
			 "check":{"kind":"json-value","files":[],"pointer":"/a","rule":"equals","value":0}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := decisionsWorkspace(t)
			committedDecisions(t, root, contents)
			report, err := BuildDecisionsResult(fixtureWorkspace("w", root))
			if err == nil {
				t.Fatalf("an unusable registry passed: %+v", report)
			}
			if !report.RegistryPresent {
				t.Fatal("an unusable registry was reported as absent")
			}
			if !diag.HasErrors(report.Diagnostics) {
				t.Fatalf("the failure carries no diagnostic: %+v", report)
			}
			for _, finding := range report.Diagnostics {
				if !strings.HasPrefix(finding.Field, featureproto.DecisionsFilename) {
					t.Errorf("diagnostic %q is not anchored on the registry", finding.Field)
				}
			}
		})
	}
}

// TestDecisionsADRMustResolveInTheWorktree keeps the justification link
// honest. The protocol checks the shape; only a caller holding the worktree
// can check that the record exists.
func TestDecisionsADRMustResolveInTheWorktree(t *testing.T) {
	registry := `{"protocolVersion":1,"decisions":[
		{"id":"D-1","statement":"a","settled":"2026-01-02","settledBy":"x",
		 "adr":"tooling/cli/doc/adr/0099-missing.md","reviewOnly":true}]}`
	root := decisionsWorkspace(t)
	committedDecisions(t, root, registry)
	if _, err := BuildDecisionsResult(fixtureWorkspace("w", root)); err == nil {
		t.Fatal("a dangling adr link passed")
	}

	writeFixtureFile(t, filepath.Join(root, "tooling", "cli", "doc", "adr", "0099-missing.md"), "# ADR\n")
	if _, err := BuildDecisionsResult(fixtureWorkspace("w", root)); err != nil {
		t.Fatalf("a resolvable adr link failed: %v", err)
	}
}

// TestDecisionsReviewOnlyNeverFails is contract point 4: a decision no check
// can prove is carried and counted, and it never changes an exit code.
func TestDecisionsReviewOnlyNeverFails(t *testing.T) {
	root := decisionsWorkspace(t)
	committedDecisions(t, root, `{"protocolVersion":1,"decisions":[
		{"id":"D-002","statement":"every pull request names what it deletes","settled":"2026-09-03","settledBy":"fdumay","reviewOnly":true},
		{"id":"D-003","statement":"every GitHub artifact is written in English","settled":"2026-09-11","settledBy":"fdumay","reviewOnly":true}]}`)
	writeFixtureFile(t, filepath.Join(root, "infra", "requirements.json"), `{"scaling":{"minInstances":7}}`)

	report, err := BuildDecisionsResult(fixtureWorkspace("w", root))
	if err != nil {
		t.Fatalf("a review-only registry failed the task: %v", err)
	}
	if report.Summary.ReviewOnly != 2 || report.Summary.Enforced != 0 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	// A registry with nothing to prove reads no file at all.
	if report.Summary.FilesChecked != 0 {
		t.Fatalf("a review-only registry walked the tree: %+v", report.Summary)
	}
}

// TestDecisionsFindingsAreOrdered keeps the published diagnostics
// deterministic: two runs over the same tree report the same findings in the
// same sequence, which is what makes a failing gate diffable.
func TestDecisionsFindingsAreOrdered(t *testing.T) {
	root := decisionsWorkspace(t)
	committedDecisions(t, root, scaleToZeroRegistry)
	for _, project := range []string{"zeta", "alpha", "middle"} {
		writeFixtureFile(t, filepath.Join(root, project, "infra", "requirements.json"),
			`{"scaling":{"minInstances":3}}`)
	}
	first, err := BuildDecisionsResult(fixtureWorkspace("w", root))
	if err == nil {
		t.Fatal("three violations did not fail the task")
	}
	paths := make([]string, 0, len(first.Findings))
	for _, finding := range first.Findings {
		paths = append(paths, finding.Path)
	}
	want := []string{"alpha/infra/requirements.json", "middle/infra/requirements.json", "zeta/infra/requirements.json"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("findings order = %v, want %v", paths, want)
	}
	second, _ := BuildDecisionsResult(fixtureWorkspace("w", root))
	if len(second.Findings) != len(first.Findings) {
		t.Fatalf("a second run reported %d findings, the first %d", len(second.Findings), len(first.Findings))
	}
	for i := range first.Findings {
		if second.Findings[i] != first.Findings[i] {
			t.Fatalf("finding %d differs between two runs:\n%+v\n%+v", i, first.Findings[i], second.Findings[i])
		}
	}
}
