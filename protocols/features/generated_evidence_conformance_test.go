package features

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
)

// conformanceGeneratedEvidenceGolden is the shared Go/TypeScript vector. Both
// languages compose the scenario below in their own terms and must reproduce
// these bytes exactly, so a divergence in association rules, derived fields, or
// canonical ordering fails in whichever language drifted.
const conformanceGeneratedEvidenceGolden = "generated-evidence.golden.json"

const conformanceEvidenceBinding = "source-v1:sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea"

// conformanceGeneratedEvidenceScenario is the semantic fixture. It deliberately
// contains one contribution nobody maps and one owned by a dependency, so the
// vector proves what a producer must NOT emit as well as what it must.
func conformanceGeneratedEvidenceScenario() GeneratedEvidenceInput {
	return GeneratedEvidenceInput{
		Issuer: Issuer{Kind: IssuerKindBuild, ID: "putnami.conformance/producer"},
		Source: SourceSelector{
			Root: LocationRootProject, OwnerProject: "conformance.example", Binding: conformanceEvidenceBinding,
		},
		Authored: &Manifest{
			ProtocolVersion: 1, Namespace: "conformance",
			Features: []Feature{{
				ID: "conformance/generated-evidence", Type: FeatureTypeFeature,
				Name:    "Generated feature evidence",
				Outcome: "A build producer proves a feature requirement from an exact technical contribution",
				Owner:   "conformance", Target: MaturityCoded,
				Requirements: []Requirement{{
					ID: "implementation", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindCapability},
				}},
			}},
		},
		Contributions: []capabilities.ContributionIdentity{
			{OwnerProject: "conformance.example", Kind: capabilities.ContributionKindConfig, Key: "receiver"},
			{OwnerProject: "conformance.example", Kind: capabilities.ContributionKindMigration, Subkind: "sql", Key: "events"},
			// Published but unmapped: it must stay out of the document.
			{OwnerProject: "conformance.example", Kind: capabilities.ContributionKindLifecycle, Subkind: "starter", Key: "receiver"},
			// Owned by a dependency: the workload producer cannot claim it.
			{OwnerProject: "go.putnami.dev/http", Kind: capabilities.ContributionKindPackage, Key: "go.putnami.dev/http"},
		},
		Mappings: []GeneratedEvidenceMapping{
			{
				Feature: "conformance/generated-evidence", Requirement: "implementation",
				Kind: capabilities.ContributionKindMigration, Subkind: "sql", Key: "events",
				Provenance: EvidenceProvenance{Root: LocationRootProject, Path: "receiver.go", Symbol: "ReceiverFeature"},
			},
			{
				Feature: "conformance/generated-evidence", Requirement: "implementation",
				Kind: capabilities.ContributionKindConfig, Key: "receiver",
				Provenance: EvidenceProvenance{Root: LocationRootProject, Path: "receiver.go", Symbol: "ReceiverFeature"},
			},
		},
	}
}

func TestGeneratedEvidenceMatchesTheSharedConformanceVector(t *testing.T) {
	document, diagnostics := BuildGeneratedEvidence(conformanceGeneratedEvidenceScenario())
	if document == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("conformance scenario must publish: %v", diagnostics)
	}
	actual, err := MarshalEvidenceDocument(document)
	if err != nil {
		t.Fatalf("marshal conformance document: %v", err)
	}
	path := filepath.Join("fixtures", "equivalence", conformanceGeneratedEvidenceGolden)
	if os.Getenv("PUTNAMI_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, actual, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	expected, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("generated evidence drifted from the shared vector\n--- want ---\n%s\n--- got ---\n%s", expected, actual)
	}
}

func TestGeneratedEvidenceVectorOmitsUnmappedAndDependencyContributions(t *testing.T) {
	document, _ := BuildGeneratedEvidence(conformanceGeneratedEvidenceScenario())
	if document == nil {
		t.Fatal("conformance scenario must publish")
	}
	if len(document.Evidence) != 2 {
		t.Fatalf("only the two mapped contributions may be published, got %d", len(document.Evidence))
	}
	for _, record := range document.Evidence {
		contribution := record.Subject.Contribution
		if contribution == nil {
			t.Fatal("generated capability evidence must carry a contribution")
		}
		if contribution.OwnerProject != "conformance.example" {
			t.Fatalf("producer published a contribution it does not own: %+v", contribution)
		}
		if contribution.Kind == capabilities.ContributionKindLifecycle {
			t.Fatalf("unmapped contribution %+v must stay unclassified", contribution)
		}
	}
}
