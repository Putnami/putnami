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
	kiroPath    = ".kiro/specs/invoice-export"
	kiroFile    = kiroPath + "/requirements.md"
	kiroMinimal = "## Introduction\n\nCustomers receive invoices.\n\n## Requirements\n\n### Requirement 1\n\n" +
		"**User Story:** As a customer, I want an invoice, so that I can pay.\n\n#### Acceptance Criteria\n\n" +
		"1. WHEN an order closes THE system SHALL issue an invoice\n2. THE system SHALL number invoices\n"
)

func kiroInput(files map[string]string) features.SpecInput {
	input := features.SpecInput{
		Feature:   contractFeature,
		Reference: features.SpecReference{Format: features.SpecFormatKiro, Path: kiroPath},
	}
	for name, text := range files {
		input.Files = append(input.Files, features.SpecFile{Path: kiroPath + "/" + name, Data: []byte(text)})
	}
	return input
}

func kiroReaderUnderTest(t *testing.T) features.SpecReader {
	t.Helper()
	reader, exists := features.SpecReaderFor(features.SpecFormatKiro)
	if !exists {
		t.Fatal("the kiro reader is not registered")
	}
	return reader
}

func readKiro(t *testing.T, text string) features.SpecProjection {
	t.Helper()
	return features.ReadSpecSource(kiroReaderUnderTest(t), kiroInput(map[string]string{"requirements.md": text}))
}

func kiroIDs(t *testing.T, projection features.SpecProjection) string {
	t.Helper()
	if projection.Spec == nil {
		t.Fatalf("no projection: %v", projection.Findings)
	}
	ids := make([]string, 0, len(projection.Spec.Requirements))
	for _, requirement := range projection.Spec.Requirements {
		ids = append(ids, requirement.ID)
	}
	return strings.Join(ids, " ")
}

func kiroFindings(projection features.SpecProjection, code string) []string {
	var fields []string
	for _, finding := range projection.Findings {
		if finding.Code == code {
			fields = append(fields, finding.Field)
		}
	}
	return fields
}

func TestRealKiroSpecsReproduceTheirGoldens(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-projection", "real-kiro-specs-reproduce-their-goldens")
	for _, name := range []string{"air-quality-dialog", "fodmap-continuous-delivery", "holo-board", "shellock", "sia-cli-app"} {
		directory := filepath.Join("conformance", "spec-source", "kiro", name)
		data, err := os.ReadFile(filepath.Join(directory, "requirements.md"))
		if err != nil {
			t.Fatal(err)
		}
		conformance.RunSpecSource(t, kiroReaderUnderTest(t), kiroInput(map[string]string{"requirements.md": string(data)}),
			filepath.Join(directory, "requirements.golden.json"))
	}
}

func TestRealKiroSpecOutsideTheLayoutIsRefusedAtALine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-findings", "a-kiro-spec-with-no-outcome-or-requirement-is-an-error-at-a-line")
	data, err := os.ReadFile(filepath.Join("conformance", "spec-source", "kiro", "localcut-captions", "requirements.md"))
	if err != nil {
		t.Fatal(err)
	}
	projection := readKiro(t, string(data))
	if projection.Spec != nil || len(kiroFindings(projection, features.ErrorCodeSpecRead)) == 0 {
		t.Fatalf("projection = %v", projection.Findings)
	}
	for _, text := range []string{"## Requirements\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n1. THE system SHALL run\n", "## Introduction\n\nWhy.\n"} {
		projection := readKiro(t, text)
		if got := kiroFindings(projection, features.ErrorCodeSpecRead); projection.Spec != nil || len(got) != 1 || got[0] != kiroFile+"#L1" {
			t.Errorf("%q: findings = %v", text, projection.Findings)
		}
	}
}

func TestKiroProcessFilesChangeNothing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-projection", "kiro-process-files-change-nothing")
	alone := readKiro(t, kiroMinimal)
	input := kiroInput(map[string]string{
		"requirements.md": kiroMinimal,
		"design.md":       "## Introduction\n\nOther.\n\n### Requirement 9\n\n#### Acceptance Criteria\n\n1. THE design SHALL differ\n",
		"tasks.md":        "- [ ] 1. Do it\n",
	})
	with := features.ReadSpecSource(kiroReaderUnderTest(t), input)
	if kiroIDs(t, with) != kiroIDs(t, alone) || len(with.Spec.Outcomes) != 1 {
		t.Fatalf("with = %+v", with.Spec)
	}
}

func TestKiroReferenceWithNoRequirementsFileIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-projection", "a-kiro-reference-with-no-requirements-md-is-an-error")
	for _, name := range []string{"design.md", "tasks.md", "product.md"} {
		projection := features.ReadSpecSource(kiroReaderUnderTest(t), kiroInput(map[string]string{name: kiroMinimal}))
		if got := kiroFindings(projection, features.ErrorCodeSpecRead); projection.Spec != nil || len(got) != 1 || got[0] != kiroPath {
			t.Errorf("%s: findings = %v", name, projection.Findings)
		}
	}
	direct := features.SpecInput{
		Feature:   contractFeature,
		Reference: features.SpecReference{Format: features.SpecFormatKiro, Path: kiroFile},
		Files:     []features.SpecFile{{Path: kiroFile, Data: []byte(kiroMinimal)}},
	}
	if projection := features.ReadSpecSource(kiroReaderUnderTest(t), direct); projection.Spec == nil {
		t.Fatalf("a reference to the file itself: %v", projection.Findings)
	}
}

func TestWrappedOrNestedCriterionStaysWhole(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-projection", "a-wrapped-or-nested-criterion-stays-whole")
	projection := readKiro(t, "## Introduction\n\nWhy.\n\n### Requirement 1\n\n**Acceptance Criteria:**\n\n"+
		"1. WHEN an order closes, THE\n   system SHALL issue:\n   - a PDF\n   - a receipt\n\n   and log it\n"+
		"2. THE system\nSHALL retry\n\nA closing remark.\n\n3. THE system SHALL stop\n")
	if got := kiroIDs(t, projection); got != "requirement-1-1 requirement-1-2 requirement-1-3" {
		t.Fatalf("ids = %s", got)
	}
	texts := projection.Spec.Requirements
	if texts[0].Text != "WHEN an order closes, THE system SHALL issue: - a PDF - a receipt and log it" {
		t.Errorf("first = %q", texts[0].Text)
	}
	if texts[1].Text != "THE system SHALL retry" || texts[2].Text != "THE system SHALL stop" {
		t.Errorf("texts = %+v", texts)
	}
}

func TestDottedCriterionNumberOpensACriterion(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-projection", "a-dotted-criterion-number-opens-a-criterion")
	projection := readKiro(t, "## Introduction\n\nWhy.\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n1.1 THE system SHALL run\n1.2. THE system SHALL stop\n")
	if got := kiroIDs(t, projection); got != "requirement-1-1 requirement-1-2" {
		t.Fatalf("ids = %s", got)
	}
	if text := projection.Spec.Requirements[1].Text; text != "THE system SHALL stop" {
		t.Fatalf("text = %q", text)
	}
}

func TestKiroCommentsAndCodeAreNotSpecText(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-projection", "kiro-comments-and-code-are-not-spec-text")
	projection := readKiro(t, "## Introduction\n\nWhy.<!-- hidden -->\n\n<!--\n### Requirement 8\n-->\n```\n### Requirement 9\n```\n\n"+
		"### Requirement 1\n\n#### Acceptance Criteria\n\n1. THE system SHALL run <!-- soon --> now\n```md\n2. THE fence SHALL hide\n```\n")
	if got := kiroIDs(t, projection); got != "requirement-1-1" {
		t.Fatalf("ids = %s", got)
	}
	if text := projection.Spec.Requirements[0].Text; text != "THE system SHALL run  now" || projection.Spec.Outcomes[0] != "Why." {
		t.Fatalf("spec = %+v", projection.Spec)
	}
}

func TestDesignLinkIsNeverADecision(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-projection", "a-design-link-is-never-a-decision")
	projection := readKiro(t, "## Introduction\n\nSee [design](design.md) and [ADR](doc/adr/0001-a.md).\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n1. THE system SHALL run\n")
	if projection.Spec == nil || len(projection.Spec.Decisions) != 0 {
		t.Fatalf("spec = %+v, findings = %v", projection.Spec, projection.Findings)
	}
}

func TestKiroAnchorNamesTheCriterion(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-requirement-identity", "a-kiro-anchor-names-the-criterion")
	projection := readKiro(t, "## Introduction\n\nWhy.\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n"+
		"<!-- putnami:id issue-an-invoice -->\n1. THE system SHALL issue\n2. THE system SHALL retry\n")
	if got := kiroIDs(t, projection); got != "issue-an-invoice requirement-1-2" {
		t.Fatalf("ids = %s", got)
	}
	if projection.Requirements[0].Unanchored || !projection.Requirements[1].Unanchored {
		t.Fatalf("records = %+v", projection.Requirements)
	}
	if projection.Requirements[0].Scenarios != nil {
		t.Fatal("Kiro has no scenarios, so its records stay out of the comparison")
	}
}

func TestUnanchoredCriterionIsPositional(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-requirement-identity", "an-unanchored-criterion-is-positional")
	projection := readKiro(t, kiroMinimal+"\n### Requirement 2: Refunds\n\n#### Acceptance Criteria\n\n1. THE system SHALL refund\n")
	if got := kiroIDs(t, projection); got != "requirement-1-1 requirement-1-2 requirement-2-1" {
		t.Fatalf("ids = %s", got)
	}
	for _, record := range projection.Requirements {
		if !record.Unanchored {
			t.Fatalf("records = %+v", projection.Requirements)
		}
	}
	if enforced := len(projection.Findings); enforced != 0 {
		t.Fatalf("report says something: %v", projection.Findings)
	}
}

func TestKiroGroupAnchorNamesItsPositionalCriteria(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-requirement-identity", "a-kiro-group-anchor-names-its-positional-criteria")
	projection := readKiro(t, "## Introduction\n\nWhy.\n\n<!-- putnami:id export -->\n### Requirement 1\n\n#### Acceptance Criteria\n\n1. THE system SHALL run\n")
	if got := kiroIDs(t, projection); got != "export-1" {
		t.Fatalf("ids = %s", got)
	}
	if !projection.Requirements[0].Unanchored {
		t.Fatal("reordering the criteria changes the ordinal, so a group anchor pins nothing")
	}
}

func TestStrayKiroAnchorIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-requirement-identity", "a-stray-kiro-anchor-is-an-error")
	for name, text := range map[string]string{
		"blank line": "## Introduction\n\nWhy.\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n<!-- putnami:id a -->\n\n1. THE system SHALL run\n",
		"last line":  kiroMinimal + "\n<!-- putnami:id a -->\n",
		"heading":    "## Introduction\n\n<!-- putnami:id a -->\n## Glossary\n\nWhy.\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n1. THE system SHALL run\n",
		"bad id":     kiroMinimal + "\n<!-- putnami:id Not_An_ID -->\n3. THE system SHALL run\n",
	} {
		projection := readKiro(t, text)
		if projection.Spec != nil || len(kiroFindings(projection, features.ErrorCodeSpecRead)) == 0 {
			t.Errorf("%s: findings = %v", name, projection.Findings)
		}
	}
}

func TestEveryEARSFormParses(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-findings", "every-ears-form-parses")
	for name, criterion := range map[string]string{
		"ubiquitous":            "THE system SHALL log each export",
		"ubiquitous lower case": "The export service shall log each export",
		"event-driven":          "WHEN an order closes THE system SHALL issue an invoice",
		"event-driven THEN":     "WHEN an order closes THEN the system SHALL issue an invoice",
		"state-driven":          "WHILE the queue is full, THE system SHALL refuse a job",
		"unwanted behavior":     "IF the store is down, THEN THE system SHALL retry",
		"optional feature":      "WHERE export is enabled, THE system SHALL show the button",
		"complex":               "WHILE online, WHEN an order closes, THE system SHALL issue an invoice",
		"bold keywords":         "**WHEN** an order closes **THE** system **SHALL** issue an invoice",
		"property":              "FOR ALL valid invoices, parsing then printing SHALL give the same bytes",
	} {
		projection := readKiro(t, "## Introduction\n\nWhy.\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n1. "+criterion+"\n")
		if projection.Spec == nil || len(projection.Findings) != 0 {
			t.Errorf("%s: findings = %v", name, projection.Findings)
		}
	}
}

func TestMalformedCriterionIsAPositionedWarning(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-findings", "a-malformed-criterion-is-a-positioned-warning")
	for _, criterion := range []string{"Invoices are numbered", "WHEN an order closes", "IF the store is down, THE system SHALL retry", "SHALL nothing"} {
		projection := readKiro(t, "## Introduction\n\nWhy.\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n1. "+criterion+"\n2. THE system SHALL run\n")
		if got := kiroIDs(t, projection); got != "requirement-1-1 requirement-1-2" {
			t.Fatalf("%q: ids = %s", criterion, got)
		}
		fields := kiroFindings(projection, features.WarningCodeSpecCriterionMalformed)
		if diag.HasErrors(projection.Findings) || len(fields) != 1 || fields[0] != kiroFile+"#L9" {
			t.Errorf("%q: findings = %v", criterion, projection.Findings)
		}
	}
}

func TestGroupWithNoCriterionIsAWarning(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-findings", "a-group-with-no-criterion-is-a-warning")
	projection := readKiro(t, kiroMinimal+"\n### Requirement 2\n\nA story with no criteria.\n\n1. A list outside the criteria\n")
	if got := kiroIDs(t, projection); got != "requirement-1-1 requirement-1-2" {
		t.Fatalf("ids = %s", got)
	}
	if got := kiroFindings(projection, features.WarningCodeSpecRequirementIgnored); len(got) != 1 || got[0] != kiroFile+"#L16" {
		t.Fatalf("findings = %v", projection.Findings)
	}
}

func TestKiroDerivedFactsAreWarnings(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-findings", "kiro-derived-facts-are-warnings")
	projection := readKiro(t, "## Status\n\nDraft\n\n"+kiroMinimal+"\n## Glossary\n\n- **Order**: a purchase\n\n## Entities\n\n- Invoice\n")
	if got := kiroFindings(projection, features.WarningCodeSpecDerivedFact); projection.Spec == nil || len(got) != 2 || got[0] != kiroFile+"#L1" {
		t.Fatalf("findings = %v", projection.Findings)
	}
	if len(projection.Findings) != 2 {
		t.Fatalf("an unknown section gave a finding: %v", projection.Findings)
	}
}

func TestLongIntroductionParagraphSplitsAtSentences(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-projection", "a-long-introduction-paragraph-splits-at-sentences")
	sentence := strings.Repeat("word ", 40) + "end."
	projection := readKiro(t, "## Introduction\n\n"+strings.Repeat(sentence+" ", 4)+"\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n1. THE system SHALL run\n")
	if projection.Spec == nil || len(projection.Spec.Outcomes) != 2 {
		t.Fatalf("spec = %+v, findings = %v", projection.Spec, projection.Findings)
	}
	if got := strings.Join(projection.Spec.Outcomes, " "); got != strings.TrimSpace(strings.Repeat(sentence+" ", 4)) {
		t.Fatalf("the outcomes lost text: %q", got)
	}
}

func TestLongIntroductionSentenceNamesItsLine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-findings", "a-long-introduction-sentence-names-its-line")
	projection := readKiro(t, "## Introduction\n\n"+strings.Repeat("word ", 120)+"\n\n### Requirement 1\n\n#### Acceptance Criteria\n\n1. THE system SHALL run\n")
	if got := kiroFindings(projection, features.ErrorCodeSpecRead); projection.Spec != nil || len(got) != 1 || got[0] != kiroFile+"#L3" {
		t.Fatalf("findings = %v", projection.Findings)
	}
}

func TestUnclosedKiroCommentOrFenceIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "kiro-findings", "an-unclosed-kiro-comment-or-fence-is-an-error")
	for _, tail := range []string{"\n<!-- never closed\n", "\n```\nnever closed\n"} {
		projection := readKiro(t, kiroMinimal+tail)
		if got := kiroFindings(projection, features.ErrorCodeSpecRead); projection.Spec != nil || len(got) != 1 || got[0] != kiroFile+"#L16" {
			t.Errorf("%q: findings = %v", tail, projection.Findings)
		}
	}
}

func FuzzKiroReaderNeverCrashes(f *testing.F) {
	f.Add(kiroMinimal)
	f.Add("<!-- putnami:id a -->\n### Requirement 1\n#### Acceptance Criteria\n1.\n- \n")
	f.Fuzz(func(t *testing.T, text string) {
		features.ReadSpecSource(kiroReaderUnderTest(t), kiroInput(map[string]string{"requirements.md": text}))
	})
}
