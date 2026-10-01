package jobs

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	protocolruntime "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/scratch"
)

// Per-test-case results for Python: pytest has no structured per-case output
// the adapter can read from outside the process, so the adapter ships a pytest
// plugin (embedded below). Each pytest run gets its own scratch directory that
// holds the plugin module and the file it writes, one JSON line per test item.
// After pytest exits, the adapter turns the lines into protocol/cli TestCase
// values and puts them into the task's result data under "testCases".

//go:embed putnami_testcases.py
var testCasePluginSource []byte

const (
	// testCasePluginModule is the module name the plugin is imported under; it
	// must match the embedded file's basename.
	testCasePluginModule = "putnami_testcases"
	// testCasesFileEnv names the file the plugin writes. The plugin records
	// nothing when it is unset.
	testCasesFileEnv = "PUTNAMI_TEST_CASES_FILE"
	// testCasesFileName is the plugin's output file inside the scratch
	// directory.
	testCasesFileName = "cases.jsonl"
	// testCaseCapturedSeparator joins a failure text and the output the case
	// printed.
	testCaseCapturedSeparator = "\n\n"
)

// testCaseSink is one pytest run's per-case scratch state. A nil sink reports
// nothing: every method is safe on it, so a scratch failure costs the per-case
// report and never the tests.
type testCaseSink struct {
	dir  *scratch.Dir
	file string
	// stale reports that the file could not be emptied before an attempt, so
	// it may hold an earlier attempt's records.
	stale bool
}

// provisionTestCases creates a scratch directory holding the plugin module and
// an empty cases file.
func provisionTestCases() (*testCaseSink, error) {
	directory, err := scratch.New("putnami-test-cases-")
	if err != nil {
		return nil, fmt.Errorf("create test case directory: %w", err)
	}
	sink := &testCaseSink{dir: directory, file: filepath.Join(directory.Path(), testCasesFileName)}
	if err := os.WriteFile(filepath.Join(directory.Path(), testCasePluginModule+".py"), testCasePluginSource, 0o644); err != nil {
		sink.remove()
		return nil, fmt.Errorf("write test case plugin: %w", err)
	}
	if err := os.WriteFile(sink.file, nil, 0o600); err != nil {
		sink.remove()
		return nil, fmt.Errorf("create test case file: %w", err)
	}
	return sink, nil
}

// pytestArgs returns the pytest arguments that load the plugin.
func (s *testCaseSink) pytestArgs() []string {
	if s == nil {
		return nil
	}
	return []string{"-p", testCasePluginModule}
}

// decorateEnv names the cases file and puts the plugin module on PYTHONPATH,
// preserving whatever path the job already assembled.
func (s *testCaseSink) decorateEnv(extra map[string]string) {
	if s == nil {
		return
	}
	extra[testCasesFileEnv] = s.file
	if existing := extra["PYTHONPATH"]; existing != "" {
		extra["PYTHONPATH"] = s.dir.Path() + string(os.PathListSeparator) + existing
	} else {
		extra["PYTHONPATH"] = s.dir.Path()
	}
}

// reset empties the cases file before a pytest attempt, so a retried run
// reports only its own cases.
func (s *testCaseSink) reset() {
	if s == nil {
		return
	}
	s.stale = os.WriteFile(s.file, nil, 0o600) != nil
}

// read returns the cases the last attempt recorded, bounded to maxBytes of
// encoded cases, and the count of cases the bound left out.
func (s *testCaseSink) read(wsRoot string, maxBytes int) ([]protocolcli.TestCase, int) {
	if s == nil || s.stale {
		return nil, 0
	}
	return readTestCases(s.file, wsRoot, maxBytes)
}

// remove deletes the scratch directory.
func (s *testCaseSink) remove() {
	if s == nil {
		return
	}
	_ = s.dir.Remove()
}

// testCaseRecord is one line the plugin writes. Module, the test module that
// collected the case, and File, where the case is declared, are absolute;
// Duration is in seconds.
type testCaseRecord struct {
	Name     string  `json:"name"`
	Module   string  `json:"module"`
	Status   string  `json:"status"`
	Duration float64 `json:"duration"`
	Output   string  `json:"output"`
	Captured string  `json:"captured"`
	File     string  `json:"file"`
	Line     int     `json:"line"`
}

// readTestCases reads the plugin's file and applies the test-case contract. A
// missing file yields no case. A last line without its newline is a record the
// process did not finish writing, and a line that does not decode is not a
// record: both are ignored, so a crashed run still reports every case it
// finished.
func readTestCases(path, wsRoot string, maxBytes int) ([]protocolcli.TestCase, int) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer file.Close()

	paths := newWorkspacePaths(wsRoot)
	reader := bufio.NewReader(file)
	var cases []protocolcli.TestCase
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break
		}
		var record testCaseRecord
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		cases = append(cases, record.testCase(paths))
	}
	return protocolcli.BoundTestCasesWithin(cases, maxBytes)
}

// testCase converts one record. Suite is the workspace-relative test module
// that collected the case, and File the one that declares it: they differ for
// a test a class inherits from another module. A record without a module takes
// its suite from File. A path outside the workspace is left empty, and a case
// without a suite is then dropped by the bound instead of reporting a machine
// path. A failed
// case's output is its failure text, followed by what it printed when both fit
// in the output bound, so the printed output never displaces the failure.
func (r testCaseRecord) testCase(paths *workspacePaths) protocolcli.TestCase {
	relative := paths.relative(r.File)
	suite := relative
	if r.Module != "" {
		suite = paths.relative(r.Module)
	}
	c := protocolcli.TestCase{
		Name:       r.Name,
		Suite:      suite,
		Status:     r.Status,
		DurationMs: durationMillis(r.Duration),
		File:       relative,
	}
	if relative != "" && r.Line > 0 {
		c.Line = r.Line
	}
	if r.Status == protocolcli.TestCaseStatusPassed {
		return c
	}
	output := r.Output
	if r.Status == protocolcli.TestCaseStatusFailed && r.Captured != "" {
		switch {
		case output == "":
			output = r.Captured
		case len(output)+len(testCaseCapturedSeparator)+len(r.Captured) <= protocolcli.TestCaseMaxOutputBytes:
			output += testCaseCapturedSeparator + r.Captured
		}
	}
	c.Output, c.OutputTruncated = protocolcli.TruncateTestCaseOutput(output)
	return c
}

// durationMillis converts pytest's duration in seconds to whole milliseconds.
func durationMillis(seconds float64) int64 {
	millis := math.Round(seconds * 1000)
	if !(millis > 0) {
		return 0
	}
	if millis >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(millis)
}

// workspacePaths turns the absolute paths pytest reports into
// workspace-relative slash paths. pytest reports the paths it was given or
// the resolved working directory, so a path that is not under the workspace
// root as written is compared again with symbolic links resolved.
type workspacePaths struct {
	root     string
	realRoot string
	resolved bool
	memo     map[string]string
}

func newWorkspacePaths(root string) *workspacePaths {
	return &workspacePaths{root: root, memo: map[string]string{}}
}

// relative returns path relative to the workspace root in slash form, or ""
// when path is not absolute or not inside the workspace.
func (w *workspacePaths) relative(path string) string {
	if path == "" || w.root == "" || !filepath.IsAbs(path) {
		return ""
	}
	if relative, ok := w.memo[path]; ok {
		return relative
	}
	relative, ok := relativeInside(w.root, path)
	if !ok {
		if !w.resolved {
			w.resolved = true
			w.realRoot, _ = filepath.EvalSymlinks(w.root)
		}
		if realPath, err := filepath.EvalSymlinks(path); err == nil && w.realRoot != "" {
			relative, _ = relativeInside(w.realRoot, realPath)
		}
	}
	w.memo[path] = relative
	return relative
}

func relativeInside(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

// attachTestCases adds the per-case results to a test result payload, beside
// its testSummary. It adds the dropped count only when the bound left cases
// out, and leaves data untouched when there is nothing to report.
func attachTestCases(data map[string]any, cases []protocolcli.TestCase, dropped int) map[string]any {
	if len(cases) == 0 && dropped == 0 {
		return data
	}
	if data == nil {
		data = map[string]any{}
	}
	if len(cases) > 0 {
		data[protocolruntime.TestCasesResultDataKey] = cases
	}
	if dropped > 0 {
		data[protocolruntime.TestCasesDroppedResultDataKey] = dropped
	}
	return data
}
