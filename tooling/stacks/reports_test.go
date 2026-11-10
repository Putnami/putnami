package stacks

import (
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const junitPass = `<?xml version="1.0"?>
<testsuites tests="3" failures="0">
  <testsuite name="pricing">
    <testcase classname="pricing" name="adds"/>
    <testcase classname="pricing" name="rounds"/>
    <testcase classname="pricing" name="later"><skipped/></testcase>
  </testsuite>
</testsuites>`

const junitFail = `<?xml version="1.0"?>
<testsuite name="api">
  <testcase classname="api" name="serves"><failure message="boom">panicked</failure></testcase>
</testsuite>`

const lcov = "SF:/src/lib.rs\nDA:1,1\nDA:2,0\nDA:3,4\nend_of_record\n"

func globUnder(base string) func(string) ([]string, error) {
	return func(p string) ([]string, error) { return globFiles(base, p) }
}

func codes(d []diag.Diagnostic) string {
	var out []string
	for _, x := range d {
		out = append(out, x.Code)
	}
	return strings.Join(out, ",")
}

func TestReadReportsSumsTestsAndCoverage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "out/junit.xml", junitPass)
	writeFile(t, dir, "out/lcov.info", lcov)
	r := &Reports{JUnit: []string{"out/*.xml"}, Coverage: []Coverage{{Format: "lcov", Paths: []string{"out/lcov.info"}}}}
	s, d := ReadReports(r, globUnder(dir), dir, Attribution{})
	if len(d) != 0 {
		t.Fatalf("diagnostics: %v", d)
	}
	if s.Tests != (TestTotals{Total: 3, Passed: 2, Skipped: 1}) {
		t.Errorf("tests = %+v", s.Tests)
	}
	if s.Coverage == nil || s.Coverage.Total != 3 || s.Coverage.Covered != 2 {
		t.Errorf("coverage = %+v", s.Coverage)
	}
	if strings.Join(s.Reports, ",") != "out/junit.xml,out/lcov.info" {
		t.Errorf("reports = %v", s.Reports)
	}
}

func TestReadReportsFailsOnAFailedCaseAndOnAMissingReport(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "out/junit.xml", junitFail)
	s, d := ReadReports(&Reports{JUnit: []string{"out/**/*.xml"}}, globUnder(dir), dir, Attribution{})
	if s.Tests.Failed != 1 || codes(d) != CodeTestFailed || !strings.Contains(d[0].Message, "api > serves") {
		t.Errorf("failed case: %+v %v", s, d)
	}
	_, d = ReadReports(&Reports{JUnit: []string{"none/*.xml"}, Coverage: []Coverage{{Format: "lcov", Paths: []string{"none.info"}}}}, globUnder(dir), dir, Attribution{})
	if codes(d) != CodeReportMissing+","+CodeReportMissing {
		t.Errorf("missing reports: %v", d)
	}
	if s, d := ReadReports(nil, globUnder(dir), dir, Attribution{}); len(d) != 0 || s.Tests.Total != 0 {
		t.Errorf("no reports: %+v %v", s, d)
	}
	_ = filepath.Join
}

func TestReadReportsMergesASourceFileAcrossCoverageReports(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "out/unit/coverage.dat", "SF:api/api.go\nDA:1,1\nDA:2,1\nDA:3,0\nDA:4,0\nend_of_record\n")
	writeFile(t, dir, "out/integration/coverage.dat", "SF:api/api.go\nDA:1,0\nDA:2,0\nDA:3,2\nDA:4,0\nend_of_record\nSF:api/routes.go\nLF:5\nLH:2\nend_of_record\n")
	writeFile(t, dir, "out/other/coverage.dat", "SF:api/routes.go\nLF:5\nLH:4\nend_of_record\n")
	r := &Reports{Coverage: []Coverage{{Format: "lcov", Paths: []string{"out/**/coverage.dat"}}}}
	s, d := ReadReports(r, globUnder(dir), dir, Attribution{})
	if len(d) != 0 {
		t.Fatalf("diagnostics: %v", d)
	}
	if s.Coverage == nil || s.Coverage.Total != 9 || s.Coverage.Covered != 7 {
		t.Errorf("coverage = %+v, want api.go's 4 lines with 3 covered once each and routes.go's largest stated 5/4", s.Coverage)
	}
}

func TestReadReportsWarnsWhenAnOptionalCoverageReportIsMissing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "out/junit.xml", junitPass)
	r := &Reports{JUnit: []string{"out/*.xml"}, Coverage: []Coverage{{Format: "cobertura", Paths: []string{"out/*/coverage.cobertura.xml"}, Optional: true}}}
	s, d := ReadReports(r, globUnder(dir), dir, Attribution{})
	if codes(d) != CodeCoverageUnavailable || d[0].Severity != diag.Warning {
		t.Fatalf("diagnostics: %v", d)
	}
	if s.Coverage != nil || s.Tests.Passed != 2 {
		t.Errorf("summary: %+v", s)
	}
}
