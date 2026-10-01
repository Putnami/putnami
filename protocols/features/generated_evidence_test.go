package features

import (
	"bytes"
	"slices"
	"testing"

	"go.putnami.dev/protocol/capabilities"
)

const proofProject = "telemetry.example"

func proofManifest() *Manifest {
	return &Manifest{ProtocolVersion: 1, Namespace: "billing", Features: []Feature{{
		ID: "billing/invoice-export", Type: FeatureTypeFeature, Name: "Invoice export",
		Outcome: "A customer exports an issued invoice", Owner: "billing", Target: MaturityModeled,
		Requirements: []Requirement{{
			ID: "implementation", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindCapability},
		}},
	}}}
}

func proofContribution() capabilities.ContributionIdentity {
	return capabilities.ContributionIdentity{
		OwnerProject: proofProject, Kind: capabilities.ContributionKindMigration, Subkind: "sql", Key: "invoices",
	}
}

func proofMapping() GeneratedEvidenceMapping {
	return GeneratedEvidenceMapping{
		Feature: "billing/invoice-export", Requirement: "implementation",
		Kind: capabilities.ContributionKindMigration, Subkind: "sql", Key: "invoices",
		Provenance: EvidenceProvenance{Root: LocationRootProject, Path: "billing.go", Symbol: "BillingFeature"},
	}
}

func proofInput(mappings ...GeneratedEvidenceMapping) GeneratedEvidenceInput {
	return GeneratedEvidenceInput{
		Issuer:        Issuer{Kind: IssuerKindBuild, ID: "go.putnami.dev/app"},
		Source:        SourceSelector{Root: LocationRootProject, OwnerProject: proofProject, Binding: testBinding},
		Authored:      proofManifest(),
		Contributions: []capabilities.ContributionIdentity{proofContribution()},
		Mappings:      mappings,
	}
}

func TestBuildGeneratedEvidenceWithoutMappingsPublishesNoRecord(t *testing.T) {
	document, diagnostics := BuildGeneratedEvidence(proofInput())
	if document == nil {
		t.Fatalf("empty mappings must still produce a valid document: %v", diagnostics)
	}
	if len(document.Evidence) != 0 {
		t.Fatalf("no mapping must publish no evidence, got %d records", len(document.Evidence))
	}
	if len(diagnostics) != 0 {
		t.Fatalf("no mapping must report nothing, got %v", diagnostics)
	}
}

func TestBuildGeneratedEvidenceDerivesEveryFieldFromTheProducer(t *testing.T) {
	document, diagnostics := BuildGeneratedEvidence(proofInput(proofMapping()))
	if document == nil {
		t.Fatalf("exact mapping must publish: %v", diagnostics)
	}
	if len(document.Evidence) != 1 {
		t.Fatalf("exact mapping must publish exactly one record, got %d", len(document.Evidence))
	}
	record := document.Evidence[0]
	if record.Stage != MaturityCoded {
		t.Fatalf("stage must come from the authored requirement, got %q", record.Stage)
	}
	if record.Outcome != EvidenceOutcomeSupports {
		t.Fatalf("generated evidence supports its requirement, got %q", record.Outcome)
	}
	if record.Issuer.Kind != IssuerKindBuild || record.Issuer.ID != "go.putnami.dev/app" {
		t.Fatalf("issuer must be the producer, got %+v", record.Issuer)
	}
	if record.Source.Binding != testBinding || record.Source.OwnerProject != proofProject {
		t.Fatalf("source must be the producer's computed binding, got %+v", record.Source)
	}
	if record.Subject.Contribution == nil || *record.Subject.Contribution != proofContribution() {
		t.Fatalf("subject must be the exact published contribution, got %+v", record.Subject)
	}
	if record.ID != GeneratedEvidenceID("billing/invoice-export", "implementation", proofContribution()) {
		t.Fatalf("record ID must be the derived identity, got %q", record.ID)
	}
}

func TestGeneratedEvidenceIDIsCanonicalAndContributionSpecific(t *testing.T) {
	first := GeneratedEvidenceID("billing/invoice-export", "implementation", proofContribution())
	if !featureIDPattern.MatchString(first) {
		t.Fatalf("generated evidence ID %q is not a canonical stable ID", first)
	}
	other := proofContribution()
	other.Key = "receipts"
	if second := GeneratedEvidenceID("billing/invoice-export", "implementation", other); second == first {
		t.Fatalf("two contributions must not share one evidence ID (%q)", first)
	}
}

func TestBuildGeneratedEvidenceIgnoresMappingOrder(t *testing.T) {
	second := proofMapping()
	second.Key = "receipts"
	second.Provenance.Symbol = "ReceiptFeature"
	contributions := []capabilities.ContributionIdentity{proofContribution(), {
		OwnerProject: proofProject, Kind: capabilities.ContributionKindMigration, Subkind: "sql", Key: "receipts",
	}}

	forward := proofInput(proofMapping(), second)
	forward.Contributions = contributions
	reverse := proofInput(second, proofMapping())
	reverse.Contributions = []capabilities.ContributionIdentity{contributions[1], contributions[0]}

	left, leftFindings := BuildGeneratedEvidence(forward)
	right, rightFindings := BuildGeneratedEvidence(reverse)
	if left == nil || right == nil {
		t.Fatalf("both orders must publish: %v / %v", leftFindings, rightFindings)
	}
	// Assert the returned records, not only the marshaled bytes:
	// MarshalEvidenceDocument canonicalizes on its way out, so comparing bytes
	// alone would pass even if the builder returned map-iteration order.
	leftIDs, rightIDs := recordIDs(left), recordIDs(right)
	if len(leftIDs) != 2 {
		t.Fatalf("want two records, got %v", leftIDs)
	}
	if !slices.Equal(leftIDs, rightIDs) {
		t.Fatalf("declaration order changed the record order: %v vs %v", leftIDs, rightIDs)
	}
	if !slices.IsSorted(leftIDs) {
		t.Fatalf("records must be emitted in canonical order, got %v", leftIDs)
	}
	leftBytes, err := MarshalEvidenceDocument(left)
	if err != nil {
		t.Fatalf("marshal forward document: %v", err)
	}
	rightBytes, err := MarshalEvidenceDocument(right)
	if err != nil {
		t.Fatalf("marshal reverse document: %v", err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatalf("declaration order changed the bytes\n--- forward ---\n%s\n--- reverse ---\n%s", leftBytes, rightBytes)
	}
}

func recordIDs(document *EvidenceDocument) []string {
	ids := make([]string, 0, len(document.Evidence))
	for _, record := range document.Evidence {
		ids = append(ids, record.ID)
	}
	return ids
}

func TestBuildGeneratedEvidenceCoalescesIdenticalMappings(t *testing.T) {
	document, diagnostics := BuildGeneratedEvidence(proofInput(proofMapping(), proofMapping()))
	if document == nil {
		t.Fatalf("byte-identical duplicates must coalesce: %v", diagnostics)
	}
	if len(document.Evidence) != 1 {
		t.Fatalf("byte-identical duplicates must fold into one record, got %d", len(document.Evidence))
	}
}

func TestBuildGeneratedEvidenceRefusesContradictoryDuplicateMappings(t *testing.T) {
	conflicting := proofMapping()
	conflicting.Provenance.Symbol = "AnotherDeclaration"
	document, diagnostics := BuildGeneratedEvidence(proofInput(proofMapping(), conflicting))
	if document != nil {
		t.Fatal("two mappings that disagree on provenance must not publish")
	}
	if !hasCode(diagnostics, ErrorCodeDuplicateEvidence) {
		t.Fatalf("want %s, got %v", ErrorCodeDuplicateEvidence, diagnostics)
	}
}

func TestBuildGeneratedEvidenceRefusesUnknownFeature(t *testing.T) {
	mapping := proofMapping()
	mapping.Feature = "billing/unknown"
	document, diagnostics := BuildGeneratedEvidence(proofInput(mapping))
	if document != nil || !hasCode(diagnostics, ErrorCodeUnknownFeature) {
		t.Fatalf("want %s, got %v", ErrorCodeUnknownFeature, diagnostics)
	}
}

func TestBuildGeneratedEvidenceRefusesAMappingWithoutAnAuthoredManifest(t *testing.T) {
	input := proofInput(proofMapping())
	input.Authored = nil
	document, diagnostics := BuildGeneratedEvidence(input)
	if document != nil || !hasCode(diagnostics, ErrorCodeUnknownFeature) {
		t.Fatalf("want %s, got %v", ErrorCodeUnknownFeature, diagnostics)
	}
}

func TestBuildGeneratedEvidenceRefusesUnknownRequirement(t *testing.T) {
	mapping := proofMapping()
	mapping.Requirement = "not-declared"
	document, diagnostics := BuildGeneratedEvidence(proofInput(mapping))
	if document != nil || !hasCode(diagnostics, ErrorCodeUnknownRequirement) {
		t.Fatalf("want %s, got %v", ErrorCodeUnknownRequirement, diagnostics)
	}
}

func TestBuildGeneratedEvidenceRefusesAModeledRequirementStage(t *testing.T) {
	input := proofInput(proofMapping())
	input.Authored.Features[0].Requirements[0].Stage = MaturityModeled
	document, diagnostics := BuildGeneratedEvidence(input)
	if document != nil || !hasCode(diagnostics, ErrorCodeInvalidStage) {
		t.Fatalf("want %s, got %v", ErrorCodeInvalidStage, diagnostics)
	}
}

func TestBuildGeneratedEvidenceRefusesARequirementThatRejectsCapabilityEvidence(t *testing.T) {
	input := proofInput(proofMapping())
	input.Authored.Features[0].Requirements[0].EvidenceKinds = []EvidenceKind{EvidenceKindAttestation}
	document, diagnostics := BuildGeneratedEvidence(input)
	if document != nil || !hasCode(diagnostics, ErrorCodeInvalidSubject) {
		t.Fatalf("want %s, got %v", ErrorCodeInvalidSubject, diagnostics)
	}
}

func TestBuildGeneratedEvidenceRefusesAnUnpublishedContribution(t *testing.T) {
	mapping := proofMapping()
	mapping.Key = "never-emitted"
	document, diagnostics := BuildGeneratedEvidence(proofInput(mapping))
	if document != nil || !hasCode(diagnostics, capabilities.ErrorCodeUnresolvedReference) {
		t.Fatalf("want %s, got %v", capabilities.ErrorCodeUnresolvedReference, diagnostics)
	}
}

func TestBuildGeneratedEvidenceCannotClaimAnotherProjectsContribution(t *testing.T) {
	input := proofInput(proofMapping())
	input.Contributions = []capabilities.ContributionIdentity{{
		OwnerProject: "go.putnami.dev/database", Kind: capabilities.ContributionKindMigration, Subkind: "sql", Key: "invoices",
	}}
	document, diagnostics := BuildGeneratedEvidence(input)
	if document != nil || !hasCode(diagnostics, capabilities.ErrorCodeUnresolvedReference) {
		t.Fatalf("a dependency-owned contribution must not resolve, got %v", diagnostics)
	}
}

// TestBuildGeneratedEvidenceReadsTheStageFromTheRequirement uses a stage other
// than coded on purpose: a producer that hardcoded "coded" would satisfy every
// other test in this file, because coded is the only stage the rest of them
// declare.
func TestBuildGeneratedEvidenceReadsTheStageFromTheRequirement(t *testing.T) {
	input := proofInput(proofMapping())
	input.Authored.Features[0].Target = MaturityWired
	input.Authored.Features[0].Requirements[0].Stage = MaturityWired
	document, diagnostics := BuildGeneratedEvidence(input)
	if document == nil {
		t.Fatalf("a wired requirement must publish: %v", diagnostics)
	}
	if document.Evidence[0].Stage != MaturityWired {
		t.Fatalf("stage must come from the authored requirement, got %q", document.Evidence[0].Stage)
	}
}

func TestBuildGeneratedEvidenceRefusesANonProducerIssuer(t *testing.T) {
	input := proofInput(proofMapping())
	input.Issuer = Issuer{Kind: IssuerKindFramework, ID: "go.putnami.dev/app"}
	document, diagnostics := BuildGeneratedEvidence(input)
	if document != nil || !hasCode(diagnostics, ErrorCodeInvalidAuthority) {
		t.Fatalf("want %s, got %v", ErrorCodeInvalidAuthority, diagnostics)
	}
}

func TestBuildGeneratedEvidenceRefusesAnUnavailableSourceBinding(t *testing.T) {
	input := proofInput(proofMapping())
	input.Source.Binding = ""
	document, diagnostics := BuildGeneratedEvidence(input)
	if document != nil || !hasCode(diagnostics, ErrorCodeInvalidSourceBinding) {
		t.Fatalf("want %s, got %v", ErrorCodeInvalidSourceBinding, diagnostics)
	}
}

func TestBuildGeneratedEvidenceRefusesProvenanceOutsideTheSourceRoot(t *testing.T) {
	mapping := proofMapping()
	mapping.Provenance = EvidenceProvenance{Root: LocationRootWorkspace, Path: "billing.go"}
	document, diagnostics := BuildGeneratedEvidence(proofInput(mapping))
	if document != nil || !hasCode(diagnostics, ErrorCodeInvalidSubject) {
		t.Fatalf("want %s, got %v", ErrorCodeInvalidSubject, diagnostics)
	}
}

func TestBuildGeneratedEvidenceRefusesAnEscapingProvenancePath(t *testing.T) {
	mapping := proofMapping()
	mapping.Provenance.Path = "../outside.go"
	document, diagnostics := BuildGeneratedEvidence(proofInput(mapping))
	if document != nil || !hasCode(diagnostics, ErrorCodePathEscape) {
		t.Fatalf("want %s, got %v", ErrorCodePathEscape, diagnostics)
	}
}
