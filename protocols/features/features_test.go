package features

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
)

const testBinding = "source-v1:sha256:0000000000000000000000000000000000000000000000000000000000000000"

func basicManifest(namespace, id string) *Manifest {
	return &Manifest{ProtocolVersion: 1, Namespace: namespace, Features: []Feature{{
		ID: id, Type: FeatureTypeFeature, Name: "Feature", Outcome: "A bounded outcome", Owner: namespace, Target: MaturityCoded,
		Requirements: []Requirement{{ID: "implementation", Stage: MaturityCoded, EvidenceKinds: []EvidenceKind{EvidenceKindCapability}}},
	}}}
}

func basicEvidence() EvidenceRecord {
	return EvidenceRecord{
		ID: "billing/evidence", Feature: "billing/feature", Requirement: "implementation", Stage: MaturityCoded, Outcome: EvidenceOutcomeSupports,
		Issuer:     Issuer{Kind: IssuerKindBuild, ID: "builder"},
		Source:     SourceSelector{Root: LocationRootProject, OwnerProject: "go.putnami.dev/example", Binding: testBinding},
		Subject:    EvidenceSubject{Kind: EvidenceKindCapability, Contribution: &capabilities.ContributionReference{OwnerProject: "go.putnami.dev/example", Kind: capabilities.ContributionKindConfig, Key: "example.enabled"}},
		Provenance: EvidenceProvenance{Root: LocationRootProject, Path: "feature.go", Symbol: "Feature"},
	}
}

func hasCode(findings []diag.Diagnostic, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func countCode(findings []diag.Diagnostic, code string) int {
	count := 0
	for _, finding := range findings {
		if finding.Code == code {
			count++
		}
	}
	return count
}

func signatures(findings []diag.Diagnostic) []string {
	out := make([]string, len(findings))
	for i, finding := range findings {
		out[i] = string(finding.Severity) + "|" + finding.Code + "|" + finding.Field + "|" + finding.Message
	}
	return out
}

func TestStrictVersionDispatchUnknownFieldsAndNulls(t *testing.T) {
	valid := `{"protocolVersion":1,"namespace":"billing","features":[]}`
	if manifest, findings := ParseAndValidateManifest([]byte(valid)); manifest == nil || diag.HasErrors(findings) {
		t.Fatalf("valid: %#v %v", manifest, findings)
	}
	// Both manifest wires are readable; splitting the version constant did not
	// widen the evidence or spec wires, which still require the exact token 1.
	if manifest, findings := ParseAndValidateManifest([]byte(`{"protocolVersion":2,"namespace":"billing","features":[]}`)); manifest == nil || diag.HasErrors(findings) {
		t.Fatalf("current manifest version rejected: %#v %v", manifest, findings)
	}
	for _, input := range []string{
		`{"protocolVersion":2,"evidence":[]}`,
		`{"protocolVersion":2,"observations":[]}`,
	} {
		var findings []diag.Diagnostic
		if strings.Contains(input, `"evidence"`) {
			_, findings = ParseEvidenceDocument([]byte(input))
		} else {
			_, findings = ParseVerificationReport([]byte(input))
		}
		if !hasCode(findings, ErrorCodeInvalidProtocolVersion) {
			t.Errorf("manifest version accepted on another wire: %s: %v", input, findings)
		}
	}

	invalidVersions := []string{
		`{"namespace":"billing","features":[]}`,
		`{"protocolVersion":null,"namespace":"billing","features":[]}`,
		`{"protocolVersion":"1","namespace":"billing","features":[]}`,
		`{"protocolVersion":1.0,"namespace":"billing","features":[]}`,
		`{"protocolVersion":1e0,"namespace":"billing","features":[]}`,
		`{"protocolVersion":3,"namespace":"billing","features":[]}`,
		`{"protocolVersion":1,"protocolVersion":1,"namespace":"billing","features":[]}`,
	}
	for _, input := range invalidVersions {
		if parsed, findings := ParseManifest([]byte(input)); parsed != nil || !hasCode(findings, ErrorCodeInvalidProtocolVersion) {
			t.Errorf("version input accepted: %s: %#v %v", input, parsed, findings)
		}
	}
	if parsed, findings := ParseManifest([]byte(`{"protocolVersion":1,"namespace":"billing","features":[],"current":"ga"}`)); parsed != nil || !hasCode(findings, ErrorCodeUnknownField) {
		t.Fatalf("unknown field: %#v %v", parsed, findings)
	}
	for _, input := range []string{
		`{"protocolVersion":1,"namespace":"billing","features":null}`,
		`{"protocolVersion":1,"namespace":"billing","features":[{"id":"billing/a","type":"feature","name":"A","outcome":"A","owner":"billing","target":"modeled","relations":null}]}`,
		`{"protocolVersion":1,"evidence":null}`,
	} {
		accepted := false
		var findings []diag.Diagnostic
		if strings.Contains(input, `"namespace"`) {
			parsed, parsedFindings := ParseManifest([]byte(input))
			accepted, findings = parsed != nil, parsedFindings
		} else {
			parsed, parsedFindings := ParseEvidenceDocument([]byte(input))
			accepted, findings = parsed != nil, parsedFindings
		}
		if accepted || !hasCode(findings, ErrorCodeParseError) {
			t.Errorf("null accepted: %s: %v", input, findings)
		}
	}
	if _, findings := ParseAndValidateManifest([]byte(`{"protocolVersion":1,"namespace":"billing"}`)); !hasCode(findings, ErrorCodeParseError) {
		t.Fatalf("missing features accepted: %v", findings)
	}
	if _, findings := ParseAndValidateEvidenceDocument([]byte(`{"protocolVersion":1}`)); !hasCode(findings, ErrorCodeParseError) {
		t.Fatalf("missing evidence accepted: %v", findings)
	}
}

func TestMaturityOrderAndTargetCappingShape(t *testing.T) {
	want := []MaturityStage{MaturityModeled, MaturityCoded, MaturityWired, MaturityDefaultOn, MaturityLiveVerified, MaturityDesignPartnerProven, MaturityGA}
	got := OrderedMaturityStages()
	if !slices.Equal(got, want) {
		t.Fatalf("maturity order = %v", got)
	}
	got[0] = MaturityGA
	if rank, ok := StageRank(MaturityModeled); !ok || rank != 0 {
		t.Fatal("caller mutated maturity rank table")
	}
	if _, ok := StageRank("unknown"); ok {
		t.Fatal("unknown stage accepted")
	}

	manifest := basicManifest("billing", "billing/feature")
	manifest.Features[0].Target = MaturityModeled
	// A later requirement is valid but remains above the authored target for
	// assessment tooling to report as unclaimed.
	if findings := ValidateManifest(manifest); diag.HasErrors(findings) {
		t.Fatalf("post-target requirement rejected: %v", findings)
	}
	manifest.Features[0].Requirements[0].Stage = MaturityModeled
	if findings := ValidateManifest(manifest); !hasCode(findings, ErrorCodeInvalidStage) {
		t.Fatalf("modeled requirement accepted: %v", findings)
	}

	manifest = basicManifest("billing", "billing/feature")
	manifest.Features[0].Target = MaturityWired
	if findings := ValidateManifest(manifest); !hasCode(findings, ErrorCodeInvalidRequirement) {
		t.Fatalf("missing wired coverage accepted: %v", findings)
	}
}

func TestCanonicalSerializationIsDeterministicAndNonMutating(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "equivalence", "go-typescript-features.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, findings := ParseAndValidateManifest(data)
	if manifest == nil || diag.HasErrors(findings) {
		t.Fatalf("golden: %v", findings)
	}
	manifest.Features[0], manifest.Features[3] = manifest.Features[3], manifest.Features[0]
	before := append([]Feature(nil), manifest.Features...)
	first, err := MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, data) {
		t.Fatalf("canonical bytes differ\n%s", first)
	}
	if !reflect.DeepEqual(before, manifest.Features) {
		t.Fatal("canonicalization mutated caller feature order")
	}
	for range 100 {
		got, err := MarshalManifest(manifest)
		if err != nil || !bytes.Equal(got, first) {
			t.Fatalf("unstable serialization: %v", err)
		}
	}
	roundTrip, parsedFindings := ParseManifest(first)
	if roundTrip == nil {
		t.Fatalf("round trip parse: %v", parsedFindings)
	}
	again, _ := MarshalManifest(roundTrip)
	if !bytes.Equal(first, again) {
		t.Fatal("manifest round trip changed bytes")
	}
}

func TestAggregateCrossDocumentRelationsDuplicatesCyclesAndOrder(t *testing.T) {
	billing := basicManifest("billing", "billing/invoice")
	checkout := basicManifest("checkout", "checkout/purchase")
	billing.Features[0].Relations = []Relation{{Kind: RelationKindDependsOn, Target: "checkout/purchase"}}
	sources := []ManifestSource{{Path: "billing/putnami.features.json", Manifest: billing}, {Path: "checkout/putnami.features.json", Manifest: checkout}}
	if findings := ValidateRepository(sources, nil); diag.HasErrors(findings) {
		t.Fatalf("cross-document relation rejected: %v", findings)
	}

	duplicate := basicManifest("billing", "billing/invoice")
	duplicateFindings := ValidateRepository(append(sources, ManifestSource{Path: "other/putnami.features.json", Manifest: duplicate}), nil)
	if !hasCode(duplicateFindings, ErrorCodeDuplicateFeature) {
		t.Fatalf("duplicate feature not found: %v", duplicateFindings)
	}
	within := basicManifest("billing", "billing/within-document")
	within.Features = append(within.Features, within.Features[0])
	withinFindings := ValidateRepository([]ManifestSource{{Path: "billing/putnami.features.json", Manifest: within}}, nil)
	if got := countCode(withinFindings, ErrorCodeDuplicateFeature); got != 1 {
		t.Fatalf("within-document duplicate reported %d times: %v", got, withinFindings)
	}

	checkout.Features[0].Relations = []Relation{{Kind: RelationKindDependsOn, Target: "billing/invoice"}}
	first := ValidateRepository(sources, nil)
	if !hasCode(first, ErrorCodeRelationCycle) {
		t.Fatalf("cross-document cycle not found: %v", first)
	}
	slices.Reverse(sources)
	second := ValidateRepository(sources, nil)
	if !slices.Equal(signatures(first), signatures(second)) {
		t.Fatalf("shuffled diagnostics changed\n%v\n%v", first, second)
	}

	checkout.Features[0].Relations = nil
	billing.Features[0].Relations[0].Target = "missing/feature"
	if findings := ValidateRepository(sources, nil); !hasCode(findings, ErrorCodeDanglingRelation) {
		t.Fatalf("dangling relation not found: %v", findings)
	}
}

func TestAggregateEvidenceReferencesDuplicatesAndAboveTarget(t *testing.T) {
	manifest := basicManifest("billing", "billing/feature")
	manifest.Features[0].Target = MaturityModeled // coded evidence is valid but unclaimed.
	record := basicEvidence()
	docA := &EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{record}}
	manifestSources := []ManifestSource{{Path: "billing/putnami.features.json", Manifest: manifest}}
	evidenceSources := make([]EvidenceSource, 1, 2)
	evidenceSources[0] = EvidenceSource{Path: "billing/schema/feature-evidence/a.json", Document: docA}
	if findings := ValidateRepository(manifestSources, evidenceSources); diag.HasErrors(findings) {
		t.Fatalf("valid post-target evidence rejected: %v", findings)
	}

	docB := &EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{record}}
	evidenceSources = append(evidenceSources, EvidenceSource{Path: "billing/schema/feature-evidence/b.json", Document: docB})
	first := ValidateRepository(manifestSources, evidenceSources)
	if !hasCode(first, ErrorCodeDuplicateEvidence) {
		t.Fatalf("duplicate evidence not found: %v", first)
	}
	slices.Reverse(evidenceSources)
	second := ValidateRepository(manifestSources, evidenceSources)
	if !slices.Equal(signatures(first), signatures(second)) {
		t.Fatalf("shuffled evidence diagnostics changed")
	}

	docB.Evidence[0].ID = "billing/other-evidence"
	docB.Evidence[0].Requirement = "unknown"
	if findings := ValidateRepository(manifestSources, evidenceSources); !hasCode(findings, ErrorCodeUnknownRequirement) {
		t.Fatalf("unknown requirement not found: %v", findings)
	}
	docB.Evidence[0].Requirement = "implementation"
	docB.Evidence[0].Stage = MaturityWired
	if findings := ValidateRepository(manifestSources, evidenceSources); !hasCode(findings, ErrorCodeStageMismatch) {
		t.Fatalf("stage mismatch not found: %v", findings)
	}
	docB.Evidence[0].Feature = "unknown/feature"
	if findings := ValidateRepository(manifestSources, evidenceSources); !hasCode(findings, ErrorCodeUnknownFeature) {
		t.Fatalf("unknown feature not found: %v", findings)
	}
}

func TestEvidenceAuthorityReferenceProvenanceAndObservationBoundaries(t *testing.T) {
	record := basicEvidence()
	record.Subject.Contribution.Subkind = "unexpected"
	findings := ValidateEvidenceDocument(&EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{record}})
	if !hasCode(findings, capabilities.ErrorCodeInvalidContributionIdentity) {
		t.Fatalf("capability diagnostic code collapsed: %v", findings)
	}
	for _, finding := range findings {
		if finding.Code == capabilities.ErrorCodeInvalidContributionIdentity && !strings.Contains(finding.Field, ".subject.contribution.") {
			t.Fatalf("capability field not prefixed: %v", finding)
		}
	}

	record = basicEvidence()
	record.Provenance.Root = LocationRootWorkspace
	if findings := ValidateEvidenceDocument(&EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{record}}); !hasCode(findings, ErrorCodeInvalidSubject) {
		t.Fatalf("unresolvable provenance accepted: %v", findings)
	}

	record = basicEvidence()
	record.Persistent = true
	if findings := ValidateEvidenceDocument(&EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{record}}); !hasCode(findings, ErrorCodeInvalidAuthority) {
		t.Fatalf("non-human persistence accepted: %v", findings)
	}
	record.ObservedAt = "secret-not-a-time"
	findings = ValidateEvidenceDocument(&EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{record}})
	if !hasCode(findings, ErrorCodeInvalidSubject) {
		t.Fatalf("content-bound observation accepted: %v", findings)
	}
	if strings.Contains(strings.Join(signatures(findings), "\n"), "secret-not-a-time") {
		t.Fatal("timestamp diagnostic leaked raw input")
	}

	record = basicEvidence()
	record.Issuer = Issuer{Kind: IssuerKindHuman, ID: "product"}
	record.Subject = EvidenceSubject{Kind: EvidenceKindAttestation, Attestation: &AttestationSubject{Claim: AttestationClaimAvailability}}
	record.Persistent = true
	record.ObservedAt = "2026-08-10T10:00:00Z"
	if findings := ValidateEvidenceDocument(&EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{record}}); diag.HasErrors(findings) {
		t.Fatalf("human temporal attestation rejected: %v", findings)
	}
}

func TestPackageSourceUsesOnlyPackageResolutionMetadata(t *testing.T) {
	record := basicEvidence()
	record.Source = SourceSelector{
		Root:         LocationRootPackage,
		OwnerProject: "go.putnami.dev/example",
		Package:      "go.putnami.dev/example/package",
		Version:      "v1.2.3",
		Binding:      testBinding,
	}
	record.Provenance.Root = LocationRootPackage
	document := &EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{record}}
	if findings := ValidateEvidenceDocument(document); !hasCode(findings, ErrorCodeInvalidSubject) {
		t.Fatalf("package selector accepted unused ownerProject: %v", findings)
	}
	record.Source.OwnerProject = ""
	document.Evidence[0] = record
	if findings := ValidateEvidenceDocument(document); diag.HasErrors(findings) {
		t.Fatalf("minimal package selector rejected: %v", findings)
	}
}

func TestInvalidPathsAndEnvironmentAreRedactionSafe(t *testing.T) {
	manifest := basicManifest("billing", "billing/feature")
	record := basicEvidence()
	record.Subject = EvidenceSubject{Kind: EvidenceKindArtifact, Artifact: &ArtifactSubject{Path: "/Users/alice/private/token.txt", Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111"}}
	record.Provenance.Path = "/tmp/customer-secret"
	record.Source.Environment = "tenant Customer-42"
	findings := ValidateRepository(
		[]ManifestSource{{Path: "/Users/alice/private/putnami.features.json", Manifest: manifest}},
		[]EvidenceSource{{Path: "/tmp/customer/schema/feature-evidence/a.json", Document: &EvidenceDocument{ProtocolVersion: 1, Evidence: []EvidenceRecord{record}}}},
	)
	joined := strings.Join(signatures(findings), "\n")
	for _, secret := range []string{"/Users/alice", "/tmp/customer", "token.txt", "Customer-42"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, joined)
		}
	}
	if !hasCode(findings, ErrorCodeInvalidPath) || !hasCode(findings, ErrorCodeInvalidSubject) {
		t.Fatalf("invalid redacted inputs not diagnosed: %v", findings)
	}
}

func TestValidAndInvalidFixtureCorpus(t *testing.T) {
	for _, name := range []string{
		"features.json",
		"features-v2.json",
		filepath.Join("..", "equivalence", "go-typescript-features.golden.json"),
		filepath.Join("..", "equivalence", "human-authored-features-v2.golden.json"),
	} {
		data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, findings := ParseAndValidateManifest(data); diag.HasErrors(findings) {
			t.Errorf("valid manifest %s: %v", name, findings)
		}
	}
	for _, name := range []string{"evidence.json", filepath.Join("..", "equivalence", "go-typescript-evidence.golden.json"), filepath.Join("..", "equivalence", "python-authored-evidence.golden.json")} {
		data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, findings := ParseAndValidateEvidenceDocument(data); diag.HasErrors(findings) {
			t.Errorf("valid evidence %s: %v", name, findings)
		}
	}
	for _, name := range []string{"spec.json", filepath.Join("..", "equivalence", "human-authored-spec.golden.json")} {
		data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, findings := ParseAndValidateSpec(data); diag.HasErrors(findings) {
			t.Errorf("valid spec %s: %v", name, findings)
		}
	}
	for _, name := range []string{"verification.json", filepath.Join("..", "equivalence", "go-typescript-verification.golden.json")} {
		data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, findings := ParseAndValidateVerificationReport(data); diag.HasErrors(findings) {
			t.Errorf("valid verification report %s: %v", name, findings)
		}
	}
	invalid, err := filepath.Glob(filepath.Join("fixtures", "invalid", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) == 0 {
		t.Fatal("no invalid fixtures")
	}
	baseData, err := os.ReadFile(filepath.Join("fixtures", "equivalence", "go-typescript-features.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	baseManifest, baseFindings := ParseAndValidateManifest(baseData)
	if baseManifest == nil || diag.HasErrors(baseFindings) {
		t.Fatalf("base manifest: %v", baseFindings)
	}
	for _, filename := range invalid {
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		var findings []diag.Diagnostic
		base := filepath.Base(filename)
		switch {
		// Two fixtures are locally valid on purpose: only the workspace
		// aggregation can decide them, so they are routed to the aggregate
		// validators instead of the single-document ones.
		case base == "features-dangling-relation.json":
			manifest, parsedFindings := ParseAndValidateManifest(data)
			if manifest == nil || diag.HasErrors(parsedFindings) {
				t.Fatalf("dangling fixture must be locally valid: %v", parsedFindings)
			}
			findings = ValidateRepository([]ManifestSource{{Path: "putnami.features.json", Manifest: manifest}}, nil)
		case base == "evidence-stage-mismatch.json":
			document, parsedFindings := ParseAndValidateEvidenceDocument(data)
			if document == nil || diag.HasErrors(parsedFindings) {
				t.Fatalf("stage mismatch fixture must be locally valid: %v", parsedFindings)
			}
			findings = ValidateRepository(
				[]ManifestSource{{Path: ManifestFilename, Manifest: baseManifest}},
				[]EvidenceSource{{Path: "schema/feature-evidence/stage-mismatch.json", Document: document}},
			)
		case base == "spec-unknown-feature.json":
			spec, parsedFindings := ParseAndValidateSpec(data)
			if spec == nil || diag.HasErrors(parsedFindings) {
				t.Fatalf("unknown feature fixture must be locally valid: %v", parsedFindings)
			}
			findings = ValidateSpecRepository(
				[]SpecSource{{Path: SpecDirectory + "/unknown-feature.json", Spec: spec}},
				AuthoredFeatureIDs([]ManifestSource{{Path: ManifestFilename, Manifest: baseManifest}}),
			)
		case strings.HasPrefix(base, "features-"):
			_, findings = ParseAndValidateManifest(data)
		case strings.HasPrefix(base, "spec-"):
			_, findings = ParseAndValidateSpec(data)
		case strings.HasPrefix(base, "verification-"):
			_, findings = ParseAndValidateVerificationReport(data)
		case strings.HasPrefix(base, "evidence-"):
			_, findings = ParseAndValidateEvidenceDocument(data)
		default:
			t.Fatalf("invalid fixture %s has no document prefix; name it features-, spec-, verification-, or evidence-", base)
		}
		if !diag.HasErrors(findings) {
			t.Errorf("invalid fixture accepted: %s", filename)
		}
	}
}

func TestEquivalenceCorpusValidatesAsOneRepository(t *testing.T) {
	manifestData, err := os.ReadFile(filepath.Join("fixtures", "equivalence", "go-typescript-features.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, findings := ParseAndValidateManifest(manifestData)
	if manifest == nil || diag.HasErrors(findings) {
		t.Fatalf("manifest: %v", findings)
	}
	evidenceSources := make([]EvidenceSource, 0, 2)
	for _, name := range []string{"go-typescript-evidence.golden.json", "python-authored-evidence.golden.json"} {
		data, err := os.ReadFile(filepath.Join("fixtures", "equivalence", name))
		if err != nil {
			t.Fatal(err)
		}
		document, parsedFindings := ParseAndValidateEvidenceDocument(data)
		if document == nil || diag.HasErrors(parsedFindings) {
			t.Fatalf("%s: %v", name, parsedFindings)
		}
		evidenceSources = append(evidenceSources, EvidenceSource{Path: "schema/feature-evidence/" + name, Document: document})
	}
	findings = ValidateRepository([]ManifestSource{{Path: ManifestFilename, Manifest: manifest}}, evidenceSources)
	if diag.HasErrors(findings) {
		t.Fatalf("equivalence repository invalid: %v", findings)
	}
}

func TestSchemasAreStrictDocumentsWithoutDerivedState(t *testing.T) {
	for _, name := range []string{"putnami-features.json", "putnami-feature-evidence.json", "putnami-spec.json", "putnami-feature-verification.json"} {
		data, err := os.ReadFile(filepath.Join("schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("schema %s: %v", name, err)
		}
		if schema["additionalProperties"] != false {
			t.Errorf("schema %s is not strict", name)
		}
		text := string(data)
		for _, forbidden := range []string{`"current"`, `"snapshot"`, `"delta"`, `"completionPercentage"`} {
			if strings.Contains(text, forbidden) {
				t.Errorf("schema %s contains derived field %s", name, forbidden)
			}
		}
	}
}
