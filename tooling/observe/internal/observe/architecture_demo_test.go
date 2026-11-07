package observe

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/reports"
)

const architectureFeature = "tooling/observe-architecture"

// observeArchitectureDemo observes one architecture demo on GitHub Actions,
// with the given options on top.
func observeArchitectureDemo(t *testing.T, root, base string, with func(*Options)) Observation {
	t.Helper()
	env := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-architecture", "GITHUB_REPOSITORY_ID": "1121", "GITHUB_RUN_ID": "41335602201", "GITHUB_JOB": "test", "GITHUB_REF": "refs/heads/feature"}
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	if with != nil {
		with(&opts)
	}
	return Run(context.Background(), opts)
}

func rulesByName(o Observation) map[string]evidence.ArchitectureRule {
	out := map[string]evidence.ArchitectureRule{}
	for _, rule := range o.Analysis.Architecture {
		out[rule.Tool+" "+rule.Rule] = rule
	}
	return out
}

func testsOf(o Observation, component string) (evidence.ComponentTests, bool) {
	for _, entry := range o.Analysis.Tests {
		if entry.Component == component {
			return entry, true
		}
	}
	return evidence.ComponentTests{}, false
}

const archUnitRuleText = "classes that reside in a package '..pricing.internal..' should only be accessed by classes that reside in a package '..pricing..'"

func TestTheArchUnitDemoShowsTheBrokenRule(t *testing.T) {
	spectest.Proves(t, architectureFeature, "architecture-change-analysis", "the-archunit-demo-shows-the-broken-rule")
	spectest.Proves(t, architectureFeature, "archunit-cases-are-architecture", "an-archunit-test-class-is-an-architecture-result")
	root, base := demoRepository(t, "archunit")
	o := observeArchitectureDemo(t, root, base, nil)
	noUnplacedReports(t, o)
	noWarnings(t, o)
	if changed := impacts(o, evidence.ImpactChanged); !reflect.DeepEqual(changed, []string{"api"}) {
		t.Errorf("changed %v", changed)
	}
	want := []evidence.ArchitectureRule{
		{Tool: "archunit", Rule: archUnitRuleText, Status: evidence.ArchitectureBroken, ViolationCount: 1, Violations: []evidence.ArchitectureViolation{{
			Message:   "Method <com.acme.api.Quotes.quote(long, int)> calls method <com.acme.pricing.internal.Rates.base()> in (Quotes.java:8)",
			File:      "api/src/main/java/com/acme/api/Quotes.java",
			Line:      8,
			Component: "api",
		}}},
		{Tool: "archunit", Rule: "no_cycles_between_modules", Status: evidence.ArchitectureKept},
	}
	if !reflect.DeepEqual(o.Analysis.Architecture, want) {
		t.Errorf("architecture %+v, want %+v", o.Analysis.Architecture, want)
	}
	// The ArchUnit cases are rules, not tests: api counts QuotesTest alone.
	if tests, _ := testsOf(o, "api"); tests.Passed != 1 || tests.Failed != 0 {
		t.Errorf("api tests %+v", tests)
	}
	requireLines(t, summary(t, o),
		"  architecture  2 rules, 1 broken\n",
		"    broken    archunit: "+archUnitRuleText+" (1 violation)\n",
		"      api api/src/main/java/com/acme/api/Quotes.java:8: Method <com.acme.api.Quotes.quote(long, int)> calls method <com.acme.pricing.internal.Rates.base()> in (Quotes.java:8)\n",
	)
	if doc := validEvidence(t, o); !reflect.DeepEqual(doc.Architecture, want) {
		t.Errorf("evidence architecture %+v", doc.Architecture)
	}
}

func TestAClassPatternMakesACaseAnArchitectureResult(t *testing.T) {
	spectest.Proves(t, architectureFeature, "archunit-cases-are-architecture", "a-class-pattern-makes-a-case-an-architecture-result")
	// Without its source, ArchitectureTest is an ordinary test class...
	root, base := demoRepositoryWithout(t, "archunit", func(name string) bool { return strings.HasSuffix(name, "/ArchitectureTest.java") })
	o := observeArchitectureDemo(t, root, base, nil)
	if len(o.Analysis.Architecture) != 0 {
		t.Errorf("architecture without an ArchUnit source %+v", o.Analysis.Architecture)
	}
	if tests, _ := testsOf(o, "api"); tests.Passed != 2 || tests.Failed != 1 {
		t.Errorf("api tests without a pattern %+v", tests)
	}
	// ...until --archunit-classes names it.
	o = observeArchitectureDemo(t, root, base, func(opts *Options) { opts.ArchUnitClasses = []string{"*ArchitectureTest"} })
	if rules := rulesByName(o); rules["archunit "+archUnitRuleText].Status != evidence.ArchitectureBroken || len(rules) != 2 {
		t.Errorf("architecture with a pattern %+v", o.Analysis.Architecture)
	}
	if tests, _ := testsOf(o, "api"); tests.Passed != 1 || tests.Failed != 0 {
		t.Errorf("api tests with a pattern %+v", tests)
	}
}

func TestTheDeptracDemoShowsTheBrokenRule(t *testing.T) {
	spectest.Proves(t, architectureFeature, "architecture-change-analysis", "the-deptrac-demo-shows-the-broken-rule")
	spectest.Proves(t, architectureFeature, "violations-are-placed", "each-violation-goes-to-the-component-of-its-file")
	root, base := demoRepository(t, "deptrac")
	o := observeArchitectureDemo(t, root, base, nil)
	noUnplacedReports(t, o)
	noWarnings(t, o)
	controller := "src/Controller/QuoteController.php"
	calculator := "packages/pricing/src/Calculator.php"
	want := []evidence.ArchitectureRule{
		{Tool: "deptrac", Rule: "Controller", Status: evidence.ArchitectureBroken, ViolationCount: 2, Violations: []evidence.ArchitectureViolation{
			{Message: `App\Controller\QuoteController must not depend on App\Repository\QuoteRepository (Controller on Repository)`, File: controller, Line: 7, Component: "."},
			{Message: `App\Controller\QuoteController must not depend on App\Repository\QuoteRepository (Controller on Repository)`, File: controller, Line: 12, Component: "."},
		}},
		{Tool: "deptrac", Rule: "Service", Status: evidence.ArchitectureKept},
		{Tool: "deptrac", Rule: "Pricing", Status: evidence.ArchitectureBroken, ViolationCount: 2, Violations: []evidence.ArchitectureViolation{
			{Message: `Acme\Pricing\Calculator must not depend on App\Service\QuoteService (Pricing on Service)`, File: calculator, Line: 7, Component: "packages/pricing"},
			{Message: `Acme\Pricing\Calculator must not depend on App\Service\QuoteService (Pricing on Service)`, File: calculator, Line: 16, Component: "packages/pricing"},
		}},
	}
	if !reflect.DeepEqual(o.Analysis.Architecture, want) {
		t.Errorf("architecture %+v, want %+v", o.Analysis.Architecture, want)
	}
	// A deptrac report is no test report.
	if len(o.Analysis.Tests) != 0 {
		t.Errorf("tests %+v", o.Analysis.Tests)
	}
	requireLines(t, summary(t, o),
		"  architecture  3 rules, 2 broken\n",
		"    broken    deptrac: Controller (2 violations)\n",
		"      . src/Controller/QuoteController.php:7: App\\Controller\\QuoteController must not depend on App\\Repository\\QuoteRepository (Controller on Repository)\n",
		"    broken    deptrac: Pricing (2 violations)\n",
		"      packages/pricing packages/pricing/src/Calculator.php:16: Acme\\Pricing\\Calculator must not depend on App\\Service\\QuoteService (Pricing on Service)\n",
	)
	if doc := validEvidence(t, o); !reflect.DeepEqual(doc.Architecture, want) {
		t.Errorf("evidence architecture %+v", doc.Architecture)
	}
}

func TestTheImportLinterDemoShowsTheBrokenContract(t *testing.T) {
	spectest.Proves(t, architectureFeature, "architecture-change-analysis", "the-import-linter-demo-shows-the-broken-contract")
	root, base := demoRepository(t, "import-linter")
	o := observeArchitectureDemo(t, root, base, nil)
	noUnplacedReports(t, o)
	noWarnings(t, o)
	want := []evidence.ArchitectureRule{
		{Tool: "import-linter", Rule: "Shared imports no service", Status: evidence.ArchitectureBroken, ViolationCount: 1, Violations: []evidence.ArchitectureViolation{
			{Message: "shared.money -> api.main (l.3)", File: "shared/src/shared/money.py", Line: 3, Component: "shared"},
		}},
		{Tool: "import-linter", Rule: "Services are independent", Status: evidence.ArchitectureKept},
	}
	if !reflect.DeepEqual(o.Analysis.Architecture, want) {
		t.Errorf("architecture %+v, want %+v", o.Analysis.Architecture, want)
	}
	requireLines(t, summary(t, o),
		"  architecture  2 rules, 1 broken\n",
		"    broken    import-linter: Shared imports no service (1 violation)\n",
		"      shared shared/src/shared/money.py:3: shared.money -> api.main (l.3)\n",
	)
	if doc := validEvidence(t, o); !reflect.DeepEqual(doc.Architecture, want) {
		t.Errorf("evidence architecture %+v", doc.Architecture)
	}
}

func TestDeptracAndImportLinterReportsAreFoundByContent(t *testing.T) {
	spectest.Proves(t, architectureFeature, "architecture-report-discovery", "deptrac-and-import-linter-reports-are-found-by-content")
	for name, want := range map[string]reports.Report{
		"deptrac":       {Path: "reports/deptrac.xml", Kind: reports.KindArchitecture, Format: reports.FormatDeptrac, Source: reports.SourceDiscovered},
		"import-linter": {Path: "import-linter.txt", Kind: reports.KindArchitecture, Format: reports.FormatImportLinter, Source: reports.SourceDiscovered},
	} {
		root, base := demoRepository(t, name)
		o := observeArchitectureDemo(t, root, base, nil)
		found := false
		for _, r := range o.Reports {
			found = found || r == want
			if r.Kind == reports.KindJUnit && r.Path == want.Path {
				t.Errorf("%s: the architecture report is also a test report", name)
			}
		}
		if !found {
			t.Errorf("%s: reports %+v, want %+v", name, o.Reports, want)
		}
	}
	// An ordinary JUnit report, or a text file that is no lint-imports
	// output, is no architecture report.
	for head, want := range map[string]string{
		`<testsuites><testsuite name="a" package="" hostname="localhost"><testcase name="t" classname="a"/></testsuite></testsuites>`: reports.FormatJUnit,
		`<testsuites><testsuite name="Unmatched skipped violations" package="" hostname="localhost"/></testsuites>`:                   reports.FormatDeptrac,
		"\x1b[1mAnalyzed 3 files, 2 dependencies.\x1b[0m\n":                                                                           reports.FormatImportLinter,
		"Analyzed the repository.\n": "",
	} {
		if got := reports.Sniff([]byte(head)); got != want {
			t.Errorf("Sniff(%q) = %q, want %q", head, got, want)
		}
	}
}

func TestTheArchitectureFlagNamesTheReports(t *testing.T) {
	spectest.Proves(t, architectureFeature, "architecture-report-discovery", "the-architecture-flag-names-the-reports")
	root, base := demoRepository(t, "import-linter")
	// A named report replaces discovery: the discovered one is not read.
	if err := os.WriteFile(filepath.Join(root, "lint-imports.log"), []byte(readDemoFile(t, "import-linter", "ci/import-linter.txt")), 0o644); err != nil {
		t.Fatal(err)
	}
	o := observeArchitectureDemo(t, root, base, func(opts *Options) { opts.Architecture = []string{"lint-imports.log"} })
	var architecture []reports.Report
	for _, r := range o.Reports {
		if r.Kind == reports.KindArchitecture {
			architecture = append(architecture, r)
		}
	}
	want := []reports.Report{{Path: "lint-imports.log", Kind: reports.KindArchitecture, Format: reports.FormatImportLinter, Source: reports.SourceNamed}}
	if !reflect.DeepEqual(architecture, want) {
		t.Errorf("architecture reports %+v, want %+v", architecture, want)
	}
	if len(o.Analysis.Architecture) != 2 {
		t.Errorf("architecture %+v", o.Analysis.Architecture)
	}
	// A named file that is no architecture report is a diagnostic.
	o = observeArchitectureDemo(t, root, base, func(opts *Options) { opts.Architecture = []string{"shared/pyproject.toml"} })
	unrecognized := false
	for _, d := range o.Diagnostics {
		unrecognized = unrecognized || d.Code == reports.CodeReportUnrecognized
	}
	if !unrecognized || len(o.Analysis.Architecture) != 0 {
		t.Errorf("a named file that is no report: %+v, architecture %+v", o.Diagnostics, o.Analysis.Architecture)
	}
}

// readDemoFile returns one file of a demo fixture.
func readDemoFile(t *testing.T, demo, name string) string {
	t.Helper()
	for _, f := range readDemo(t, demo) {
		if f.name == name {
			return f.content
		}
	}
	t.Fatalf("demo %s has no %s", demo, name)
	return ""
}
