package parse

import (
	"errors"
	"regexp"
	"strings"

	"go.putnami.dev/evidence/readers"
	diag "go.putnami.dev/protocol/diagnostic"
)

// TestSummary aggregates test results.
type TestSummary struct {
	Total       int          `json:"total"`
	Passed      int          `json:"passed"`
	Failed      int          `json:"failed"`
	Skipped     int          `json:"skipped"`
	Assertions  int          `json:"assertions"`
	Time        float64      `json:"time"`
	FailedTests []FailedTest `json:"failedTests,omitempty"`
	// Cases lists every test case of the report, in report order.
	Cases []TestCase `json:"-"`
}

// Test-case statuses, the words of protocols/cli's TestCase.Status.
const (
	TestCasePassed  = "passed"
	TestCaseFailed  = "failed"
	TestCaseSkipped = "skipped"
)

// TestCase is one test case of a JUnit report. Its paths are the ones the
// report states: relative to the directory bun ran in.
type TestCase struct {
	// Name is the names of the describe blocks that enclose the case, then
	// the case name, joined by " > ".
	Name string
	// SuiteFile is the test file the case belongs to.
	SuiteFile string
	// File and Line locate the case. File falls back to the innermost
	// enclosing suite's file; Line is 0 when the report has none.
	File string
	Line int
	// Status is TestCasePassed, TestCaseFailed or TestCaseSkipped.
	Status     string
	DurationMs int64
	// Output is the failure message and body of a failed case, or the skip
	// message of a skipped one.
	Output string
}

// FailedTest identifies a failing test case.
type FailedTest struct {
	Name    string       `json:"name"`
	File    string       `json:"file,omitempty"`
	Line    int          `json:"line,omitempty"`
	Failure *TestFailure `json:"failure,omitempty"`
}

// TestFailure holds failure details.
type TestFailure struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

// emptyTestSummary returns a zero-value summary.
func emptyTestSummary() TestSummary {
	return TestSummary{FailedTests: []FailedTest{}}
}

// ParseJUnitFile parses a JUnit XML file and returns a test summary.
// Returns an empty summary if the file doesn't exist. The counts are the
// totals the report's root states.
func ParseJUnitFile(path string) (TestSummary, error) {
	report, diagnostics := readers.ReadFile(path, readers.ReadJUnit)
	if len(diagnostics) == 1 && diagnostics[0].Code == readers.CodeMissing {
		return emptyTestSummary(), nil
	}
	if diag.HasErrors(diagnostics) {
		return emptyTestSummary(), errors.New(diag.ErrorText(diagnostics))
	}

	summary := TestSummary{
		Total:       report.Totals.Tests,
		Passed:      report.Totals.Tests - report.Totals.Failures - report.Totals.Skipped,
		Failed:      report.Totals.Failures,
		Skipped:     report.Totals.Skipped,
		Assertions:  report.Totals.Assertions,
		Time:        report.Totals.Time,
		FailedTests: []FailedTest{},
	}
	for _, tc := range report.Cases {
		summary.Cases = append(summary.Cases, testCase(tc))
		if tc.Failure != nil {
			message := tc.Failure.Message
			if message == "" {
				message = tc.Failure.Body
			}
			summary.FailedTests = append(summary.FailedTests, FailedTest{
				Name:    tc.Name,
				File:    caseFile(tc),
				Line:    tc.Line,
				Failure: &TestFailure{Type: tc.Failure.Type, Message: message},
			})
		}
	}
	return summary, nil
}

// testCase maps a report case to a bun case. A top-level suite of a bun report
// is one test file and each suite below it is one describe block: the case's
// name is the describe names, then its own, joined by " > ".
func testCase(tc readers.TestCase) TestCase {
	names := []string{}
	suiteFile := ""
	if len(tc.Suites) > 0 {
		suiteFile = tc.Suites[0].File
		for _, describe := range tc.Suites[1:] {
			names = append(names, describe.Name)
		}
	}
	file := caseFile(tc)
	if suiteFile == "" {
		suiteFile = file
	}
	testCase := TestCase{
		Name:       strings.Join(append(names, tc.Name), " > "),
		SuiteFile:  suiteFile,
		File:       file,
		Line:       tc.Line,
		Status:     TestCasePassed,
		DurationMs: tc.DurationMs,
	}
	switch {
	case tc.Failure != nil:
		testCase.Status = TestCaseFailed
		testCase.Output = failureOutput(tc.Failure)
	case tc.Skipped != nil:
		testCase.Status = TestCaseSkipped
		testCase.Output = strings.TrimSpace(tc.Skipped.Message)
		if testCase.Output == "" {
			testCase.Output = strings.TrimSpace(tc.Skipped.Body)
		}
	}
	return testCase
}

// classnameFileRe matches a classname that names a test file, the form
// Vitest's junit reporter writes in place of a file attribute.
var classnameFileRe = regexp.MustCompile(`\.(test|spec)\.[cm]?[jt]sx?$`)

// caseFile is the file of a case: its file attribute, else the one of its
// nearest suite, else its classname when that names a test file.
func caseFile(tc readers.TestCase) string {
	if tc.File != "" || !classnameFileRe.MatchString(tc.Classname) {
		return tc.File
	}
	return tc.Classname
}

// failureOutput joins a failure's message and body. A body that already
// carries the message, as bun's "Type: message" and stack do, stands alone.
func failureOutput(failure *readers.Failure) string {
	message := strings.TrimSpace(failure.Message)
	body := strings.TrimSpace(failure.Body)
	switch {
	case body == "":
		return message
	case message == "" || strings.Contains(body, message):
		return body
	default:
		return message + "\n" + body
	}
}
