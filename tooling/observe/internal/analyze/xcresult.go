package analyze

import (
	"strings"

	readers "go.putnami.dev/evidence/readers"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/observe/internal/reports"
)

// Diagnostic codes of an .xcresult bundle.
const (
	// CodeXCResultToolsMissing is a bundle read on a runner without xcrun.
	CodeXCResultToolsMissing = "observe.xcresult_tools_missing"
	// CodeXCResultUnread is an xcrun command that failed on a bundle, or
	// printed what the reader refuses.
	CodeXCResultUnread = "observe.xcresult_unread"
)

// xcresultConversion is what a job without xcrun runs instead.
const xcresultConversion = "convert it to JUnit and Cobertura on the macOS runner, such as with " +
	"xcresultparser --output-format junit and --output-format cobertura, and name the reports with --junit and --coverage"

// XCRun runs xcrun with args and returns what it printed on its standard
// output. A nil XCRun is a runner without xcrun.
type XCRun func(args ...string) ([]byte, error)

// xcresultTests reads a bundle's tests through xcresulttool.
func (r *Result) xcresultTests(in Input, report reports.Report, abs string) (readers.TestReport, bool) {
	if !r.xcrunAvailable(in, report) {
		return readers.TestReport{}, false
	}
	return xcrunRead(r, in, report, readers.ReadXCResultTests, "xcresulttool", "get", "test-results", "tests", "--path", abs)
}

// xcresultCoverage reads a bundle's coverage through xccov, as one report:
// the archive's files with their lines, and the files only the report lists
// with their totals.
func (r *Result) xcresultCoverage(in Input, report reports.Report, abs string) []readers.CoverageReport {
	if !r.xcrunAvailable(in, report) {
		return nil
	}
	totals, totalsRead := xcrunRead(r, in, report, readers.ReadXccovReport, "xccov", "view", "--report", "--json", abs)
	lines, linesRead := xcrunRead(r, in, report, readers.ReadXccovArchive, "xccov", "view", "--archive", "--json", abs)
	if !totalsRead && !linesRead {
		return nil
	}
	merged := readers.CoverageReport{Files: lines.Files}
	listed := map[string]bool{}
	for _, file := range lines.Files {
		listed[file.Path] = true
	}
	for _, file := range totals.Files {
		if !listed[file.Path] {
			merged.Files = append(merged.Files, file)
		}
	}
	return []readers.CoverageReport{merged}
}

// xcrunAvailable reports whether xcrun can read a bundle, and warns once per
// bundle when it cannot.
func (r *Result) xcrunAvailable(in Input, report reports.Report) bool {
	if in.XCRun != nil {
		return true
	}
	if r.xcresultWarned == nil {
		r.xcresultWarned = map[string]bool{}
	}
	if !r.xcresultWarned[report.Path] {
		r.xcresultWarned[report.Path] = true
		r.warn(CodeXCResultToolsMissing, report.Path, "the .xcresult bundle %s is not read: xcrun is not on this runner's PATH; %s", report.Path, xcresultConversion)
	}
	return false
}

// xcrunRead runs one xcrun command on a bundle and reads what it printed.
func xcrunRead[T any](r *Result, in Input, report reports.Report, read func([]byte) (T, []diag.Diagnostic), args ...string) (T, bool) {
	var zero T
	command := "xcrun " + strings.Join(args[:len(args)-1], " ")
	output, err := in.XCRun(args...)
	if err != nil {
		r.warn(CodeXCResultUnread, report.Path, "%s %s failed: %v; %s", command, report.Path, err, xcresultConversion)
		return zero, false
	}
	result, diags := read(output)
	if diag.HasErrors(diags) {
		r.warn(CodeXCResultUnread, report.Path, "%s %s printed what observe cannot read: %s; %s", command, report.Path, diag.ErrorText(diags), xcresultConversion)
		return zero, false
	}
	return result, true
}
