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

	diag "go.putnami.dev/protocol/diagnostic"
)

func basicSpec(feature string) *Spec {
	return &Spec{
		ProtocolVersion: 1,
		Feature:         feature,
		Outcomes:        []string{"A customer reaches the stated result"},
		Requirements: []SpecRequirement{
			{ID: "export-format", Text: "An export states its format in the response content type."},
		},
	}
}

func jsonFieldNames(t *testing.T, value any) []string {
	t.Helper()
	structType := reflect.TypeOf(value)
	names := make([]string, 0, structType.NumField())
	for i := range structType.NumField() {
		tag, ok := structType.Field(i).Tag.Lookup("json")
		if !ok {
			t.Fatalf("%s.%s has no json tag", structType.Name(), structType.Field(i).Name)
		}
		names = append(names, strings.Split(tag, ",")[0])
	}
	return names
}

func TestSpecStrictVersionUnknownFieldsAndNulls(t *testing.T) {
	valid := `{"protocolVersion":1,"feature":"billing/invoicing","outcomes":["A customer issues an invoice"],"requirements":[]}`
	if spec, findings := ParseAndValidateSpec([]byte(valid)); spec == nil || diag.HasErrors(findings) {
		t.Fatalf("valid: %#v %v", spec, findings)
	}
	for _, input := range []string{
		`{"feature":"billing/invoicing","outcomes":["A"],"requirements":[]}`,
		`{"protocolVersion":null,"feature":"billing/invoicing","outcomes":["A"],"requirements":[]}`,
		`{"protocolVersion":"1","feature":"billing/invoicing","outcomes":["A"],"requirements":[]}`,
		`{"protocolVersion":1.0,"feature":"billing/invoicing","outcomes":["A"],"requirements":[]}`,
		`{"protocolVersion":1e0,"feature":"billing/invoicing","outcomes":["A"],"requirements":[]}`,
		`{"protocolVersion":2,"feature":"billing/invoicing","outcomes":["A"],"requirements":[]}`,
		`{"protocolVersion":1,"protocolVersion":1,"feature":"billing/invoicing","outcomes":["A"],"requirements":[]}`,
	} {
		if parsed, findings := ParseSpec([]byte(input)); parsed != nil || !hasCode(findings, ErrorCodeInvalidProtocolVersion) {
			t.Errorf("version input accepted: %s: %#v %v", input, parsed, findings)
		}
	}

	// A spec is a reader, never an authority: the fields that would let it mint
	// a feature, a maturity stage, evidence, or a graph node are not merely
	// ignored, they are rejected by the strict decoder.
	for _, minting := range []string{
		`"features":[]`, `"maturity":"coded"`, `"target":"ga"`, `"evidence":[]`,
		`"nodes":[]`, `"edges":[]`, `"owner":"billing"`, `"sourceBinding":"source-v1"`,
	} {
		input := `{"protocolVersion":1,"feature":"billing/invoicing","outcomes":["A"],"requirements":[],` + minting + `}`
		if parsed, findings := ParseSpec([]byte(input)); parsed != nil || !hasCode(findings, ErrorCodeUnknownField) {
			t.Errorf("minting field accepted: %s: %#v %v", minting, parsed, findings)
		}
	}

	for _, input := range []string{
		`{"protocolVersion":1,"feature":"billing/invoicing","outcomes":null,"requirements":[]}`,
		`{"protocolVersion":1,"feature":"billing/invoicing","outcomes":["A"],"requirements":null}`,
		`{"protocolVersion":1,"feature":"billing/invoicing","outcomes":["A"],"requirements":[],"nonGoals":null}`,
		`{"protocolVersion":1,"feature":"billing/invoicing","outcomes":["A"],"requirements":[],"decisions":null}`,
	} {
		if parsed, findings := ParseSpec([]byte(input)); parsed != nil || !hasCode(findings, ErrorCodeParseError) {
			t.Errorf("null accepted: %s: %#v %v", input, parsed, findings)
		}
	}

	for _, input := range []string{
		`{"protocolVersion":1,"feature":"billing/invoicing","requirements":[]}`,
		`{"protocolVersion":1,"feature":"billing/invoicing","outcomes":["A"]}`,
	} {
		if _, findings := ParseAndValidateSpec([]byte(input)); !hasCode(findings, ErrorCodeParseError) {
			t.Errorf("missing collection accepted: %s: %v", input, findings)
		}
	}
}

func TestSpecPublicFieldSetStaysMinimal(t *testing.T) {
	wantSpecFields := []string{"$schema", "protocolVersion", "feature", "outcomes", "nonGoals", "requirements", "decisions"}
	if got := jsonFieldNames(t, Spec{}); !slices.Equal(got, wantSpecFields) {
		t.Fatalf("spec field set drifted: %v", got)
	}
	if got := jsonFieldNames(t, SpecRequirement{}); !slices.Equal(got, []string{"id", "text"}) {
		t.Fatalf("spec requirement field set drifted: %v", got)
	}

	data, err := os.ReadFile(filepath.Join("schemas", "putnami-spec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		ID         string                     `json:"$id"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != SpecSchemaURL {
		t.Fatalf("schema $id %q does not match SpecSchemaURL", schema.ID)
	}
	published := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		published = append(published, name)
	}
	slices.Sort(published)
	wantPublished := append([]string(nil), wantSpecFields...)
	slices.Sort(wantPublished)
	if !slices.Equal(published, wantPublished) {
		t.Fatalf("schema properties diverged from the Go wire type: %v", published)
	}
	if !slices.Equal(schema.Required, []string{"protocolVersion", "feature", "outcomes", "requirements"}) {
		t.Fatalf("schema required set drifted: %v", schema.Required)
	}
}

func TestSpecCanonicalSerializationIsDeterministicAndNonMutating(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "equivalence", "human-authored-spec.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, findings := ParseAndValidateSpec(data)
	if spec == nil || diag.HasErrors(findings) {
		t.Fatalf("golden: %v", findings)
	}
	spec.Requirements = append([]SpecRequirement{spec.Requirements[1]}, spec.Requirements[0])
	spec.Decisions = append(spec.Decisions, "tooling/cli/doc/adr/0001-cli-foundation-boundaries.md")
	slices.Reverse(spec.Decisions)
	beforeRequirements := append([]SpecRequirement(nil), spec.Requirements...)
	beforeDecisions := append([]string(nil), spec.Decisions...)
	beforeOutcomes := append([]string(nil), spec.Outcomes...)

	first, err := MarshalSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeRequirements, spec.Requirements) || !reflect.DeepEqual(beforeDecisions, spec.Decisions) || !reflect.DeepEqual(beforeOutcomes, spec.Outcomes) {
		t.Fatal("canonicalization mutated caller-owned slices")
	}
	for range 100 {
		got, err := MarshalSpec(spec)
		if err != nil || !bytes.Equal(got, first) {
			t.Fatalf("unstable serialization: %v", err)
		}
	}
	roundTrip, parsedFindings := ParseSpec(first)
	if roundTrip == nil {
		t.Fatalf("round trip parse: %v", parsedFindings)
	}
	again, err := MarshalSpec(roundTrip)
	if err != nil || !bytes.Equal(first, again) {
		t.Fatalf("spec round trip changed bytes: %v", err)
	}

	// Outcomes are ordered prose, not keyed identities: reordering them is a
	// content change the canonical form must preserve rather than normalize.
	reordered := CanonicalSpec(spec)
	slices.Reverse(reordered.Outcomes)
	shuffledOutcomes, err := MarshalSpec(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(shuffledOutcomes, first) {
		t.Fatal("canonicalization silently reordered authored prose")
	}

	// Empty and absent collections stay distinct so canonical bytes round-trip.
	empty := basicSpec("billing/invoicing")
	empty.Requirements = []SpecRequirement{}
	emptyBytes, err := MarshalSpec(empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(emptyBytes), `"requirements": []`) || strings.Contains(string(emptyBytes), "nonGoals") {
		t.Fatalf("empty collections are not canonical:\n%s", emptyBytes)
	}
	if !bytes.HasSuffix(emptyBytes, []byte("}\n")) || bytes.HasSuffix(emptyBytes, []byte("\n\n")) {
		t.Fatalf("canonical bytes must end with exactly one trailing newline:\n%q", emptyBytes)
	}
	reparsed, parsedFindings := ParseAndValidateSpec(emptyBytes)
	if reparsed == nil || diag.HasErrors(parsedFindings) {
		t.Fatalf("empty-collection round trip: %v", parsedFindings)
	}
	twice, err := MarshalSpec(reparsed)
	if err != nil || !bytes.Equal(twice, emptyBytes) {
		t.Fatalf("empty-collection round trip changed bytes: %v", err)
	}
}

func TestSpecLocalValidationBoundaries(t *testing.T) {
	spec := basicSpec("billing/invoicing")
	spec.Outcomes = []string{}
	if findings := ValidateSpec(spec); !hasCode(findings, ErrorCodeInvalidID) {
		t.Fatalf("outcome-less spec accepted: %v", findings)
	}

	spec = basicSpec("Billing/Invoicing")
	if findings := ValidateSpec(spec); !hasCode(findings, ErrorCodeUnknownFeature) {
		t.Fatalf("non-canonical feature reference accepted: %v", findings)
	}

	spec = basicSpec("billing/invoicing")
	spec.Outcomes = []string{"a\x00secret\x00control statement"}
	if findings := ValidateSpec(spec); !hasCode(findings, ErrorCodeInvalidID) {
		t.Fatalf("control characters accepted: %v", findings)
	}

	spec = basicSpec("billing/invoicing")
	spec.Requirements[0].ID = "Export Format"
	if findings := ValidateSpec(spec); !hasCode(findings, ErrorCodeInvalidRequirement) {
		t.Fatalf("non-canonical requirement ID accepted: %v", findings)
	}

	spec = basicSpec("billing/invoicing")
	spec.Requirements[0].Text = "   "
	if findings := ValidateSpec(spec); !hasCode(findings, ErrorCodeInvalidRequirement) {
		t.Fatalf("blank requirement text accepted: %v", findings)
	}

	spec = basicSpec("billing/invoicing")
	spec.Requirements = append(spec.Requirements, SpecRequirement{ID: "export-format", Text: "A second sentence for the same identity."})
	if got := countCode(ValidateSpec(spec), ErrorCodeInvalidRequirement); got != 1 {
		t.Fatalf("duplicate requirement ID reported %d times: %v", got, ValidateSpec(spec))
	}

	spec = basicSpec("billing/invoicing")
	spec.Decisions = []string{"doc/adr/0001-workspace-decision.md", "protocols/features/doc/adr/0001-minimal-spec-contract.md"}
	if findings := ValidateSpec(spec); diag.HasErrors(findings) {
		t.Fatalf("workspace and project decision links rejected: %v", findings)
	}

	spec.Decisions = []string{"doc/adr/0001-workspace-decision.md", "doc/adr/0001-workspace-decision.md"}
	if findings := ValidateSpec(spec); !hasCode(findings, ErrorCodeInvalidDecision) {
		t.Fatalf("duplicate decision link accepted: %v", findings)
	}

	for _, link := range []string{
		"protocols/features/docs/decisions/0001-second-protocol.md",
		"protocols/features/doc/adr/0001-second-protocol.txt",
		"protocols/features/doc/adr",
		"doc/adr/0001.md/nested.md",
	} {
		spec.Decisions = []string{link}
		if findings := ValidateSpec(spec); !hasCode(findings, ErrorCodeInvalidDecision) {
			t.Errorf("decision link %q accepted outside the doc/adr convention: %v", link, findings)
		}
	}
}

func TestSpecDecisionLinksAreContainedAndRedactionSafe(t *testing.T) {
	spec := basicSpec("billing/invoicing")
	spec.Decisions = []string{"../../Users/alice/private/doc/adr/0001-token.md"}
	findings := ValidateSpec(spec)
	if !hasCode(findings, ErrorCodePathEscape) {
		t.Fatalf("traversing decision link accepted: %v", findings)
	}

	spec.Decisions = []string{"/Users/alice/private/doc/adr/0001-token.md"}
	escaping := ValidateSpec(spec)
	if !hasCode(escaping, ErrorCodeInvalidPath) {
		t.Fatalf("absolute decision link accepted: %v", escaping)
	}
	joined := strings.Join(append(signatures(findings), signatures(escaping)...), "\n")
	for _, secret := range []string{"/Users/alice", "private", "0001-token"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("diagnostics leaked %q: %s", secret, joined)
		}
	}
}

func TestSpecDecisionLinkLengthMatchesSchema(t *testing.T) {
	const (
		prefix = "doc/adr/"
		suffix = ".md"
	)
	for _, test := range []struct {
		name      string
		length    int
		wantError bool
	}{
		{name: "at maximum", length: 512},
		{name: "over maximum", length: 513, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := basicSpec("billing/invoicing")
			spec.Decisions = []string{prefix + strings.Repeat("a", test.length-len(prefix)-len(suffix)) + suffix}
			data, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			parsed, findings := ParseAndValidateSpec(data)
			if parsed == nil {
				t.Fatalf("valid JSON did not parse: %v", findings)
			}
			if got := hasCode(findings, ErrorCodeInvalidDecision); got != test.wantError {
				t.Fatalf("decision link length %d: invalid decision = %v, want %v; findings: %v", test.length, got, test.wantError, findings)
			}
		})
	}
}

func TestSpecRepositoryResolvesFeaturesDuplicatesAndDiscoveryPaths(t *testing.T) {
	billing := basicManifest("billing", "billing/invoicing")
	checkout := basicManifest("checkout", "checkout/purchase")
	manifests := []ManifestSource{
		{Path: "billing/putnami.features.json", Manifest: billing},
		{Path: "checkout/putnami.features.json", Manifest: checkout},
	}
	authored := AuthoredFeatureIDs(manifests)
	if !slices.Equal(authored, []string{"billing/invoicing", "checkout/purchase"}) {
		t.Fatalf("authored feature IDs = %v", authored)
	}

	sources := []SpecSource{
		{Path: "billing/specs/invoicing.json", Spec: basicSpec("billing/invoicing")},
		{Path: "specs/purchase.json", Spec: basicSpec("checkout/purchase")},
	}
	if findings := ValidateSpecRepository(sources, authored); diag.HasErrors(findings) {
		t.Fatalf("valid spec repository rejected: %v", findings)
	}

	// A spec never mints a feature: an unauthored reference is an error, not a
	// new catalog entry.
	unauthored := []SpecSource{{Path: "billing/specs/ghost.json", Spec: basicSpec("billing/ghost")}}
	if findings := ValidateSpecRepository(unauthored, authored); !hasCode(findings, ErrorCodeUnknownFeature) {
		t.Fatalf("unauthored feature reference accepted: %v", findings)
	}
	if findings := ValidateSpecRepository(unauthored, nil); !hasCode(findings, ErrorCodeUnknownFeature) {
		t.Fatalf("empty authored catalog accepted a spec: %v", findings)
	}

	duplicated := append(append([]SpecSource(nil), sources...), SpecSource{
		Path: "billing/specs/invoicing-again.json", Spec: basicSpec("billing/invoicing"),
	})
	first := ValidateSpecRepository(duplicated, authored)
	if !hasCode(first, ErrorCodeDuplicateSpec) {
		t.Fatalf("duplicate spec not found: %v", first)
	}
	slices.Reverse(duplicated)
	second := ValidateSpecRepository(duplicated, authored)
	if !slices.Equal(signatures(first), signatures(second)) {
		t.Fatalf("shuffled spec diagnostics changed\n%v\n%v", first, second)
	}

	for _, path := range []string{
		"billing/specs/nested/invoicing.json",
		"billing/invoicing.json",
		"billing/specs/invoicing.yaml",
		"specs.json",
	} {
		findings := ValidateSpecRepository([]SpecSource{{Path: path, Spec: basicSpec("billing/invoicing")}}, authored)
		if !hasCode(findings, ErrorCodeOutsideDiscoveryRoot) {
			t.Errorf("spec discovered at %q accepted: %v", path, findings)
		}
	}

	traversing := ValidateSpecRepository([]SpecSource{{Path: "../outside/specs/invoicing.json", Spec: basicSpec("billing/invoicing")}}, authored)
	if !hasCode(traversing, ErrorCodePathEscape) {
		t.Fatalf("traversing discovery path accepted: %v", traversing)
	}

	if findings := ValidateSpecRepository([]SpecSource{{Path: "specs/missing.json"}}, authored); !hasCode(findings, ErrorCodeParseError) {
		t.Fatalf("nil spec accepted: %v", findings)
	}
}

func TestSpecRepositoryLeavesTheFeatureCatalogUntouched(t *testing.T) {
	manifest := basicManifest("billing", "billing/invoicing")
	manifests := []ManifestSource{{Path: ManifestFilename, Manifest: manifest}}
	before := ValidateRepository(manifests, nil)
	authored := AuthoredFeatureIDs(manifests)

	spec := basicSpec("billing/invoicing")
	spec.Decisions = []string{"protocols/features/doc/adr/0001-minimal-spec-contract.md"}
	if findings := ValidateSpecRepository([]SpecSource{{Path: "specs/invoicing.json", Spec: spec}}, authored); diag.HasErrors(findings) {
		t.Fatalf("valid spec rejected: %v", findings)
	}

	after := ValidateRepository(manifests, nil)
	if !slices.Equal(signatures(before), signatures(after)) {
		t.Fatal("spec validation changed authored feature findings")
	}
	if got := AuthoredFeatureIDs(manifests); !slices.Equal(got, authored) {
		t.Fatalf("spec validation changed the authored feature catalog: %v", got)
	}
	if len(manifest.Features[0].Requirements) != 1 || manifest.Features[0].Target != MaturityCoded {
		t.Fatal("spec validation mutated the authored feature declaration")
	}
}

func TestSpecDecisionLinksResolveToCommittedRecords(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("fixtures", "equivalence", "human-authored-spec.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, findings := ParseAndValidateSpec(data)
	if spec == nil || diag.HasErrors(findings) {
		t.Fatalf("golden: %v", findings)
	}
	if len(spec.Decisions) == 0 {
		t.Fatal("the canonical vector must exercise at least one decision link")
	}
	workspaceRoot := filepath.Join("..", "..")
	for _, decision := range spec.Decisions {
		//nolint:gosec // the path is a validated contained protocol path from a committed fixture
		if _, err := os.Stat(filepath.Join(workspaceRoot, filepath.FromSlash(decision))); err != nil {
			t.Errorf("decision link %q does not resolve to a committed record: %v", decision, err)
		}
	}
}

func TestSpecDiagnosticCodesAreReserved(t *testing.T) {
	for _, code := range []string{ErrorCodeDuplicateSpec, ErrorCodeInvalidDecision} {
		if !ValidDiagnosticCodes[code] {
			t.Errorf("diagnostic code %q is not in the reserved automation vocabulary", code)
		}
		if !strings.HasPrefix(code, "features.") {
			t.Errorf("diagnostic code %q leaves the features vocabulary", code)
		}
	}
}
