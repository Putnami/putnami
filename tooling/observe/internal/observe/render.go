package observe

import (
	"fmt"
	"slices"
	"strings"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

// maxListedComponents caps the components and dependency changes the text
// summary lists; maxListedLineFiles caps the changed-line coverage files.
const (
	maxListedComponents = 30
	maxListedLineFiles  = 10
)

// writeAnalysis writes the changed and dependent components, the tests and
// coverage per component, the changed-line coverage and the dependency
// changes.
func writeAnalysis(b *strings.Builder, a analyze.Result) {
	byImpact := map[string][]evidence.Component{}
	for _, c := range a.Components {
		byImpact[c.Impact] = append(byImpact[c.Impact], c)
	}
	changed, dependent := byImpact[evidence.ImpactChanged], byImpact[evidence.ImpactDependent]
	fmt.Fprintf(b, "  components  %d changed, %d dependent, %d unaffected\n", len(changed), len(dependent), len(byImpact[evidence.ImpactUnaffected]))
	listed := 0
	for _, c := range slices.Concat(changed, dependent) {
		if listed == maxListedComponents {
			fmt.Fprintf(b, "    ... and %d more\n", len(changed)+len(dependent)-listed)
			break
		}
		listed++
		line := fmt.Sprintf("    %-9s %s", c.Impact, componentLabel(c))
		if c.Impact == evidence.ImpactDependent {
			line += ", depends on " + strings.Join(c.DependsOn, ", ")
		}
		fmt.Fprintln(b, line)
	}
	writeTests(b, a)
	writeChangedLines(b, a.ChangedLineCoverage)
	fmt.Fprintf(b, "  dependencies  %s\n", count(len(a.DependencyChanges), "change"))
	for i, d := range a.DependencyChanges {
		if i == maxListedComponents {
			fmt.Fprintf(b, "    ... and %d more\n", len(a.DependencyChanges)-i)
			break
		}
		fmt.Fprintf(b, "    %-10s %s %s, in %s\n", d.Change, d.Name, versions(d), d.Lockfile)
	}
}

func componentLabel(c evidence.Component) string {
	label := c.ID
	if c.Name != "" && c.Name != c.ID {
		label += " (" + c.Name + ")"
	}
	return label + " [" + c.Ecosystem + ", " + c.Source + "]"
}

func versions(d evidence.DependencyChange) string {
	switch d.Change {
	case evidence.DependencyAdded:
		return d.To
	case evidence.DependencyRemoved:
		return d.From
	}
	return d.From + " -> " + d.To
}

// writeTests writes one line per component that has tests or coverage,
// changed and dependent components first.
func writeTests(b *strings.Builder, a analyze.Result) {
	tests := map[string]evidence.ComponentTests{}
	for _, t := range a.Tests {
		tests[t.Component] = t
	}
	coverage := map[string]evidence.ComponentCoverage{}
	for _, c := range a.Coverage {
		coverage[c.Component] = c
	}
	var order []string
	for _, impact := range []string{evidence.ImpactChanged, evidence.ImpactDependent, evidence.ImpactUnaffected} {
		for _, c := range a.Components {
			_, hasTests := tests[c.ID]
			_, hasCoverage := coverage[c.ID]
			if c.Impact == impact && (hasTests || hasCoverage) {
				order = append(order, c.ID)
			}
		}
	}
	if len(order) == 0 {
		fmt.Fprintln(b, "  tests       no test or coverage report maps to a component")
		return
	}
	fmt.Fprintln(b, "  tests and coverage per component")
	width := 0
	for _, id := range order {
		width = max(width, len(id))
	}
	for i, id := range order {
		if i == maxListedComponents {
			fmt.Fprintf(b, "    ... and %d more\n", len(order)-i)
			break
		}
		parts := []string{}
		if t, ok := tests[id]; ok {
			parts = append(parts, fmt.Sprintf("%d passed, %d failed, %d skipped", t.Passed, t.Failed, t.Skipped))
		} else {
			parts = append(parts, "no tests")
		}
		if c, ok := coverage[id]; ok {
			parts = append(parts, "coverage "+ratio(c.CoveredLines, c.TotalLines))
		} else {
			parts = append(parts, "no coverage")
		}
		fmt.Fprintf(b, "    %-*s  %s\n", width, id, strings.Join(parts, ", "))
		if t, ok := tests[id]; ok {
			for _, f := range t.FailingCases {
				fmt.Fprintf(b, "      failed %s\n", failingLabel(f))
			}
		}
	}
}

func failingLabel(f evidence.FailingCase) string {
	label := f.Name
	if f.Suite != "" {
		label = f.Suite + " > " + label
	}
	if f.Message != "" {
		message, _, _ := strings.Cut(f.Message, "\n")
		label += ": " + message
	}
	return label
}

// writeChangedLines writes the changed-line coverage and its least covered
// files.
func writeChangedLines(b *strings.Builder, c *evidence.ChangedLineCoverage) {
	if c == nil {
		fmt.Fprintln(b, "  changed lines  no coverage of the changed lines: it needs a base and a coverage report")
		return
	}
	fmt.Fprintf(b, "  changed lines  %s covered\n", ratio(c.CoveredLines, c.TotalLines))
	files := slices.Clone(c.Files)
	slices.SortStableFunc(files, func(x, y evidence.ChangedLineCoverageFile) int {
		return (x.CoveredLines*max(y.TotalLines, 1) - y.CoveredLines*max(x.TotalLines, 1))
	})
	for i, f := range files {
		if i == maxListedLineFiles {
			fmt.Fprintf(b, "    ... and %d more\n", len(files)-i)
			break
		}
		line := fmt.Sprintf("    %s  %s", ratio(f.CoveredLines, f.TotalLines), f.Path)
		if len(f.UncoveredLines) > 0 {
			line += ", uncovered " + lineList(f.UncoveredLines)
		}
		fmt.Fprintln(b, line)
	}
}

func ratio(covered, total int) string {
	if total == 0 {
		return "0 of 0 lines"
	}
	return fmt.Sprintf("%d of %d lines (%.1f%%)", covered, total, float64(covered)*100/float64(total))
}

// lineList writes line numbers as ranges: 3-5, 9.
func lineList(lines []int) string {
	var parts []string
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", lines[i], lines[j]))
		} else {
			parts = append(parts, fmt.Sprint(lines[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}
