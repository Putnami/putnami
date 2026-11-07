package observe

import (
	"fmt"
	"slices"
	"strings"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

// maxListedComponents caps the components and dependency changes the text
// summary lists; maxListedLineFiles caps the changed-line coverage files;
// maxListedEdges caps the components one line names after "depends on".
const (
	maxListedComponents = 30
	maxListedLineFiles  = 10
	maxListedEdges      = 10
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
	edges := make(map[string][]string, len(a.Components))
	for _, c := range a.Components {
		edges[c.ID] = c.DependsOn
	}
	listed := 0
	for _, c := range slices.Concat(changed, dependent) {
		if listed == maxListedComponents {
			fmt.Fprintf(b, "    ... and %d more\n", len(changed)+len(dependent)-listed)
			break
		}
		listed++
		line := fmt.Sprintf("    %-9s %s", c.Impact, componentLabel(c))
		dependsOn := c.DependsOn
		if c.Impact == evidence.ImpactChanged {
			dependsOn = changedDependencies(a.Components, edges, c.ID)
		}
		if len(dependsOn) > maxListedEdges {
			line += ", depends on " + strings.Join(dependsOn[:maxListedEdges], ", ") + fmt.Sprintf(" and %d more", len(dependsOn)-maxListedEdges)
		} else if len(dependsOn) > 0 {
			line += ", depends on " + strings.Join(dependsOn, ", ")
		}
		fmt.Fprintln(b, line)
	}
	writeTests(b, a)
	writeChangedLines(b, a.ChangedLineCoverage)
	fmt.Fprintf(b, "  dependencies  %s%s\n", count(len(a.DependencyChanges), "change"), sbomLabel(a.SBOM))
	for i, d := range a.DependencyChanges {
		if i == maxListedComponents {
			fmt.Fprintf(b, "    ... and %d more\n", len(a.DependencyChanges)-i)
			break
		}
		fmt.Fprintf(b, "    %-10s %s %s, in %s\n", d.Change, d.Name, versions(d), d.Lockfile)
		if facts := dependencyFacts(d, a.SBOM); facts != "" {
			fmt.Fprintf(b, "               %s\n", facts)
		}
	}
}

// changedDependencies returns the changed components other than id that id
// reaches through the edges, directly or through other components, in the
// order of the components. A change that edits a component and one that
// depends on it marks both changed; the summary keeps the edge between them.
func changedDependencies(components []evidence.Component, edges map[string][]string, id string) []string {
	seen := map[string]bool{id: true}
	queue := slices.Clone(edges[id])
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if !seen[next] {
			seen[next] = true
			queue = append(queue, edges[next]...)
		}
	}
	var out []string
	for _, c := range components {
		if c.ID != id && c.Impact == evidence.ImpactChanged && seen[c.ID] {
			out = append(out, c.ID)
		}
	}
	return out
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

// maxListedVulnerabilities caps the vulnerability ids one summary line names.
const maxListedVulnerabilities = 5

// sbomLabel names the software bills of materials (SBOM) the dependency facts
// come from; empty when none was read.
func sbomLabel(sbom *evidence.SBOM) string {
	if sbom == nil {
		return ""
	}
	side := func(d *evidence.SBOMDocument) string {
		if d == nil {
			return "unknown"
		}
		if d.Format == evidence.SBOMCycloneDX {
			return "CycloneDX " + d.SpecVersion
		}
		return "SPDX " + d.SpecVersion
	}
	return ", SBOM head " + side(sbom.Head) + ", base " + side(sbom.Base)
}

// dependencyFacts sums up what the SBOMs say of one dependency change: the
// vulnerabilities it fixes, introduces and keeps, and its licenses. A side
// without an SBOM is unknown; a package the SBOM of a side does not list is
// said to be so.
func dependencyFacts(d evidence.DependencyChange, sbom *evidence.SBOM) string {
	if sbom == nil {
		return ""
	}
	baseKnown := sbom.Base != nil && d.Change != evidence.DependencyAdded
	headKnown := sbom.Head != nil && d.Change != evidence.DependencyRemoved
	var parts []string
	if baseKnown && d.Before == nil {
		parts = append(parts, "not listed in the base SBOM")
		baseKnown = false
	}
	if headKnown && d.After == nil {
		parts = append(parts, "not listed in the head SBOM")
		headKnown = false
	}
	switch {
	case baseKnown && headKnown:
		fixed, introduced, kept := vulnerabilityDiff(d.Before.Vulnerabilities, d.After.Vulnerabilities)
		parts = append(parts, "fixes "+vulnerabilityList(fixed)+", introduces "+vulnerabilityList(introduced)+fmt.Sprintf(", keeps %d", len(kept)))
	case d.Change == evidence.DependencyAdded && headKnown:
		parts = append(parts, "introduces "+vulnerabilityList(d.After.Vulnerabilities))
	case d.Change == evidence.DependencyRemoved && baseKnown:
		parts = append(parts, "fixes "+vulnerabilityList(d.Before.Vulnerabilities))
	case headKnown:
		parts = append(parts, vulnerabilityList(d.After.Vulnerabilities)+" known at the head, base unknown")
	case baseKnown:
		parts = append(parts, vulnerabilityList(d.Before.Vulnerabilities)+" known at the base, head unknown")
	case len(parts) > 0:
	case d.Change == evidence.DependencyRemoved:
		parts = append(parts, "base unknown")
	default:
		parts = append(parts, "head unknown")
	}
	if licenses := licenseChange(d); licenses != "" {
		parts = append(parts, licenses)
	}
	return strings.Join(parts, "; ")
}

// vulnerabilityDiff splits the vulnerabilities of two states by id: the ones
// only before, only after, and in both.
func vulnerabilityDiff(before, after []evidence.Vulnerability) (fixed, introduced, kept []evidence.Vulnerability) {
	has := func(list []evidence.Vulnerability, id string) bool {
		return slices.ContainsFunc(list, func(v evidence.Vulnerability) bool { return v.ID == id })
	}
	for _, v := range before {
		if has(after, v.ID) {
			kept = append(kept, v)
		} else {
			fixed = append(fixed, v)
		}
	}
	for _, v := range after {
		if !has(before, v.ID) {
			introduced = append(introduced, v)
		}
	}
	return fixed, introduced, kept
}

// vulnerabilityList counts vulnerabilities and names the first ones with
// their severity.
func vulnerabilityList(list []evidence.Vulnerability) string {
	if len(list) == 0 {
		return "0"
	}
	var names []string
	for i, v := range list {
		if i == maxListedVulnerabilities {
			names = append(names, fmt.Sprintf("and %d more", len(list)-i))
			break
		}
		names = append(names, v.ID+" "+v.Severity)
	}
	return fmt.Sprintf("%d (%s)", len(list), strings.Join(names, ", "))
}

// licenseChange names the licenses of a dependency, before -> after when
// both sides are known and differ; a side that lists none reads "none".
func licenseChange(d evidence.DependencyChange) string {
	var before, after string
	if d.Before != nil {
		before = strings.Join(d.Before.Licenses, ", ")
	}
	if d.After != nil {
		after = strings.Join(d.After.Licenses, ", ")
	}
	switch {
	case before == after:
		if before == "" {
			return ""
		}
		return "license " + after
	case d.Before == nil:
		return "license " + after
	case d.After == nil:
		return "license " + before
	}
	return "license " + orNone(before) + " -> " + orNone(after)
}

func orNone(licenses string) string {
	if licenses == "" {
		return "none"
	}
	return licenses
}

// writeDecisions writes the decision records the change concerns: status,
// path and title, then the changed files each governs and its own change.
// A change that concerns none writes nothing.
func writeDecisions(b *strings.Builder, list []evidence.Decision) {
	if len(list) == 0 {
		return
	}
	fmt.Fprintf(b, "  decisions  %s\n", count(len(list), "decision"))
	for i, d := range list {
		if i == maxListedComponents {
			fmt.Fprintf(b, "    ... and %d more\n", len(list)-i)
			break
		}
		var notes []string
		if len(d.Governs) > 0 {
			notes = append(notes, "governs "+count(len(d.Governs), "changed file"))
		}
		if d.Change != "" {
			notes = append(notes, d.Change+" by the change")
		}
		fmt.Fprintf(b, "    %-10s %s: %s (%s)\n", d.Status, d.Path, d.Title, strings.Join(notes, ", "))
	}
}
