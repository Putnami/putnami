package analyze

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	readers "go.putnami.dev/evidence/readers"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/reports"
)

// readReports reads every report: the tests and the coverage per component,
// and the merged line coverage per repository file.
func (r *Result) readReports(in Input, m *mapper) map[string]lineHits {
	tests := map[string]*evidence.ComponentTests{}
	lines := map[string]lineHits{}
	stated := map[string]readers.Counts{}
	for _, report := range in.Reports {
		abs := report.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(in.Root, filepath.FromSlash(report.Path))
		}
		switch report.Kind {
		case reports.KindJUnit:
			result, diags := readers.ReadFile(abs, readers.ReadJUnit)
			if r.unread(report, diags) {
				continue
			}
			r.attributeTests(report, result, m, tests)
		case reports.KindCoverage:
			result, ok := r.readCoverage(report, abs)
			if !ok {
				continue
			}
			r.mapCoverage(report, result, m, lines, stated)
		}
	}
	sortedComponents(tests, &r.Tests)
	coverage := map[string]*evidence.ComponentCoverage{}
	add := func(file string, covered, total int) {
		owner, ok := m.set.Owner(file)
		if !ok {
			return
		}
		entry := coverage[owner]
		if entry == nil {
			entry = &evidence.ComponentCoverage{Component: owner}
			coverage[owner] = entry
		}
		entry.CoveredLines += covered
		entry.TotalLines += total
	}
	for file, hits := range lines {
		covered := 0
		for _, n := range hits {
			if n > 0 {
				covered++
			}
		}
		add(file, covered, len(hits))
	}
	for file, counts := range stated {
		if _, measured := lines[file]; !measured {
			add(file, min(counts.Covered, counts.Total), counts.Total)
		}
	}
	sortedComponents(coverage, &r.Coverage)
	return lines
}

// unread reports a report a reader refused, and whether it did.
func (r *Result) unread(report reports.Report, diags []diag.Diagnostic) bool {
	if !diag.HasErrors(diags) {
		return false
	}
	r.warn(CodeReportUnread, report.Path, "the %s report %s is left out: %s", report.Format, report.Path, diag.ErrorText(diags))
	return true
}

func (r *Result) readCoverage(report reports.Report, abs string) (readers.CoverageReport, bool) {
	var read func([]byte) (readers.CoverageReport, []diag.Diagnostic)
	switch report.Format {
	case reports.FormatLCOV:
		read = readers.ReadLCOV
	case reports.FormatCobertura:
		read = readers.ReadCobertura
	case reports.FormatClover:
		read = readers.ReadClover
	case reports.FormatGoCover:
		read = readers.ReadGoCover
	default:
		return readers.CoverageReport{}, false
	}
	result, diags := readers.ReadFile(abs, read)
	return result, !r.unread(report, diags)
}

// mapCoverage maps each file of a coverage report to a repository file and
// merges its lines: a line two reports instrument keeps its largest count.
func (r *Result) mapCoverage(report reports.Report, result readers.CoverageReport, m *mapper, lines map[string]lineHits, stated map[string]readers.Counts) {
	bases := m.bases(report.Path)
	var sourceBases []string
	for _, source := range result.Sources {
		if isAbsolute(source) {
			if rel, ok := m.relative(source); ok {
				sourceBases = append(sourceBases, rel)
			} else if rel, ok := m.suffix(source, m.isDir); ok {
				// A source written in another checkout directory.
				sourceBases = append(sourceBases, rel)
			}
			continue
		}
		for _, base := range bases {
			sourceBases = append(sourceBases, joinDir(base, source))
		}
	}
	unmapped := 0
	for _, file := range result.Files {
		rel, ok := m.resolve(file.Path, append(sourceBases, bases...))
		if !ok && report.Format == reports.FormatGoCover {
			// A Go profile names import paths; only a located file that
			// exists is one.
			if located, _, found := m.set.Locate(file.Path); found && m.isFile(located) {
				rel, ok = located, true
			}
		}
		if !ok {
			unmapped++
			continue
		}
		if len(file.Lines) == 0 {
			current := stated[rel]
			stated[rel] = readers.Counts{Total: max(current.Total, file.LineCounts.Total), Covered: max(current.Covered, file.LineCounts.Covered)}
			continue
		}
		hits := lines[rel]
		if hits == nil {
			hits = lineHits{}
			lines[rel] = hits
		}
		for _, line := range file.Lines {
			if current, seen := hits[line.Number]; !seen || line.Hits > current {
				hits[line.Number] = line.Hits
			}
		}
	}
	if unmapped > 0 {
		r.warn(CodeCoverageUnmapped, report.Path, "%d of the %d files of %s are not files of the repository and are left out", unmapped, len(result.Files), report.Path)
	}
}

func joinDir(base, dir string) string {
	if base == "." {
		return dir
	}
	return base + "/" + dir
}

// attributeTests gives each case of a JUnit report to a component: the one
// of the test target that wrote the report, else the one that holds its test
// file, else the one of the Go package its suite names, else the one that
// holds the report.
//
// The test file is the case's file, else its class name read as a path
// (Vitest), else its dotted class name read as a Python module: pytest writes
// tests.test_api.TestLogin for a case of tests/test_api.py, and its default
// xunit2 report has no file attribute. Else it is the dotted class name read
// as a Dart test file: tojunit writes test.price for test/price_test.dart.
func (r *Result) attributeTests(report reports.Report, result readers.TestReport, m *mapper, tests map[string]*evidence.ComponentTests) {
	bases := m.bases(report.Path)
	reportOwner, reportOwned := m.set.Owner(report.Path)
	if isAbsolute(report.Path) {
		reportOwned = false
	}
	// A test log names the test target that wrote it, such as a Bazel log of
	// bazel-testlogs: every case goes to the target's component.
	targetOwner, targeted := "", false
	if !isAbsolute(report.Path) {
		targetOwner, targeted = m.set.ReportOwner(report.Path)
	}
	unattributed := 0
	for _, c := range result.Cases {
		owner, file, ok := "", "", false
		candidates := append([]string{c.File, c.Classname}, moduleFiles(c.Classname)...)
		for _, candidate := range append(candidates, dartTestFiles(c.Classname)...) {
			if rel, found := m.resolve(candidate, bases); found {
				file = rel
				owner, ok = m.set.Owner(rel)
				break
			}
		}
		if targeted {
			owner, ok = targetOwner, true
		}
		if !ok {
			names := []string{c.Classname}
			for i := len(c.Suites) - 1; i >= 0; i-- {
				names = append(names, c.Suites[i].Name)
			}
			for _, name := range names {
				if name == "" {
					continue
				}
				if _, component, found := m.set.Locate(name); found {
					owner, ok = component, true
					break
				}
			}
		}
		if !ok && reportOwned {
			owner, ok = reportOwner, true
		}
		if !ok {
			unattributed++
			continue
		}
		entry := tests[owner]
		if entry == nil {
			entry = &evidence.ComponentTests{Component: owner}
			tests[owner] = entry
		}
		entry.DurationMs += c.DurationMs
		switch c.Status {
		case readers.StatusFailed:
			entry.Failed++
			if len(entry.FailingCases) < evidence.MaxFailingCases {
				failing := evidence.FailingCase{Name: bounded(c.Name), File: file}
				if failing.Name == "" {
					failing.Name = "(unnamed)"
				}
				if len(c.Suites) > 0 {
					failing.Suite = bounded(c.Suites[len(c.Suites)-1].Name)
				} else {
					failing.Suite = bounded(c.Classname)
				}
				if c.Failure != nil {
					failing.Message = bounded(c.Failure.Message)
				}
				entry.FailingCases = append(entry.FailingCases, failing)
			}
		case readers.StatusSkipped:
			entry.Skipped++
		default:
			entry.Passed++
		}
	}
	if unattributed > 0 {
		r.warn(CodeTestsUnattributed, report.Path, "%d test cases of %s belong to no component: neither their file nor the report lies in one", unattributed, report.Path)
	}
}

// dottedName is a class name that can name a Python module: dot-separated
// names, as pytest writes them from the test file's path.
var dottedName = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// moduleFiles returns the files a dotted class name may name, longest first:
// tests.test_api.TestLogin may be tests/test_api/TestLogin.py,
// tests/test_api.py or tests.py.
func moduleFiles(classname string) []string {
	if !dottedName.MatchString(classname) {
		return nil
	}
	parts := strings.Split(classname, ".")
	out := make([]string, 0, len(parts))
	for n := len(parts); n > 0; n-- {
		out = append(out, strings.Join(parts[:n], "/")+".py")
	}
	return out
}

// tojunitName is a class name tojunit can write: the test file's path without
// _test.dart or .dart, with each / written as a ., after an optional leading
// . for an absolute path.
var tojunitName = regexp.MustCompile(`^\.?[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)

// dartTestFiles returns the Dart test files a tojunit class name may name,
// longest first. Its leading segments may be the directories of an absolute
// path, or the --package prefix, so each shorter tail is tried too:
// .ci.repo.apps.mobile.test.widget may be apps/mobile/test/widget_test.dart.
func dartTestFiles(classname string) []string {
	if !tojunitName.MatchString(classname) {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(classname, "."), ".")
	var out []string
	for start := max(0, len(parts)-maxSuffixSegments); start < len(parts); start++ {
		stem := strings.Join(parts[start:], "/")
		out = append(out, stem+"_test.dart", stem+".dart")
	}
	return out
}

// bounded cuts s to the evidence string bound, on a rune boundary.
func bounded(s string) string {
	if len(s) <= evidence.MaxStringBytes {
		return s
	}
	cut := evidence.MaxStringBytes - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
