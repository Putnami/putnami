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

const (
	openSpecFixtures  = "conformance/spec-source/openspec"
	openSpecReference = "openspec/specs/invoice-export"
	openSpecFile      = openSpecReference + "/spec.md"
)

func openSpecReader(t *testing.T) features.SpecReader {
	t.Helper()
	reader, exists := features.SpecReaderFor(features.SpecFormatOpenSpec)
	if !exists {
		t.Fatal("no OpenSpec reader ships")
	}
	return reader
}

func openSpecInput(text string) features.SpecInput {
	return features.SpecInput{
		Feature:   contractFeature,
		Reference: features.SpecReference{Format: features.SpecFormatOpenSpec, Path: openSpecReference},
		Files:     []features.SpecFile{{Path: openSpecFile, Data: []byte(text)}},
	}
}

func openSpecFixture(t *testing.T, name string) features.SpecInput {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(openSpecFixtures, name, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	return openSpecInput(string(data))
}

func readOpenSpec(t *testing.T, text string) features.SpecProjection {
	t.Helper()
	return features.ReadSpecSource(openSpecReader(t), openSpecInput(text))
}

func requirementIDs(spec *features.Spec) string {
	ids := make([]string, 0, len(spec.Requirements))
	for _, requirement := range spec.Requirements {
		ids = append(ids, requirement.ID)
	}
	return strings.Join(ids, ",")
}

func TestRealOpenSpecSpecsReproduceTheirGoldens(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-projection", "real-openspec-specs-reproduce-their-goldens")
	spectest.Proves(t, "features/feature-contracts", "openspec-findings", "the-same-openspec-bytes-give-the-same-result")
	for _, name := range []string{"fission-cli-feedback", "nita-projects", "undatum-distribution", "runtime-env-ci-cd"} {
		conformance.RunSpecSource(t, openSpecReader(t), openSpecFixture(t, name), filepath.Join(openSpecFixtures, name, "spec.golden.json"))
	}
}

func TestBrownfieldOpenSpecSpecsParseWithPositionedFindings(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-findings", "brownfield-openspec-specs-parse-with-positioned-findings")
	cases := []struct{ name, want string }{
		{"runtime-env-ci-cd", features.WarningCodeSpecPlaceholderPurpose + "@" + openSpecFile + "#L5"},
		{"editorjs-bundle", features.ErrorCodeSpecRead + "@" + openSpecFile + "#L4"},
		{"nano-brain-embed-queue", features.ErrorCodeSpecRequirementTooLong + "@" + openSpecFile + "#L6," + features.WarningCodeSpecPlaceholderPurpose + "@" + openSpecFile + "#L4"},
	}
	for _, testCase := range cases {
		projection := features.ReadSpecSource(openSpecReader(t), openSpecFixture(t, testCase.name))
		if got := codes(projection.Findings); got != testCase.want {
			t.Errorf("%s: findings = %s, want %s", testCase.name, got, testCase.want)
		}
	}
}

func TestOpenSpecReadsOnlyItsSpecFile(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-projection", "openspec-reads-only-its-spec-file")
	text := "## Purpose\nOutcome.\n\n## Requirements\n### Requirement: One\nThe system SHALL one.\n"
	input := openSpecInput(text)
	input.Files = append(input.Files,
		features.SpecFile{Path: openSpecReference + "/design.md", Data: []byte("### Requirement: Design\n")},
		features.SpecFile{Path: openSpecReference + "/nested/spec.md", Data: []byte("not read")})
	if projection := features.ReadSpecSource(openSpecReader(t), input); requirementIDs(projection.Spec) != "one" {
		t.Fatalf("got %v", projection)
	}
	input.Reference.Path = openSpecFile
	if projection := features.ReadSpecSource(openSpecReader(t), input); projection.Spec == nil {
		t.Fatalf("a reference to the file itself: %v", projection.Findings)
	}
	input.Reference.Path, input.Files = "openspec/specs", input.Files[1:]
	if projection := features.ReadSpecSource(openSpecReader(t), input); projection.Spec != nil ||
		codes(projection.Findings) != features.ErrorCodeSpecRead+"@openspec/specs" {
		t.Fatalf("a folder with no spec.md: %v", projection.Findings)
	}
}

func TestOpenSpecChangeIsNotRead(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-projection", "an-openspec-change-is-not-read")
	path := "openspec/changes/add-export/specs/invoice-export"
	projection := features.ReadSpecSource(openSpecReader(t), features.SpecInput{
		Feature:   contractFeature,
		Reference: features.SpecReference{Format: features.SpecFormatOpenSpec, Path: path},
		Files:     []features.SpecFile{{Path: path + "/spec.md", Data: []byte("## Purpose\nOutcome.\n")}},
	})
	if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@"+path {
		t.Fatalf("got %v", projection.Findings)
	}
}

func TestOpenSpecCommentsAndCodeAreNotSpecText(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-projection", "openspec-comments-and-code-are-not-spec-text")
	projection := readOpenSpec(t, strings.Join([]string{
		"# Title", "", "## Purpose", "Outcome.", "<!-- hidden -->", "", "## Notes", "### Requirement: Elsewhere", "",
		"## Requirements", "```", "### Requirement: Fenced", "```", "<!--", "### Requirement: Commented", "-->",
		"### Requirement: Real", "**Priority**: high", "The system SHALL", "\tact.", "", "~~~", "SHALL not appear", "~~~",
		"Second paragraph.", "#### Notes", "After the body.", "#### Scenario: Works ##", "- **WHEN** x", "",
		"### Grouping", "#### Scenario: Orphan", "",
	}, "\n"))
	if projection.Spec == nil || requirementIDs(projection.Spec) != "real" {
		t.Fatalf("got %v, %s", projection.Spec, codes(projection.Findings))
	}
	if got := projection.Spec.Requirements[0].Text; got != "The system SHALL act. Second paragraph." {
		t.Fatalf("text = %q", got)
	}
	scenarios := projection.Requirements[0].Scenarios
	if len(scenarios) != 1 || scenarios[0].Check != "works" || scenarios[0].Position.Line != 28 {
		t.Fatalf("scenarios = %+v", scenarios)
	}
	onlyMetadata := readOpenSpec(t, "## Purpose\nO.\n## Requirements\n### Requirement: A\n**Constraint**: MUST hold.\n")
	if got := onlyMetadata.Spec.Requirements[0].Text; got != "**Constraint**: MUST hold." {
		t.Fatalf("metadata body = %q", got)
	}
	crlf := readOpenSpec(t, "\xef\xbb\xbf## Purpose\r\nO.\r\n## Requirements\r\n### Requirement: A\r\nThe text.\r\n")
	if crlf.Spec == nil || crlf.Spec.Requirements[0].Text != "The text." || crlf.Requirements[0].Position.Line != 4 {
		t.Fatalf("crlf = %+v", crlf)
	}
}

func TestOnlyADRLinksBecomeOpenSpecDecisions(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-projection", "only-adr-links-become-openspec-decisions")
	projection := readOpenSpec(t, strings.Join([]string{
		"## Purpose", "See [one](doc/adr/0001-one.md) and [site](https://example.com/doc/adr/0001.md).", "",
		"## Requirements", "### Requirement: A",
		"Per [two](../../../doc/adr/0002-two.md#why), [one](doc/adr/0001-one.md), [design](design.md),",
		"[plan](doc/plan.md) and [out](../../../../doc/adr/0003-out.md).",
	}, "\n"))
	if got := strings.Join(projection.Spec.Decisions, ","); got != "doc/adr/0001-one.md,doc/adr/0002-two.md" {
		t.Fatalf("decisions = %s, findings = %s", got, codes(projection.Findings))
	}
}

func TestOpenSpecNameGivesTheID(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-requirement-identity", "an-openspec-name-gives-the-id")
	projection := readOpenSpec(t, "## Purpose\nO.\n## Requirements\n### Requirement: Export, as PDF ###\nThe text.\n### requirement: lower\nThe text.\n")
	if requirementIDs(projection.Spec) != "export-as-pdf,lower" || projection.Requirements[0].Unanchored {
		t.Fatalf("got %+v", projection)
	}
	refused := readOpenSpec(t, "## Purpose\nO.\n## Requirements\n### Requirement: Über\nThe text.\n### Requirement:\nThe text.\n")
	want := features.ErrorCodeSpecRead + "@" + openSpecFile + "#L4," + features.ErrorCodeSpecRead + "@" + openSpecFile + "#L6"
	if refused.Spec != nil || codes(refused.Findings) != want {
		t.Fatalf("got %s", codes(refused.Findings))
	}
}

func TestOpenSpecAnchorNamesTheRequirement(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-requirement-identity", "an-openspec-anchor-names-the-requirement")
	projection := readOpenSpec(t, "## Purpose\nO.\n## Requirements\n<!-- putnami:id export -->\n### Requirement: Über\nThe text.\n<!-- putnami:id kept -->\n### Requirement: Renamed\nThe text.\n")
	if requirementIDs(projection.Spec) != "export,kept" || len(projection.Findings) != 0 {
		t.Fatalf("got %+v", projection)
	}
}

func TestTwoOpenSpecNamesWithOneIDFailWithBothPositions(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-requirement-identity", "two-openspec-names-with-one-id-fail-with-both-positions")
	projection := readOpenSpec(t, "## Purpose\nO.\n## Requirements\n### Requirement: Export PDF\nThe text.\n### Requirement: export-pdf\nThe text.\n")
	if projection.Spec != nil || len(projection.Findings) != 1 || projection.Findings[0].Field != openSpecFile+"#L6" ||
		!strings.Contains(projection.Findings[0].Message, openSpecFile+":4") {
		t.Fatalf("got %+v", projection.Findings)
	}
}

func TestStrayOpenSpecAnchorIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-requirement-identity", "a-stray-openspec-anchor-is-an-error")
	at := func(line string) string { return features.ErrorCodeSpecRead + "@" + openSpecFile + "#L" + line }
	cases := map[string]string{
		"## Purpose\nO.\n<!-- putnami:id a -->\n## Requirements\n":                                     at("3"),
		"## Purpose\nO.\n## Requirements\n<!-- putnami:id a -->\n\n### Requirement: B\nT.\n":           at("4"),
		"## Purpose\nO.\n## Requirements\n<!-- putnami:id a -->":                                       at("4"),
		"## Purpose\nO.\n## Requirements\n<!-- putnami:id Not_A_Segment -->\n### Requirement: B\nT.\n": at("4"),
	}
	for text, want := range cases {
		if projection := readOpenSpec(t, text); projection.Spec != nil || codes(projection.Findings) != want {
			t.Errorf("%q: got %s, want %s", text, codes(projection.Findings), want)
		}
	}
}

func TestOpenSpecWithNoPurposeIsAnErrorAtALine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-findings", "an-openspec-spec-with-no-purpose-is-an-error-at-a-line")
	projection := readOpenSpec(t, "# Title\n\n## Requirements\n")
	if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@"+openSpecFile+"#L1" {
		t.Fatalf("got %s", codes(projection.Findings))
	}
	empty := readOpenSpec(t, "# Title\n\n## Purpose\n\n## Requirements\n")
	if empty.Spec != nil || codes(empty.Findings) != features.ErrorCodeSpecRead+"@"+openSpecFile+"#L3" {
		t.Fatalf("got %s", codes(empty.Findings))
	}
	leading := readOpenSpec(t, "# Title\n\nThe leading prose\non two lines.\n\n- An item.\n\n## Requirements\n")
	if leading.Spec == nil || strings.Join(leading.Spec.Outcomes, "|") != "The leading prose on two lines.|An item." {
		t.Fatalf("leading = %+v", leading)
	}
	bold := readOpenSpec(t, "## Purpose\n**Scope**: the export.\n")
	if bold.Spec == nil || bold.Spec.Outcomes[0] != "**Scope**: the export." {
		t.Fatalf("bold = %+v", bold)
	}
}

func TestPlaceholderPurposeIsAWarningAndStaysTheOutcome(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-findings", "a-placeholder-purpose-is-a-warning-and-stays-the-outcome")
	for _, text := range []string{"TBD - created by archiving change add-x. Update Purpose after archive.", "TODO write this", "tbd.", "todo: later"} {
		projection := readOpenSpec(t, "## Purpose\n"+text+"\n")
		if projection.Spec == nil || projection.Spec.Outcomes[0] != text ||
			codes(projection.Findings) != features.WarningCodeSpecPlaceholderPurpose+"@"+openSpecFile+"#L2" {
			t.Errorf("%q: got %s", text, codes(projection.Findings))
		}
	}
	for _, text := range []string{"The retry budget is TBD pending benchmarks.", "Todo el sistema.", "TODOs are tracked."} {
		if projection := readOpenSpec(t, "## Purpose\n"+text+"\n"); len(projection.Findings) != 0 {
			t.Errorf("%q: got %s", text, codes(projection.Findings))
		}
	}
}

func TestOverLongPurposeParagraphNamesASplit(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-findings", "an-over-long-purpose-paragraph-names-a-split")
	projection := readOpenSpec(t, "## Purpose\nShort.\n\n"+strings.Repeat("a", 513)+"\n")
	if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@"+openSpecFile+"#L4" ||
		!strings.Contains(projection.Findings[0].Message, "split it") {
		t.Fatalf("got %+v", projection.Findings)
	}
}

func TestDerivedOpenSpecSectionIsAWarning(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "openspec-findings", "a-derived-openspec-section-is-a-warning")
	projection := readOpenSpec(t, "## Purpose\nO.\n### Status\nDraft.\n## Requirements\n### Requirement: A\nThe text.\n## Routes\n- GET /x\n## Layout\n")
	want := features.WarningCodeSpecDerivedFact + "@" + openSpecFile + "#L3," + features.WarningCodeSpecDerivedFact + "@" + openSpecFile + "#L8"
	if projection.Spec == nil || codes(projection.Findings) != want || len(projection.Spec.Outcomes) != 1 {
		t.Fatalf("got %s, %+v", codes(projection.Findings), projection.Spec)
	}
	if diag.HasErrors(projection.Findings) {
		t.Fatal("a derived section must stay a warning")
	}
}
