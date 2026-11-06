// Package readers reads the test and coverage reports a CI writes: JUnit XML,
// TRX, LCOV, Cobertura XML, Clover XML and Go cover profiles.
//
// A reader turns one report into typed facts and nothing more. A test report
// becomes one TestCase per test case, a coverage report one FileCoverage per
// source file with its instrumented lines and their hit counts. Paths stay as
// the report states them, in slash form: mapping a case or a file to a
// component, or an absolute path or a Go import path to a repository path, is
// the caller's.
//
// A reader never panics and never returns half a report. A report it cannot
// read yields an empty result and one error diagnostic with a stable code.
package readers

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Diagnostic codes.
const (
	// CodeMalformed is a report that does not decode, whose root is not its
	// format's, or that states an impossible value.
	CodeMalformed = "readers.malformed"
	// CodeTooLarge is a report larger than MaxReportBytes, or a coverage
	// report that marks more than MaxLineMarks lines.
	CodeTooLarge = "readers.too_large"
	// CodeMissing is a report file that does not exist.
	CodeMissing = "readers.missing"
	// CodeUnreadable is a report file that exists but cannot be read.
	CodeUnreadable = "readers.unreadable"
)

// MaxReportBytes caps the size of one report.
const MaxReportBytes = 256 << 20

// MaxLineMarks caps the line entries one coverage report marks, a line
// stated twice counted twice; a Go block marks every line of its span.
const MaxLineMarks = 4_000_000

// ReadFile reads the report at path with read. A missing file, or an empty
// path, returns CodeMissing, a file larger than MaxReportBytes CodeTooLarge, and a file that
// cannot be read CodeUnreadable; each diagnostic names path in its field.
func ReadFile[T any](path string, read func([]byte) (T, []diag.Diagnostic)) (T, []diag.Diagnostic) {
	var zero T
	if path == "" {
		return zero, []diag.Diagnostic{diag.Errorf(CodeMissing, "", "report path is empty")}
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return zero, []diag.Diagnostic{diag.Errorf(CodeMissing, path, "report %s does not exist", path)}
		}
		return zero, []diag.Diagnostic{diag.Errorf(CodeUnreadable, path, "report %s cannot be read: %v", path, err)}
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxReportBytes+1))
	if err != nil {
		return zero, []diag.Diagnostic{diag.Errorf(CodeUnreadable, path, "report %s cannot be read: %v", path, err)}
	}
	result, diagnostics := read(data)
	for i := range diagnostics {
		if diagnostics[i].Field == "" {
			diagnostics[i].Field = path
		}
	}
	return result, diagnostics
}

// tooLarge reports data over MaxReportBytes.
func tooLarge(data []byte) []diag.Diagnostic {
	if len(data) <= MaxReportBytes {
		return nil
	}
	return []diag.Diagnostic{diag.Errorf(CodeTooLarge, "", "report is larger than %d bytes", MaxReportBytes)}
}

func malformed(format string, args ...any) []diag.Diagnostic {
	return []diag.Diagnostic{diag.Errorf(CodeMalformed, "", format, args...)}
}

// slashPath returns path in slash form whatever host wrote it.
func slashPath(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}

// Counts is a number of instrumented items and how many of them ran.
type Counts struct {
	Total   int `json:"total"`
	Covered int `json:"covered"`
}

// Line is one instrumented source line.
type Line struct {
	// Number is the line number, from 1.
	Number int `json:"number"`
	// Hits is how many times the tests executed the line; 0 is uncovered.
	Hits int64 `json:"hits"`
}

// FileCoverage is the coverage of one source file.
type FileCoverage struct {
	// Path is the file's path as the report states it, in slash form.
	Path string `json:"path"`
	// Lines are the instrumented lines, in increasing order, one per number.
	Lines []Line `json:"lines,omitempty"`
	// LineCounts counts Lines. For a file whose report lists no line, it is
	// the totals the report states (LCOV's LF and LH).
	LineCounts Counts `json:"lineCounts"`
	// Functions counts the functions the report lists for the file. For a
	// file whose report lists none, it is the totals the report states
	// (LCOV's FNF and FNH, a Clover file's methods metrics); zero for a format
	// without functions.
	Functions Counts `json:"functions"`
}

// CoverageReport is one coverage report.
type CoverageReport struct {
	// Sources are the source roots a Cobertura report states, in report
	// order; a Cobertura file path is relative to one of them.
	Sources []string `json:"sources,omitempty"`
	// Files is one entry per source file, in the order the report first names it.
	Files []FileCoverage `json:"files"`
}

// coverageBuilder merges the entries a report states for its files.
type coverageBuilder struct {
	order []string
	files map[string]*fileBuilder
	// marks counts the line entries recorded, up to MaxLineMarks.
	marks int
}

type fileBuilder struct {
	builder *coverageBuilder
	lines   map[int]int64
	// functions maps a function's identity to whether it ran.
	functions map[string]bool
	// stated are the totals the report states for the file, when it does.
	statedLines, statedFunctions *Counts
}

func newCoverageBuilder() *coverageBuilder {
	return &coverageBuilder{files: map[string]*fileBuilder{}}
}

func (b *coverageBuilder) file(path string) *fileBuilder {
	path = slashPath(path)
	file, ok := b.files[path]
	if !ok {
		file = &fileBuilder{builder: b, lines: map[int]int64{}, functions: map[string]bool{}}
		b.files[path] = file
		b.order = append(b.order, path)
	}
	return file
}

// line records a line's count; a line stated twice keeps its largest count.
// Past MaxLineMarks entries it records nothing and returns CodeTooLarge.
func (f *fileBuilder) line(number int, hits int64) []diag.Diagnostic {
	f.builder.marks++
	if f.builder.marks > MaxLineMarks {
		return []diag.Diagnostic{diag.Errorf(CodeTooLarge, "", "report marks more than %d lines", MaxLineMarks)}
	}
	if current, ok := f.lines[number]; !ok || hits > current {
		f.lines[number] = hits
	}
	return nil
}

func (f *fileBuilder) function(identity string, ran bool) {
	f.functions[identity] = f.functions[identity] || ran
}

// stateLines records totals a report states; totals stated twice keep the
// largest of each.
func (f *fileBuilder) stateLines(counts Counts) { f.statedLines = largest(f.statedLines, counts) }

func (f *fileBuilder) stateFunctions(counts Counts) {
	f.statedFunctions = largest(f.statedFunctions, counts)
}

func largest(current *Counts, counts Counts) *Counts {
	if current == nil {
		return &counts
	}
	return &Counts{Total: max(current.Total, counts.Total), Covered: max(current.Covered, counts.Covered)}
}

func (b *coverageBuilder) report(sources []string) CoverageReport {
	report := CoverageReport{Sources: sources, Files: make([]FileCoverage, 0, len(b.order))}
	for _, path := range b.order {
		file := b.files[path]
		coverage := FileCoverage{Path: path}
		numbers := make([]int, 0, len(file.lines))
		for number := range file.lines {
			numbers = append(numbers, number)
		}
		slices.Sort(numbers)
		for _, number := range numbers {
			hits := file.lines[number]
			coverage.Lines = append(coverage.Lines, Line{Number: number, Hits: hits})
			coverage.LineCounts.Total++
			if hits > 0 {
				coverage.LineCounts.Covered++
			}
		}
		if len(numbers) == 0 && file.statedLines != nil {
			coverage.LineCounts = *file.statedLines
		}
		for _, ran := range file.functions {
			coverage.Functions.Total++
			if ran {
				coverage.Functions.Covered++
			}
		}
		if len(file.functions) == 0 && file.statedFunctions != nil {
			coverage.Functions = *file.statedFunctions
		}
		report.Files = append(report.Files, coverage)
	}
	return report
}

// checkLine refuses a line number below 1 or a negative count.
func checkLine(where string, number int, hits int64) []diag.Diagnostic {
	if number < 1 {
		return malformed("%s: line number %d is below 1", where, number)
	}
	if hits < 0 {
		return malformed("%s: line %d has a negative count %d", where, number, hits)
	}
	return nil
}
