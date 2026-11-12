package features

import (
	"encoding/json"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const documentFeature = `{"id":"billing/invoice-export","type":"feature","name":"Export","outcome":"A customer exports an invoice","owner":"billing","target":"coded","requirements":[{"id":"implementation","stage":"coded","evidenceKinds":["capability"]}]}`

func TestAFeatureDocumentStandsForAOneFeatureManifest(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-feature", "a-feature-document-stands-for-a-one-feature-manifest")
	data := `{"$schema":"` + FeatureDocumentSchemaURL + `","protocolVersion":2,"namespace":"billing","feature":` + documentFeature + `}`
	document, findings := ParseAndValidateFeatureDocument([]byte(data))
	if document == nil || diag.HasErrors(findings) {
		t.Fatalf("valid document refused: %v", findings)
	}
	manifest := document.Manifest()
	if manifest.ProtocolVersion != 2 || manifest.Namespace != "billing" || len(manifest.Features) != 1 || manifest.Features[0].ID != "billing/invoice-export" {
		t.Fatalf("document manifest = %#v", manifest)
	}
	var nilDocument *FeatureDocument
	if nilDocument.Manifest() != nil {
		t.Fatal("a nil document stands for a manifest")
	}
}

func TestAFeatureDocumentFindingNamesTheFeature(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-feature", "a-feature-document-finding-names-its-feature")
	invalid := strings.Replace(documentFeature, `"stage":"coded"`, `"stage":"shipped"`, 1)
	_, findings := ParseAndValidateFeatureDocument([]byte(`{"protocolVersion":2,"namespace":"billing","feature":` + invalid + `}`))
	if !hasCode(findings, ErrorCodeInvalidStage) {
		t.Fatalf("invalid stage accepted: %v", findings)
	}
	for _, finding := range findings {
		if strings.HasPrefix(finding.Field, "features[") {
			t.Errorf("finding names the manifest position %q, want a position in the document", finding.Field)
		}
		if !strings.HasPrefix(finding.Field, "feature") {
			t.Errorf("finding field %q does not name the feature", finding.Field)
		}
	}
}

func TestAFeatureDocumentRefusesAForwardFieldAndAManifestMember(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-feature", "a-feature-document-finding-names-its-feature")
	verified := strings.Replace(documentFeature, `"evidenceKinds":["capability"]`,
		`"evidenceKinds":["attestation"],"verification":{"kind":"acceptance","checks":["exports"]}`, 1)
	parsed, findings := ParseFeatureDocument([]byte(`{"protocolVersion":1,"namespace":"billing","feature":` + verified + `}`))
	if parsed != nil || !hasCode(findings, ErrorCodeUnknownField) {
		t.Fatalf("version 1 document accepted the verification field: %#v %v", parsed, findings)
	}
	if !strings.HasPrefix(findings[0].Field, "feature.requirements[0]") {
		t.Errorf("forward field finding names %q, want a position under feature", findings[0].Field)
	}

	for _, input := range []string{
		`{"protocolVersion":2,"namespace":"billing","features":[` + documentFeature + `]}`,
		`{"protocolVersion":2,"namespace":"billing","feature":null}`,
		`{"protocolVersion":4,"namespace":"billing","feature":` + documentFeature + `}`,
	} {
		if parsed, findings := ParseFeatureDocument([]byte(input)); parsed != nil || !diag.HasErrors(findings) {
			t.Errorf("document accepted: %s: %v", input, findings)
		}
	}
}

func TestFeatureDocumentFieldMapsOnlyTheFirstFeature(t *testing.T) {
	for field, want := range map[string]string{
		"features[0]":                 "feature",
		"features[0].requirements[1]": "feature.requirements[1]",
		"features[01]":                "features[01]",
		"features[1].id":              "features[1].id",
		"namespace":                   "namespace",
	} {
		if got := FeatureDocumentField(field); got != want {
			t.Errorf("FeatureDocumentField(%q) = %q, want %q", field, got, want)
		}
	}
}

func TestManifestRootReadsBothForms(t *testing.T) {
	for _, test := range []struct {
		path string
		root string
		ok   bool
	}{
		{path: "putnami.features.json", root: "", ok: true},
		{path: "tooling/cli/putnami.features.json", root: "tooling/cli", ok: true},
		{path: "features/export.json", root: "", ok: true},
		{path: "tooling/cli/features/run.json", root: "tooling/cli", ok: true},
		{path: "tooling/cli/features/run.md", ok: false},
		{path: "tooling/cli/specs/run.json", ok: false},
		{path: "tooling/cli/putnami.json", ok: false},
	} {
		root, ok := ManifestRoot(test.path)
		if root != test.root || ok != test.ok {
			t.Errorf("ManifestRoot(%q) = %q, %v, want %q, %v", test.path, root, ok, test.root, test.ok)
		}
	}
}

func TestValidateRepositoryNamesTheDocumentsOfADuplicateFeature(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-feature", "a-feature-document-finding-names-its-feature")
	document := func(path string) ManifestSource {
		manifest := basicManifest("billing", "billing/invoice-export")
		return ManifestSource{Document: true, Path: path, Manifest: manifest}
	}
	findings := ValidateRepository([]ManifestSource{
		document("billing/features/a.json"),
		document("billing/features/b.json"),
	}, nil)
	if !hasCode(findings, ErrorCodeDuplicateFeature) {
		t.Fatalf("duplicate feature across documents accepted: %v", findings)
	}
	text := diagnosticText(findings)
	for _, want := range []string{"billing/features/a.json#feature", "billing/features/b.json#feature"} {
		if !strings.Contains(text, want) {
			t.Errorf("duplicate finding does not name %s: %s", want, text)
		}
	}
	if strings.Contains(text, "#features[0]") {
		t.Errorf("duplicate finding names a manifest position: %s", text)
	}
}

func TestManifestFormConflictNamesTheRoot(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-feature", "a-root-that-holds-both-forms-is-refused")
	for root, want := range map[string]string{"": ManifestFilename, "tooling/cli": "tooling/cli/" + ManifestFilename} {
		finding := ManifestFormConflict(root)
		if finding.Code != ErrorCodeManifestFormConflict || finding.Field != want || !ValidDiagnosticCodes[finding.Code] {
			t.Errorf("ManifestFormConflict(%q) = %#v", root, finding)
		}
	}
}

func TestSpecTextReadsAStringOrAnArrayOfSentences(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-feature", "requirement-text-is-a-string-or-an-array-of-sentences")
	for input, want := range map[string]SpecText{
		`"One sentence."`:                   {"One sentence."},
		`["First sentence.","Second one."]`: {"First sentence.", "Second one."},
	} {
		var text SpecText
		if err := json.Unmarshal([]byte(input), &text); err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if strings.Join(text, "|") != strings.Join(want, "|") {
			t.Errorf("%s read as %#v, want %#v", input, text, want)
		}
		written, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		if string(written) != input {
			t.Errorf("%s written back as %s", input, written)
		}
	}
	if got := (SpecText{"First sentence.", "Second one."}).String(); got != "First sentence. Second one." {
		t.Errorf("String() = %q", got)
	}
	for _, input := range []string{`1`, `[1]`, `{"text":"a"}`} {
		var text SpecText
		if err := json.Unmarshal([]byte(input), &text); err == nil {
			t.Errorf("%s read as %#v", input, text)
		}
	}
}

func TestSpecRequirementTextBoundsTheJoinedStatement(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "one-file-per-feature", "requirement-text-is-a-string-or-an-array-of-sentences")
	for _, test := range []struct {
		name  string
		text  SpecText
		field string
	}{
		{name: "empty array", text: SpecText{}, field: "requirements[0].text"},
		{name: "blank sentence", text: SpecText{"A sentence.", "  "}, field: "requirements[0].text[1]"},
		{name: "joined over the bound", text: SpecText{strings.Repeat("a", 300), strings.Repeat("b", 300)}, field: "requirements[0].text"},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := basicSpec("billing/invoicing")
			spec.Requirements[0].Text = test.text
			findings := ValidateSpec(spec)
			if !hasCode(findings, ErrorCodeInvalidRequirement) || !strings.Contains(diagnosticText(findings), test.field) {
				t.Fatalf("findings = %v, want %s at %s", findings, ErrorCodeInvalidRequirement, test.field)
			}
		})
	}
	spec := basicSpec("billing/invoicing")
	spec.Requirements[0].Text = SpecText{"An export states its format.", "It names the content type."}
	if findings := ValidateSpec(spec); diag.HasErrors(findings) {
		t.Fatalf("sentence array refused: %v", findings)
	}
}

func diagnosticText(findings []diag.Diagnostic) string {
	var text strings.Builder
	for _, finding := range findings {
		text.WriteString(finding.Field + " " + finding.Message + "\n")
	}
	return text.String()
}
