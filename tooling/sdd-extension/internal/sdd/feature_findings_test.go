package sdd

import (
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// findingsReferences are the references the fixtures name this extension by.
var findingsReferences = []string{"@putnami/sdd", "/tools/sdd"}

// provenFeature is a coded feature of project with two artifact
// requirements, the second of which no evidence proves.
func provenFeature(id, proven, unproven string, relations ...featureproto.Relation) featureproto.Feature {
	return featureproto.Feature{
		ID: id, Type: featureproto.FeatureTypeFeature, Name: "Feature " + id, Outcome: "Outcome of " + id, Owner: "team", Target: featureproto.MaturityCoded,
		Relations: relations,
		Requirements: []featureproto.Requirement{
			{ID: proven, Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindArtifact}},
			{ID: unproven, Stage: featureproto.MaturityCoded, EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindArtifact}},
		},
	}
}

func writeFeatureManifest(t *testing.T, root, project string, features ...featureproto.Feature) {
	t.Helper()
	data, err := featureproto.MarshalManifest(&featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion, Namespace: strings.Split(features[0].ID, "/")[0], Features: features,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, project, featureproto.ManifestFilename), string(data))
}

// escapingArtifactEvidence writes, under project, evidence for requirement
// whose artifact is a link out of the project's root: the evaluation reads it
// after it assesses, so it reports the escape as an error beside its
// assessment warnings.
func escapingArtifactEvidence(t *testing.T, root, project, owner, feature, requirement string) {
	t.Helper()
	symlinkIn(t, root, "../outside.txt", project+"/leak.txt")
	writeFeatureEvidence(t, filepath.Join(root, project, featureproto.EvidenceDirectory, "proof.json"), featureproto.EvidenceRecord{
		ID: project + "/proof", Feature: feature, Requirement: requirement, Stage: featureproto.MaturityCoded, Outcome: featureproto.EvidenceOutcomeSupports,
		Issuer:     featureproto.Issuer{Kind: featureproto.IssuerKindTest, ID: "evidence-producer"},
		Source:     featureproto.SourceSelector{Root: featureproto.LocationRootProject, OwnerProject: owner, Binding: "source-v1:sha256:" + strings.Repeat("a", 64)},
		Subject:    featureproto.EvidenceSubject{Kind: featureproto.EvidenceKindArtifact, Artifact: &featureproto.ArtifactSubject{Path: "leak.txt", Digest: "sha256:" + strings.Repeat("b", 64)}},
		Provenance: featureproto.EvidenceProvenance{Root: featureproto.LocationRootProject, Path: "proof_test.go"},
	})
}

// findingsWorkspace is a Git work tree of four projects. billing and platform
// author features and name this extension, billing by its name and platform
// by its path, and the evaluation of each reports errors and warnings. docs
// names the extension and authors no feature or spec; legacy authors a
// feature and names another extension. billing's feature relates to
// platform's, across the project boundary.
func findingsWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	root := candidateWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"findings","includes":["billing","platform","docs","legacy"]}`)
	writeFixtureFile(t, filepath.Join(root, "outside.txt"), "not a project's file\n")
	for _, project := range []string{"billing", "platform", "docs", "legacy"} {
		writeFixtureFile(t, filepath.Join(root, project, "putnami.json"), `{"name":"@acme/`+project+`","type":"library"}`)
	}
	writeFeatureManifest(t, root, "billing", provenFeature("billing/invoicing", "issue", "refund",
		featureproto.Relation{Kind: featureproto.RelationKindParent, Target: "platform/commerce"}))
	escapingArtifactEvidence(t, root, "billing", "@acme/billing", "billing/invoicing", "issue")
	writeFeatureManifest(t, root, "platform", provenFeature("platform/commerce", "sell", "ship"))
	escapingArtifactEvidence(t, root, "platform", "@acme/platform", "platform/commerce", "sell")
	writeFixtureFile(t, filepath.Join(root, "docs", "README.md"), "# Docs\n")
	writeFeatureManifest(t, root, "legacy", featureproto.Feature{
		ID: "legacy/report", Type: featureproto.FeatureTypeFeature, Name: "Report", Outcome: "A report", Owner: "team", Target: featureproto.MaturityModeled,
	})
	return workspace.NewWorkspace(root, nil, []*workspace.Project{
		{ID: "/billing", Name: "@acme/billing", Path: "billing", Extensions: []string{"/go/extension", "@putnami/sdd"}},
		{ID: "/docs", Name: "@acme/docs", Path: "docs", Extensions: []string{"@putnami/sdd"}},
		{ID: "/legacy", Name: "@acme/legacy", Path: "legacy", Extensions: []string{"/go/extension"}},
		{ID: "/platform", Name: "@acme/platform", Path: "platform", Extensions: []string{"/tools/sdd"}},
	})
}

// statedFinding is what a fragment states, and what the per-project task's
// diagnostic states when it is located at the file it names.
type statedFinding struct {
	path, symbol, rule, severity, message string
}

func (f statedFinding) String() string {
	return f.severity + " " + f.rule + " " + f.path + "#" + f.symbol + ": " + f.message
}

func statedFindings(fragments []evidence.Fragment) []statedFinding {
	stated := make([]statedFinding, 0, len(fragments))
	for _, fragment := range fragments {
		stated = append(stated, statedFinding{fragment.Subject.Path, fragment.Subject.Symbol, fragment.Rule, fragment.Severity, fragment.Message})
	}
	return stated
}

// perProjectFindings runs the per-project builder for each project and
// locates each diagnostic at the file its field names: the union, without
// repeats, of what the per-project tasks reported.
func perProjectFindings(t *testing.T, ws *workspace.Workspace, projects ...string) []statedFinding {
	t.Helper()
	seen := map[statedFinding]bool{}
	for _, project := range projects {
		report, _ := BuildFeatureTaskValidationResult(ws, Selection{Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{project}})
		for _, finding := range report.Diagnostics {
			path, symbol, _ := strings.Cut(finding.Field, "#")
			seen[statedFinding{path, symbol, finding.Code, string(finding.Severity), finding.Message}] = true
		}
	}
	findings := make([]statedFinding, 0, len(seen))
	for finding := range seen {
		findings = append(findings, finding)
	}
	return findings
}

func sortedFindings(findings []statedFinding) []string {
	lines := make([]string, 0, len(findings))
	for _, finding := range findings {
		lines = append(lines, finding.String())
	}
	sort.Strings(lines)
	return lines
}

// TestTheWorkspaceTaskStatesEveryProjectsFindings pins the producer half of
// the feature validation: one evaluation per project the per-project command
// activates on, and every diagnostic of each stated as a valid finding
// fragment at the workspace file it names, with the rule, severity and
// message of the per-project task's diagnostic, and nothing more.
func TestTheWorkspaceTaskStatesEveryProjectsFindings(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"every-projects-findings-are-stated-at-the-files-they-name")
	ws := findingsWorkspace(t)

	report, fragments, err := BuildFeatureFindingsResult(ws, findingsReferences, 4)
	if err != nil {
		t.Fatalf("BuildFeatureFindingsResult: %v", err)
	}
	// docs authors no feature or spec and legacy does not name the extension:
	// the per-project command activated on neither.
	if want := []string{"/billing", "/platform"}; !slices.Equal(report.Projects, want) {
		t.Fatalf("validated projects = %v, want %v", report.Projects, want)
	}

	got := sortedFindings(statedFindings(fragments))
	want := sortedFindings(perProjectFindings(t, ws, "/billing", "/platform"))
	if !slices.Equal(got, want) {
		t.Fatalf("stated findings differ from the per-project tasks' diagnostics.\n got:\n  %s\n want:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// Each project's evaluation reports errors and warnings, each located in a
	// file of that project, so the fixture measures both severities.
	for _, project := range []string{"billing", "platform"} {
		var errors, warnings int
		for _, fragment := range fragments {
			if !strings.HasPrefix(fragment.Subject.Path, project+"/") {
				continue
			}
			switch fragment.Severity {
			case evidence.FindingError:
				errors++
			case evidence.FindingWarning:
				warnings++
			}
		}
		if errors == 0 || warnings == 0 {
			t.Errorf("%s holds %d error and %d warning finding(s), want both", project, errors, warnings)
		}
	}
	if report.Findings.Errors == 0 || report.Findings.Warnings == 0 || report.Findings.Errors+report.Findings.Warnings+report.Findings.Info != len(fragments) {
		t.Errorf("finding counts = %+v for %d fragment(s)", report.Findings, len(fragments))
	}

	// The fragments are a file the strict reader accepts, workspace-relative,
	// in the order they were returned, and the same bytes on a second build.
	encoded, err := evidence.EncodeFragments(fragments)
	if err != nil {
		t.Fatalf("EncodeFragments: %v", err)
	}
	parsed, diagnostics := evidence.ParseFragments(encoded)
	if diagnostics != nil || len(parsed) != len(fragments) {
		t.Fatalf("ParseFragments = %d fragment(s), %v", len(parsed), diagnostics)
	}
	for _, fragment := range parsed {
		if fragment.Kind != evidence.FragmentKindFinding || fragment.Source.Format != FeatureFindingsFormat ||
			filepath.IsAbs(fragment.Subject.Path) || strings.Contains(fragment.Subject.Path, ws.Root) {
			t.Errorf("fragment = %+v, want a putnami-sdd finding at a workspace-relative path", fragment)
		}
	}
	// The bytes do not depend on how many evaluations run at once.
	for _, workers := range []int{1, 2, 16} {
		againReport, again, err := BuildFeatureFindingsResult(ws, findingsReferences, workers)
		if err != nil {
			t.Fatal(err)
		}
		if second, _ := evidence.EncodeFragments(again); string(second) != string(encoded) || !reflect.DeepEqual(againReport, report) {
			t.Fatalf("a build with %d worker(s) over the same tree answered otherwise", workers)
		}
	}
}

// TestWorkspaceWideFindingsAreStatedOnceAtTheFileTheyName pins the checks
// whose finding one evaluation reports about another project's file: the
// duplicate declaration of a feature id, and an authored manifest that does
// not parse, which every evaluation reports. Each is stated once, at the file
// it names, so the CLI counts it for the project that owns that file.
func TestWorkspaceWideFindingsAreStatedOnceAtTheFileTheyName(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"every-projects-findings-are-stated-at-the-files-they-name")
	root := candidateWorkspace(t)
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"findings","includes":["billing","platform","broken"]}`)
	modeled := func(id string) featureproto.Feature {
		return featureproto.Feature{ID: id, Type: featureproto.FeatureTypeFeature, Name: "Feature " + id, Outcome: "Outcome of " + id, Owner: "team", Target: featureproto.MaturityModeled}
	}
	writeFeatureManifest(t, root, "billing", modeled("billing/invoicing"))
	// Two projects may author one namespace, and each may then declare an id
	// the other declares.
	writeFeatureManifest(t, root, "platform", modeled("billing/invoicing"))
	writeFixtureFile(t, filepath.Join(root, "broken", featureproto.ManifestFilename), `{"protocolVersion": 1,`)
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{
		{ID: "/billing", Name: "billing", Path: "billing", Extensions: []string{"@putnami/sdd"}},
		{ID: "/broken", Name: "broken", Path: "broken"},
		{ID: "/platform", Name: "platform", Path: "platform", Extensions: []string{"@putnami/sdd"}},
	})

	// billing's own task reported both, though neither names a file of it.
	billing, _ := BuildFeatureTaskValidationResult(ws, Selection{Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/billing"}})
	var fields []string
	for _, finding := range billing.Diagnostics {
		if finding.Severity == diag.Error {
			fields = append(fields, finding.Code+" "+finding.Field)
		}
	}
	sort.Strings(fields)
	wantFields := []string{
		featureproto.ErrorCodeDuplicateFeature + " platform/putnami.features.json#features[0].id",
		featureproto.ErrorCodeParseError + " broken/putnami.features.json",
	}
	sort.Strings(wantFields)
	if !slices.Equal(fields, wantFields) {
		t.Fatalf("billing's own errors = %v, want %v", fields, wantFields)
	}

	_, fragments, err := BuildFeatureFindingsResult(ws, findingsReferences, 4)
	if err != nil {
		t.Fatal(err)
	}
	var stated []string
	for _, fragment := range fragments {
		if strings.HasPrefix(fragment.Subject.Path, "billing/") {
			t.Errorf("a finding is located in billing, which holds no error: %+v", fragment)
		}
		if fragment.Severity == evidence.FindingError {
			stated = append(stated, fragment.Rule+" "+fragment.Subject.Path+"#"+fragment.Subject.Symbol)
		}
	}
	want := []string{
		featureproto.ErrorCodeParseError + " broken/putnami.features.json#",
		featureproto.ErrorCodeDuplicateFeature + " platform/putnami.features.json#features[0].id",
	}
	if !slices.Equal(stated, want) {
		t.Fatalf("stated errors = %v, want each once at the file it names: %v", stated, want)
	}
}

// TestACleanWorkspaceStatesNoFinding pins the empty file: a workspace whose
// evaluations report nothing gives no fragment, and its file holds no byte.
func TestACleanWorkspaceStatesNoFinding(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"every-projects-findings-are-stated-at-the-files-they-name")
	root := candidateWorkspace(t)
	writeFeatureManifest(t, root, "billing", featureproto.Feature{
		ID: "billing/invoicing", Type: featureproto.FeatureTypeFeature, Name: "Invoicing", Outcome: "Invoices", Owner: "team", Target: featureproto.MaturityModeled,
	})
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{{ID: "/billing", Name: "billing", Path: "billing", Extensions: []string{"@putnami/sdd"}}})
	report, fragments, err := BuildFeatureFindingsResult(ws, findingsReferences, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragments) != 0 || report.Summary.Features != 1 || !slices.Equal(report.Projects, []string{"/billing"}) {
		t.Fatalf("report = %+v, fragments = %+v; want one feature assessed and no finding", report, fragments)
	}
	encoded, err := evidence.EncodeFragments(fragments)
	if err != nil || len(encoded) != 0 {
		t.Fatalf("EncodeFragments = %q, %v; want an empty file", encoded, err)
	}
}

// TestAFindingThatNamesNoFileIsPlacedAtTheFileItIsAbout pins the subjects of
// the engine's fields that are not a path.
func TestAFindingThatNamesNoFileIsPlacedAtTheFileItIsAbout(t *testing.T) {
	project := &workspace.Project{ID: "/libs/billing", Path: "libs/billing"}
	roots := map[string]bool{"libs/billing": true, "libs/ledger": true, "": true}
	for _, tc := range []struct {
		code, field string
		want        evidence.FragmentSubject
	}{
		{featureproto.WarningCodeMissingEvidence, "libs/billing/putnami.features.json#features[0].requirements[1]",
			evidence.FragmentSubject{Path: "libs/billing/putnami.features.json", Symbol: "features[0].requirements[1]"}},
		{featureproto.ErrorCodeParseError, "libs/ledger/features/export.json",
			evidence.FragmentSubject{Path: "libs/ledger/features/export.json"}},
		{featureproto.ErrorCodeSensitiveContent, "libs/billing/" + featureproto.EvidenceDirectory,
			evidence.FragmentSubject{Path: "libs/billing/" + featureproto.EvidenceDirectory}},
		{featureproto.WarningCodeUnresolvedFeatureAuthority, "libs/ledger",
			evidence.FragmentSubject{Path: "libs/ledger/putnami.json", Symbol: "featureAuthority"}},
		{featureproto.WarningCodeUnresolvedFeatureAuthority, "",
			evidence.FragmentSubject{Path: "putnami.json", Symbol: "featureAuthority"}},
		{featureproto.ErrorCodeInvalidPath, "projects",
			evidence.FragmentSubject{Path: "putnami.workspace.json", Symbol: "projects"}},
		{featureproto.ErrorCodeInvalidSubject, "revision.kind",
			evidence.FragmentSubject{Path: "putnami.workspace.json", Symbol: "revision.kind"}},
		{"capabilities.parse_error", "capabilities#contribution",
			evidence.FragmentSubject{Path: "libs/billing/putnami.features.json", Symbol: "capabilities#contribution"}},
		{featureproto.ErrorCodeInvalidPath, "../escape.json",
			evidence.FragmentSubject{Path: "libs/billing/putnami.features.json", Symbol: "../escape.json"}},
	} {
		got := featureFindingSubject(diag.Diagnostic{Severity: diag.Warning, Code: tc.code, Field: tc.field}, project, roots)
		if got != tc.want {
			t.Errorf("subject of %s %q = %+v, want %+v", tc.code, tc.field, got, tc.want)
		}
		if diags := evidence.ValidateFragment("fragment", featureFinding(diag.Diagnostic{Severity: diag.Warning, Code: tc.code, Field: tc.field, Message: "m"}, project, roots)); diags != nil {
			t.Errorf("fragment for %q is invalid: %v", tc.field, diags)
		}
	}
}

// TestTheMemoReaderAnswersCopies pins that a caller sorting an answer of the
// shared reader changes no later answer.
func TestTheMemoReaderAnswersCopies(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "billing", "features", "b.json"), "{}")
	writeFixtureFile(t, filepath.Join(root, "billing", "features", "a.json"), "{}")
	cut, err := featureengine.NewTaskReaderFrom(nil, root)
	if err != nil {
		t.Fatal(err)
	}
	reader := newMemoReader(cut)
	first, err := reader.ReadDir("billing", "features")
	if err != nil || len(first) != 2 {
		t.Fatalf("ReadDir = %v, %v", first, err)
	}
	original := first[0].Name
	first[0].Name = "changed"
	data, _ := reader.ReadFile("billing", "features/a.json")
	data[0] = 'x'
	second, _ := reader.ReadDir("billing", "features")
	again, _ := reader.ReadFile("billing", "features/a.json")
	if second[0].Name != original || string(again) != "{}" {
		t.Fatalf("answers after a caller's edit = %v, %q", second, again)
	}
}
