package features_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/conformance"
	"go.putnami.dev/protocol/features/spectest"
)

const specKitPath = "specs/001-invoice-export"

func specKitInput(files map[string]string) features.SpecInput {
	input := features.SpecInput{
		Feature:   contractFeature,
		Reference: features.SpecReference{Format: features.SpecFormatSpecKit, Path: specKitPath},
	}
	for name, text := range files {
		input.Files = append(input.Files, features.SpecFile{Path: specKitPath + "/" + name, Data: []byte(text)})
	}
	return input
}

func specKitReader(t *testing.T) features.SpecReader {
	t.Helper()
	reader, exists := features.SpecReaderFor(features.SpecFormatSpecKit)
	if !exists {
		t.Fatal("the speckit reader is not registered")
	}
	return reader
}

func readSpecKit(t *testing.T, text string) features.SpecProjection {
	t.Helper()
	return features.ReadSpecSource(specKitReader(t), specKitInput(map[string]string{"spec.md": text}))
}

func specKitCorpus(t *testing.T, name string) features.SpecInput {
	t.Helper()
	directory := filepath.Join("conformance", "spec-source", "speckit", name)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".md") {
			data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[entry.Name()] = string(data)
		}
	}
	return specKitInput(files)
}

const specKitMinimal = "## Requirements\n\n### Functional Requirements\n\n- **FR-001**: System MUST export.\n\n## Success Criteria\n\n- **SC-001**: Done.\n"

func TestSpecKitTemplateProjectsWithPositionedFindings(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-projection", "the-speckit-template-projects-with-positioned-findings")
	projection := features.ReadSpecSource(specKitReader(t), specKitCorpus(t, "template"))
	if projection.Spec == nil {
		t.Fatalf("findings = %v", projection.Findings)
	}
	if len(projection.Spec.Requirements) != 7 || len(projection.Spec.Outcomes) != 4 || len(projection.Scenarios) != 4 {
		t.Fatalf("requirements = %d, outcomes = %d, scenarios = %d", len(projection.Spec.Requirements), len(projection.Spec.Outcomes), len(projection.Scenarios))
	}
	file := specKitPath + "/spec.md#L"
	markers := projection.EnforceOnly(features.VerificationModeReport)
	for _, finding := range append(append([]diag.Diagnostic{}, projection.Findings...), markers...) {
		if !strings.HasPrefix(finding.Field, file) {
			t.Fatalf("unpositioned finding %v", finding)
		}
	}
	if got := len(projection.Findings); got != 4 {
		t.Fatalf("derived facts = %v", projection.Findings)
	}
	if got := len(markers); got != 2 {
		t.Fatalf("markers = %v", markers)
	}
}

func TestFilledSpecKitSpecReproducesItsGolden(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-projection", "a-filled-speckit-spec-reproduces-its-golden")
	conformance.RunSpecSource(t, specKitReader(t), specKitCorpus(t, "filled"), "conformance/spec-source/speckit/filled.golden.json")
}

func TestSpecKitProcessFilesChangeNothing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-projection", "speckit-process-files-change-nothing")
	alone := readSpecKit(t, specKitMinimal)
	process := "## Functional Requirements\n\n- **FR-900**: Process state. [NEEDS CLARIFICATION]\n"
	withProcess := features.ReadSpecSource(specKitReader(t), specKitInput(map[string]string{
		"spec.md": specKitMinimal, "plan.md": process, "tasks.md": process, "research.md": process, "data-model.md": process, "contracts/api.md": process,
	}))
	first, _ := features.MarshalSpec(alone.Spec)
	second, err := features.MarshalSpec(withProcess.Spec)
	if err != nil || string(first) != string(second) || len(withProcess.Findings) != 0 {
		t.Fatalf("err = %v, spec = %s, findings = %v", err, second, withProcess.Findings)
	}
}

func TestSpecKitReferenceWithNoSpecFileIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-projection", "a-speckit-reference-with-no-spec-md-is-an-error")
	projection := features.ReadSpecSource(specKitReader(t), specKitInput(map[string]string{"plan.md": specKitMinimal}))
	if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@"+specKitPath {
		t.Fatalf("got %s", codes(projection.Findings))
	}
}

func TestSpecKitFileReferenceReadsThatFile(t *testing.T) {
	t.Parallel()
	input := specKitInput(map[string]string{"spec.md": specKitMinimal})
	input.Reference.Path = specKitPath + "/spec.md"
	if projection := features.ReadSpecSource(specKitReader(t), input); projection.Spec == nil {
		t.Fatalf("findings = %v", projection.Findings)
	}
}

func TestCommentsAndCodeAreNotSpecText(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-projection", "comments-and-code-are-not-spec-text")
	text := "<!--\n## Functional Requirements\n- **FR-100**: Hidden. [NEEDS CLARIFICATION]\n-->\n" +
		"```markdown\n## Functional Requirements\n- **FR-101**: Hidden. [NEEDS CLARIFICATION]\n```\n" +
		"The `[NEEDS CLARIFICATION]` marker is documented here.\n\n" + specKitMinimal
	projection := readSpecKit(t, text)
	markers := projection.EnforceOnly(features.VerificationModeReport)
	if projection.Spec == nil || len(projection.Spec.Requirements) != 1 || projection.Spec.Requirements[0].ID != "fr-001" || len(markers) != 0 {
		t.Fatalf("spec = %+v, markers = %v, findings = %v", projection.Spec, markers, projection.Findings)
	}
}

func TestSpecKitListItemsContinueAndSectionsEnd(t *testing.T) {
	t.Parallel()
	text := "## Out of Scope\n\n- First\n  goal.\n* Second goal.\n\n## Assumptions\n\n- Not a goal.\n\n" +
		"## Functional Requirements\n\n### Export\n\n- **FR-001:** Bold colon\n  wraps.\n\n**FR-002**: No bullet.\n- Not labeled.\n\n" +
		"## Notes\n\n- A note.\n\n## Success Criteria\r\n\r\n- **SC-001**: Windows line.\r\n"
	projection := readSpecKit(t, text)
	if projection.Spec == nil {
		t.Fatalf("findings = %v", projection.Findings)
	}
	spec := projection.Spec
	if strings.Join(spec.NonGoals, "|") != "First goal.|Second goal." || len(spec.Requirements) != 2 ||
		spec.Requirements[0].Text != "Bold colon wraps." || spec.Requirements[1].ID != "fr-002" || spec.Outcomes[0] != "Windows line." {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestSpecKitAnchorNamesTheRequirement(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-requirement-identity", "a-speckit-anchor-names-the-requirement")
	projection := readSpecKit(t, strings.Replace(specKitMinimal, "- **FR-001**", "<!-- putnami:id export-it -->\n- **FR-001**", 1))
	if projection.Spec == nil || projection.Spec.Requirements[0].ID != "export-it" || projection.Requirements[0].Unanchored {
		t.Fatalf("got %+v", projection)
	}
}

func TestUnanchoredSpecKitRequirementIsPositional(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-requirement-identity", "an-unanchored-speckit-requirement-is-positional")
	projection := readSpecKit(t, specKitMinimal)
	if projection.Spec == nil || projection.Spec.Requirements[0].ID != "fr-001" || !projection.Requirements[0].Unanchored {
		t.Fatalf("got %+v", projection)
	}
	if findings := projection.EnforceOnly(features.VerificationModeEnforce); codes(findings) != features.ErrorCodeSpecRequirementUnanchored+"@"+specKitPath+"/spec.md#L5" {
		t.Fatalf("got %s", codes(findings))
	}
}

func TestSpecKitAnchorNamesAScenarioCheck(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-requirement-identity", "a-speckit-anchor-names-a-scenario-check")
	text := "### User Story 1 - Export (Priority: P1)\n\n<!-- putnami:id exports-a-month -->\n1. **Given** a month, **When** exported, **Then** a file\n" +
		"2. **Given** nothing, **When** exported, **Then** an error\n3. Not a scenario.\n\n" + specKitMinimal
	projection := readSpecKit(t, text)
	if projection.Spec == nil || len(projection.Scenarios) != 2 {
		t.Fatalf("got %+v", projection)
	}
	first, second := projection.Scenarios[0], projection.Scenarios[1]
	if first.Check != "exports-a-month" || first.Name != "Given a month, When exported, Then a file" || first.Position.Line != 4 ||
		second.Check != "given-nothing-when-exported-then-an-error" {
		t.Fatalf("scenarios = %+v", projection.Scenarios)
	}
}

func TestStraySpecKitAnchorIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-requirement-identity", "a-stray-speckit-anchor-is-an-error")
	file := "@" + specKitPath + "/spec.md#L"
	cases := map[string]struct{ text, want string }{
		"a blank line":      {"<!-- putnami:id a -->\n\n" + specKitMinimal, file + "1"},
		"above a paragraph": {"<!-- putnami:id a -->\nProse.\n\n" + specKitMinimal, file + "1"},
		"at the end":        {specKitMinimal + "<!-- putnami:id a -->\n", file + "10"},
		"two in a row":      {"<!-- putnami:id a -->\n<!-- putnami:id b -->\n" + specKitMinimal, file + "1"},
		"a bad grammar":     {"<!-- putnami:id Not_An_ID -->\n" + specKitMinimal, file + "1"},
	}
	for name, test := range cases {
		projection := readSpecKit(t, test.text)
		if projection.Spec != nil || !strings.Contains(codes(projection.Findings), features.ErrorCodeSpecRead+test.want) {
			t.Fatalf("%s: got %s", name, codes(projection.Findings))
		}
	}
}

func TestEachClarificationMarkerIsOneEnforceFinding(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-findings", "each-clarification-marker-is-one-enforce-finding")
	text := strings.Replace(specKitMinimal, "export.", "export to [NEEDS CLARIFICATION: where?] as [NEEDS CLARIFICATION].", 1)
	projection := readSpecKit(t, text)
	if projection.Spec == nil || len(projection.Findings) != 0 {
		t.Fatalf("got %+v", projection)
	}
	marker := features.FindingCodeSpecNeedsClarification + "@" + specKitPath + "/spec.md#L5"
	if got := codes(projection.EnforceOnly(features.VerificationModeReport)); got != marker+","+marker {
		t.Fatalf("got %s", got)
	}
}

func TestDerivedFactsAreWarnings(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-findings", "derived-facts-are-warnings")
	text := "# Feature\n\n**Feature Branch**: `001-x`\n**Created**: 2026-01-01\n**Status**: Draft\n**Input**: Words.\n\n" + specKitMinimal +
		"\n### Key Entities\n\n- **Invoice**: A bill.\n\n**Status**: Below the header.\n"
	projection := readSpecKit(t, text)
	if projection.Spec == nil {
		t.Fatalf("findings = %v", projection.Findings)
	}
	file := "@" + specKitPath + "/spec.md#L"
	code := features.WarningCodeSpecDerivedFact + file
	if got := codes(projection.Findings); got != code+"18,"+code+"3,"+code+"4,"+code+"5" {
		t.Fatalf("got %s", got)
	}
	for _, finding := range projection.Findings {
		if finding.Severity != diag.Warning {
			t.Fatalf("finding = %v", finding)
		}
	}
}

func TestAnOutcomeFindingNamesItsLine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "spec-reader-contract", "an-outcome-finding-names-its-line")
	projection := readSpecKit(t, strings.Replace(specKitMinimal, "Done.", strings.Repeat("a", 513), 1))
	report := projection.EnforceOnly(features.VerificationModeReport)
	if projection.Spec == nil || codes(report) != features.FindingCodeSpecTextTooLong+"@"+specKitPath+"/spec.md#L9" {
		t.Fatalf("got %s, report %s", codes(projection.Findings), codes(report))
	}
}

func TestFileScenariosAreComparedWithEveryDeclaredCheck(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "scenarios-never-change-checks", "file-scenarios-are-compared-with-every-declared-check")
	text := "### User Story 1 - A (Priority: P1)\n\n<!-- putnami:id declared -->\n1. **Given** a, **Then** b\n2. **Given** c, **Then** d\n\n" + specKitMinimal
	projection := readSpecKit(t, text)
	feature := features.Feature{Requirements: []features.Requirement{{
		ID: "fr-001", Verification: &features.VerificationCriterion{Checks: []string{"declared", "unproved"}},
	}}}
	file := "@" + specKitPath + "/spec.md#L"
	code := features.WarningCodeSpecScenarioMismatch + file
	if got := codes(features.CompareSpecScenarios(projection, feature)); got != code+"11,"+code+"5" {
		t.Fatalf("got %s", got)
	}
	if strings.Join(feature.Requirements[0].Verification.Checks, ",") != "declared,unproved" {
		t.Fatal("the comparison changed the checks")
	}
}

func TestAnEnforceFindingFollowsTheMode(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "enforce-only-findings", "an-enforce-finding-is-a-warning-in-report-and-an-error-in-enforce")
	text := strings.Replace(specKitMinimal, "- **FR-001**", "<!-- putnami:id export-it -->\n- **FR-001**", 1)
	projection := readSpecKit(t, strings.Replace(text, "export.", "export [NEEDS CLARIFICATION].", 1))
	report := projection.EnforceOnly(features.VerificationModeReport)
	enforce := projection.EnforceOnly(features.VerificationModeEnforce)
	if len(report) != 1 || report[0].Severity != diag.Warning || len(enforce) != 1 || enforce[0].Severity != diag.Error || enforce[0].Field != report[0].Field {
		t.Fatalf("report = %v, enforce = %v", report, enforce)
	}
	if again := projection.EnforceOnly(features.VerificationModeReport); again[0].Severity != diag.Warning {
		t.Fatal("EnforceOnly changed the projection")
	}
}

func TestReportSaysNothingAboutAnUnanchoredRequirement(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "enforce-only-findings", "report-says-nothing-about-an-unanchored-requirement")
	if findings := readSpecKit(t, specKitMinimal).EnforceOnly(features.VerificationModeReport); len(findings) != 0 {
		t.Fatalf("got %v", findings)
	}
}

func TestANonGoalFindingNamesItsLine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "spec-reader-contract", "a-non-goal-finding-names-its-line")
	projection := readSpecKit(t, specKitMinimal+"\n## Out of Scope\n\n- "+strings.Repeat("a", 513)+"\n")
	report := projection.EnforceOnly(features.VerificationModeReport)
	if projection.Spec == nil || codes(report) != features.FindingCodeSpecTextTooLong+"@"+specKitPath+"/spec.md#L13" {
		t.Fatalf("got %s, report %s", codes(projection.Findings), codes(report))
	}
}

func TestANestedItemStaysInItsRequirement(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-projection", "a-nested-item-stays-in-its-requirement")
	text := "## Requirements\n\n- **FR-001**: System MUST export in:\n  - CSV\n  - JSON\n\n    Both are UTF-8.\n- **FR-002**: Second.\n\t- **FR-003**: Tab indented.\n\n" +
		"## Success Criteria\n\n- **SC-001**: Done.\n\n## Out of Scope\n\n- Goal:\n  - detail\n- Other.\n"
	projection := readSpecKit(t, text)
	if projection.Spec == nil {
		t.Fatalf("findings = %v", projection.Findings)
	}
	spec := projection.Spec
	if len(spec.Requirements) != 3 || spec.Requirements[0].Text != "System MUST export in: - CSV - JSON Both are UTF-8." ||
		spec.Requirements[2].ID != "fr-003" || strings.Join(spec.NonGoals, "|") != "Goal: - detail|Other." {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestSpecKitSpecWithNoOutcomeIsAnErrorAtALine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-projection", "a-speckit-spec-with-no-outcome-is-an-error-at-a-line")
	file := features.ErrorCodeSpecRead + "@" + specKitPath + "/spec.md#L"
	if got := codes(readSpecKit(t, "# Feature\n\n## Success Criteria\n\nProse only.\n").Findings); got != file+"3" {
		t.Fatalf("got %s", got)
	}
	if got := codes(readSpecKit(t, "# Feature\n").Findings); got != file+"1" {
		t.Fatalf("got %s", got)
	}
}

func TestAnUnclosedCommentOrFenceIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-projection", "an-unclosed-comment-or-fence-is-an-error")
	file := features.ErrorCodeSpecRead + "@" + specKitPath + "/spec.md#L"
	for name, opening := range map[string]string{"comment": "<!-- open", "fence": "```"} {
		if got := codes(readSpecKit(t, specKitMinimal+"\n"+opening+"\nHidden.\n").Findings); got != file+"11" {
			t.Fatalf("%s: got %s", name, got)
		}
	}
	if projection := readSpecKit(t, "Write `<!--` to open a comment.\n\n"+specKitMinimal); projection.Spec == nil {
		t.Fatalf("inline code: %s", codes(projection.Findings))
	}
}

func TestAnFRLabelThatProjectsNothingIsAWarning(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "speckit-findings", "an-fr-label-that-projects-nothing-is-a-warning")
	text := specKitMinimal + "\n## Notes\n\n- **FR-002**: Outside the section.\n- FR-003: Unbolded.\n- FR-001 is mentioned in prose.\n"
	projection := readSpecKit(t, text)
	code := features.WarningCodeSpecRequirementIgnored + "@" + specKitPath + "/spec.md#L"
	if projection.Spec == nil || codes(projection.Findings) != code+"13,"+code+"14" {
		t.Fatalf("got %s", codes(projection.Findings))
	}
}

func TestABOMDoesNotHideTheFirstHeading(t *testing.T) {
	t.Parallel()
	projection := readSpecKit(t, "\ufeff## Functional Requirements\n\n- **FR-001**: One.\n\n## Success Criteria\n\n- **SC-001**: Done.\n")
	if projection.Spec == nil || len(projection.Spec.Requirements) != 1 || len(projection.Findings) != 0 {
		t.Fatalf("got %+v", projection)
	}
}

func TestAnIndentedScenarioIsItsOwnScenario(t *testing.T) {
	t.Parallel()
	text := "### User Story 1 - A (Priority: P1)\n\n- **Acceptance Scenarios**:\n  1. **Given** a, **Then** b\n     - and c\n  2. **Given** d, **Then** e\n\n" + specKitMinimal
	projection := readSpecKit(t, text)
	if projection.Spec == nil || len(projection.Scenarios) != 2 || projection.Scenarios[0].Name != "Given a, Then b - and c" {
		t.Fatalf("scenarios = %+v", projection.Scenarios)
	}
}

func TestSiblingSubsectionsStayInTheRequirements(t *testing.T) {
	t.Parallel()
	body := "### Functional Requirements\n\n- **FR-001**: One.\n\n### Non-Functional Requirements\n\n- **FR-002**: Two.\n\n"
	other := "### Non-Functional Requirements\n\n- **FR-002**: Two.\n\n### Functional Requirements\n\n- **FR-001**: One.\n\n"
	for _, text := range []string{body, other} {
		projection := readSpecKit(t, "## Requirements\n\n"+text+"## Success Criteria\n\n- **SC-001**: Done.\n")
		if projection.Spec == nil || len(projection.Spec.Requirements) != 2 || len(projection.Findings) != 0 {
			t.Fatalf("got %+v", projection)
		}
	}
}

func TestLabelsTheReaderDoesNotTakeWarnInEverySection(t *testing.T) {
	t.Parallel()
	text := "## Functional Requirements\n\n- **fr-001**: Lower case.\n- **FR-001**: Taken.\n\n## Success Criteria\n\n- **SC-001**: Done.\n\n## Out of Scope\n\n- **FR-002**: Dropped.\n"
	projection := readSpecKit(t, text)
	code := features.WarningCodeSpecRequirementIgnored + "@" + specKitPath + "/spec.md#L"
	if projection.Spec == nil || codes(projection.Findings) != code+"12,"+code+"3" {
		t.Fatalf("got %s", codes(projection.Findings))
	}
	if !strings.Contains(projection.Findings[0].Message, "non-goal") {
		t.Fatalf("message = %s", projection.Findings[0].Message)
	}
}
