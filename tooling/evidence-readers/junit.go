package readers

import (
	"bytes"
	"encoding/xml"
	"math"
	"slices"
	"strconv"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Test case statuses.
const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

// TestReport is one JUnit report.
type TestReport struct {
	// Totals are the counts the root element states. A tool that writes no
	// count on its root leaves them zero; the cases are the facts.
	Totals Totals `json:"totals"`
	// Cases is one entry per testcase element, in document order.
	Cases []TestCase `json:"cases"`
}

// Totals are the counts a JUnit root element states.
type Totals struct {
	Tests      int `json:"tests"`
	Failures   int `json:"failures"`
	Errors     int `json:"errors"`
	Skipped    int `json:"skipped"`
	Assertions int `json:"assertions"`
	// Time is the stated duration, in seconds.
	Time float64 `json:"time"`
}

// TestCase is one testcase element.
type TestCase struct {
	// Name is the case's name attribute.
	Name string `json:"name"`
	// Classname is the case's classname attribute.
	Classname string `json:"classname,omitempty"`
	// Suites are the testsuite elements that enclose the case, outermost
	// first. A testsuites root is not one of them; a testsuite root is.
	Suites []Suite `json:"suites,omitempty"`
	// File is the case's file attribute, else the one of its nearest
	// enclosing suite that states one, in slash form.
	File string `json:"file,omitempty"`
	// Line is the case's line attribute; 0 when it states none.
	Line int `json:"line,omitempty"`
	// Status is StatusPassed, StatusFailed or StatusSkipped.
	Status string `json:"status"`
	// DurationMs is the case's time attribute in whole milliseconds.
	DurationMs int64 `json:"durationMs"`
	// Failure is the failure or error the case carries, for a failed case.
	Failure *Failure `json:"failure,omitempty"`
	// Skipped is the skip the case carries, for a skipped case.
	Skipped *Skipped `json:"skipped,omitempty"`
}

// Suite is one testsuite element.
type Suite struct {
	Name string `json:"name,omitempty"`
	// File is the suite's file attribute, in slash form.
	File string `json:"file,omitempty"`
}

// Failure is a case's first failure element, else its first error element.
type Failure struct {
	// Error is true for an error element: the case did not finish.
	Error   bool   `json:"error,omitempty"`
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
	// Body is the element's text, as the report states it.
	Body string `json:"body,omitempty"`
}

// Skipped is a case's skipped element.
type Skipped struct {
	Message string `json:"message,omitempty"`
	Body    string `json:"body,omitempty"`
}

// junitNode is any element of a JUnit report. Children keeps the suites and
// cases of a suite, and the outcome of a case, in document order.
type junitNode struct {
	XMLName    xml.Name
	Name       string      `xml:"name,attr"`
	Classname  string      `xml:"classname,attr"`
	File       string      `xml:"file,attr"`
	Line       string      `xml:"line,attr"`
	Time       string      `xml:"time,attr"`
	Tests      string      `xml:"tests,attr"`
	Failures   string      `xml:"failures,attr"`
	Errors     string      `xml:"errors,attr"`
	Skipped    string      `xml:"skipped,attr"`
	Assertions string      `xml:"assertions,attr"`
	Type       string      `xml:"type,attr"`
	Message    string      `xml:"message,attr"`
	Body       string      `xml:",chardata"`
	Children   []junitNode `xml:",any"`
}

// ReadJUnit reads a JUnit XML report whose root is testsuites or testsuite.
func ReadJUnit(data []byte) (TestReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return TestReport{}, diagnostics
	}
	var root junitNode
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&root); err != nil {
		return TestReport{}, malformed("JUnit report does not decode: %v", err)
	}
	var suites []Suite
	switch root.XMLName.Local {
	case "testsuites":
	case "testsuite":
		suites = []Suite{{Name: root.Name, File: slashPath(root.File)}}
	default:
		return TestReport{}, malformed("JUnit report root is %q, want testsuites or testsuite", root.XMLName.Local)
	}
	totals, diagnostics := junitTotals(root)
	if diagnostics != nil {
		return TestReport{}, diagnostics
	}
	report := TestReport{Totals: totals, Cases: []TestCase{}}
	if diagnostics := collectJUnit(root, suites, &report.Cases); diagnostics != nil {
		return TestReport{}, diagnostics
	}
	return report, nil
}

func junitTotals(root junitNode) (Totals, []diag.Diagnostic) {
	var totals Totals
	for _, count := range []struct {
		name  string
		value string
		into  *int
	}{
		{"tests", root.Tests, &totals.Tests},
		{"failures", root.Failures, &totals.Failures},
		{"errors", root.Errors, &totals.Errors},
		{"skipped", root.Skipped, &totals.Skipped},
		{"assertions", root.Assertions, &totals.Assertions},
	} {
		value, diagnostics := count32(count.value, "JUnit root "+count.name)
		if diagnostics != nil {
			return Totals{}, diagnostics
		}
		*count.into = value
	}
	totals.Time = seconds(root.Time)
	return totals, nil
}

// collectJUnit appends the cases of element and of its nested suites, in
// document order, each under suites.
func collectJUnit(element junitNode, suites []Suite, out *[]TestCase) []diag.Diagnostic {
	for _, child := range element.Children {
		switch child.XMLName.Local {
		case "testsuite":
			nested := append(slices.Clone(suites), Suite{Name: child.Name, File: slashPath(child.File)})
			if diagnostics := collectJUnit(child, nested, out); diagnostics != nil {
				return diagnostics
			}
		case "testcase":
			testCase, diagnostics := junitCase(child, suites)
			if diagnostics != nil {
				return diagnostics
			}
			*out = append(*out, testCase)
		}
	}
	return nil
}

func junitCase(tc junitNode, suites []Suite) (TestCase, []diag.Diagnostic) {
	line, diagnostics := count32(tc.Line, "JUnit case "+strconv.Quote(tc.Name)+" line")
	if diagnostics != nil {
		return TestCase{}, diagnostics
	}
	testCase := TestCase{
		Name:       tc.Name,
		Classname:  tc.Classname,
		Suites:     slices.Clone(suites),
		File:       slashPath(tc.File),
		Line:       line,
		Status:     StatusPassed,
		DurationMs: durationMs(seconds(tc.Time)),
	}
	for i := len(suites) - 1; testCase.File == "" && i >= 0; i-- {
		testCase.File = suites[i].File
	}
	var failure, failingError, skipped *junitNode
	for i := range tc.Children {
		outcome := &tc.Children[i]
		switch {
		case outcome.XMLName.Local == "failure" && failure == nil:
			failure = outcome
		case outcome.XMLName.Local == "error" && failingError == nil:
			failingError = outcome
		case outcome.XMLName.Local == "skipped" && skipped == nil:
			skipped = outcome
		}
	}
	switch {
	case failure != nil:
		testCase.Status = StatusFailed
		testCase.Failure = &Failure{Type: failure.Type, Message: failure.Message, Body: failure.Body}
	case failingError != nil:
		testCase.Status = StatusFailed
		testCase.Failure = &Failure{Error: true, Type: failingError.Type, Message: failingError.Message, Body: failingError.Body}
	case skipped != nil:
		testCase.Status = StatusSkipped
		testCase.Skipped = &Skipped{Message: skipped.Message, Body: skipped.Body}
	}
	return testCase, nil
}

// count32 parses a count attribute: empty is 0, anything but a decimal from 0
// to math.MaxInt32 is malformed.
func count32(value, where string) (int, []diag.Diagnostic) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 0 {
		return 0, malformed("%s %q is not a count", where, value)
	}
	return int(parsed), nil
}

// seconds parses a time attribute; a value that is not a finite number is 0.
func seconds(value string) float64 {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0
	}
	return parsed
}

// durationMs converts a duration in seconds to whole milliseconds: 0 for a
// value that is not positive, math.MaxInt64 for one past the int64 range.
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
