package readers

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// xcNode is one node of the test tree xcresulttool prints.
type xcNode struct {
	Name              string   `json:"name"`
	NodeType          string   `json:"nodeType"`
	Result            string   `json:"result"`
	Duration          string   `json:"duration"`
	DurationInSeconds *float64 `json:"durationInSeconds"`
	Children          []xcNode `json:"children"`
}

// xcResultTests is the part of the test-results tests output the reader uses.
type xcResultTests struct {
	TestNodes *[]xcNode `json:"testNodes"`
}

// The node types of the test tree that carry a case, enclose it, or explain
// its result. Any other node, such as a Device, a Test Plan Configuration, an
// Arguments or a Repetition node, is read through.
const (
	xcTestPlan       = "Test Plan"
	xcUnitTestBundle = "Unit test bundle"
	xcUITestBundle   = "UI test bundle"
	xcTestSuite      = "Test Suite"
	xcTestCase       = "Test Case"
	xcFailureMessage = "Failure Message"
)

// ReadXCResultTests reads the JSON that xcrun xcresulttool get test-results
// tests prints for an .xcresult bundle, Xcode 16 and later: an object whose
// testNodes array holds the test tree. Each Test Case node is one case, in
// document order, its suites the Test Plan, test bundle and Test Suite nodes
// that enclose it, outermost first, and its classname the test bundle's name
// joined with a dot to the outermost enclosing suite's, as Module.Class, the
// way xcbeautify names an XCTest case. The output states no totals and no
// test file.
//
// Passed and Expected Failure pass, Failed fails and any other result skips;
// the first Failure Message node below the case, at any depth (under a
// Repetition or a device), is the failure or skip message, and all of them,
// one per line, the failure's body when there are several. The duration is
// durationInSeconds, else the duration string, such as 0.25s or 0,25s.
func ReadXCResultTests(data []byte) (TestReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return TestReport{}, diagnostics
	}
	var tests xcResultTests
	if err := json.Unmarshal(data, &tests); err != nil {
		return TestReport{}, malformed("xcresulttool test results do not decode: %v", err)
	}
	if tests.TestNodes == nil {
		return TestReport{}, malformed("xcresulttool test results have no testNodes array")
	}
	reader := xcTestReader{report: TestReport{Cases: []TestCase{}}}
	for _, node := range *tests.TestNodes {
		if diagnostics := reader.walk(node, nil, "", "", 1); diagnostics != nil {
			return TestReport{}, diagnostics
		}
	}
	return reader.report, nil
}

type xcTestReader struct {
	report TestReport
}

// walk reads node, enclosed by suites, under the test bundle bundle and the
// outermost Test Suite outer, at depth from 1.
func (r *xcTestReader) walk(node xcNode, suites []Suite, bundle, outer string, depth int) []diag.Diagnostic {
	if depth > MaxSuiteDepth {
		return malformed("xcresulttool test nodes nest deeper than %d", MaxSuiteDepth)
	}
	switch node.NodeType {
	case xcTestCase:
		return r.testCase(node, suites, bundle, outer, depth)
	case xcTestPlan, xcUnitTestBundle, xcUITestBundle, xcTestSuite:
		// The cases of one suite share the slice.
		suites = append(slices.Clip(suites), Suite{Name: node.Name})
		switch {
		case node.NodeType == xcUnitTestBundle || node.NodeType == xcUITestBundle:
			bundle, outer = node.Name, ""
		case node.NodeType == xcTestSuite && outer == "":
			outer = node.Name
		}
	}
	for _, child := range node.Children {
		if diagnostics := r.walk(child, suites, bundle, outer, depth+1); diagnostics != nil {
			return diagnostics
		}
	}
	return nil
}

func (r *xcTestReader) testCase(node xcNode, suites []Suite, bundle, outer string, depth int) []diag.Diagnostic {
	duration, diagnostics := xcDuration(node)
	if diagnostics != nil {
		return diagnostics
	}
	classname := bundle
	if outer != "" {
		classname = strings.TrimPrefix(bundle+"."+outer, ".")
	}
	testCase := TestCase{Name: node.Name, Classname: classname, Suites: suites, DurationMs: duration}
	var messages []string
	if diagnostics := xcFailureMessages(node.Children, depth+1, &messages); diagnostics != nil {
		return diagnostics
	}
	first := ""
	if len(messages) > 0 {
		first = messages[0]
	}
	switch node.Result {
	case "Passed", "Expected Failure":
		testCase.Status = StatusPassed
	case "Failed":
		testCase.Status = StatusFailed
		testCase.Failure = &Failure{Message: first}
		if len(messages) > 1 {
			testCase.Failure.Body = strings.Join(messages, "\n")
		}
	default:
		testCase.Status = StatusSkipped
		testCase.Skipped = &Skipped{Message: first}
	}
	r.report.Cases = append(r.report.Cases, testCase)
	return nil
}

// xcFailureMessages collects the Failure Message nodes among nodes and their
// descendants, in document order.
func xcFailureMessages(nodes []xcNode, depth int, messages *[]string) []diag.Diagnostic {
	if len(nodes) > 0 && depth > MaxSuiteDepth {
		return malformed("xcresulttool test nodes nest deeper than %d", MaxSuiteDepth)
	}
	for _, node := range nodes {
		if node.NodeType == xcFailureMessage {
			*messages = append(*messages, node.Name)
		}
		if diagnostics := xcFailureMessages(node.Children, depth+1, messages); diagnostics != nil {
			return diagnostics
		}
	}
	return nil
}

// xcDuration reads a case's duration in milliseconds: durationInSeconds,
// else the duration string, a number of seconds with a dot or a comma as its
// decimal separator and an s suffix. A string that does not read so, or reads
// as an infinite or not-a-number value, is 0; a negative durationInSeconds is
// malformed.
func xcDuration(node xcNode) (int64, []diag.Diagnostic) {
	if node.DurationInSeconds != nil {
		if math.IsNaN(*node.DurationInSeconds) || *node.DurationInSeconds < 0 {
			return 0, malformed("xcresulttool case %q has a negative duration", node.Name)
		}
		return durationMs(*node.DurationInSeconds), nil
	}
	value := strings.TrimSuffix(strings.TrimSpace(node.Duration), "s")
	parsed, err := strconv.ParseFloat(strings.Replace(value, ",", ".", 1), 64)
	if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return 0, nil
	}
	return durationMs(parsed), nil
}

// xccovReport is the part of the xccov view --report --json output the reader
// uses.
type xccovReport struct {
	Targets *[]struct {
		Files []struct {
			Path            string `json:"path"`
			CoveredLines    int    `json:"coveredLines"`
			ExecutableLines int    `json:"executableLines"`
			Functions       []struct {
				Name           string `json:"name"`
				LineNumber     int    `json:"lineNumber"`
				CoveredLines   int    `json:"coveredLines"`
				ExecutionCount int64  `json:"executionCount"`
			} `json:"functions"`
		} `json:"files"`
	} `json:"targets"`
}

// ReadXccovReport reads the JSON that xcrun xccov view --report --json prints
// for an .xcresult bundle: an object whose targets array lists each target's
// files. Each path is one file, in the order first named, with no line: its
// line totals are the executableLines and coveredLines the report states, and
// its functions those it lists, one covered when it ran or covers a line. A
// file two targets list keeps the largest totals. The report states no line:
// ReadXccovArchive reads them.
func ReadXccovReport(data []byte) (CoverageReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return CoverageReport{}, diagnostics
	}
	var report xccovReport
	if err := json.Unmarshal(data, &report); err != nil {
		return CoverageReport{}, malformed("xccov report does not decode: %v", err)
	}
	if report.Targets == nil {
		return CoverageReport{}, malformed("xccov report has no targets array")
	}
	builder := newCoverageBuilder()
	for _, target := range *report.Targets {
		for _, file := range target.Files {
			if file.Path == "" {
				return CoverageReport{}, malformed("xccov report names a file without a path")
			}
			if file.ExecutableLines < 0 || file.CoveredLines < 0 {
				return CoverageReport{}, malformed("xccov file %s states a negative line count", file.Path)
			}
			entry := builder.file(file.Path)
			entry.stateLines(Counts{Total: file.ExecutableLines, Covered: file.CoveredLines})
			for _, function := range file.Functions {
				if function.CoveredLines < 0 || function.ExecutionCount < 0 {
					return CoverageReport{}, malformed("xccov function %s of %s states a negative count", function.Name, file.Path)
				}
				entry.function(function.Name+":"+strconv.Itoa(function.LineNumber), function.ExecutionCount > 0 || function.CoveredLines > 0)
			}
		}
	}
	return builder.report(nil), nil
}

// xccovLine is one line of a file of the xccov view --archive --json output.
type xccovLine struct {
	Line           int   `json:"line"`
	IsExecutable   bool  `json:"isExecutable"`
	ExecutionCount int64 `json:"executionCount"`
}

// ReadXccovArchive reads the JSON that xcrun xccov view --archive --json prints
// for an .xcresult bundle: an object that maps each file path to its lines.
// Each path is one file, in path order; its instrumented lines are those with
// isExecutable true, each with its executionCount, and a line that is not
// executable is no line.
func ReadXccovArchive(data []byte) (CoverageReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return CoverageReport{}, diagnostics
	}
	trimmed := bytes.TrimSpace(data)
	if !bytes.HasPrefix(trimmed, []byte("{")) {
		return CoverageReport{}, malformed("xccov archive is not a JSON object")
	}
	var archive map[string][]xccovLine
	if err := json.Unmarshal(trimmed, &archive); err != nil {
		return CoverageReport{}, malformed("xccov archive does not decode: %v", err)
	}
	paths := make([]string, 0, len(archive))
	for path := range archive {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	builder := newCoverageBuilder()
	for _, path := range paths {
		if path == "" {
			return CoverageReport{}, malformed("xccov archive names a file without a path")
		}
		entry := builder.file(path)
		for _, line := range archive[path] {
			if diagnostics := checkLine("xccov archive "+path, line.Line, line.ExecutionCount); diagnostics != nil {
				return CoverageReport{}, diagnostics
			}
			if !line.IsExecutable {
				continue
			}
			if diagnostics := entry.line(line.Line, line.ExecutionCount); diagnostics != nil {
				return CoverageReport{}, diagnostics
			}
		}
	}
	return builder.report(nil), nil
}
