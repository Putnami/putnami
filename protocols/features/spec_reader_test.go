package features_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/conformance"
	"go.putnami.dev/protocol/features/spectest"
)

const contractFeature = "billing/invoice-export"

func minimalInput(text string) features.SpecInput {
	return features.SpecInput{
		Feature:   contractFeature,
		Reference: features.SpecReference{Format: features.SpecFormatOpenSpec, Path: "spec/invoice"},
		Files:     []features.SpecFile{{Path: "spec/invoice/spec.txt", Data: []byte(text)}},
	}
}

func codes(findings []diag.Diagnostic) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		parts = append(parts, finding.Code+"@"+finding.Field)
	}
	return strings.Join(parts, ",")
}

func TestReaderProjectsTheConformanceFixture(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "spec-reader-contract", "a-reader-is-deterministic")
	data, err := os.ReadFile("conformance/spec-source/minimal.txt")
	if err != nil {
		t.Fatal(err)
	}
	input := minimalInput(string(data))
	conformance.RunSpecSource(t, conformance.MinimalSpecReader{}, input, "conformance/spec-source/minimal.golden.json")
	projection := features.ReadSpecSource(conformance.MinimalSpecReader{}, input)
	unanchored := []string{}
	for i, record := range projection.Requirements {
		if record.Unanchored {
			unanchored = append(unanchored, projection.Spec.Requirements[i].ID)
		}
	}
	if strings.Join(unanchored, ",") != "fr-002" {
		t.Fatalf("unanchored = %v", unanchored)
	}
}

func TestReaderFindingNamesItsFileAndLine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "spec-reader-contract", "a-reader-finding-names-its-file-and-line")
	projection := features.ReadSpecSource(conformance.MinimalSpecReader{}, minimalInput("outcome O\nrequirement !!! | text\n"))
	if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@spec/invoice/spec.txt#L2" {
		t.Fatalf("got %s", codes(projection.Findings))
	}
}

func TestOverLongRequirementNamesASplit(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "spec-reader-contract", "an-over-long-requirement-names-a-split")
	long := strings.Repeat("a", 513)
	projection := features.ReadSpecSource(conformance.MinimalSpecReader{}, minimalInput("outcome "+long+"\nrequirement Long | "+long+"\n"))
	if projection.Spec == nil || len(projection.Findings) != 0 || projection.Spec.Requirements[0].Text.String() != long {
		t.Fatalf("an external text over the bound must be read whole: %v", projection.Findings)
	}
	want := features.FindingCodeSpecTextTooLong + "@spec/invoice#outcomes[0]," + features.FindingCodeSpecTextTooLong + "@spec/invoice/spec.txt#L2"
	report := projection.EnforceOnly(features.VerificationModeReport)
	if codes(report) != want || diag.HasErrors(report) || !strings.Contains(report[1].Message, "split it into two requirements") {
		t.Fatalf("report = %v", report)
	}
	if enforce := projection.EnforceOnly(features.VerificationModeEnforce); codes(enforce) != want || len(enforce) != 2 ||
		enforce[0].Severity != diag.Error || enforce[1].Severity != diag.Error {
		t.Fatalf("enforce = %v", enforce)
	}
	native := projection.Spec
	external := []features.SpecSource{{Path: "spec/invoice", Spec: native, External: true}}
	if findings := features.ValidateSpecRepository(external, []string{native.Feature}); len(findings) != 0 {
		t.Fatalf("the repository check bounded an external text again: %v", findings)
	}
	if findings := features.ValidateSpec(native); !diag.HasErrors(findings) {
		t.Fatal("a native spec keeps the bound as an error")
	}
}

func TestTwoRequirementsWithOneIDNameBothPositions(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "spec-reader-contract", "two-requirements-with-one-id-name-both-positions")
	projection := features.ReadSpecSource(conformance.MinimalSpecReader{}, minimalInput("outcome O\nrequirement A b | x\nrequirement A-B | y\n"))
	if len(projection.Findings) != 1 || projection.Findings[0].Field != "spec/invoice/spec.txt#L3" ||
		!strings.Contains(projection.Findings[0].Message, "spec.txt:2") {
		t.Fatalf("got %v", projection.Findings)
	}
}

func TestRequirementNameNormalizesToOneID(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "requirement-identity-mapping", "a-requirement-name-normalizes-to-one-id")
	for name, want := range map[string]string{
		"Export an invoice":    "export-an-invoice",
		"  OAuth2: sign-in!  ": "oauth2-sign-in",
		"a__b--c":              "a-b-c",
		"FR-001":               "fr-001",
		"1.1":                  "1-1",
	} {
		if got, ok := features.NormalizeRequirementName(name); !ok || got != want {
			t.Errorf("%q = %q, %v", name, got, ok)
		}
	}
}

func TestAccentedLetterKeepsItsBaseLetter(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "requirement-identity-mapping", "an-accented-letter-keeps-its-base-letter")
	for name, want := range map[string]string{
		"Überprüfung":       "uberprufung",
		"Export für Kunden": "export-fur-kunden",
		"Règle 2 — totaux":  "regle-2-totaux",
		"cafe\u0301":        "cafe",
		"ﬁle":               "file",
		"Straße":            "stra-e",
		"\xffa\xfeb":        "a-b",
	} {
		if got, ok := features.NormalizeRequirementName(name); !ok || got != want {
			t.Errorf("%q = %q, %v", name, got, ok)
		}
	}
	for _, name := range []string{"日本語", "ß", "\u0301", "!!!", "", strings.Repeat("a", 129), strings.Repeat("é", 129)} {
		if got, ok := features.NormalizeRequirementName(name); ok {
			t.Errorf("%q = %q", name, got)
		}
	}
}

func TestGoldenNamesMapToGoldenIDs(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "requirement-identity-mapping", "the-go-mapping-gives-the-golden-ids")
	data, err := os.ReadFile("fixtures/equivalence/requirement-ids.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Names []struct {
			Name string  `json:"name"`
			ID   *string `json:"id"`
		} `json:"names"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Names) == 0 {
		t.Fatal("the golden holds no name")
	}
	for _, entry := range golden.Names {
		got, ok := features.NormalizeRequirementName(entry.Name)
		switch {
		case entry.ID == nil && ok:
			t.Errorf("%q = %q, want no ID", entry.Name, got)
		case entry.ID != nil && (!ok || got != *entry.ID):
			t.Errorf("%q = %q, %v, want %q", entry.Name, got, ok, *entry.ID)
		}
	}
}

func TestAnchorFollowsTheSegmentGrammar(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "requirement-identity-mapping", "an-anchor-follows-the-segment-grammar")
	for line, want := range map[string]string{
		"<!-- putnami:id invoice-export -->": "invoice-export",
		"  <!--putnami:id a1 -->":            "a1",
		"@putnami-id:rule-one":               "rule-one",
	} {
		if got, isAnchor, err := features.ParseSpecAnchor(line); got != want || !isAnchor || err != nil {
			t.Errorf("%q = %q, %v, %v", line, got, isAnchor, err)
		}
	}
	if _, isAnchor, err := features.ParseSpecAnchor("<!-- putnami:id Not_Valid -->"); !isAnchor || err == nil {
		t.Errorf("a bad ID must be an anchor with an error, got %v, %v", isAnchor, err)
	}
	if _, isAnchor, _ := features.ParseSpecAnchor("see <!-- putnami:id inline --> here"); isAnchor {
		t.Error("an anchor stands alone on its line")
	}
}

func TestSpecFormatPolicyRefusesABadList(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "spec-format-policy", "the-spec-format-policy-refuses-a-bad-list")
	for _, value := range []any{"openspec", []any{7}, []any{"asciidoc"}, []any{"kiro", "kiro"}} {
		options := map[string]map[string]any{"sdd": {"specFormats": value}}
		if _, _, err := features.DecodeSpecFormats("workspace", options); err == nil {
			t.Errorf("%v must be refused", value)
		}
	}
}

func TestProjectListReplacesTheWorkspaceList(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "spec-format-policy", "a-project-list-replaces-the-workspace-list")
	workspace := map[string]map[string]any{"sdd": {"specFormats": []any{"openspec", "kiro"}}}
	project := map[string]map[string]any{"sdd": {"specFormats": []any{}}}
	if formats, err := features.ResolveSpecFormats("workspace", workspace, "p", project); err != nil || len(formats) != 0 {
		t.Fatalf("an empty project list clears the inherited one: %v, %v", formats, err)
	}
	if formats, err := features.ResolveSpecFormats("workspace", workspace, "p", nil); err != nil || len(formats) != 2 {
		t.Fatalf("no project list inherits: %v, %v", formats, err)
	}
	if formats, err := features.ResolveSpecFormats("workspace", nil, "p", nil); err != nil || len(formats) != 0 {
		t.Fatalf("the default is empty: %v, %v", formats, err)
	}
}

func TestScenarioMismatchIsAWarningEitherWay(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "scenarios-never-change-checks", "a-scenario-mismatch-is-a-warning-either-way")
	projection := features.ReadSpecSource(conformance.MinimalSpecReader{}, minimalInput("outcome O\nrequirement A | x\nscenario Kept\nscenario Extra\n"))
	checks := []string{"kept", "missing"}
	feature := features.Feature{ID: contractFeature, Requirements: []features.Requirement{{
		ID: "a", Verification: &features.VerificationCriterion{Kind: features.VerificationKindAcceptance, Checks: checks},
	}}}
	findings := features.CompareSpecScenarios(projection, feature)
	if diag.HasErrors(findings) {
		t.Fatalf("a mismatch is never an error: %v", findings)
	}
	if got := codes(findings); got != features.WarningCodeSpecScenarioMismatch+"@spec/invoice/spec.txt#L2,"+features.WarningCodeSpecScenarioMismatch+"@spec/invoice/spec.txt#L4" {
		t.Fatalf("got %s", got)
	}
	if len(feature.Requirements[0].Verification.Checks) != 2 {
		t.Fatal("the comparison changed the check set")
	}
}

func TestManifestCarriesAClosedSpecReference(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "external-spec-reference", "a-version-3-manifest-carries-a-closed-spec-reference")
	manifest := func(version int, spec string) string {
		return strings.ReplaceAll(strings.ReplaceAll(`{"protocolVersion":@V@,"namespace":"billing","features":[{"id":"billing/a","type":"feature","name":"A","outcome":"O","owner":"o","target":"modeled"@SPEC@}]}`,
			"@V@", string(rune('0'+version))), "@SPEC@", spec)
	}
	if _, findings := features.ParseAndValidateManifest([]byte(manifest(3, `,"spec":{"format":"speckit","path":"specs/001-a"}`))); diag.HasErrors(findings) {
		t.Fatalf("got %v", findings)
	}
	for _, spec := range []string{
		`,"spec":{"format":"speckit"}`,
		`,"spec":{"format":"asciidoc","path":"a"}`,
		`,"spec":{"format":"speckit","path":"a","extra":1}`,
		`,"spec":null`,
	} {
		if _, findings := features.ParseAndValidateManifest([]byte(manifest(3, spec))); !diag.HasErrors(findings) {
			t.Errorf("%s must be refused", spec)
		}
	}
}

func TestOlderManifestRefusesTheSpecReference(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "external-spec-reference", "an-older-manifest-refuses-the-spec-reference")
	for _, version := range []string{"1", "2"} {
		document := `{"protocolVersion":` + version + `,"namespace":"billing","features":[{"id":"billing/a","type":"feature","name":"A","outcome":"O","owner":"o","target":"modeled","spec":{"format":"kiro","path":"a"}}]}`
		parsed, findings := features.ParseManifest([]byte(document))
		if parsed != nil || codes(findings) != features.ErrorCodeUnknownField+"@features[0].spec" {
			t.Errorf("version %s: %v", version, findings)
		}
	}
}

func TestSpecReferenceCannotLeaveTheWorkspace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "external-spec-reference", "a-spec-reference-cannot-leave-the-workspace")
	for _, path := range []string{"../outside", "a/../../b", "/etc/passwd", "C:/x", "a\\b", ".", "", "a//b", "a/"} {
		manifest := &features.Manifest{ProtocolVersion: features.ManifestProtocolVersion, Namespace: "billing", Features: []features.Feature{{
			ID: "billing/a", Type: features.FeatureTypeFeature, Name: "A", Outcome: "O", Owner: "o", Target: features.MaturityModeled,
			Spec: &features.SpecReference{Format: features.SpecFormatKiro, Path: path},
		}}}
		if !diag.HasErrors(features.ValidateManifest(manifest)) {
			t.Errorf("path %q must be refused", path)
		}
	}
}

type noScenarioReader struct{}

func (noScenarioReader) Read(input features.SpecInput) features.SpecReading {
	reading := conformance.MinimalSpecReader{}.Read(input)
	for i := range reading.Requirements {
		reading.Requirements[i].Scenarios = nil
	}
	return reading
}

func TestFormatWithNoScenariosIsNotCompared(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "scenarios-never-change-checks", "a-format-with-no-scenarios-is-not-compared")
	projection := features.ReadSpecSource(noScenarioReader{}, minimalInput("outcome O\nrequirement A | x\n"))
	feature := features.Feature{ID: contractFeature, Requirements: []features.Requirement{{
		ID: "a", Verification: &features.VerificationCriterion{Kind: features.VerificationKindAcceptance, Checks: []string{"declared"}},
	}}}
	if findings := features.CompareSpecScenarios(projection, feature); len(findings) != 0 {
		t.Fatalf("got %v", findings)
	}
}

func TestCriteriaGroupNamesTheFormatOfAnExternalPath(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "external-spec-reference", "a-criteria-group-names-the-format-of-an-external-path")
	projection := func(group string) []byte {
		return []byte(`{"protocolVersion":1,"groups":[{"feature":"billing/a",` + group + `,"specRequirements":["r"]}]}`)
	}
	for _, valid := range []string{`"spec":"specs/a.json"`, `"spec":"openspec/specs/a","format":"openspec"`} {
		if _, findings := features.ParseAndValidateSpecCriteriaProjection(projection(valid)); diag.HasErrors(findings) {
			t.Errorf("%s: %v", valid, findings)
		}
	}
	for _, invalid := range []string{`"spec":"openspec/specs/a"`, `"spec":"openspec/specs/a","format":"asciidoc"`, `"spec":"../a","format":"kiro"`} {
		if _, findings := features.ParseAndValidateSpecCriteriaProjection(projection(invalid)); !diag.HasErrors(findings) {
			t.Errorf("%s must be refused", invalid)
		}
	}
}

func TestEveryDiagnosticCodeOfTheContractIsReserved(t *testing.T) {
	t.Parallel()
	for _, code := range []string{
		features.ErrorCodeInvalidSpecReference, features.ErrorCodeSpecFormatNotEnabled, features.ErrorCodeSpecFormatUnavailable,
		features.ErrorCodeSpecRead, features.FindingCodeSpecTextTooLong, features.ErrorCodeSpecRequirementUnanchored,
		features.WarningCodeSpecScenarioMismatch, features.WarningCodeSpecDerivedFact, features.WarningCodeSpecRequirementIgnored, features.FindingCodeSpecNeedsClarification,
	} {
		if !features.ValidDiagnosticCodes[code] {
			t.Errorf("%s is not reserved", code)
		}
	}
}
