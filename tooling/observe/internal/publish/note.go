package publish

import (
	"fmt"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/analyze"
	"go.putnami.dev/tooling/observe/internal/observe"
)

const (
	// maxDescription is GitHub's limit on a commit status description.
	maxDescription = 140
	// maxNoteBytes keeps the note under GitHub's 65 536-character limit.
	maxNoteBytes = 60000
	// maxRows caps each table of the note; maxFailing caps the failing tests
	// it lists.
	maxRows    = 30
	maxFailing = 20
	// maxCell caps one table cell or quoted message.
	maxCell = 200
)

// Description is the one-line summary of the commit status: the changed and
// dependent components, the changed-line coverage and the dependency
// changes, at most 140 characters. Without a base nothing was measured, and
// it says so.
func Description(o observe.Observation) string {
	if o.Subject.BaseSHA == "" {
		return "the base of the change was not resolved, so no change was measured; see the job log"
	}
	a := o.Analysis
	changed, dependent := 0, 0
	for _, c := range a.Components {
		switch c.Impact {
		case evidence.ImpactChanged:
			changed++
		case evidence.ImpactDependent:
			dependent++
		}
	}
	lines := "changed lines not measured"
	if c := a.ChangedLineCoverage; c != nil && c.TotalLines > 0 {
		lines = "changed lines " + percent(c.CoveredLines, c.TotalLines) + " covered"
	}
	// Three counts of at most 19 digits keep the line under maxDescription.
	return fmt.Sprintf("%d changed, %d dependent components; %s; %s",
		changed, dependent, lines, count(len(a.DependencyChanges), "dependency change"))
}

// Note is the Markdown summary note. Its first line is Marker.
func Note(o observe.Observation) string {
	var b strings.Builder
	fmt.Fprintln(&b, Marker)
	fmt.Fprintln(&b, "### putnami observe")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "**%s**\n\n", Description(o))
	head := "Commit " + code(short(o.Subject.SHA))
	if o.Subject.BaseSHA != "" {
		head += ", measured from " + code(short(o.Subject.BaseSHA))
	}
	if page := runURL(o.CI.URL); page != "" {
		head += fmt.Sprintf(", [CI run](%s)", page)
	}
	fmt.Fprintf(&b, "%s.\n", head)
	writeComponents(&b, o.Analysis)
	writeFailing(&b, o.Analysis)
	writeChangedLines(&b, o.Analysis.ChangedLineCoverage)
	writeDependencies(&b, o.Analysis.DependencyChanges)
	writeDiagnostics(&b, o.Diagnostics)
	return bounded(b.String())
}

// writeComponents writes the changed and dependent components with their
// tests and coverage.
func writeComponents(b *strings.Builder, a analyze.Result) {
	var rows []evidence.Component
	for _, impact := range []string{evidence.ImpactChanged, evidence.ImpactDependent} {
		for _, c := range a.Components {
			if c.Impact == impact {
				rows = append(rows, c)
			}
		}
	}
	if len(rows) == 0 {
		fmt.Fprint(b, "\nNo component holds a changed file.\n")
		return
	}
	tests := map[string]evidence.ComponentTests{}
	for _, t := range a.Tests {
		tests[t.Component] = t
	}
	coverage := map[string]evidence.ComponentCoverage{}
	for _, c := range a.Coverage {
		coverage[c.Component] = c
	}
	fmt.Fprint(b, "\n| Component | Impact | Tests | Coverage |\n|---|---|---|---|\n")
	for i, c := range rows {
		if i == maxRows {
			fmt.Fprintf(b, "\n... and %d more components.\n", len(rows)-i)
			break
		}
		testCell := "no tests"
		if t, ok := tests[c.ID]; ok {
			testCell = fmt.Sprintf("%d passed, %d failed, %d skipped", t.Passed, t.Failed, t.Skipped)
		}
		coverageCell := "no coverage"
		if cov, ok := coverage[c.ID]; ok {
			coverageCell = fmt.Sprintf("%s of %d lines", percent(cov.CoveredLines, cov.TotalLines), cov.TotalLines)
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", cell(code(c.ID)), c.Impact, testCell, coverageCell)
	}
}

// writeFailing lists the failing test cases of every component.
func writeFailing(b *strings.Builder, a analyze.Result) {
	var lines []string
	total := 0
	for _, t := range a.Tests {
		for _, f := range t.FailingCases {
			total++
			if len(lines) == maxFailing {
				continue
			}
			name := f.Name
			if f.Suite != "" {
				name = f.Suite + " > " + name
			}
			line := "- " + code(name)
			if message, _, _ := strings.Cut(f.Message, "\n"); strings.TrimSpace(message) != "" {
				line += ": " + code(message)
			}
			lines = append(lines, line)
		}
	}
	if total == 0 {
		return
	}
	fmt.Fprintf(b, "\n**Failing tests** (%d)\n\n%s\n", total, strings.Join(lines, "\n"))
	if total > len(lines) {
		fmt.Fprintf(b, "- ... and %d more\n", total-len(lines))
	}
}

// writeChangedLines writes the files whose changed lines are the least
// covered.
func writeChangedLines(b *strings.Builder, c *evidence.ChangedLineCoverage) {
	if c == nil || len(c.Files) == 0 {
		return
	}
	files := slices.Clone(c.Files)
	slices.SortStableFunc(files, func(x, y evidence.ChangedLineCoverageFile) int {
		return x.CoveredLines*max(y.TotalLines, 1) - y.CoveredLines*max(x.TotalLines, 1)
	})
	fmt.Fprintf(b, "\n**Changed lines**: %d of %d covered (%s)\n\n", c.CoveredLines, c.TotalLines, percent(c.CoveredLines, c.TotalLines))
	fmt.Fprint(b, "| File | Covered | Uncovered lines |\n|---|---|---|\n")
	for i, f := range files {
		if i == maxRows {
			fmt.Fprintf(b, "\n... and %d more files.\n", len(files)-i)
			break
		}
		uncovered := lineList(f.UncoveredLines)
		if uncovered == "" {
			uncovered = "none"
		}
		fmt.Fprintf(b, "| %s | %d of %d | %s |\n", cell(code(f.Path)), f.CoveredLines, f.TotalLines, cell(uncovered))
	}
}

func writeDependencies(b *strings.Builder, changes []evidence.DependencyChange) {
	if len(changes) == 0 {
		return
	}
	fmt.Fprintf(b, "\n**Dependency changes** (%d)\n\n| Package | Change | Version | Lockfile |\n|---|---|---|---|\n", len(changes))
	for i, d := range changes {
		if i == maxRows {
			fmt.Fprintf(b, "\n... and %d more dependency changes.\n", len(changes)-i)
			break
		}
		version := d.From + " → " + d.To
		switch d.Change {
		case evidence.DependencyAdded:
			version = d.To
		case evidence.DependencyRemoved:
			version = d.From
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", cell(code(d.Name)), d.Change, cell(code(version)), cell(code(d.Lockfile)))
	}
}

// writeDiagnostics counts the diagnostics and names their codes; the job log
// holds their messages.
func writeDiagnostics(b *strings.Builder, diags []diag.Diagnostic) {
	if len(diags) == 0 {
		return
	}
	var codes []string
	for _, d := range diags {
		if !slices.Contains(codes, d.Code) {
			codes = append(codes, d.Code)
		}
	}
	quoted := make([]string, len(codes))
	for i, c := range codes {
		quoted[i] = code(c)
	}
	fmt.Fprintf(b, "\n%s in the job log: %s.\n", count(len(diags), "diagnostic"), strings.Join(quoted, ", "))
}

// bounded cuts a note longer than maxNoteBytes at a line boundary.
func bounded(note string) string {
	if len(note) <= maxNoteBytes {
		return note
	}
	cut := strings.LastIndexByte(note[:maxNoteBytes], '\n')
	return note[:cut+1] + "\n... the summary is cut here; the job log holds the rest.\n"
}

// code quotes text as a Markdown code span on one line, so a name taken from
// the repository renders as text, never as Markdown or HTML.
func code(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	text = strings.ReplaceAll(text, "`", "'")
	if runes := []rune(text); len(runes) > maxCell {
		text = string(runes[:maxCell]) + "..."
	}
	if text == "" {
		return ""
	}
	return "`" + text + "`"
}

// cell escapes the pipe that would end a table cell.
func cell(text string) string {
	return strings.ReplaceAll(text, "|", `\|`)
}

func percent(covered, total int) string {
	if total == 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", float64(covered)*100/float64(total))
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
