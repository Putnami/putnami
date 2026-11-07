package stacks

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	readers "go.putnami.dev/evidence/readers"
	diag "go.putnami.dev/protocol/diagnostic"
)

// Diagnostic codes of the report reading.
const (
	CodeReportMissing = "stacks.report_missing"
	CodeTestFailed    = "stacks.test_failed"
)

// Summary is what a task's reports say.
type Summary struct {
	// Tests counts the JUnit cases by status.
	Tests TestTotals `json:"tests"`
	// Failed names each failed case, "suite > name", at most maxFailedNames.
	Failed []string `json:"failed,omitempty"`
	// Coverage is the line coverage of every coverage report, summed.
	Coverage *readers.Counts `json:"coverage,omitempty"`
	// Reports are the report files read, relative to the project directory.
	Reports []string `json:"reports,omitempty"`
}

// TestTotals count test cases.
type TestTotals struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

const maxFailedNames = 50

// coverageReaders maps a profile's coverage format to its reader.
var coverageReaders = map[string]func([]byte) (readers.CoverageReport, []diag.Diagnostic){
	"lcov":      readers.ReadLCOV,
	"cobertura": readers.ReadCobertura,
	"jacoco":    readers.ReadJaCoCo,
	"clover":    readers.ReadClover,
	"gocover":   readers.ReadGoCover,
}

// ReadReports reads every report a task names. paths resolves a report
// pattern to absolute file paths; projectAbs is the project directory, which
// the summary's report names are relative to. A JUnit pattern set that
// matches no file is CodeReportMissing: a test command that ran nothing is no
// pass.
func ReadReports(r *Reports, paths func(pattern string) ([]string, error), projectAbs string) (Summary, []diag.Diagnostic) {
	var s Summary
	var diags []diag.Diagnostic
	if r == nil {
		return s, nil
	}
	rel := func(p string) string {
		if out, err := filepath.Rel(projectAbs, p); err == nil {
			return filepath.ToSlash(out)
		}
		return filepath.ToSlash(p)
	}
	if len(r.JUnit) > 0 {
		files, err := expandAll(r.JUnit, paths)
		if err != nil {
			return s, []diag.Diagnostic{diag.Errorf(CodeReportMissing, "", "%v", err)}
		}
		if len(files) == 0 {
			diags = append(diags, diag.Errorf(CodeReportMissing, "", "the test task wrote no JUnit report matching %s", strings.Join(r.JUnit, ", ")))
		}
		for _, f := range files {
			report, d := readers.ReadFile(f, readers.ReadJUnit)
			diags = append(diags, relabel(d, rel(f))...)
			s.Reports = append(s.Reports, rel(f))
			for _, c := range report.Cases {
				s.Tests.Total++
				switch c.Status {
				case readers.StatusFailed:
					s.Tests.Failed++
					if len(s.Failed) < maxFailedNames {
						s.Failed = append(s.Failed, caseName(c))
					}
				case readers.StatusSkipped:
					s.Tests.Skipped++
				default:
					s.Tests.Passed++
				}
			}
		}
	}
	for _, c := range r.Coverage {
		read := coverageReaders[c.Format]
		files, err := expandAll(c.Paths, paths)
		if err != nil {
			diags = append(diags, diag.Errorf(CodeReportMissing, "", "%v", err))
			continue
		}
		if len(files) == 0 {
			diags = append(diags, diag.Errorf(CodeReportMissing, "", "the test task wrote no %s coverage report matching %s", c.Format, strings.Join(c.Paths, ", ")))
		}
		for _, f := range files {
			report, d := readers.ReadFile(f, read)
			diags = append(diags, relabel(d, rel(f))...)
			s.Reports = append(s.Reports, rel(f))
			if s.Coverage == nil {
				s.Coverage = &readers.Counts{}
			}
			for _, fc := range report.Files {
				s.Coverage.Total += fc.LineCounts.Total
				s.Coverage.Covered += fc.LineCounts.Covered
			}
		}
	}
	if s.Tests.Failed > 0 {
		diags = append(diags, diag.Errorf(CodeTestFailed, "", "%d of %d tests failed: %s", s.Tests.Failed, s.Tests.Total, strings.Join(s.Failed, "; ")))
	}
	return s, diags
}

func expandAll(patterns []string, paths func(string) ([]string, error)) ([]string, error) {
	var out []string
	for _, p := range patterns {
		files, err := paths(p)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func caseName(c readers.TestCase) string {
	prefix := c.Classname
	if prefix == "" && len(c.Suites) > 0 {
		prefix = c.Suites[len(c.Suites)-1].Name
	}
	if prefix == "" {
		return c.Name
	}
	return prefix + " > " + c.Name
}

// relabel names the report in each diagnostic of a reader by its project path
// rather than the absolute one.
func relabel(d []diag.Diagnostic, name string) []diag.Diagnostic {
	for i := range d {
		d[i].Field = name
	}
	return d
}

// globFiles returns the regular files a slash pattern matches under base,
// with "**" for any number of directories, sorted.
func globFiles(base, pattern string) ([]string, error) {
	if !strings.Contains(pattern, "**") {
		matches, err := filepath.Glob(filepath.Join(base, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", pattern, err)
		}
		return regularFiles(matches), nil
	}
	var out []string
	err := filepath.WalkDir(base, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if p != base && (entry.Name() == ".git" || entry.Name() == ".putnami") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return nil
		}
		if MatchPattern(pattern, filepath.ToSlash(rel)) {
			out = append(out, p)
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

func regularFiles(matches []string) []string {
	var out []string
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() {
			out = append(out, m)
		}
	}
	slices.Sort(out)
	return out
}

// MatchPattern reports whether a slash path matches a slash pattern whose
// segments are path.Match patterns or "**", any number of segments.
func MatchPattern(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := filepath.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}
