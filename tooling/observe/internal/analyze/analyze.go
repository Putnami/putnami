// Package analyze computes what a change touches: the changed and dependent
// components, the tests and coverage of each component, the coverage of the
// changed lines and the dependency changes of the lockfiles, with the licenses
// and known vulnerabilities the software bills of materials list for them.
//
// It reads the components the detectors found, the reports the CI wrote and
// the git diff; it runs nothing. Every finding is a diagnostic.
package analyze

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/components"
	"go.putnami.dev/tooling/observe/internal/gitbase"
	"go.putnami.dev/tooling/observe/internal/reports"
)

// Diagnostic codes.
const (
	CodeReportUnread      = "observe.report_unread"
	CodeTestsUnattributed = "observe.tests_unattributed"
	CodeCoverageUnmapped  = "observe.coverage_unmapped"
	CodeLockfileUnread    = "observe.lockfile_unreadable"
)

// Revision names one side of the change.
type Revision int

// The two sides of a change.
const (
	Base Revision = iota
	Head
)

// Input is what one analysis reads.
type Input struct {
	// Root is the repository's top directory.
	Root string
	// Set is the components the detectors found.
	Set components.Set
	// ChangedFiles is the diff from the merge base to the head.
	ChangedFiles []gitbase.ChangedFile
	// Reports are the test and coverage reports found.
	Reports []reports.Report
	// AddedLines are the lines the change adds per path at the head; nil
	// without a base.
	AddedLines map[string][]int
	// Show returns the content of a repository path at one side of the
	// change; found is false when that side has no such file.
	Show func(side Revision, path string) (data []byte, found bool, err error)
}

// Result is what the analysis found.
type Result struct {
	// Components are every component, with its impact and edges.
	Components []evidence.Component `json:"components"`
	// Owners maps each changed path to the component that holds it.
	Owners map[string]string `json:"-"`
	// Tests and Coverage are per component, sorted by component.
	Tests    []evidence.ComponentTests    `json:"tests,omitempty"`
	Coverage []evidence.ComponentCoverage `json:"coverage,omitempty"`
	// ChangedLineCoverage is nil without a base or a coverage report.
	ChangedLineCoverage *evidence.ChangedLineCoverage `json:"changedLineCoverage,omitempty"`
	// DependencyChanges are the lockfile differences.
	DependencyChanges []evidence.DependencyChange `json:"dependencyChanges,omitempty"`
	// SBOM names the software bills of materials the dependency states were
	// read from; nil when none was read.
	SBOM        *evidence.SBOM    `json:"sbom,omitempty"`
	Diagnostics []diag.Diagnostic `json:"-"`
}

// Run analyzes one change.
func Run(in Input) Result {
	r := Result{Owners: map[string]string{}}
	changed := map[string]bool{}
	for _, file := range in.ChangedFiles {
		if owner, ok := in.Set.Owner(file.Path); ok {
			r.Owners[file.Path] = owner
			changed[owner] = true
		}
	}
	impacts := in.Set.Impacts(changed)
	for _, c := range in.Set.Components {
		r.Components = append(r.Components, evidence.Component{
			ID: c.ID, Name: c.Name, Ecosystem: c.Ecosystem, Source: c.Source, Impact: impacts[c.ID], DependsOn: c.DependsOn,
		})
	}
	m := newMapper(in.Root, in.Set)
	lines := r.readReports(in, m)
	r.changedLineCoverage(in, lines)
	r.dependencyChanges(in)
	r.sbomFacts(in)
	return r
}

// mapper resolves the paths reports state to repository paths.
type mapper struct {
	root   string
	set    components.Set
	exists map[string]bool
}

func newMapper(root string, set components.Set) *mapper {
	return &mapper{root: filepath.Clean(root), set: set, exists: map[string]bool{}}
}

// isFile reports whether the repository-relative path is a file.
func (m *mapper) isFile(rel string) bool {
	if known, ok := m.exists[rel]; ok {
		return known
	}
	info, err := os.Stat(filepath.Join(m.root, filepath.FromSlash(rel)))
	ok := err == nil && !info.IsDir()
	m.exists[rel] = ok
	return ok
}

// relative turns an absolute path into a repository path, when it lies under
// the root.
func (m *mapper) relative(abs string) (string, bool) {
	rel, err := filepath.Rel(m.root, filepath.Clean(filepath.FromSlash(abs)))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// maxSuffixSegments bounds the trailing segments tried for an absolute path
// written on another machine.
const maxSuffixSegments = 12

// resolve maps a path a report states to an existing repository file. bases
// are the repository directories a relative path may be relative to, in
// order. An absolute path outside the root is matched by its trailing
// segments, for a report written in another checkout directory.
func (m *mapper) resolve(stated string, bases []string) (string, bool) {
	stated = strings.ReplaceAll(strings.TrimSpace(stated), `\`, "/")
	if stated == "" {
		return "", false
	}
	if isAbsolute(stated) {
		if rel, ok := m.relative(stated); ok && m.isFile(rel) {
			return rel, true
		}
		if rel, ok := m.suffix(stated, m.isFile); ok {
			return rel, true
		}
		return "", false
	}
	for _, base := range bases {
		joined := path.Join(base, stated)
		if joined == ".." || strings.HasPrefix(joined, "../") {
			continue
		}
		if m.isFile(joined) {
			return joined, true
		}
	}
	return "", false
}

// suffix matches an absolute path written in another checkout by its longest
// trailing segments that name an existing repository path. A path with a ".."
// segment matches nothing.
func (m *mapper) suffix(abs string, exists func(string) bool) (string, bool) {
	segments := strings.Split(strings.TrimLeft(abs, "/"), "/")
	if slices.Contains(segments, "..") {
		return "", false
	}
	for start := max(1, len(segments)-maxSuffixSegments); start < len(segments); start++ {
		if rel := path.Join(segments[start:]...); exists(rel) {
			return rel, true
		}
	}
	return "", false
}

// isDir reports whether the repository path is a directory.
func (m *mapper) isDir(rel string) bool {
	info, err := os.Stat(filepath.Join(m.root, filepath.FromSlash(rel)))
	return err == nil && info.IsDir()
}

func isAbsolute(p string) bool {
	return strings.HasPrefix(p, "/") || (len(p) > 2 && p[1] == ':' && p[2] == '/')
}

// bases are the directories a path of the report at reportPath is relative
// to: the report's component, the root, then the report's directory.
func (m *mapper) bases(reportPath string) []string {
	var out []string
	add := func(dir string) {
		if dir != "" && !slices.Contains(out, dir) {
			out = append(out, dir)
		}
	}
	if owner, ok := m.set.Owner(reportPath); ok {
		add(owner)
	}
	add(".")
	if !isAbsolute(reportPath) {
		add(path.Dir(reportPath))
	}
	return out
}

// lineHits is the merged line coverage of one file: hits per line.
type lineHits map[int]int64

func sortedComponents[T any](byComponent map[string]*T, list *[]T) {
	ids := make([]string, 0, len(byComponent))
	for id := range byComponent {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		*list = append(*list, *byComponent[id])
	}
}

func (r *Result) warn(code, field, format string, args ...any) {
	r.Diagnostics = append(r.Diagnostics, diag.Warningf(code, field, format, args...))
}
