package features

import (
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// findingsByField indexes findings by field and fails when two findings share
// one, so a test can state the exact set of fields it expects.
func findingsByField(t *testing.T, findings []diag.Diagnostic) map[string]diag.Diagnostic {
	t.Helper()
	byField := make(map[string]diag.Diagnostic, len(findings))
	for _, finding := range findings {
		if _, exists := byField[finding.Field]; exists {
			t.Fatalf("two findings on field %q: %v", finding.Field, findings)
		}
		byField[finding.Field] = finding
	}
	return byField
}

func assertFields(t *testing.T, byField map[string]diag.Diagnostic, want ...string) {
	t.Helper()
	got := make([]string, 0, len(byField))
	for field := range byField {
		got = append(got, field)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("fields = %q, want %q", got, want)
	}
}

func TestManifestDiagnosticsNameFilePositions(t *testing.T) {
	zeta := Feature{
		ID: "billing/zeta", Type: FeatureTypeFeature, Name: strings.Repeat("x", 513),
		Outcome: "A bounded outcome", Owner: "billing", Target: MaturityCoded,
		Relations: []Relation{
			{Kind: "zz-unknown", Target: "billing/alpha"},
			{Kind: RelationKindDependsOn, Target: "billing/alpha"},
		},
		Requirements: []Requirement{
			{ID: "zeta-requirement", Stage: "unknown-stage", EvidenceKinds: []EvidenceKind{EvidenceKindAttestation}},
			{
				ID: "alpha-requirement", Stage: MaturityCoded,
				EvidenceKinds: []EvidenceKind{"zz-kind", EvidenceKindAttestation},
				Verification:  &VerificationCriterion{Kind: VerificationKindAcceptance, Checks: []string{"zeta-check", "Not A Check"}},
			},
		},
	}
	alpha := basicManifest("billing", "billing/alpha").Features[0]
	manifest := &Manifest{ProtocolVersion: ManifestProtocolVersion, Namespace: "billing", Features: []Feature{zeta, alpha, alpha}}

	byField := findingsByField(t, ValidateManifest(manifest))
	assertFields(t, byField,
		"features[0].name",
		"features[0].relations[0].kind",
		"features[0].requirements[0].stage",
		"features[0].requirements[1].evidenceKinds[0]",
		"features[0].requirements[1].verification.checks[1]",
		"features[2].id",
	)
	if message := byField["features[0].name"].Message; !strings.Contains(message, "512 bytes") {
		t.Errorf("name message does not state its bound: %q", message)
	}
	if message := byField["features[2].id"].Message; !strings.HasSuffix(message, "first declaration is features[1]") {
		t.Errorf("duplicate does not name its first file position: %q", message)
	}

	for _, name := range []string{"outcome", "owner"} {
		feature := alpha
		if name == "outcome" {
			feature.Outcome = strings.Repeat("x", 513)
		} else {
			feature.Owner = strings.Repeat("x", 513)
		}
		findings := ValidateManifest(&Manifest{ProtocolVersion: 1, Namespace: "billing", Features: []Feature{feature}})
		if len(findings) != 1 || findings[0].Field != "features[0]."+name || !strings.Contains(findings[0].Message, "512 bytes") {
			t.Errorf("oversized %s not reported with its bound: %v", name, findings)
		}
	}
}

func TestEvidenceAndReportDiagnosticsNameFilePositions(t *testing.T) {
	late := basicEvidence()
	late.ID, late.Feature, late.Stage = "zeta/evidence", "zeta/feature", "unknown-stage"
	early := basicEvidence()
	evidence := []EvidenceRecord{late, early, early}
	byField := findingsByField(t, ValidateEvidenceDocument(&EvidenceDocument{ProtocolVersion: 1, Evidence: evidence}))
	assertFields(t, byField, "evidence[0].stage", "evidence[2].id")
	if message := byField["evidence[2].id"].Message; !strings.HasSuffix(message, "first declaration is evidence[1]") {
		t.Errorf("duplicate does not name its first file position: %q", message)
	}

	valid := statusObservation("lifecycle", "alpha-check", ObservationPassed)
	invalid := statusObservation("lifecycle", "zeta-check", ObservationPassed)
	invalid.Provenance.Path = "../outside.go"
	report := &VerificationReport{ProtocolVersion: 1, Observations: []VerificationObservation{invalid, valid, valid}}
	byField = findingsByField(t, ValidateVerificationReport(report))
	assertFields(t, byField, "observations[0].provenance.path", "observations[2]")
	if message := byField["observations[2]"].Message; !strings.HasSuffix(message, "first observation is observations[1]") {
		t.Errorf("duplicate does not name its first file position: %q", message)
	}
}

func TestRepositoryDiagnosticsNameFilePositions(t *testing.T) {
	zeta := basicManifest("billing", "billing/zeta").Features[0]
	zeta.Relations = []Relation{
		{Kind: RelationKindParent, Target: "billing/alpha"},
		{Kind: RelationKindDependsOn, Target: "billing/missing"},
	}
	alpha := basicManifest("billing", "billing/alpha").Features[0]
	manifest := &Manifest{ProtocolVersion: 1, Namespace: "billing", Features: []Feature{zeta, alpha}}

	unknown := basicEvidence()
	unknown.ID, unknown.Feature, unknown.Requirement = "billing/zeta-evidence", "billing/zeta", "unknown"
	supported := basicEvidence()
	supported.ID, supported.Feature = "billing/alpha-evidence", "billing/alpha"
	document := &EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{unknown, supported}}

	findings := ValidateRepository(
		[]ManifestSource{{Path: "billing/putnami.features.json", Manifest: manifest}},
		[]EvidenceSource{{Path: "billing/schema/feature-evidence/a.json", Document: document}},
	)
	byField := findingsByField(t, findings)
	assertFields(t, byField,
		"billing/putnami.features.json#features[0].relations[1].target",
		"billing/schema/feature-evidence/a.json#evidence[0].requirement",
	)
}
