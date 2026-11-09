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
	CodeReportMissing    = "stacks.report_missing"
	CodeTestFailed       = "stacks.test_failed"
	CodeTestUnattributed = "stacks.test_unattributed"
	CodeCoverageUnmapped = "stacks.coverage_unmapped"
)

// Owner says whose a report entry is.
type Owner int

// The owners of a report entry.
const (
	// OwnerTask is the component the task runs.
	OwnerTask Owner = iota
	// OwnerOther is another component of the stack, whose own task counts it.
	OwnerOther
	// OwnerNone is no component.
	OwnerNone
)

// Attribution decides whose each report entry is. A nil member reads every
// entry as the task's.
type Attribution struct {
	// Case is the owner of a JUnit case.
	Case func(readers.TestCase) Owner
	// File is the owner of a coverage report's file, by the path the report
	// states.
	File func(path string) Owner
}

// maxNamed bounds the cases and files a diagnostic names.
const maxNamed = 10

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
	// OtherCases counts the cases of other components, left out of Tests.
	OtherCases int `json:"otherCases,omitempty"`
	// OtherFiles counts the coverage files of other components, left out of
	// Coverage.
	OtherFiles int `json:"otherFiles,omitempty"`
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
// pass. attr decides whose each case and coverage file is: another
// component's entries are left out of the summary, a case of no component is
// counted with CodeTestUnattributed, and a coverage file of no component is
// left out with CodeCoverageUnmapped.
func ReadReports(r *Reports, paths func(pattern string) ([]string, error), projectAbs string, attr Attribution) (Summary, []diag.Diagnostic) {
	var s Summary
	var diags []diag.Diagnostic
	if r == nil {
		return s, nil
	}
	var unattributed, unmapped []string
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
				switch owner(attr.Case, c) {
				case OwnerOther:
					s.OtherCases++
					continue
				case OwnerNone:
					unattributed = append(unattributed, caseName(c))
				}
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
		if len(files) == 0 && !c.Optional {
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
				switch owner(attr.File, fc.Path) {
				case OwnerOther:
					s.OtherFiles++
					continue
				case OwnerNone:
					unmapped = append(unmapped, fc.Path)
					continue
				}
				s.Coverage.Total += fc.LineCounts.Total
				s.Coverage.Covered += fc.LineCounts.Covered
			}
		}
	}
	if len(unattributed) > 0 {
		diags = append(diags, diag.Warningf(CodeTestUnattributed, "", "%d test cases name no component and are counted as this one's: %s", len(unattributed), named(unattributed)))
	}
	if len(unmapped) > 0 {
		diags = append(diags, diag.Warningf(CodeCoverageUnmapped, "", "%d coverage files lie in no component and are left out of the coverage: %s", len(unmapped), named(unmapped)))
	}
	if s.Tests.Failed > 0 {
		diags = append(diags, diag.Errorf(CodeTestFailed, "", "%d of %d tests failed: %s", s.Tests.Failed, s.Tests.Total, strings.Join(s.Failed, "; ")))
	}
	return s, diags
}

func owner[T any](of func(T) Owner, entry T) Owner {
	if of == nil {
		return OwnerTask
	}
	return of(entry)
}

// named lists at most maxNamed names, then how many more there are.
func named(names []string) string {
	if len(names) <= maxNamed {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:maxNamed], ", "), len(names)-maxNamed)
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
// with "**" for any number of directories, sorted. A "**" walk never enters a
// directory named in excludes.
func globFiles(base, pattern string, excludes ...string) ([]string, error) {
	if !strings.Contains(pattern, "**") {
		matches, err := filepath.Glob(filepath.Join(base, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", pattern, err)
		}
		return regularFiles(matches), nil
	}
	// Walk only below the pattern's literal prefix: "src/**" walks src/,
	// never the build directory beside it.
	start := base
	segments := strings.Split(pattern, "/")
	for _, seg := range segments[:len(segments)-1] {
		if seg == "**" || strings.ContainsAny(seg, "*?[") {
			break
		}
		start = filepath.Join(start, seg)
	}
	if info, err := os.Stat(start); err != nil || !info.IsDir() {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(start, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if p != start && (entry.Name() == ".git" || entry.Name() == ".putnami" || slices.Contains(excludes, entry.Name())) {
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
