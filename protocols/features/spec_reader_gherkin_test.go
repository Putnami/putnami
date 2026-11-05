package features_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/conformance"
	"go.putnami.dev/protocol/features/spectest"
)

const gherkinPath = "features/invoice-export.feature"

func gherkinReader(t *testing.T) features.SpecReader {
	t.Helper()
	reader, exists := features.SpecReaderFor(features.SpecFormatGherkin)
	if !exists {
		t.Fatal("the gherkin reader is not registered")
	}
	return reader
}

func gherkinInput(path string, files map[string]string) features.SpecInput {
	input := features.SpecInput{
		Feature:   contractFeature,
		Reference: features.SpecReference{Format: features.SpecFormatGherkin, Path: path},
	}
	for name, text := range files {
		input.Files = append(input.Files, features.SpecFile{Path: name, Data: []byte(text)})
	}
	return input
}

func readGherkin(t *testing.T, text string) features.SpecProjection {
	t.Helper()
	return features.ReadSpecSource(gherkinReader(t), gherkinInput(gherkinPath, map[string]string{gherkinPath: text}))
}

// gherkinRequirements renders each requirement as "<id>@<line>:<scenario lines>".
func gherkinRequirements(projection features.SpecProjection) string {
	if projection.Spec == nil {
		return "<nil>"
	}
	parts := make([]string, 0, len(projection.Spec.Requirements))
	for i, requirement := range projection.Spec.Requirements {
		record := projection.Requirements[i]
		lines := make([]string, 0, len(record.Scenarios))
		for _, scenario := range record.Scenarios {
			lines = append(lines, scenario.Check+"@"+strings.TrimPrefix(scenario.Position.Field(), record.Position.File+"#L"))
		}
		parts = append(parts, requirement.ID+"@"+strings.TrimPrefix(record.Position.Field(), record.Position.File+"#L")+":"+strings.Join(lines, ","))
	}
	return strings.Join(parts, " ")
}

func gherkinCorpus(t *testing.T) features.SpecInput {
	t.Helper()
	directory := filepath.Join("conformance", "spec-source", "gherkin", "invoice-export")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files["features/invoice-export/"+entry.Name()] = string(data)
	}
	return gherkinInput("features/invoice-export", files)
}

func TestGherkinCorpusReproducesItsGolden(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-projection", "a-gherkin-corpus-reproduces-its-golden")
	conformance.RunSpecSource(t, gherkinReader(t), gherkinCorpus(t), filepath.Join("conformance", "spec-source", "gherkin", "invoice-export.golden.json"))
}

func TestGherkinDirectoryReadsEveryFeatureFile(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-projection", "a-gherkin-directory-reads-every-feature-file")
	projection := features.ReadSpecSource(gherkinReader(t), gherkinCorpus(t))
	want := "export-audit@1:an-export-leaves-an-audit-entry@2 export-a-closed-month@12:month-exported@16,an-open-month-does-not-export@20 each-row-matches-its-invoice@29:a-row-carries-the-invoice-total@30"
	if got := gherkinRequirements(projection); got != want || len(projection.Findings) != 0 {
		t.Fatalf("got %s, findings %v", got, projection.Findings)
	}
	if files := projection.Requirements[0].Position.File + " " + projection.Requirements[1].Position.File; files != "features/invoice-export/audit.feature features/invoice-export/export.feature" {
		t.Fatalf("files = %s", files)
	}
}

func TestFeatureWithNoDescriptionGivesItsNameAsOutcome(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-projection", "a-feature-with-no-description-gives-its-name-as-outcome")
	projection := readGherkin(t, "Feature: Invoices export\n  Scenario: It runs\n    Given a\n")
	if projection.Spec == nil || strings.Join(projection.Spec.Outcomes, "|") != "Invoices export" {
		t.Fatalf("spec = %+v, findings = %v", projection.Spec, projection.Findings)
	}
	unnamed := readGherkin(t, "Feature:\n  Rule: Kept\n")
	if unnamed.Spec != nil || codes(unnamed.Findings) != features.ErrorCodeSpecRead+"@"+gherkinPath+"#L1" {
		t.Fatalf("findings = %s", codes(unnamed.Findings))
	}
}

func TestRuleIsARequirementOfItsNameAndDescription(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-projection", "a-rule-is-a-requirement-of-its-name-and-description")
	projection := readGherkin(t, "Feature: F\n  Rule: Totals are exact.\n    No rounding.\n\n    Cents stay.\n  Rule: Bare\n")
	if projection.Spec == nil {
		t.Fatalf("findings = %v", projection.Findings)
	}
	got := []string{projection.Spec.Requirements[0].Text, projection.Spec.Requirements[1].Text}
	if strings.Join(got, "|") != "Totals are exact: No rounding. Cents stay.|Bare" || gherkinRequirements(projection) != "totals-are-exact@2: bare@6:" {
		t.Fatalf("got %q, %s", got, gherkinRequirements(projection))
	}
}

func TestFeatureWithNoRuleIsItsOwnRequirement(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-projection", "a-feature-with-no-rule-is-its-own-requirement")
	projection := readGherkin(t, "Feature: Export\n  Exports run.\n  Scenario: One\n")
	if got := gherkinRequirements(projection); got != "export@1:one@3" || projection.Spec.Requirements[0].Text != "Export" {
		t.Fatalf("got %s", got)
	}
	if got := gherkinRequirements(readGherkin(t, "Feature: F\n  Rule: R\n")); got != "r@2:" {
		t.Fatalf("a Feature with rules and no own scenario is no requirement: %s", got)
	}
}

func TestDocStringHidesNoKeywordAndAddsNoText(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-projection", "a-doc-string-hides-no-keyword-and-adds-no-text")
	text := "Feature: F\n  Scenario: One\n    Given\n      ```\n      Rule: not a rule\n      Scenario: not a scenario\n      ```\n  Scenario: Two\n    Given a\n      | Rule: a cell |\n"
	if got := gherkinRequirements(readGherkin(t, text)); got != "f@1:one@2,two@8" {
		t.Fatalf("got %s", got)
	}
}

// upstreamFeature is what the cucumber/gherkin parser gives for one file of its
// own corpus, reduced to what a reader projects.
type upstreamFeature struct {
	Feature *struct {
		Line        int      `json:"line"`
		Name        string   `json:"name"`
		Description []string `json:"description"`
		Scenarios   []int    `json:"scenarios"`
		Rules       []struct {
			Line      int    `json:"line"`
			Name      string `json:"name"`
			Scenarios []int  `json:"scenarios"`
		} `json:"rules"`
	} `json:"feature"`
}

func TestReaderAgreesWithTheUpstreamCorpus(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-projection", "the-reader-agrees-with-the-upstream-corpus")
	data, err := os.ReadFile(filepath.Join("fixtures", "gherkin", "upstream.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus map[string]upstreamFeature
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus) < 40 {
		t.Fatalf("the upstream corpus holds %d files", len(corpus))
	}
	for name, expected := range corpus {
		text, err := os.ReadFile(filepath.Join("fixtures", "gherkin", "upstream", name))
		if err != nil {
			t.Fatal(err)
		}
		reading := gherkinReader(t).Read(gherkinInput(name, map[string]string{name: string(text)}))
		if expected.Feature == nil {
			if !diag.HasErrors(reading.Findings) {
				t.Errorf("%s: no Feature and no error", name)
			}
			continue
		}
		want, outcomes := []string{}, expected.Feature.Description
		if len(outcomes) == 0 && expected.Feature.Name != "" {
			outcomes = []string{expected.Feature.Name}
		}
		refusedAt := map[string]bool{}
		// A name with no ID takes its position, so only a Feature with nothing
		// to state is refused.
		if len(expected.Feature.Rules) == 0 || len(expected.Feature.Scenarios) > 0 {
			want = append(want, upstreamOwner(expected.Feature.Line, expected.Feature.Scenarios))
		}
		for _, rule := range expected.Feature.Rules {
			want = append(want, upstreamOwner(rule.Line, rule.Scenarios))
		}
		if len(outcomes) == 0 {
			refusedAt[features.SpecPosition{File: name, Line: expected.Feature.Line}.Field()] = true
		}
		if len(refusedAt) > 0 {
			// A Feature with nothing to state is refused at its line.
			fields := map[string]bool{}
			for _, finding := range reading.Findings {
				fields[finding.Field] = true
			}
			if reading.Spec != nil || len(fields) == 0 || !subset(fields, refusedAt) {
				t.Errorf("%s: findings %v, want errors at %v", name, reading.Findings, refusedAt)
			}
			continue
		}
		got := make([]string, 0, len(reading.Requirements))
		for _, record := range reading.Requirements {
			lines := make([]int, 0, len(record.Scenarios))
			for _, scenario := range record.Scenarios {
				lines = append(lines, scenario.Position.Line)
			}
			got = append(got, upstreamOwner(record.Position.Line, lines))
		}
		gotOutcomes := []string{}
		if reading.Spec != nil {
			gotOutcomes = reading.Spec.Outcomes
		}
		if strings.Join(got, " ") != strings.Join(want, " ") || strings.Join(gotOutcomes, "|") != strings.Join(outcomes, "|") {
			t.Errorf("%s:\n requirements %v, want %v\n outcomes %q, want %q", name, got, want, gotOutcomes, outcomes)
		}
	}
}

func subset(fields, allowed map[string]bool) bool {
	for field := range fields {
		if !allowed[field] {
			return false
		}
	}
	return true
}

func upstreamOwner(line int, scenarios []int) string {
	parts := make([]string, 0, len(scenarios))
	for _, scenario := range scenarios {
		parts = append(parts, strings.TrimPrefix(features.SpecPosition{Line: scenario}.Field(), "#L"))
	}
	return strings.TrimPrefix(features.SpecPosition{Line: line}.Field(), "#L") + ":" + strings.Join(parts, ",")
}

func TestPutnamiIDTagNamesTheRequirement(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-identity", "a-putnami-id-tag-names-the-requirement")
	text := "@putnami-id:export\nFeature: Exporting things\n  Scenario: One\n"
	if got := gherkinRequirements(readGherkin(t, text)); got != "export@2:one@3" {
		t.Fatalf("got %s", got)
	}
	reordered := "Feature: F\n  @putnami-id:second\n  Rule: Renamed later\n  @putnami-id:first\n  Rule: Also renamed\n"
	if got := gherkinRequirements(readGherkin(t, reordered)); got != "second@3: first@5:" {
		t.Fatalf("got %s", got)
	}
}

func TestPutnamiCheckTagNamesTheScenarioCheck(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-identity", "a-putnami-check-tag-names-the-scenario-check")
	text := "Feature: F\n  @putnami-check:exported\n  Scenario: The accountant exports March\n  Scenario: Une exportation échoue\n  Scenario: It runs\n"
	projection := readGherkin(t, text)
	if got := gherkinRequirements(projection); got != "f@1:exported@3,une-exportation-echoue@4,it-runs@5" {
		t.Fatalf("got %s", got)
	}
}

func TestAnchorTagMayShareItsLine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-identity", "an-anchor-tag-may-share-its-line")
	text := "Feature: F\n  @slow @putnami-id:totals @wip # a comment\n  @owner:billing\n  Rule: R\n    @smoke @putnami-check:month-exported\n    Example: E\n"
	projection := readGherkin(t, text)
	if got := gherkinRequirements(projection); got != "totals@4:month-exported@6" || len(projection.Findings) != 0 {
		t.Fatalf("got %s, findings %v", got, projection.Findings)
	}
}

func TestNameWithNoIDNeedsATag(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-identity", "a-name-with-no-id-needs-a-tag")
	projection := readGherkin(t, "Feature: F\n  Rule: 黄金律\n")
	if got := gherkinRequirements(projection); got != "requirement-1@2:" || len(projection.Findings) != 0 {
		t.Fatalf("got %s, findings = %v", got, projection.Findings)
	}
	if enforce := projection.EnforceOnly(features.VerificationModeEnforce); codes(enforce) != features.ErrorCodeSpecRequirementUnanchored+"@"+gherkinPath+"#L2" {
		t.Fatalf("enforce = %v", enforce)
	}
	if got := gherkinRequirements(readGherkin(t, "Feature: F\n  @putnami-id:golden-rule\n  Rule: Règle d'or\n")); got != "golden-rule@3:" {
		t.Fatalf("got %s", got)
	}
}

func TestMisplacedAnchorTagIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-identity", "a-misplaced-anchor-tag-is-an-error")
	for name, test := range map[string]struct {
		text string
		line string
	}{
		"id above a scenario":        {"Feature: F\n  @putnami-id:one\n  Scenario: One\n", "2"},
		"check above a rule":         {"Feature: F\n  @putnami-check:one\n  Rule: R\n", "2"},
		"check above the feature":    {"@putnami-check:one\nFeature: F\n", "1"},
		"id above examples":          {"Feature: F\n  Scenario Outline: O\n    @putnami-id:rows\n    Examples:\n", "3"},
		"id above a background":      {"Feature: F\n  @putnami-id:setup\n  Background:\n", "2"},
		"id at the end of the file":  {"Feature: F\n  Scenario: S\n  @putnami-id:late\n", "3"},
		"id on a Feature with rules": {"@putnami-id:whole\nFeature: F\n  Rule: R\n", "1"},
		"check above a step":         {"Feature: F\n  Scenario: A\n    @putnami-check:x\n    Given a\n  Scenario: B\n", "3"},
	} {
		projection := readGherkin(t, test.text)
		if want := features.ErrorCodeSpecRead + "@" + gherkinPath + "#L" + test.line; codes(projection.Findings) != want {
			t.Errorf("%s: got %s, want %s", name, codes(projection.Findings), want)
		}
	}
}

func TestRepeatedOrMalformedAnchorTagIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-identity", "a-repeated-or-malformed-anchor-tag-is-an-error")
	for name, text := range map[string]string{
		"repeated id":      "Feature: F\n  @putnami-id:a\n  @putnami-id:b\n  Rule: R\n",
		"repeated check":   "Feature: F\n  @putnami-check:a @putnami-check:b\n  Scenario: S\n",
		"id with no ID":    "Feature: F\n  @putnami-id\n  Rule: R\n",
		"upper-case id":    "Feature: F\n  @putnami-id:Totals\n  Rule: R\n",
		"empty check":      "Feature: F\n  @putnami-check:\n  Scenario: S\n",
		"underscore check": "Feature: F\n  @putnami-check:a_b\n  Scenario: S\n",
	} {
		projection := readGherkin(t, text)
		if projection.Spec != nil || len(projection.Findings) != 1 || projection.Findings[0].Code != features.ErrorCodeSpecRead {
			t.Errorf("%s: findings = %v", name, projection.Findings)
		}
	}
}

func TestScenarioBelongsToItsRuleOrItsFeature(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-scenarios", "a-scenario-belongs-to-its-rule-or-its-feature")
	text := "Feature: F\n  Scenario: Before\n  Rule: R\n    Example: Inside\n"
	projection := readGherkin(t, text)
	if got := gherkinRequirements(projection); got != "f@1:before@2 r@3:inside@4" || projection.Scenarios != nil {
		t.Fatalf("got %s, file scenarios %v", got, projection.Scenarios)
	}
}

func TestScenarioOutlineCountsOnce(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-scenarios", "a-scenario-outline-counts-once")
	text := "Feature: F\n  Scenario Outline: Export <month>\n    When <month>\n    Examples:\n      | month |\n      | May |\n    Examples: More\n      | June |\n"
	if got := gherkinRequirements(readGherkin(t, text)); got != "f@1:export-month@2" {
		t.Fatalf("got %s", got)
	}
}

func TestGherkinMismatchIsAPositionedWarningEitherWay(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-scenarios", "a-gherkin-mismatch-is-a-positioned-warning-either-way")
	text := "Feature: F\n  @putnami-id:totals\n  Rule: R\n    @putnami-check:declared\n    Scenario: One\n    Scenario: Stray\n"
	feature := features.Feature{Requirements: []features.Requirement{{
		ID: "totals", Verification: &features.VerificationCriterion{Checks: []string{"declared", "unproved"}},
	}}}
	findings := features.CompareSpecScenarios(readGherkin(t, text), feature)
	code := features.WarningCodeSpecScenarioMismatch + "@" + gherkinPath + "#L"
	if got := codes(findings); got != code+"3,"+code+"6" || diag.HasErrors(findings) {
		t.Fatalf("got %s", got)
	}
	if strings.Join(feature.Requirements[0].Verification.Checks, ",") != "declared,unproved" {
		t.Fatal("the comparison changed the checks")
	}
}

func TestEveryGherkinLanguageParses(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-languages", "every-gherkin-language-parses")
	catalogs, err := filepath.Glob(filepath.Join("gherkin", "locales", "*.json"))
	if err != nil || len(catalogs) < 80 {
		t.Fatalf("catalogs = %d, err = %v", len(catalogs), err)
	}
	for _, catalog := range catalogs {
		var words map[string][]string
		data, err := os.ReadFile(catalog)
		if err == nil {
			err = json.Unmarshal(data, &words)
		}
		if err != nil {
			t.Fatal(err)
		}
		language := strings.TrimSuffix(filepath.Base(catalog), ".json")
		for _, feature := range words["feature"] {
			for _, rule := range words["rule"] {
				for _, scenario := range append(words["scenario"], words["scenarioOutline"]...) {
					text := "# language: " + language + "\n" + feature + ": F\n  @putnami-id:r\n  " + rule + ": R\n    " + scenario + ": S\n"
					if got := gherkinRequirements(readGherkin(t, text)); got != "r@4:s@5" {
						t.Errorf("%s %q %q %q: got %s", language, feature, rule, scenario, got)
					}
				}
			}
		}
	}
}

func TestFrenchFeatureFileParses(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-languages", "a-french-feature-file-parses")
	text := "# language: fr\nFonctionnalité: Export des factures\n  Un comptable exporte un mois clos.\n\n" +
		"  Contexte:\n    Soit un mois clos\n\n  @putnami-id:export-mensuel\n  Règle: Un mois clos donne un fichier\n\n" +
		"    @putnami-check:mars-exporte\n    Scénario: Le comptable exporte mars\n      Quand il exporte mars\n      Alors un fichier existe\n\n" +
		"    Plan du scénario: Un mois ouvert\n      Quand il exporte <mois>\n      Exemples:\n        | mois |\n        | avril |\n"
	projection := readGherkin(t, text)
	if got := gherkinRequirements(projection); got != "export-mensuel@9:mars-exporte@12,un-mois-ouvert@16" || len(projection.Findings) != 0 {
		t.Fatalf("got %s, findings %v", got, projection.Findings)
	}
	if strings.Join(projection.Spec.Outcomes, "|") != "Un comptable exporte un mois clos." {
		t.Fatalf("outcomes = %q", projection.Spec.Outcomes)
	}
	english := readGherkin(t, strings.TrimPrefix(text, "# language: fr\n"))
	if english.Spec != nil || codes(english.Findings) != features.ErrorCodeSpecRead+"@"+gherkinPath+"#L1" {
		t.Fatalf("without the header the file is English: %s", codes(english.Findings))
	}
}

func TestUnknownLanguageIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-languages", "an-unknown-language-is-an-error")
	projection := readGherkin(t, "\n# language: xx-nowhere\nFeature: F\n")
	if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@"+gherkinPath+"#L2" {
		t.Fatalf("findings = %s", codes(projection.Findings))
	}
	if got := gherkinRequirements(readGherkin(t, "Feature: F\n  # language: xx-nowhere\n")); got != "f@1:" {
		t.Fatalf("a header below the Feature is a comment: %s", got)
	}
}

func TestFileWithNoFeatureIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-malformed-files", "a-file-with-no-feature-is-an-error")
	for _, text := range []string{"", "# only a comment\n", "Scenario: S\n"} {
		projection := readGherkin(t, text)
		if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@"+gherkinPath+"#L1" {
			t.Errorf("%q: findings = %s", text, codes(projection.Findings))
		}
	}
}

func TestSecondFeatureInOneFileIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-malformed-files", "a-second-feature-in-one-file-is-an-error")
	projection := readGherkin(t, "Feature: A\n  Scenario: S\n\nFeature: B\n  Scenario: T\n")
	if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@"+gherkinPath+"#L4" {
		t.Fatalf("findings = %s", codes(projection.Findings))
	}
}

func TestTextBeforeTheFeatureIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-malformed-files", "a-file-with-no-feature-is-an-error")
	projection := readGherkin(t, "# a comment\n@tag\nRule: Early\nFeature: F\n")
	if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@"+gherkinPath+"#L3" {
		t.Fatalf("findings = %s", codes(projection.Findings))
	}
}

func TestADescriptionDocStringOpensNothing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-projection", "a-doc-string-hides-no-keyword-and-adds-no-text")
	text := "Feature: F\n  Scenario: One\n    \"\"\"\n    Given a\n  Scenario: Two\n  Rule: R\n    Described.\n    \"\"\"\n    Scenario: Three\n"
	if got := gherkinRequirements(readGherkin(t, text)); got != "f@1:one@2,two@5 r@6:three@9" {
		t.Fatalf("got %s", got)
	}
}

func TestUnclosedDocStringIsAnError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "gherkin-malformed-files", "an-unclosed-doc-string-is-an-error")
	projection := readGherkin(t, "Feature: F\n  Scenario: S\n    Given\n      \"\"\"\n  Rule: hidden\n")
	if projection.Spec != nil || codes(projection.Findings) != features.ErrorCodeSpecRead+"@"+gherkinPath+"#L4" {
		t.Fatalf("findings = %s", codes(projection.Findings))
	}
}

func TestGherkinCRLFAndBOMReadAsLF(t *testing.T) {
	t.Parallel()
	text := "\ufeffFeature: F\r\n  Why.\r\n  Scenario: S\r\n"
	projection := readGherkin(t, text)
	if got := gherkinRequirements(projection); got != "f@1:s@3" || projection.Spec.Outcomes[0] != "Why." {
		t.Fatalf("got %s, %v", got, projection.Findings)
	}
}
