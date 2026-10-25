package sdd

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const coverageFeatureManifest = `{
  "protocolVersion": 1,
  "namespace": "demo",
  "features": [{"id":"demo/outcome","type":"feature","name":"Outcome","outcome":"A user gets an outcome","owner":"demo","target":"modeled"}]
}`

const coverageSpec = `{
  "protocolVersion": 1,
  "feature": "demo/outcome",
  "outcomes": ["A user gets an outcome"],
  "requirements": []
}`

// noFeatureReason is the reason a fixture's featureAuthority.none gives.
const noFeatureReason = "a wire contract is not a user outcome"

// coverageWorkspace is the fixture the coverage tests judge: /featured
// declares the feature demo/outcome, /specced hosts a spec, and /wire has no
// document of its own, only the featureAuthority passed in. extra members join
// the three.
func coverageWorkspace(t *testing.T, authority *wsproto.ProjectFeatureAuthority, extra ...*workspace.Project) *workspace.Workspace {
	t.Helper()
	root := decisionsWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "featured", featureproto.ManifestFilename), coverageFeatureManifest)
	writeFixtureFile(t, filepath.Join(root, "specced", featureproto.SpecDirectory, "outcome.json"), coverageSpec)
	writeFixtureFile(t, filepath.Join(root, "wire", "README.md"), "# wire\n")
	wire := appProject("wire", "wire")
	wire.Config = &wsproto.ProjectConfig{Name: "wire", FeatureAuthority: authority}
	projects := append([]*workspace.Project{appProject("featured", "featured"), appProject("specced", "specced"), wire}, extra...)
	return fixtureWorkspace("w", root, projects...)
}

// coverageOptions is the option blocks of a workspace that adopts the gate
// with the debt list given.
func coverageOptions(debt ...any) map[string]map[string]any {
	return map[string]map[string]any{"sdd": {FeatureCoverageOption: map[string]any{"debt": append([]any{}, debt...)}}}
}

// coverageFindings renders each finding as "<code> <field>".
func coverageFindings(report FeatureCoverageReport) []string {
	findings := make([]string, 0, len(report.Diagnostics))
	for _, finding := range report.Diagnostics {
		findings = append(findings, finding.Code+" "+finding.Field)
	}
	return findings
}

// TestFeatureCoverageUncoveredProjectFailsNamingIt is the acceptance line for
// a project that answers nothing: it fails naming the project on the file a
// reader opens, and the debt list is the one way to defer it.
func TestFeatureCoverageUncoveredProjectFailsNamingIt(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-project-carries-feature-coverage", "an-uncovered-project-fails-naming-it")
	ws := coverageWorkspace(t, nil)

	report, err := BuildFeatureCoverageResult(ws, coverageOptions())
	if err == nil {
		t.Fatal("an uncovered project did not fail the task")
	}
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Errorf("error = %v, want it classified as invalid configuration", err)
	}
	if len(report.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one", report.Diagnostics)
	}
	finding := report.Diagnostics[0]
	if finding.Code != ErrorCodeUncoveredProject || finding.Field != "wire/putnami.json#featureAuthority" ||
		!strings.Contains(finding.Message, "project /wire ") {
		t.Fatalf("finding = %+v, want /wire named on its putnami.json", finding)
	}
	if report.Projects != 3 || report.FeatureOrSpec != 2 || report.Authority != 0 || !reflect.DeepEqual(report.Uncovered, []string{"/wire"}) {
		t.Fatalf("report = %+v, want 3 projects, 2 with a feature or spec, and /wire uncovered", report)
	}

	deferred, err := BuildFeatureCoverageResult(ws, coverageOptions("/wire"))
	if err != nil {
		t.Fatalf("a listed debt failed the task: %v (%+v)", err, deferred.Diagnostics)
	}
	if !deferred.Adopted || !reflect.DeepEqual(deferred.Debt, []string{"/wire"}) || !reflect.DeepEqual(deferred.Uncovered, []string{"/wire"}) {
		t.Fatalf("report = %+v, want /wire uncovered and in debt", deferred)
	}
}

// TestFeatureCoverageReviewedAuthorityCovers keeps both reviewed answers
// green: none with its reason, and an owner that names an authored feature.
func TestFeatureCoverageReviewedAuthorityCovers(t *testing.T) {
	for _, authority := range []*wsproto.ProjectFeatureAuthority{{None: noFeatureReason}, {Owner: "demo/outcome"}} {
		report, err := BuildFeatureCoverageResult(coverageWorkspace(t, authority), coverageOptions())
		if err != nil {
			t.Fatalf("featureAuthority %+v failed the task: %v (%+v)", *authority, err, report.Diagnostics)
		}
		if report.Authority != 1 || report.FeatureOrSpec != 2 || len(report.Uncovered) != 0 {
			t.Fatalf("featureAuthority %+v: report = %+v, want /wire covered by its authority", *authority, report)
		}
	}
}

// TestFeatureCoverageCoveredDebtEntryFails is what makes the list shrink: an
// entry whose project answers fails until the entry is removed.
func TestFeatureCoverageCoveredDebtEntryFails(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-project-carries-feature-coverage", "a-covered-debt-entry-fails")
	ws := coverageWorkspace(t, &wsproto.ProjectFeatureAuthority{None: noFeatureReason})

	report, err := BuildFeatureCoverageResult(ws, coverageOptions("/featured", "/specced", "/wire"))
	if err == nil {
		t.Fatal("covered debt entries did not fail the task")
	}
	const debt = "putnami.workspace.json#options.sdd.featureCoverage.debt"
	want := []string{
		ErrorCodeStaleCoverageDebt + " " + debt + "[0]",
		ErrorCodeStaleCoverageDebt + " " + debt + "[1]",
		ErrorCodeStaleCoverageDebt + " " + debt + "[2]",
	}
	if got := coverageFindings(report); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	if message := report.Diagnostics[0].Message; !strings.HasPrefix(message, "/featured ") || !strings.Contains(message, "remove the entry") {
		t.Errorf("message = %q, want /featured named and the fix", message)
	}
	if message := report.Diagnostics[2].Message; !strings.HasPrefix(message, "/wire ") || !strings.Contains(message, "featureAuthority") {
		t.Errorf("message = %q, want /wire named and its featureAuthority as the answer", message)
	}
}

// TestFeatureCoverageUnknownDebtEntryFails keeps a renamed or deleted project
// from leaving a dead entry behind.
func TestFeatureCoverageUnknownDebtEntryFails(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-project-carries-feature-coverage", "an-unknown-debt-entry-fails")
	ws := coverageWorkspace(t, &wsproto.ProjectFeatureAuthority{None: noFeatureReason})

	report, err := BuildFeatureCoverageResult(ws, coverageOptions("/cloud/runtime/go"))
	if err == nil {
		t.Fatal("an unknown debt entry did not fail the task")
	}
	if len(report.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one", report.Diagnostics)
	}
	finding := report.Diagnostics[0]
	if finding.Code != ErrorCodeUnknownCoverageDebt || finding.Field != "putnami.workspace.json#options.sdd.featureCoverage.debt[0]" ||
		!strings.Contains(finding.Message, "/cloud/runtime/go") {
		t.Fatalf("finding = %+v, want the unknown id named on its entry", finding)
	}
}

// TestFeatureCoverageUnresolvableOwnerDoesNotCover keeps featureAuthority from
// being the cheapest way to silence the gate: an owner that names no authored
// feature covers nothing, and the finding names the owner.
func TestFeatureCoverageUnresolvableOwnerDoesNotCover(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-project-carries-feature-coverage", "an-unresolvable-owner-does-not-cover")
	ws := coverageWorkspace(t, &wsproto.ProjectFeatureAuthority{Owner: "demo/does-not-exist"})

	report, err := BuildFeatureCoverageResult(ws, coverageOptions())
	if err == nil {
		t.Fatal("an unresolvable owner covered its project")
	}
	if len(report.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %+v, want exactly one", report.Diagnostics)
	}
	finding := report.Diagnostics[0]
	if finding.Code != ErrorCodeUncoveredProject || finding.Field != "wire/putnami.json#featureAuthority" ||
		!strings.Contains(finding.Message, `"demo/does-not-exist"`) {
		t.Fatalf("finding = %+v, want the dangling owner named on /wire's putnami.json", finding)
	}
	if report.Authority != 0 || !reflect.DeepEqual(report.Uncovered, []string{"/wire"}) {
		t.Fatalf("report = %+v, want /wire uncovered", report)
	}
}

// TestFeatureCoverageJudgesAProjectWithoutPutnamiJSON keeps the gate on every
// member the wire carries. A module that a language extension discovers has
// no putnami.json, so no featureAuthority: it answers with a feature or a
// spec of its own, or fails.
func TestFeatureCoverageJudgesAProjectWithoutPutnamiJSON(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-project-carries-feature-coverage", "a-project-without-putnami-json-is-judged")
	ws := coverageWorkspace(t, &wsproto.ProjectFeatureAuthority{None: noFeatureReason}, appProject("module", "module"))

	report, err := BuildFeatureCoverageResult(ws, coverageOptions())
	if err == nil {
		t.Fatal("a member without a putnami.json was not judged")
	}
	if got, want := coverageFindings(report), []string{ErrorCodeUncoveredProject + " module/putnami.json#featureAuthority"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}

	writeFixtureFile(t, filepath.Join(ws.Root, "module", featureproto.SpecDirectory, "module.json"),
		strings.Replace(coverageSpec, "demo/outcome", "demo/module", 1))
	report, err = BuildFeatureCoverageResult(ws, coverageOptions())
	if err != nil {
		t.Fatalf("a member with a spec of its own failed the task: %v (%+v)", err, report.Diagnostics)
	}
	if report.Projects != 4 || report.FeatureOrSpec != 3 || report.Authority != 1 {
		t.Fatalf("report = %+v, want /module covered by its spec", report)
	}
}

// TestFeatureCoverageAbsentOptionIsAdoption keeps a workspace that has not
// opted in green, and tells it what the gate would find.
func TestFeatureCoverageAbsentOptionIsAdoption(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-project-carries-feature-coverage", "an-absent-option-is-adoption")
	ws := coverageWorkspace(t, nil)
	for name, options := range map[string]map[string]map[string]any{
		"no option block":                 nil,
		"an sdd block without the member": {"sdd": {"owners": []any{"@lead"}}},
	} {
		report, err := BuildFeatureCoverageResult(ws, options)
		if err != nil {
			t.Fatalf("%s: an unadopted gate failed the task: %v", name, err)
		}
		if report.Adopted || len(report.Diagnostics) != 0 || !reflect.DeepEqual(report.Uncovered, []string{"/wire"}) {
			t.Fatalf("%s: report = %+v, want an unadopted judgment that lists /wire", name, report)
		}
		if !strings.Contains(report.Note, "not adopted") || !strings.Contains(report.Note, "1 project(s)") {
			t.Errorf("%s: note = %q, want the opt-in and the count", name, report.Note)
		}
	}
}

// TestFeatureCoverageMalformedBlockFailsClosed pins that the step never
// guesses a list it cannot read: the list decides which projects fail.
func TestFeatureCoverageMalformedBlockFailsClosed(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "every-project-carries-feature-coverage", "a-malformed-list-fails-closed")
	ws := coverageWorkspace(t, nil)
	const block = "putnami.workspace.json#options.sdd.featureCoverage"
	for _, test := range []struct {
		name     string
		coverage any
		field    string
		message  string
	}{
		{"not an object", []any{"/wire"}, block, "must be an object"},
		{"an unknown member", map[string]any{"debt": []any{}, "allow": []any{}}, block, `unknown member "allow"`},
		{"no debt member", map[string]any{}, block + ".debt", "is required"},
		{"a debt that is not an array", map[string]any{"debt": "/wire"}, block + ".debt", "must be an array"},
		{"a null entry", map[string]any{"debt": []any{nil}}, block + ".debt[0]", "is null"},
		{"a number entry", map[string]any{"debt": []any{float64(1)}}, block + ".debt[0]", "is a number"},
		{"a relative entry", map[string]any{"debt": []any{"wire"}}, block + ".debt[0]", `is "wire"`},
		{"the root", map[string]any{"debt": []any{"/"}}, block + ".debt[0]", `is "/"`},
		{"a padded entry", map[string]any{"debt": []any{"/wire "}}, block + ".debt[0]", `is "/wire "`},
		{"a duplicate entry", map[string]any{"debt": []any{"/wire", "/wire"}}, block + ".debt[1]", "lists /wire twice"},
	} {
		t.Run(test.name, func(t *testing.T) {
			report, err := BuildFeatureCoverageResult(ws, map[string]map[string]any{"sdd": {FeatureCoverageOption: test.coverage}})
			if err == nil {
				t.Fatal("a malformed block did not fail the task")
			}
			if !errors.Is(err, protocolcli.ErrInvalidConfig) {
				t.Errorf("error = %v, want it classified as invalid configuration", err)
			}
			if len(report.Diagnostics) != 1 {
				t.Fatalf("diagnostics = %+v, want exactly one", report.Diagnostics)
			}
			finding := report.Diagnostics[0]
			if finding.Code != ErrorCodeInvalidFeatureCoverage || finding.Field != test.field || !strings.Contains(finding.Message, test.message) {
				t.Fatalf("finding = %+v, want %s on %s saying %q", finding, ErrorCodeInvalidFeatureCoverage, test.field, test.message)
			}
			if report.Adopted || report.Projects != 0 {
				t.Errorf("report = %+v, want no judgment from an unreadable list", report)
			}
		})
	}
}
