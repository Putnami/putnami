package testjob

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/hostenv"
)

// The files every runner leaves in the task's output directory.
const (
	JUnitFile = "results.junit.xml"
	LCOVFile  = "lcov.info"
)

// RunSuite runs a project's suite with the runner command names, and leaves
// results.junit.xml, and lcov.info when params.Coverage holds, in outputPath.
// It returns whether the runner succeeded and its transcript.
func RunSuite(command Command, projectPath, outputPath string, params TestParams) (bool, string, error) {
	switch command.Runner {
	case RunnerVitest, RunnerJest:
		return runNodeSuite(command, projectPath, outputPath, params)
	default:
		return RunTests(command.Program, projectPath, outputPath, params)
	}
}

// runNodeSuite runs Vitest or Jest under node. The runner's coverage report and
// Jest's JSON result are written to a scratch directory, so the output keeps
// only the files the task declares.
func runNodeSuite(command Command, projectPath, outputPath string, params TestParams) (bool, string, error) {
	if err := os.MkdirAll(outputPath, 0o755); err != nil {
		return false, "", err
	}
	scratch, err := os.MkdirTemp("", "putnami-ts-test-")
	if err != nil {
		return false, "", err
	}
	defer os.RemoveAll(scratch)
	for _, stale := range []string{JUnitFile, LCOVFile} {
		if err := os.Remove(filepath.Join(outputPath, stale)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, "", err
		}
	}

	coverageDir := filepath.Join(scratch, "coverage")
	coverage := params.Coverage && command.CoverageUnavailable == ""
	var args []string
	jestResult := filepath.Join(scratch, "jest-result.json")
	if command.Runner == RunnerVitest {
		args = vitestArgs(filepath.Join(outputPath, JUnitFile), coverageDir, coverage, params)
	} else {
		args = jestArgs(jestResult, coverageDir, coverage, params)
	}

	result, err := execRunFunc(command.Program, append([]string{command.Script}, args...),
		exec.Dir(projectPath),
		exec.UnsetEnv(append(hostenv.PlatformIdentityVars(), hostenv.JobVars()...)...),
		exec.Env(buildTestEnv(params)),
		exec.Timeout(resolveTestTimeout(params)),
	)
	if err != nil {
		return false, "", err
	}
	logs := result.Stdout + result.Stderr

	// The suite ran: a report this task cannot read fails the run, and is never
	// an error that would start the suite again.
	success := result.Success
	if command.Runner == RunnerJest {
		if err := ConvertJestResult(jestResult, filepath.Join(outputPath, JUnitFile), projectPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			success, logs = false, logs+"\nputnami: "+err.Error()+"\n"
		}
	}
	if coverage {
		if err := RelocateLCOV(filepath.Join(coverageDir, LCOVFile), filepath.Join(outputPath, LCOVFile), projectPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			success, logs = false, logs+"\nputnami: coverage report: "+err.Error()+"\n"
		}
	}
	return success, logs, nil
}

// vitestArgs are the arguments of `vitest run`: the default console reporter,
// the junit reporter into junitPath and, with coverage, the lcovonly reporter
// into coverageDir. Everything else comes from the project's configuration.
func vitestArgs(junitPath, coverageDir string, coverage bool, params TestParams) []string {
	args := []string{"run", "--reporter=default", "--reporter=junit", "--outputFile.junit=" + junitPath}
	if coverage {
		args = append(args,
			"--coverage.enabled=true",
			"--coverage.reporter=lcovonly",
			"--coverage.reportsDirectory="+coverageDir,
		)
	}
	if params.Test != "" {
		args = append(args, "--testNamePattern="+params.Test)
	}
	if params.Bail > 0 {
		args = append(args, fmt.Sprintf("--bail=%d", params.Bail))
	}
	if params.UpdateSnapshots {
		args = append(args, "--update")
	}
	if params.PassWithNoTests {
		args = append(args, "--passWithNoTests")
	}
	return args
}

// jestArgs are the arguments of jest: its JSON result into resultPath and,
// with coverage, the lcovonly reporter into coverageDir. Everything else comes
// from the project's configuration.
func jestArgs(resultPath, coverageDir string, coverage bool, params TestParams) []string {
	args := []string{"--json", "--outputFile=" + resultPath}
	if !params.UpdateSnapshots {
		args = append(args, "--ci")
	}
	if coverage {
		args = append(args, "--coverage", "--coverageReporters=lcovonly", "--coverageDirectory="+coverageDir)
	}
	if params.Test != "" {
		args = append(args, "--testNamePattern="+params.Test)
	}
	if params.Bail > 0 {
		args = append(args, fmt.Sprintf("--bail=%d", params.Bail))
	}
	if params.UpdateSnapshots {
		args = append(args, "--updateSnapshot")
	}
	if params.PassWithNoTests {
		args = append(args, "--passWithNoTests")
	}
	return args
}

// jestResult is the part of Jest's --json result the conversion reads.
type jestResult struct {
	TestResults []struct {
		Name             string  `json:"name"`
		Status           string  `json:"status"`
		Message          string  `json:"message"`
		StartTime        float64 `json:"startTime"`
		EndTime          float64 `json:"endTime"`
		AssertionResults []struct {
			AncestorTitles  []string `json:"ancestorTitles"`
			Title           string   `json:"title"`
			Status          string   `json:"status"`
			Duration        *float64 `json:"duration"`
			FailureMessages []string `json:"failureMessages"`
			Location        *struct {
				Line int `json:"line"`
			} `json:"location"`
		} `json:"assertionResults"`
	} `json:"testResults"`
}

// junitDocument is the JUnit report the conversion writes: one suite per test
// file, carrying the file, and one case per Jest test.
type junitDocument struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	File     string      `xml:"file,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     string      `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	File      string        `xml:"file,attr"`
	Line      int           `xml:"line,attr,omitempty"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *struct{}     `xml:"skipped,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

// ConvertJestResult converts the Jest --json result at resultPath into a
// JUnit report at junitPath, with test files relative to projectPath. A test
// file Jest could not run is one failed case named after the file, so the
// report counts it. It returns an error wrapping os.ErrNotExist when Jest
// wrote no result.
func ConvertJestResult(resultPath, junitPath, projectPath string) error {
	data, err := os.ReadFile(resultPath) //nolint:gosec // a scratch file this task created
	if err != nil {
		return err
	}
	var result jestResult
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("jest --json result: %w", err)
	}
	doc := junitDocument{Name: "jest"}
	var total float64
	for _, file := range result.TestResults {
		rel := projectRelative(file.Name, projectPath)
		seconds := 0.0
		if file.EndTime > file.StartTime {
			seconds = (file.EndTime - file.StartTime) / 1000
		}
		total += seconds
		suite := junitSuite{Name: rel, File: rel, Time: formatSeconds(seconds)}
		for _, assertion := range file.AssertionResults {
			testCase := junitCase{
				Name:      strings.Join(append(append([]string{}, assertion.AncestorTitles...), assertion.Title), " > "),
				Classname: rel,
				File:      rel,
				Time:      "0",
			}
			if assertion.Duration != nil {
				testCase.Time = formatSeconds(*assertion.Duration / 1000)
			}
			if assertion.Location != nil {
				testCase.Line = assertion.Location.Line
			}
			switch assertion.Status {
			case "passed":
			case "failed":
				body := stripANSI(strings.Join(assertion.FailureMessages, "\n"))
				testCase.Failure = &junitFailure{Message: firstLine(body), Type: "AssertionError", Body: body}
				suite.Failures++
			default:
				// pending, todo, skipped, disabled and focused-out tests.
				testCase.Skipped = &struct{}{}
				suite.Skipped++
			}
			suite.Cases = append(suite.Cases, testCase)
		}
		if len(file.AssertionResults) == 0 && file.Status == "failed" {
			body := stripANSI(file.Message)
			suite.Cases = append(suite.Cases, junitCase{
				Name: "Test suite failed to run", Classname: rel, File: rel, Time: formatSeconds(seconds),
				Failure: &junitFailure{Message: firstLine(body), Type: "SuiteError", Body: body},
			})
			suite.Failures++
		}
		suite.Tests = len(suite.Cases)
		doc.Tests += suite.Tests
		doc.Failures += suite.Failures
		doc.Skipped += suite.Skipped
		doc.Suites = append(doc.Suites, suite)
	}
	sort.SliceStable(doc.Suites, func(i, j int) bool { return doc.Suites[i].Name < doc.Suites[j].Name })
	doc.Time = formatSeconds(total)
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(junitPath, append([]byte(xml.Header), append(out, '\n')...), 0o644) //nolint:gosec // a report in the task output
}

// RelocateLCOV copies the LCOV report at from to to, with each source file
// that lies under projectPath rewritten relative to it, the form Bun writes.
func RelocateLCOV(from, to, projectPath string) error {
	in, err := os.Open(from) //nolint:gosec // a scratch file this task created
	if err != nil {
		return err
	}
	defer in.Close()
	var out strings.Builder
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if file, ok := strings.CutPrefix(line, "SF:"); ok {
			line = "SF:" + projectRelative(file, projectPath)
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return os.WriteFile(to, []byte(out.String()), 0o644) //nolint:gosec // a report in the task output
}

// projectRelative returns file relative to projectPath, in slash form, when it
// is an absolute path under it; otherwise file in slash form.
func projectRelative(file, projectPath string) string {
	if filepath.IsAbs(file) {
		if rel, err := filepath.Rel(projectPath, file); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(file)
}

func formatSeconds(seconds float64) string {
	return fmt.Sprintf("%.3f", seconds)
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func stripANSI(text string) string {
	return ansiRe.ReplaceAllString(text, "")
}
