package parse

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
)

// TestFileLocator resolves the base name of a test file that one package's
// test output names to its workspace-relative path. It returns "" when it
// cannot resolve the file.
type TestFileLocator func(pkg, file string) string

// ModuleTestFileLocator resolves the test files of the packages that the
// module at moduleRoot owns. A package of the module lives in moduleRoot
// joined with its import path below modulePath, so the locator resolves only
// a package whose import path is modulePath or below it, only a bare file
// name, and only a regular file that exists there inside workspaceRoot. An
// empty modulePath resolves nothing.
func ModuleTestFileLocator(workspaceRoot, moduleRoot, modulePath string) TestFileLocator {
	return func(pkg, file string) string {
		if modulePath == "" || file == "" || strings.ContainsAny(file, `/\`) {
			return ""
		}
		var below string
		switch {
		case pkg == modulePath:
		case strings.HasPrefix(pkg, modulePath+"/"):
			below = pkg[len(modulePath)+1:]
		default:
			return ""
		}
		abs := filepath.Join(moduleRoot, filepath.FromSlash(below), file)
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() {
			return ""
		}
		rel, err := filepath.Rel(workspaceRoot, abs)
		if err != nil || filepath.IsAbs(rel) || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		return filepath.ToSlash(rel)
	}
}

// testCaseScaffoldRe matches the lines `go test -v` prints around each test:
// the RUN, PAUSE, CONT and NAME headers and the PASS, FAIL and SKIP result
// lines. A test case's status and duration carry what they say.
var testCaseScaffoldRe = regexp.MustCompile(`^(?:=== (?:RUN|PAUSE|CONT|NAME)\b|[ \t]*--- (?:PASS|FAIL|SKIP): )`)

// testCaseLocationRe matches the location the testing package prefixes to a
// t.Log, t.Error or t.Skip line: an indented bare `name_test.go:N:`.
var testCaseLocationRe = regexp.MustCompile(`^[ \t]+([^\s/\\:]+_test\.go):([0-9]+):`)

// TestCaseList parses `go test -json` output into one test case per test run
// that reached a pass, fail or skip event, in the order the runs started. A
// subtest is a case of its own, named as the runner names it ("TestX/sub"). A
// package-level event, a line that is not a JSON event, and a run that never
// reached a result are not cases.
//
// The suite is the package import path and the duration is the event's
// Elapsed. A failed or skipped case carries the lines the test printed without
// the runner's RUN and result lines, and, when locate resolves it, the first
// test-file location in them. A passed case carries neither. A nil locate
// leaves every case without a location.
func TestCaseList(output string, locate TestFileLocator) []protocolcli.TestCase {
	type testRun struct {
		pkg     string
		test    string
		output  strings.Builder
		action  string
		elapsed float64
	}
	var runs []*testRun
	open := make(map[testEventIdentity]*testRun)
	scanner := LineScanner(strings.NewReader(output))
	for scanner.Scan() {
		var evt TestEvent
		if json.Unmarshal(scanner.Bytes(), &evt) != nil || evt.Test == "" {
			continue
		}
		key := testEventIdentity{Package: evt.Package, Test: evt.Test}
		run := open[key]
		if run == nil {
			run = &testRun{pkg: evt.Package, test: evt.Test}
			open[key] = run
			runs = append(runs, run)
		}
		switch evt.Action {
		case "output":
			run.output.WriteString(evt.Output)
		case "pass", "fail", "skip":
			run.action, run.elapsed = evt.Action, evt.Elapsed
			if evt.Action == "pass" {
				run.output.Reset()
			}
			delete(open, key)
		}
	}

	cases := make([]protocolcli.TestCase, 0, len(runs))
	for _, run := range runs {
		var status string
		switch run.action {
		case "pass":
			status = protocolcli.TestCaseStatusPassed
		case "fail":
			status = protocolcli.TestCaseStatusFailed
		case "skip":
			status = protocolcli.TestCaseStatusSkipped
		default:
			continue
		}
		testCase := protocolcli.TestCase{
			Name:       run.test,
			Suite:      run.pkg,
			Status:     status,
			DurationMs: durationMs(run.elapsed),
		}
		if status != protocolcli.TestCaseStatusPassed {
			lines := testCaseLines(run.output.String())
			testCase.Output = strings.Join(dedentLines(lines), "\n")
			if locate != nil {
				testCase.File, testCase.Line = testCaseLocation(run.pkg, lines, locate)
			}
		}
		cases = append(cases, testCase)
	}
	return cases
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

// testCaseLines splits one run's output into the lines the test printed,
// without the runner's scaffolding lines and without the blank lines around
// them.
func testCaseLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if testCaseScaffoldRe.MatchString(line) {
			continue
		}
		lines = append(lines, line)
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// dedentLines removes the leading whitespace that every non-blank line
// shares: the indentation the testing package adds for the test's depth.
func dedentLines(lines []string) []string {
	common := ""
	first := true
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if first {
			common, first = indent, false
			continue
		}
		n := 0
		for n < len(common) && n < len(indent) && common[n] == indent[n] {
			n++
		}
		common = common[:n]
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out[i] = line[len(common):]
	}
	return out
}

// testCaseLocation returns the first test-file location in a case's lines
// when locate resolves it, and no location otherwise: a later location is
// never used in its place.
func testCaseLocation(pkg string, lines []string, locate TestFileLocator) (string, int) {
	for _, line := range lines {
		m := testCaseLocationRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lineNum, err := strconv.Atoi(m[2])
		if err != nil || lineNum < 1 {
			continue
		}
		if file := locate(pkg, m[1]); file != "" {
			return file, lineNum
		}
		return "", 0
	}
	return "", 0
}
