package readers

import (
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestAnArchUnitFailureReadsToItsRuleAndViolations(t *testing.T) {
	spectest.Proves(t, feature, "archunit-cases-are-rules", "an-archunit-failure-reads-to-its-rule-and-violations")
	report, diagnostics := ReadFile("testdata/archunit/surefire.xml", ReadJUnit)
	if diagnostics != nil {
		t.Fatal(diagnostics)
	}
	var rules []ArchitectureRule
	for _, c := range report.Cases {
		if rule, ok := ArchUnitRule(c); ok {
			rules = append(rules, rule)
		}
	}
	golden(t, "testdata/archunit/surefire.xml", rules)
	broken := rules[0]
	if broken.Status != RuleBroken || broken.Count != 2 || len(broken.Violations) != 2 {
		t.Fatalf("broken rule %+v", broken)
	}
	if got, want := broken.Violations[0], (Violation{
		Message: "Method <com.acme.api.Quotes.quote(java.lang.String)> calls method <com.acme.pricing.internal.Rates.base()> in (Quotes.java:14)",
		Subject: "com.acme.api.Quotes", File: "Quotes.java", Line: 14,
	}); got != want {
		t.Errorf("violation %+v, want %+v", got, want)
	}
	if got := broken.Violations[1]; got.Subject != "com.acme.api.Quotes" || got.Line != 0 {
		t.Errorf("a nested class at line 0: %+v", got)
	}
	// A failure that states no violation counts one, with its first line.
	if empty := rules[2]; empty.Status != RuleBroken || empty.Count != 1 || len(empty.Violations) != 1 || empty.Name != "controllers_are_annotated" {
		t.Errorf("rule that checked no class %+v", empty)
	}
	// The violations are read from the body when the message is cut.
	body, _ := ArchUnitRule(TestCase{Name: "n", Status: StatusFailed, Failure: &Failure{
		Message: "Architecture Violation [Priority: MEDIUM] - Rule 'r' was violated (3 times):",
		Body:    "java.lang.AssertionError:\nArchitecture Violation [Priority: MEDIUM] - Rule 'r' was violated (3 times):\nClass <a.B> is in (B.java:1)\n\tat a.Test.run(Test.java:9)\n",
	}})
	if body.Name != "r" || body.Count != 3 || len(body.Violations) != 1 || body.Violations[0].Subject != "a.B" {
		t.Errorf("rule from the body %+v", body)
	}
	if !UsesArchUnit([]byte("import com.tngtech.archunit.junit.ArchTest;")) || UsesArchUnit([]byte("import org.junit.jupiter.api.Test;")) {
		t.Error("UsesArchUnit reads the ArchUnit import")
	}
}

func TestAPassingArchUnitCaseIsAKeptRule(t *testing.T) {
	spectest.Proves(t, feature, "archunit-cases-are-rules", "a-passing-archunit-case-is-a-kept-rule")
	rule, ok := ArchUnitRule(TestCase{Name: "no_cycles", Status: StatusPassed})
	if !ok || !reflect.DeepEqual(rule, ArchitectureRule{Name: "no_cycles", Status: RuleKept}) {
		t.Errorf("passing case %+v %v", rule, ok)
	}
	if _, ok := ArchUnitRule(TestCase{Name: "ignored", Status: StatusSkipped}); ok {
		t.Error("a skipped case is no rule")
	}
}

func TestADeptracJUnitReportReadsToItsLayers(t *testing.T) {
	spectest.Proves(t, feature, "deptrac-junit-reads-layers", "a-deptrac-junit-report-reads-to-its-layers")
	for _, fixture := range fixtures(t, "deptrac") {
		report, diagnostics := ReadFile(fixture, ReadDeptrac)
		if diagnostics != nil {
			t.Fatalf("%s: %v", fixture, diagnostics)
		}
		golden(t, fixture, report)
	}
	report, _ := ReadFile("testdata/deptrac/symfony.xml", ReadDeptrac)
	want := []ArchitectureRule{
		{Name: "Controller", Status: RuleBroken, Count: 2, Violations: []Violation{
			{Message: `App\Controller\QuoteController must not depend on App\Repository\QuoteRepository (Controller on Repository)`, Subject: `App\Controller\QuoteController`, Line: 9},
			{Message: `App\Controller\QuoteController must not depend on App\Repository\QuoteRepository (Controller on Repository)`, Subject: `App\Controller\QuoteController`, Line: 24},
		}},
		{Name: "Service", Status: RuleKept},
		{Name: "Repository", Status: RuleKept},
	}
	if !reflect.DeepEqual(report.Rules, want) {
		t.Errorf("rules %+v, want %+v", report.Rules, want)
	}
	skipped, _ := ReadFile("testdata/deptrac/skipped-errors.xml", ReadDeptrac)
	if len(skipped.Rules) != 1 || skipped.Rules[0].Name != "Domain" {
		t.Errorf("the unmatched skipped violations suite is no rule: %+v", skipped.Rules)
	}
}

func TestAnImportLinterOutputReadsToItsContracts(t *testing.T) {
	spectest.Proves(t, feature, "import-linter-text-reads-contracts", "an-import-linter-output-reads-to-its-contracts")
	for _, fixture := range fixtures(t, "import-linter") {
		report, diagnostics := ReadFile(fixture, ReadImportLinter)
		if diagnostics != nil {
			t.Fatalf("%s: %v", fixture, diagnostics)
		}
		golden(t, fixture, report)
	}
	report, _ := ReadFile("testdata/import-linter/lint-imports.txt", ReadImportLinter)
	want := []ArchitectureRule{
		{Name: "Shared does not import the services", Status: RuleKept},
		{Name: "Forbidden modules", Status: RuleBroken, Count: 2, Violations: []Violation{
			{Message: "shared.money -> api.main (l.3)", Subject: "shared.money", Line: 3},
			{Message: "shared.rates -> shared.money (l.5, l.9), shared.money -> api.main (l.3)", Subject: "shared.rates", Line: 5},
		}},
		{Name: "Layered architecture", Status: RuleBroken, Count: 1, Violations: []Violation{
			{Message: "worker.tasks (l.4), & worker.jobs -> api.client (l.?)", Subject: "worker.tasks", Line: 4},
		}},
		{Name: "Independent workers", Status: RuleKept},
	}
	if !reflect.DeepEqual(report.Rules, want) {
		t.Errorf("rules %+v, want %+v", report.Rules, want)
	}
	// A broken contract whose details list no chain counts one violation.
	bare, _ := ReadImportLinter([]byte("Analyzed 1 files, 0 dependencies.\n\nExhaustive layers BROKEN\n\nContracts: 0 kept, 1 broken.\n"))
	if len(bare.Rules) != 1 || bare.Rules[0].Count != 1 || bare.Rules[0].Violations != nil {
		t.Errorf("broken contract without chains %+v", bare.Rules)
	}
}

func TestAnImportLinterOutputWithoutContractsIsMalformed(t *testing.T) {
	spectest.Proves(t, feature, "import-linter-text-reads-contracts", "an-output-without-contracts-is-malformed")
	for _, data := range []string{"", "Contracts: 1 kept, 0 broken.\n", "Analyzed 1 files, 0 dependencies.\nnothing\n"} {
		if report, diagnostics := ReadImportLinter([]byte(data)); len(diagnostics) != 1 || diagnostics[0].Code != CodeMalformed || report.Rules != nil {
			t.Errorf("%q: %+v %v, want %s", data, report, diagnostics, CodeMalformed)
		}
	}
}
