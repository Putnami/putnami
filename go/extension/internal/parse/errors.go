// Package parse extracts structured diagnostics from Go tool output.
//
// It handles compiler errors, lint errors, test JSON output, and coverage
// profiles — converting raw text into Putnami JSONL diagnostic events.
package parse

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/jsonl"
)

// diagPath builds a workspace-relative file path for diagnostics.
func diagPath(workspaceRoot, projectPath, file string) string {
	var abs string
	if filepath.IsAbs(file) {
		abs = file
	} else {
		abs = filepath.Join(projectPath, file)
	}
	if rel, err := filepath.Rel(workspaceRoot, abs); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(filepath.Clean(abs))
}

// DiagPath renders a tool-reported file path (absolute, or relative to runDir)
// as a workspace-relative diagnostic path. Batch lint jobs run one tool process
// from a shared root, so they resolve each finding's path once it has been
// attributed to the owning project.
func DiagPath(workspaceRoot, runDir, file string) string {
	return diagPath(workspaceRoot, runDir, file)
}

var (
	// file.go:42:10: error message
	buildErrorRe = regexp.MustCompile(`^(.+\.go):(\d+):(\d+):\s*(.+)$`)
	// file.go:42:10: message (linter) OR file.go:42: message
	lintErrorRe = regexp.MustCompile(`^(.+\.go):(\d+):(?:(\d+):)?\s*(.+)`)
	// file_test.go:42: error message (indented in go test -json output)
	testDiagRe = regexp.MustCompile(`^\s+(.+_test\.go):(\d+):\s*(.+)`)
)

// GoBuildErrors parses Go compiler output and emits diagnostics.
func GoBuildErrors(output, workspaceRoot, projectPath string, emit *jsonl.Emitter) {
	emitted := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if m := buildErrorRe.FindStringSubmatch(line); m != nil {
			lineNum, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			emit.DiagnosticWithCode("error", m[4], diagPath(workspaceRoot, projectPath, m[1]), lineNum, col, "")
			emitted++
		}
	}
	if emitted == 0 && output != "" {
		emit.Diagnostic("error", output, "", 0)
	}
}

// LintResult holds the number of errors and unique files from a lint run.
type LintResult struct {
	Errors int
	Files  int
}

// LintErrors parses golangci-lint / staticcheck output and emits diagnostics.
// Returns the number of errors and unique files found.
func LintErrors(output, workspaceRoot, projectPath string, emit *jsonl.Emitter) LintResult {
	count := 0
	files := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if m := lintErrorRe.FindStringSubmatch(line); m != nil {
			lineNum, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3]) // empty when the linter omits a column
			path := diagPath(workspaceRoot, projectPath, m[1])
			emit.DiagnosticWithCode("error", m[4], path, lineNum, col, "")
			files[path] = struct{}{}
			count++
		}
	}
	if count > 0 {
		emit.Metric("lint-errors", count, "count")
	} else if output != "" {
		emit.Diagnostic("error", output, "", 0)
		emit.Metric("lint-errors", 1, "count")
		count = 1
	}
	return LintResult{Errors: count, Files: len(files)}
}

// LintFinding is a single golangci-lint / staticcheck finding located at a file
// position. File is exactly as the tool reported it (absolute when the tool ran
// with an absolute path mode, otherwise relative to its run directory), so a
// batch job can attribute the finding to the project that owns the file before
// rendering a workspace-relative path.
type LintFinding struct {
	File    string
	Line    int
	Column  int
	Message string
}

// LintFindings parses golangci-lint / staticcheck text output into structured
// findings plus any leftover non-finding lines (tool logs, run summaries, or
// crash output). It performs no attribution or path rewriting, so batch lint
// jobs can route each finding to the owning project and surface leftover text as
// a group-level failure. Solo jobs keep using LintErrors, whose emit-and-count
// behavior is unchanged.
func LintFindings(output string) (findings []LintFinding, leftover []string) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if m := lintErrorRe.FindStringSubmatch(line); m != nil {
			lineNum, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3]) // empty when the linter omits a column
			findings = append(findings, LintFinding{
				File:    m[1],
				Line:    lineNum,
				Column:  col,
				Message: m[4],
			})
			continue
		}
		if strings.TrimSpace(line) != "" {
			leftover = append(leftover, line)
		}
	}
	return findings, leftover
}

// TestEvent represents a single Go test JSON event.
type TestEvent struct {
	Action  string `json:"Action"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
	Package string `json:"Package"`
	// Elapsed is the run time in seconds that a pass, fail or skip event
	// reports.
	Elapsed float64 `json:"Elapsed,omitempty"`
}

// TestCounts holds aggregated test result counts.
type TestCounts struct {
	Passed  int
	Failed  int
	Skipped int
	// PackagesFailed counts package-level FAIL events (Test is empty). These
	// stay out of the test counts, but they are the only record of a failure
	// that no individual test reported — a data race detected after the last
	// test finished, a panic once the tests are done, a TestMain exit code, or
	// a build/vet error. Without them a failed run renders as "N/N passed".
	PackagesFailed int
}

// Total returns the sum of passed, failed, and skipped test counts.
func (c TestCounts) Total() int { return c.Passed + c.Failed + c.Skipped }

// TestJSON parses `go test -json` output and returns counts.
// Only test-level events (where Test is non-empty) feed Passed/Failed/Skipped,
// so package-level pass/fail/skip events cannot inflate the counts. A
// package-level fail is still recorded separately in PackagesFailed.
func TestJSON(output string) TestCounts {
	var counts TestCounts
	scanner := LineScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var evt TestEvent
		if json.Unmarshal([]byte(line), &evt) != nil {
			continue
		}
		if evt.Test == "" {
			if evt.Action == "fail" {
				counts.PackagesFailed++
			}
			continue
		}
		switch evt.Action {
		case "pass":
			counts.Passed++
		case "fail":
			counts.Failed++
		case "skip":
			counts.Skipped++
		}
	}
	return counts
}

// TestFailureDiagnostics emits the diagnostics for a failed `go test` run. It
// always emits at least one diagnostic, so a failing step can never render as
// "(no details emitted)". Call it only for runs that actually failed.
func TestFailureDiagnostics(output string, locate TestFileLocator, emit *jsonl.Emitter) {
	for _, diagnostic := range TestFailureDiagnosticList(output, locate) {
		emit.DiagnosticWithCode(
			diagnostic.Severity,
			diagnostic.Description,
			diagnostic.File,
			diagnostic.Line,
			diagnostic.Column,
			diagnostic.Category,
		)
	}
}

// ToolDiagnostic is a structured diagnostic independent of an emitter. Batch
// jobs use it to preserve per-project attribution before the scheduler renders
// each split result.
type ToolDiagnostic struct {
	Category    string `json:"category,omitempty"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
}

type testEventIdentity struct {
	Package string
	Test    string
}

// TestDiagnosticList parses `go test -json` output for failure locations. Go's
// JSON stream uses the same `file_test.go:line` shape for t.Log, t.Skip, and
// assertion output, so a location is only an error when its package/test
// identity later reaches a fail event.
//
// The testing package names a test file by its base name, which is relative
// to the directory of the package that failed, not to the project. locate
// resolves it from that package; a location it cannot resolve keeps its
// message and carries no file or line, so a diagnostic never points at a path
// that does not exist. A nil locate resolves nothing.
func TestDiagnosticList(output string, locate TestFileLocator) []ToolDiagnostic {
	failed := make(map[testEventIdentity]struct{})
	failCount := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var evt TestEvent
		if json.Unmarshal([]byte(line), &evt) != nil {
			continue
		}
		if evt.Action != "fail" {
			continue
		}
		identity := testEventIdentity{Package: evt.Package, Test: evt.Test}
		if _, exists := failed[identity]; !exists && evt.Test != "" {
			failCount++
		}
		failed[identity] = struct{}{}
	}

	var diagnostics []ToolDiagnostic
	scanner = bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var evt TestEvent
		if json.Unmarshal([]byte(line), &evt) != nil {
			continue
		}
		if evt.Action != "output" {
			continue
		}
		if _, causal := failed[testEventIdentity{Package: evt.Package, Test: evt.Test}]; !causal {
			continue
		}
		if m := testDiagRe.FindStringSubmatch(evt.Output); m != nil {
			diagnostic := ToolDiagnostic{Severity: "error", Description: m[3]}
			if locate != nil {
				if file := locate(evt.Package, m[1]); file != "" {
					diagnostic.File = file
					diagnostic.Line, _ = strconv.Atoi(m[2])
				}
			}
			diagnostics = append(diagnostics, diagnostic)
		}
	}

	if len(diagnostics) == 0 && failCount > 0 {
		diagnostics = append(diagnostics, ToolDiagnostic{
			Severity:    "error",
			Description: fmt.Sprintf("%d test(s) failed", failCount),
		})
	}
	return diagnostics
}

// hasUnexplainedTestFailure reports whether the stream carries failure evidence
// that a located test diagnostic does not explain. A package-level fail normally
// wraps its failed tests, but when it stands alone it carries a different cause
// — a post-test panic or race, TestMain exit, or build/vet failure. Non-JSON or
// oversized records likewise belong to the raw tool output, not to the located
// diagnostic parser.
func hasUnexplainedTestFailure(output string) bool {
	failedTests := make(map[string]bool)
	failedPackages := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		var evt TestEvent
		line := scanner.Text()
		if json.Unmarshal([]byte(line), &evt) != nil {
			if strings.TrimSpace(line) != "" {
				return true
			}
			continue
		}
		if evt.Action != "fail" {
			continue
		}
		if evt.Test == "" {
			failedPackages[evt.Package] = true
			continue
		}
		failedTests[evt.Package] = true
	}
	if scanner.Err() != nil {
		return true
	}
	for pkg := range failedPackages {
		if !failedTests[pkg] {
			return true
		}
	}
	return false
}

// TestFailureDiagnosticList returns the diagnostics for a failed `go test` run.
// It never returns an empty slice, and never returns only the "N test(s) failed"
// count: unless some diagnostic pins a source location, the cause is still
// unexplained, so the raw output is carried alongside. GoBuildErrors and
// LintErrors already surface unparsed tool output that way; the test path needs
// the same floor so a data race reported after the last test, a panic once the
// tests are done, a TestMain exit code, a build/vet error, or a run with
// `--test-json=false` can never fail silently.
//
// TestDiagnosticList stays the pure parser and still returns nothing when there
// is nothing to parse, so call this one only for runs that actually failed.
func TestFailureDiagnosticList(output string, locate TestFileLocator) []ToolDiagnostic {
	diagnostics := TestDiagnosticList(output, locate)
	unexplained := hasUnexplainedTestFailure(output)
	for _, diagnostic := range diagnostics {
		if diagnostic.File != "" && !unexplained {
			return diagnostics
		}
	}
	return append(diagnostics, ToolDiagnostic{
		Category:    "GO_TEST_FAILURE",
		Severity:    "error",
		Description: TestFailureText(output),
	})
}

// TestFailureText renders raw `go test` output as diagnostic text: `-json`
// events are unwrapped back into the lines the toolchain printed, anything else
// is used verbatim, and the result is bounded. It never returns an empty
// string, so callers always have something to report.
func TestFailureText(output string) string {
	text := strings.ToValidUTF8(unwrapTestOutput(output), "\uFFFD")
	text = boundFailureOutput(text)
	if text == "" {
		return "go test failed without any diagnostic output"
	}
	return text
}

// maxFailureOutputBytes is the report's public per-message budget. Applying it
// at the causal selector prevents a later plain-tail cut from discarding the
// panic, race or FAIL block this package deliberately chose.
const maxFailureOutputBytes = protocolcli.ReportMaxMessageBytes

// failureBlockRe matches the first line of the blocks worth keeping when the
// output does not fit the budget. `go test ./...` keeps running after a package
// fails, so the block that explains the failure is often far from the end and a
// plain tail would drop it.
var failureBlockRe = regexp.MustCompile(`(?m)^[ \t]*(?:WARNING: DATA RACE|panic:|fatal error:|--- FAIL:)`)

// verboseScaffoldRe matches the per-test lines `go test` prints only under -v.
// `-json` forces -v on, so an unwrapped JSON stream is padded with one RUN and
// one PASS line per test — hundreds of lines of no diagnostic value that would
// bury the failure. Everything else (failures, race reports, panics, package
// results) is kept.
var verboseScaffoldRe = regexp.MustCompile(`^(?:=== (?:RUN|PAUSE|CONT|NAME)\b|[ \t]*--- (?:PASS|SKIP): )`)

// unwrapTestOutput turns `go test -json` output back into the plain text the
// toolchain printed. Output that is not JSON (the `--test-json=false` cadence,
// or a toolchain error that never reached the JSON writer) is returned as is —
// there the caller chose the format, so nothing is dropped.
func unwrapTestOutput(output string) string {
	var lines []string
	sawEvent := false
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		var evt TestEvent
		if json.Unmarshal([]byte(line), &evt) == nil && evt.Action != "" {
			sawEvent = true
			// Go 1.26 emits compile and vet failures as build-output events.
			// Output presence, not the action spelling, defines transcript data.
			if evt.Output != "" && !verboseScaffoldRe.MatchString(evt.Output) {
				lines = append(lines, strings.TrimRight(evt.Output, "\n"))
			}
			continue
		}
		lines = append(lines, line)
	}
	// A JSON output event can legitimately exceed Scanner's token limit when a
	// test logs a large value. Returning the events parsed before that point
	// would hide the real failure that followed, so retain the original stream
	// and let the outer size bound select a useful excerpt instead.
	if err := scanner.Err(); err != nil {
		return strings.TrimSpace(output)
	}
	if !sawEvent {
		return strings.TrimSpace(output)
	}
	// A JSON stream whose events carried no output at all still has the raw form
	// to fall back on.
	if text := strings.TrimSpace(strings.Join(lines, "\n")); text != "" {
		return text
	}
	return strings.TrimSpace(output)
}

// boundFailureOutput trims text to maxFailureOutputBytes and marks what it
// dropped. It keeps the tail — where the final FAIL lines live — unless a
// failure block starts earlier, which outranks the tail.
func boundFailureOutput(text string) string {
	if len(text) <= maxFailureOutputBytes {
		return text
	}
	marker := fmt.Sprintf("... (truncated from %d bytes)\n", len(text))
	budget := maxFailureOutputBytes - len(marker)
	if budget <= 0 {
		return marker[:maxFailureOutputBytes]
	}
	start := len(text) - budget
	if nl := strings.IndexByte(text[start:], '\n'); nl >= 0 {
		start += nl + 1 // begin on a line boundary
	} else {
		for start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
	}
	if loc := failureBlockRe.FindStringIndex(text); loc != nil && loc[0] < start {
		start = loc[0]
	}
	end := min(start+budget, len(text))
	for end < len(text) && end > start && !utf8.RuneStart(text[end]) {
		end--
	}
	return marker + text[start:end]
}

// FileCoverage holds coverage data for a single source file.
type FileCoverage struct {
	File              string  `json:"file"`
	TotalStatements   int     `json:"totalStatements"`
	CoveredStatements int     `json:"coveredStatements"`
	Percentage        float64 `json:"percentage"`
}

// CoverageResult holds aggregate and per-file coverage data.
type CoverageResult struct {
	TotalStatements   int            `json:"totalStatements"`
	CoveredStatements int            `json:"coveredStatements"`
	Percentage        float64        `json:"percentage"`
	Files             []FileCoverage `json:"files"`
}

// Coverage parses a Go coverage profile and returns the coverage percentage.
func Coverage(coverprofile string) (float64, error) {
	result, err := CoverageDetails(coverprofile)
	if err != nil {
		return 0, err
	}
	return result.Percentage, nil
}

// CoverageDiagnostics emits diagnostics for files with zero coverage.
func CoverageDiagnostics(result CoverageResult, workspaceRoot, projectPath string, emit *jsonl.Emitter) {
	for _, diagnostic := range CoverageDiagnosticList(result, workspaceRoot, projectPath) {
		emit.DiagnosticWithCode(
			diagnostic.Severity,
			diagnostic.Description,
			diagnostic.File,
			diagnostic.Line,
			diagnostic.Column,
			diagnostic.Category,
		)
	}
}

// CoverageDiagnosticList returns one warning for every instrumented source file
// with zero coverage.
func CoverageDiagnosticList(result CoverageResult, workspaceRoot, projectPath string) []ToolDiagnostic {
	var diagnostics []ToolDiagnostic
	for _, f := range result.Files {
		if f.Percentage > 0 || f.TotalStatements == 0 {
			continue
		}
		// Strip the module prefix to get a relative file path.
		file := f.File
		if idx := strings.Index(file, "/"); idx >= 0 {
			candidate := filepath.Join(projectPath, file[idx+1:])
			if _, err := os.Stat(candidate); err == nil {
				file = diagPath(workspaceRoot, projectPath, file[idx+1:])
			}
		}
		diagnostics = append(diagnostics, ToolDiagnostic{
			Category:    "UNCOVERED_FILE",
			Severity:    "warning",
			Description: fmt.Sprintf("0%% coverage (%d statements)", f.TotalStatements),
			File:        file,
		})
	}
	return diagnostics
}

// ErrCoverageProfileUnreadable marks a coverage profile that exists but could
// not be read to its end. Its blocks are unknown, so no percentage is derived
// from it.
var ErrCoverageProfileUnreadable = errors.New("coverage profile unreadable")

// LineScanner returns a line scanner with no line-length limit. A Go test
// stream or coverage profile can carry a line longer than bufio's 64 KiB
// default, and a scanner that stops there reads a prefix of its input.
func LineScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), math.MaxInt)
	return scanner
}

// CoverageDetails parses a Go coverage profile and returns aggregate and per-file coverage.
// A missing profile returns an fs.ErrNotExist error. A profile that exists but
// cannot be read to its end returns an error wrapping
// ErrCoverageProfileUnreadable, never the part read before the failure.
func CoverageDetails(coverprofile string) (CoverageResult, error) {
	f, err := os.Open(coverprofile)
	if errors.Is(err, fs.ErrNotExist) {
		return CoverageResult{}, err
	}
	if err != nil {
		return CoverageResult{}, fmt.Errorf("%w: %w", ErrCoverageProfileUnreadable, err)
	}
	defer f.Close()
	result, err := coverageDetails(f)
	if err != nil {
		// A file read error already names the profile; any other cause does not.
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) {
			err = fmt.Errorf("%s: %w", coverprofile, err)
		}
		return CoverageResult{}, fmt.Errorf("%w: %w", ErrCoverageProfileUnreadable, err)
	}
	return result, nil
}

func coverageDetails(profile io.Reader) (CoverageResult, error) {
	// Deduplicate coverage blocks. When -coverpkg=./... is used with multiple
	// test packages, the same source block can appear multiple times in the
	// profile. We keep the max count per block (matching go tool cover behavior).
	type blockEntry struct {
		file  string
		stmts int
		count int
	}
	blocks := make(map[string]*blockEntry) // keyed by full location (file:start,end)
	var blockOrder []string

	scanner := LineScanner(profile)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		// Format: pkg/file.go:start.col,end.col statements count
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		stmts, err := strconv.Atoi(parts[len(parts)-2])
		if err != nil {
			continue
		}
		count, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			continue
		}
		loc := parts[0]
		colonIdx := strings.LastIndex(loc, ":")
		if colonIdx < 0 {
			continue
		}

		if b, ok := blocks[loc]; ok {
			if count > b.count {
				b.count = count
			}
		} else {
			blocks[loc] = &blockEntry{file: loc[:colonIdx], stmts: stmts, count: count}
			blockOrder = append(blockOrder, loc)
		}
	}
	// With -coverpkg, the first package's section already lists every block, so
	// a partial read keeps the full statement total and loses covered counts.
	if err := scanner.Err(); err != nil {
		return CoverageResult{}, err
	}

	// Accumulate per-file statement counts from deduplicated blocks.
	type fileStats struct {
		total   int
		covered int
	}
	perFile := make(map[string]*fileStats)
	var fileOrder []string
	for _, loc := range blockOrder {
		b := blocks[loc]
		fs, ok := perFile[b.file]
		if !ok {
			fs = &fileStats{}
			perFile[b.file] = fs
			fileOrder = append(fileOrder, b.file)
		}
		fs.total += b.stmts
		if b.count > 0 {
			fs.covered += b.stmts
		}
	}

	var result CoverageResult
	for _, file := range fileOrder {
		fs := perFile[file]
		result.TotalStatements += fs.total
		result.CoveredStatements += fs.covered
		pct := 0.0
		if fs.total > 0 {
			pct = math.Round(float64(fs.covered)/float64(fs.total)*10000) / 100
		}
		result.Files = append(result.Files, FileCoverage{
			File:              file,
			TotalStatements:   fs.total,
			CoveredStatements: fs.covered,
			Percentage:        pct,
		})
	}
	if result.TotalStatements > 0 {
		result.Percentage = math.Round(float64(result.CoveredStatements)/float64(result.TotalStatements)*10000) / 100
	}
	return result, nil
}
