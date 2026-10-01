package parse

import (
	"encoding/xml"
	"math"
	"os"
	"slices"
	"strings"
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

// JUnit XML structures
type junitTestSuites struct {
	XMLName    xml.Name         `xml:"testsuites"`
	Name       string           `xml:"name,attr"`
	Tests      int              `xml:"tests,attr"`
	Assertions int              `xml:"assertions,attr"`
	Failures   int              `xml:"failures,attr"`
	Skipped    int              `xml:"skipped,attr"`
	Time       float64          `xml:"time,attr"`
	TestSuites []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	Name       string           `xml:"name,attr"`
	File       string           `xml:"file,attr"`
	Tests      int              `xml:"tests,attr"`
	Assertions int              `xml:"assertions,attr"`
	Failures   int              `xml:"failures,attr"`
	Skipped    int              `xml:"skipped,attr"`
	Time       float64          `xml:"time,attr"`
	Line       int              `xml:"line,attr"`
	TestCases  []junitTestCase  `xml:"testcase"`
	TestSuites []junitTestSuite `xml:"testsuite"`
}

type junitTestCase struct {
	Name       string        `xml:"name,attr"`
	Classname  string        `xml:"classname,attr"`
	File       string        `xml:"file,attr"`
	Line       int           `xml:"line,attr"`
	Time       float64       `xml:"time,attr"`
	Assertions int           `xml:"assertions,attr"`
	Failure    *junitFailure `xml:"failure"`
	Skipped    *junitSkipped `xml:"skipped"`
}

type junitFailure struct {
	Type    string `xml:"type,attr"`
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

// emptyTestSummary returns a zero-value summary.
func emptyTestSummary() TestSummary {
	return TestSummary{FailedTests: []FailedTest{}}
}

// ParseJUnitFile parses a JUnit XML file and returns a test summary.
// Returns an empty summary if the file doesn't exist.
func ParseJUnitFile(path string) (TestSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyTestSummary(), nil
		}
		return emptyTestSummary(), err
	}

	var suites junitTestSuites
	if err := xml.Unmarshal(data, &suites); err != nil {
		return emptyTestSummary(), err
	}

	// Collect failed tests recursively
	var failedTests []FailedTest
	collectFailed(suites.TestSuites, &failedTests)

	var cases []TestCase
	for _, fileSuite := range suites.TestSuites {
		collectCases(fileSuite, fileSuite.File, nil, fileSuite.File, &cases)
	}

	return TestSummary{
		Total:       suites.Tests,
		Passed:      suites.Tests - suites.Failures - suites.Skipped,
		Failed:      suites.Failures,
		Skipped:     suites.Skipped,
		Assertions:  suites.Assertions,
		Time:        suites.Time,
		FailedTests: failedTests,
		Cases:       cases,
	}, nil
}

// collectCases appends every case of suite and of its nested suites. A
// top-level suite of a bun report is one test file and each suite below it is
// one describe block: describes names the blocks that enclose suite, and file
// is the innermost file a suite states. The cases of a suite come before the
// cases of its nested suites.
func collectCases(suite junitTestSuite, testFile string, describes []string, file string, out *[]TestCase) {
	for _, tc := range suite.TestCases {
		testCase := TestCase{
			Name:       strings.Join(append(slices.Clone(describes), tc.Name), " > "),
			SuiteFile:  testFile,
			File:       tc.File,
			Line:       tc.Line,
			Status:     TestCasePassed,
			DurationMs: durationMs(tc.Time),
		}
		if testCase.File == "" {
			testCase.File = file
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
		*out = append(*out, testCase)
	}
	for _, nested := range suite.TestSuites {
		nestedFile := nested.File
		if nestedFile == "" {
			nestedFile = file
		}
		collectCases(nested, testFile, append(slices.Clone(describes), nested.Name), nestedFile, out)
	}
}

// durationMs converts a duration in seconds to whole milliseconds: 0 for a
// value that is not positive or not a number, math.MaxInt64 for one past the
// int64 range.
func durationMs(seconds float64) int64 {
	millis := math.Round(seconds * 1000)
	if !(millis > 0) {
		return 0
	}
	if millis >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(millis)
}

// failureOutput joins a failure's message and body. A body that already
// carries the message, as bun's "Type: message" and stack do, stands alone.
func failureOutput(failure *junitFailure) string {
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

func collectFailed(suites []junitTestSuite, out *[]FailedTest) {
	for _, suite := range suites {
		for _, tc := range suite.TestCases {
			if tc.Failure != nil {
				ft := FailedTest{
					Name: tc.Name,
					File: tc.File,
					Line: tc.Line,
				}
				if ft.File == "" {
					ft.File = suite.File
				}
				msg := tc.Failure.Message
				if msg == "" {
					msg = tc.Failure.Body
				}
				ft.Failure = &TestFailure{
					Type:    tc.Failure.Type,
					Message: msg,
				}
				*out = append(*out, ft)
			}
		}
		collectFailed(suite.TestSuites, out)
	}
}
