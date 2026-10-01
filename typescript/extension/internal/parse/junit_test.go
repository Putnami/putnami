package parse

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEmptyTestSummary(t *testing.T) {
	s := emptyTestSummary()
	if s.Total != 0 || s.Failed != 0 || s.Skipped != 0 {
		t.Errorf("expected zero counts, got %+v", s)
	}
	if s.FailedTests == nil {
		t.Error("expected non-nil FailedTests slice")
	}
}

func TestParseJUnitFile_NotFound(t *testing.T) {
	summary, err := ParseJUnitFile("/nonexistent/results.junit.xml")
	if err != nil {
		t.Errorf("expected nil error for missing file, got %v", err)
	}
	if summary.Total != 0 {
		t.Errorf("expected 0 total, got %d", summary.Total)
	}
}

func TestParseJUnitFile_AllPassed(t *testing.T) {
	content := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="bun test" tests="5" assertions="10" failures="0" skipped="0" time="1.234">
  <testsuite name="src/foo.test.ts" file="src/foo.test.ts" tests="3" assertions="6" failures="0" skipped="0" time="0.5">
    <testcase name="does something" classname="src/foo.test.ts" file="src/foo.test.ts" line="5" time="0.1" assertions="2"/>
    <testcase name="does another thing" classname="src/foo.test.ts" file="src/foo.test.ts" line="10" time="0.2" assertions="2"/>
    <testcase name="handles edge case" classname="src/foo.test.ts" file="src/foo.test.ts" line="15" time="0.2" assertions="2"/>
  </testsuite>
  <testsuite name="src/bar.test.ts" file="src/bar.test.ts" tests="2" assertions="4" failures="0" skipped="0" time="0.734">
    <testcase name="bar test 1" classname="src/bar.test.ts" file="src/bar.test.ts" line="5" time="0.3" assertions="2"/>
    <testcase name="bar test 2" classname="src/bar.test.ts" file="src/bar.test.ts" line="10" time="0.434" assertions="2"/>
  </testsuite>
</testsuites>`

	path := writeJUnitFile(t, content)
	summary, err := ParseJUnitFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.Total != 5 {
		t.Errorf("expected 5 total, got %d", summary.Total)
	}
	if summary.Passed != 5 {
		t.Errorf("expected 5 passed, got %d", summary.Passed)
	}
	if summary.Failed != 0 {
		t.Errorf("expected 0 failed, got %d", summary.Failed)
	}
	if summary.Skipped != 0 {
		t.Errorf("expected 0 skipped, got %d", summary.Skipped)
	}
	if summary.Assertions != 10 {
		t.Errorf("expected 10 assertions, got %d", summary.Assertions)
	}
	if summary.Time != 1.234 {
		t.Errorf("expected time 1.234, got %f", summary.Time)
	}
	if len(summary.FailedTests) != 0 {
		t.Errorf("expected 0 failed tests, got %d", len(summary.FailedTests))
	}
}

func TestParseJUnitFile_WithFailures(t *testing.T) {
	content := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="bun test" tests="3" assertions="3" failures="1" skipped="0" time="0.5">
  <testsuite name="src/test.test.ts" file="src/test.test.ts" tests="3" assertions="3" failures="1" skipped="0" time="0.5">
    <testcase name="passes" classname="src/test.test.ts" file="src/test.test.ts" line="5" time="0.1" assertions="1"/>
    <testcase name="fails" classname="src/test.test.ts" file="src/test.test.ts" line="10" time="0.2" assertions="1">
      <failure type="AssertionError" message="Expected 1 to equal 2"/>
    </testcase>
    <testcase name="also passes" classname="src/test.test.ts" file="src/test.test.ts" line="15" time="0.2" assertions="1"/>
  </testsuite>
</testsuites>`

	path := writeJUnitFile(t, content)
	summary, err := ParseJUnitFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.Total != 3 {
		t.Errorf("expected 3 total, got %d", summary.Total)
	}
	if summary.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", summary.Failed)
	}
	if summary.Passed != 2 {
		t.Errorf("expected 2 passed, got %d", summary.Passed)
	}
	if len(summary.FailedTests) != 1 {
		t.Fatalf("expected 1 failed test, got %d", len(summary.FailedTests))
	}

	ft := summary.FailedTests[0]
	if ft.Name != "fails" {
		t.Errorf("expected name 'fails', got %q", ft.Name)
	}
	if ft.File != "src/test.test.ts" {
		t.Errorf("expected file 'src/test.test.ts', got %q", ft.File)
	}
	if ft.Failure == nil {
		t.Fatal("expected non-nil failure")
	}
	if ft.Failure.Type != "AssertionError" {
		t.Errorf("expected type 'AssertionError', got %q", ft.Failure.Type)
	}
	if ft.Failure.Message != "Expected 1 to equal 2" {
		t.Errorf("unexpected failure message: %q", ft.Failure.Message)
	}
}

func TestParseJUnitFile_WithSkipped(t *testing.T) {
	content := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="bun test" tests="4" assertions="2" failures="0" skipped="2" time="0.3">
  <testsuite name="src/skip.test.ts" file="src/skip.test.ts" tests="4" assertions="2" failures="0" skipped="2" time="0.3">
    <testcase name="runs" classname="src/skip.test.ts" file="src/skip.test.ts" line="1" time="0.1" assertions="1"/>
    <testcase name="skipped 1" classname="src/skip.test.ts" file="src/skip.test.ts" line="5" time="0" assertions="0">
      <skipped/>
    </testcase>
    <testcase name="also runs" classname="src/skip.test.ts" file="src/skip.test.ts" line="9" time="0.2" assertions="1"/>
    <testcase name="skipped 2" classname="src/skip.test.ts" file="src/skip.test.ts" line="13" time="0" assertions="0">
      <skipped/>
    </testcase>
  </testsuite>
</testsuites>`

	path := writeJUnitFile(t, content)
	summary, err := ParseJUnitFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if summary.Skipped != 2 {
		t.Errorf("expected 2 skipped, got %d", summary.Skipped)
	}
	if summary.Passed != 2 {
		t.Errorf("expected 2 passed (total - failed - skipped), got %d", summary.Passed)
	}
}

func TestParseJUnitFile_FailedTestUseSuiteFileWhenCaseFileMissing(t *testing.T) {
	content := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="1" failures="1" skipped="0" time="0.1">
  <testsuite name="suite" file="src/suite.test.ts" tests="1" failures="1" skipped="0" time="0.1">
    <testcase name="broken" classname="suite" line="3" time="0.1" assertions="1">
      <failure type="Error" message="Something broke"/>
    </testcase>
  </testsuite>
</testsuites>`

	path := writeJUnitFile(t, content)
	summary, err := ParseJUnitFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(summary.FailedTests) != 1 {
		t.Fatalf("expected 1 failed test, got %d", len(summary.FailedTests))
	}
	// Test case has no file attr; should fall back to suite file
	if summary.FailedTests[0].File != "src/suite.test.ts" {
		t.Errorf("expected suite file fallback 'src/suite.test.ts', got %q", summary.FailedTests[0].File)
	}
}

func TestParseJUnitFile_InvalidXML(t *testing.T) {
	path := writeJUnitFile(t, "not valid xml <><><")
	_, err := ParseJUnitFile(path)
	if err == nil {
		t.Error("expected error for invalid XML")
	}
}

func writeJUnitFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "results.junit.xml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write junit file: %v", err)
	}
	return path
}

func TestParseJUnitFile_CasesWalkNestedDescribes(t *testing.T) {
	// The shape bun writes: one top-level suite per test file, one nested
	// suite per describe block, and a classname that lists the describes in
	// reverse, double-escaped.
	content := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="bun test" tests="5" assertions="5" failures="2" skipped="1" time="1.5">
  <testsuite name="test/parser.test.ts" file="test/parser.test.ts" tests="5" assertions="5" failures="2" skipped="1" time="0">
    <testcase name="top level" classname="" time="0.0125" file="test/parser.test.ts" line="3" assertions="1" />
    <testsuite name="parser" file="test/parser.test.ts" line="5" tests="4" assertions="4" failures="2" skipped="1" time="1.2">
      <testcase name="parses" classname="parser" time="1.2" file="test/parser.test.ts" line="6" assertions="1" />
      <testsuite name="errors" file="test/parser.test.ts" line="10" tests="3" assertions="3" failures="2" skipped="1" time="0.003">
        <testcase name="rejects empty input" classname="errors &amp;gt; parser" time="0.001" file="test/parser.test.ts" line="11" assertions="1">
          <failure type="AssertionError" message="expect(received).toBe(expected)&#10;&#10;Expected: 1&#10;Received: 2&#10;">AssertionError: expect(received).toBe(expected)&#10;&#10;Expected: 1&#10;Received: 2&#10;&#10;      at test/parser.test.ts:12:5&#10;</failure>
        </testcase>
        <testcase name="reports the column" classname="errors &amp;gt; parser" time="0.002" assertions="1">
          <failure type="Error" message="column mismatch">at test/parser.test.ts:16:3</failure>
        </testcase>
        <testcase name="later" classname="errors &amp;gt; parser" time="0" file="test/parser.test.ts" line="20" assertions="0">
          <skipped message="needs a fixture" />
        </testcase>
      </testsuite>
    </testsuite>
  </testsuite>
  <testsuite name="test/other.test.ts" file="test/other.test.ts" tests="1" assertions="0" failures="0" skipped="1" time="0">
    <testcase name="todo" classname="" time="0" file="test/other.test.ts" line="1" assertions="0">
      <skipped />
    </testcase>
  </testsuite>
</testsuites>`

	summary, err := ParseJUnitFile(writeJUnitFile(t, content))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []TestCase{
		{Name: "top level", SuiteFile: "test/parser.test.ts", File: "test/parser.test.ts", Line: 3, Status: TestCasePassed, DurationMs: 13},
		{Name: "parser > parses", SuiteFile: "test/parser.test.ts", File: "test/parser.test.ts", Line: 6, Status: TestCasePassed, DurationMs: 1200},
		{
			Name:       "parser > errors > rejects empty input",
			SuiteFile:  "test/parser.test.ts",
			File:       "test/parser.test.ts",
			Line:       11,
			Status:     TestCaseFailed,
			DurationMs: 1,
			Output:     "AssertionError: expect(received).toBe(expected)\n\nExpected: 1\nReceived: 2\n\n      at test/parser.test.ts:12:5",
		},
		{
			Name:       "parser > errors > reports the column",
			SuiteFile:  "test/parser.test.ts",
			File:       "test/parser.test.ts",
			Status:     TestCaseFailed,
			DurationMs: 2,
			Output:     "column mismatch\nat test/parser.test.ts:16:3",
		},
		{
			Name:      "parser > errors > later",
			SuiteFile: "test/parser.test.ts",
			File:      "test/parser.test.ts",
			Line:      20,
			Status:    TestCaseSkipped,
			Output:    "needs a fixture",
		},
		{Name: "todo", SuiteFile: "test/other.test.ts", File: "test/other.test.ts", Line: 1, Status: TestCaseSkipped},
	}
	if !reflect.DeepEqual(summary.Cases, want) {
		t.Fatalf("Cases =\n  %#v\nwant\n  %#v", summary.Cases, want)
	}
	// The counts still come from the report's totals.
	if summary.Total != 5 || summary.Failed != 2 || summary.Skipped != 1 || len(summary.FailedTests) != 2 {
		t.Fatalf("summary = %+v, want the report's totals and two failed tests", summary)
	}
}

func TestParseJUnitFile_CaseFailureOutputKeepsMessageAndBody(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure string
		want    string
	}{
		{name: "message only", failure: `<failure message="  boom  " />`, want: "boom"},
		{name: "body only", failure: `<failure>  stack  </failure>`, want: "stack"},
		{name: "body starts with message", failure: `<failure message="boom">boom&#10;  at x.ts:1:1</failure>`, want: "boom\n  at x.ts:1:1"},
		{name: "distinct message and body", failure: `<failure message="boom">at x.ts:1:1</failure>`, want: "boom\nat x.ts:1:1"},
		{name: "neither", failure: `<failure type="AssertionError" />`, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := `<testsuites tests="1" failures="1"><testsuite name="a.test.ts" file="a.test.ts">` +
				`<testcase name="c" time="0">` + test.failure + `</testcase></testsuite></testsuites>`
			summary, err := ParseJUnitFile(writeJUnitFile(t, content))
			if err != nil {
				t.Fatal(err)
			}
			if len(summary.Cases) != 1 || summary.Cases[0].Status != TestCaseFailed {
				t.Fatalf("Cases = %#v, want one failed case", summary.Cases)
			}
			if got := summary.Cases[0].Output; got != test.want {
				t.Fatalf("Output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseJUnitFile_CaseDurationClampsToTheInt64Range(t *testing.T) {
	content := `<testsuites tests="4"><testsuite name="a.test.ts" file="a.test.ts">` +
		`<testcase name="timed" time="0.0125" />` +
		`<testcase name="negative" time="-1" />` +
		`<testcase name="not a number" time="NaN" />` +
		`<testcase name="huge" time="1e300" />` +
		`</testsuite></testsuites>`
	summary, err := ParseJUnitFile(writeJUnitFile(t, content))
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{13, 0, 0, math.MaxInt64}
	if len(summary.Cases) != len(want) {
		t.Fatalf("Cases = %#v, want %d cases", summary.Cases, len(want))
	}
	for i, testCase := range summary.Cases {
		if testCase.DurationMs != want[i] {
			t.Errorf("%s DurationMs = %d, want %d", testCase.Name, testCase.DurationMs, want[i])
		}
	}
}
