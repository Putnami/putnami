package readers

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Architecture rule statuses.
const (
	RuleKept   = "kept"
	RuleBroken = "broken"
)

// ArchitectureReport is the result of one architecture checker run.
type ArchitectureReport struct {
	// Rules are the rules the checker evaluated, in report order.
	Rules []ArchitectureRule `json:"rules"`
}

// ArchitectureRule is one rule a checker evaluated.
type ArchitectureRule struct {
	// Name is the rule as the checker names it: an ArchUnit rule text, a
	// deptrac layer, an import-linter contract.
	Name string `json:"name"`
	// Status is RuleKept or RuleBroken.
	Status string `json:"status"`
	// Count is the number of violations the checker reported: 0 for a kept
	// rule, at least 1 and at least len(Violations) for a broken one.
	Count int `json:"count"`
	// Violations are the violations the report details, in report order.
	Violations []Violation `json:"violations,omitempty"`
}

// Violation is one violation of a rule, as the checker states it.
type Violation struct {
	// Message is the violation's text.
	Message string `json:"message"`
	// Subject is the class or module that violates the rule, as the checker
	// names it: a Java class, a PHP class, a Python module.
	Subject string `json:"subject,omitempty"`
	// File is the source file the checker names, as it names it: ArchUnit
	// states a file name alone.
	File string `json:"file,omitempty"`
	// Line is the line the checker names, from 1; 0 when it names none.
	Line int `json:"line,omitempty"`
}

// archUnitPackage is the package every ArchUnit rule imports.
const archUnitPackage = "com.tngtech.archunit"

// UsesArchUnit reports whether a test source file imports ArchUnit.
func UsesArchUnit(source []byte) bool {
	return bytes.Contains(source, []byte(archUnitPackage))
}

var (
	// archUnitRule is the head line ArchUnit writes for a violated rule:
	// Architecture Violation [Priority: MEDIUM] - Rule '...' was violated (2 times):
	archUnitRule = regexp.MustCompile(`Rule '(.*)' was violated \((\d+) times?\)`)
	// archUnitLocation is the source location ArchUnit ends a violation with.
	archUnitLocation = regexp.MustCompile(`\(([^()\s:]+\.[A-Za-z]+):(\d+)\)\s*$`)
	// archUnitSubject is the first code unit a violation names.
	archUnitSubject = regexp.MustCompile(`<([^<>]+)>`)
)

// ArchUnitRule reads a JUnit case whose test runs ArchUnit rules as one rule.
// A passing case is a kept rule named by the case; a skipped case is none.
// A failing case is a broken rule named by the rule its message states, else
// by the case: each line after the first of the message is one violation.
func ArchUnitRule(c TestCase) (ArchitectureRule, bool) {
	switch c.Status {
	case StatusPassed:
		return ArchitectureRule{Name: c.Name, Status: RuleKept}, true
	case StatusFailed:
	default:
		return ArchitectureRule{}, false
	}
	if c.Failure == nil {
		return archUnitFailure(c.Name, ""), true
	}
	// Surefire may cut a long message; its body holds the whole failure.
	rule := archUnitFailure(c.Name, c.Failure.Message)
	if body := archUnitFailure(c.Name, c.Failure.Body); len(body.Violations) > len(rule.Violations) {
		rule = body
	}
	return rule, true
}

// archUnitFailure reads the text of a failure as a broken rule.
func archUnitFailure(name, text string) ArchitectureRule {
	rule := ArchitectureRule{Name: name, Status: RuleBroken}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	head := 0
	for head < len(lines) && !archUnitRule.MatchString(lines[head]) {
		head++
	}
	if head == len(lines) {
		// A failure that states no rule, such as a rule that checked no
		// class, is one violation.
		message := strings.TrimSpace(lines[0])
		if message == "" {
			message = "the rule failed"
		}
		rule.Count = 1
		rule.Violations = []Violation{{Message: message}}
		return rule
	}
	match := archUnitRule.FindStringSubmatch(lines[head])
	if match[1] != "" {
		rule.Name = match[1]
	}
	rule.Count, _ = strconv.Atoi(match[2])
	for _, line := range lines[head+1:] {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "at ") {
			// A stack trace follows the violations in a failure body.
			break
		}
		if line != "" {
			rule.Violations = append(rule.Violations, archUnitViolation(line))
		}
	}
	rule.Count = max(rule.Count, len(rule.Violations), 1)
	return rule
}

// archUnitViolation reads one violation line: its first code unit, cut
// before its member, as the subject, and its source location.
func archUnitViolation(line string) Violation {
	v := Violation{Message: line}
	file := ""
	if match := archUnitLocation.FindStringSubmatch(line); match != nil {
		file = match[1]
		v.File = file
		v.Line, _ = strconv.Atoi(match[2])
	}
	if match := archUnitSubject.FindStringSubmatch(line); match != nil {
		v.Subject = archUnitClass(match[1], file)
	}
	return v
}

// archUnitClass cuts a code unit (com.acme.Quotes.get(), com.acme.Quotes$1,
// com.acme.Quotes.rates) to its top-level class: the segment the file name
// names, else the last one that starts with an upper-case letter.
func archUnitClass(unit, file string) string {
	unit, _, _ = strings.Cut(unit, "(")
	segments := strings.Split(unit, ".")
	stem, _, _ := strings.Cut(file, ".")
	cut := -1
	for i, segment := range segments {
		segment, _, _ = strings.Cut(segment, "$")
		segments[i] = segment
		if stem != "" && segment == stem {
			cut = i
			break
		}
		if segment != "" && segment[0] >= 'A' && segment[0] <= 'Z' {
			cut = i
		}
	}
	if cut < 0 {
		return unit
	}
	return strings.Join(segments[:cut+1], ".")
}

// deptracSkippedSuite is the suite deptrac writes for the skip rules that
// matched no violation: it is no layer.
const deptracSkippedSuite = "Unmatched skipped violations"

// deptracFailure is the message deptrac writes for one violation:
// Depender:N must not depend on Dependent (LayerA on LayerB).
var deptracFailure = regexp.MustCompile(`^(.+?):(\d+) must not depend on (.+)$`)

type deptracNode struct {
	XMLName  xml.Name
	Name     string        `xml:"name,attr"`
	Message  string        `xml:"message,attr"`
	Children []deptracNode `xml:",any"`
}

// ReadDeptrac reads the report of deptrac --formatter=junit: one rule per
// layer testsuite, kept when none of its cases fails, with one violation per
// failure element.
func ReadDeptrac(data []byte) (ArchitectureReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return ArchitectureReport{}, diagnostics
	}
	var root deptracNode
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&root); err != nil {
		return ArchitectureReport{}, malformed("deptrac report does not decode: %v", err)
	}
	if root.XMLName.Local != "testsuites" {
		return ArchitectureReport{}, malformed("deptrac report root is %q, not testsuites", root.XMLName.Local)
	}
	report := ArchitectureReport{Rules: []ArchitectureRule{}}
	for _, suite := range root.Children {
		if suite.XMLName.Local != "testsuite" || suite.Name == deptracSkippedSuite {
			continue
		}
		rule := ArchitectureRule{Name: suite.Name, Status: RuleKept}
		for _, testcase := range suite.Children {
			if testcase.XMLName.Local != "testcase" {
				continue
			}
			for _, outcome := range testcase.Children {
				if outcome.XMLName.Local != "failure" {
					continue
				}
				v := Violation{Message: outcome.Message}
				if match := deptracFailure.FindStringSubmatch(outcome.Message); match != nil {
					v.Subject = match[1]
					v.Line, _ = strconv.Atoi(match[2])
					v.Message = match[1] + " must not depend on " + match[3]
				}
				rule.Violations = append(rule.Violations, v)
			}
		}
		if len(rule.Violations) > 0 {
			rule.Status, rule.Count = RuleBroken, len(rule.Violations)
		}
		report.Rules = append(report.Rules, rule)
	}
	return report, nil
}

var (
	// ansiEscape is a terminal color or style sequence.
	ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
	// importLinterAnalyzed is the line lint-imports prints before its
	// contracts.
	importLinterAnalyzed = regexp.MustCompile(`^Analyzed \d+ files, \d+ dependencies\.$`)
	// importLinterContract is one contract result line, with its optional
	// ignored-import and warning counts and duration.
	importLinterContract = regexp.MustCompile(`^(.+) (KEPT|BROKEN)( \([^()]*\))?( \[[0-9.]+m?s\])?$`)
	// importLinterSummary closes the contract lines.
	importLinterSummary = regexp.MustCompile(`^Contracts: \d+ kept, \d+ broken\.$`)
	// importLinterImport is one import line of a chain: a dash opens the
	// chain, an ampersand adds an importer or an imported module of the same
	// step, and the step is importer -> imported (l.3, l.7), or a module and
	// its lines when the next line names where it leads.
	importLinterImport = regexp.MustCompile(`^(-\s+|&\s+)?(\S+)(?: -> (\S+))? \(((?:l\.(?:\d+|\?))(?:, l\.(?:\d+|\?))*)\)$`)
	importLinterLine   = regexp.MustCompile(`l\.(\d+)`)
	// headingRule is the underline of a heading.
	headingRule = regexp.MustCompile(`^-+$`)
)

// ReadImportLinter reads the text lint-imports prints: one rule per contract
// line, and under Broken contracts one violation per import chain.
func ReadImportLinter(data []byte) (ArchitectureReport, []diag.Diagnostic) {
	if diagnostics := tooLarge(data); diagnostics != nil {
		return ArchitectureReport{}, diagnostics
	}
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), MaxReportBytes)
	for scanner.Scan() {
		lines = append(lines, strings.TrimRight(ansiEscape.ReplaceAllString(scanner.Text(), ""), " \t\r"))
	}
	if scanner.Err() != nil {
		return ArchitectureReport{}, malformed("import-linter output cannot be read: %v", scanner.Err())
	}
	start := -1
	for i, line := range lines {
		if importLinterAnalyzed.MatchString(line) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ArchitectureReport{}, malformed("import-linter output has no Analyzed N files, M dependencies line")
	}
	report := ArchitectureReport{Rules: []ArchitectureRule{}}
	index := map[string]int{}
	i := start
	for ; i < len(lines); i++ {
		line := lines[i]
		if importLinterSummary.MatchString(line) {
			break
		}
		match := importLinterContract.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		rule := ArchitectureRule{Name: match[1], Status: RuleKept}
		if match[2] == "BROKEN" {
			rule.Status, rule.Count = RuleBroken, 1
		}
		index[rule.Name] = len(report.Rules)
		report.Rules = append(report.Rules, rule)
	}
	if len(report.Rules) == 0 {
		return ArchitectureReport{}, malformed("import-linter output lists no contract")
	}
	// The details: a contract heading, its name over a line of dashes, then
	// its chains, each a first import line and its indented continuation.
	current := -1
	chain := -1
	for ; i < len(lines); i++ {
		line := lines[i]
		if i+1 < len(lines) && headingRule.MatchString(lines[i+1]) && len(lines[i+1]) == len(line) {
			current, chain = -1, -1
			if at, ok := index[line]; ok && report.Rules[at].Status == RuleBroken {
				current = at
			}
			i++
			continue
		}
		if current < 0 {
			continue
		}
		trimmed := strings.TrimSpace(line)
		match := importLinterImport.FindStringSubmatch(trimmed)
		if match == nil {
			if trimmed == "" {
				chain = -1
			}
			continue
		}
		rule := &report.Rules[current]
		step := match[2] + " (" + match[4] + ")"
		if match[3] != "" {
			step = match[2] + " -> " + match[3] + " (" + match[4] + ")"
		}
		if strings.HasPrefix(match[1], "-") || chain < 0 {
			v := Violation{Message: step, Subject: match[2]}
			if number := importLinterLine.FindStringSubmatch(match[4]); number != nil {
				v.Line, _ = strconv.Atoi(number[1])
			}
			rule.Violations = append(rule.Violations, v)
			chain = len(rule.Violations) - 1
			continue
		}
		v := &rule.Violations[chain]
		if strings.HasPrefix(match[1], "&") {
			step = "& " + step
		}
		v.Message += ", " + step
	}
	for at := range report.Rules {
		if rule := &report.Rules[at]; rule.Status == RuleBroken {
			rule.Count = max(1, len(rule.Violations))
		}
	}
	return report, nil
}
