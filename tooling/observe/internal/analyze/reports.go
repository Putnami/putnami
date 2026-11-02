package analyze

import (
	"path/filepath"
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
// that holds its test file, else the one of the Go package its suite names,
// else the one that holds the report.
func (r *Result) attributeTests(report reports.Report, result readers.TestReport, m *mapper, tests map[string]*evidence.ComponentTests) {
	bases := m.bases(report.Path)
	reportOwner, reportOwned := m.set.Owner(report.Path)
	if isAbsolute(report.Path) {
		reportOwned = false
	}
	unattributed := 0
	for _, c := range result.Cases {
		owner, file, ok := "", "", false
		for _, candidate := range []string{c.File, c.Classname} {
			if rel, found := m.resolve(candidate, bases); found {
				file = rel
				owner, ok = m.set.Owner(rel)
				break
			}
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
