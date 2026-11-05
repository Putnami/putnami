package features

import (
	"path"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// specKitFileName is the one file of a Spec Kit feature directory that holds
// the spec. plan.md, tasks.md, research.md, data-model.md and contracts/ are
// process state.
const specKitFileName = "spec.md"

type specKitSection int

const (
	specKitNoSection specKitSection = iota
	specKitSuccessCriteria
	specKitOutOfScope
	specKitRequirements
	specKitUserStory
	specKitKeyEntities
)

var (
	specKitHeading     = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	specKitFence       = regexp.MustCompile("^[ \\t]*(`{3,}|~{3,})")
	specKitBreak       = regexp.MustCompile(`^ {0,3}(?:-{3,}|\*{3,}|_{3,})[ \t]*$`)
	specKitListItem    = regexp.MustCompile(`^[ \t]*(?:[-*+]|[0-9]+[.)])[ \t]+(.*)$`)
	specKitLabel       = regexp.MustCompile(`^\*\*((?:FR|SC)-[0-9]+[A-Za-z]?)(?::\*\*|\*\*[ \t]*:?)[ \t]*(.*)$`)
	specKitLooseLabel  = regexp.MustCompile(`(?i)^(?:\*\*FR-[0-9]+[A-Za-z]?(?:\*\*|:)|FR-[0-9]+[A-Za-z]?:)`)
	specKitScenario    = regexp.MustCompile(`(?i)^(?:\*\*)?given\b`)
	specKitMetadata    = regexp.MustCompile(`^\*\*(Feature Branch|Created|Status)\*\*:`)
	specKitMarker      = regexp.MustCompile(`\[NEEDS CLARIFICATION(?::[^\]]*)?\]`)
	specKitInlineCode  = regexp.MustCompile("`[^`]*`")
	specKitHTMLComment = regexp.MustCompile(`^\s*<!--`)
)

// specKitReader projects the spec.md of one GitHub Spec Kit feature.
//
// A Spec Kit scenario sits under a user story and nothing ties a story to a
// requirement, so the scenarios belong to the file.
type specKitReader struct{}

// Read implements SpecReader.
func (specKitReader) Read(input SpecInput) SpecReading {
	file, found := specKitFile(input)
	if !found {
		return SpecReading{Findings: []diag.Diagnostic{diag.Errorf(ErrorCodeSpecRead, input.Reference.Path,
			"a speckit reference names a feature directory or its %s, and this one holds none", specKitFileName)}}
	}
	scan := specKitScan{
		file:    file.Path,
		reading: SpecReading{Spec: &Spec{}, Scenarios: []SpecScenario{}},
	}
	text := strings.TrimPrefix(string(file.Data), "\ufeff")
	for index, line := range strings.Split(text, "\n") {
		scan.line(strings.TrimRight(line, "\r"), index+1)
	}
	scan.finish()
	if scan.reading.Spec == nil {
		return scan.reading
	}
	for i := range scan.reading.Scenarios {
		if scenario := &scan.reading.Scenarios[i]; scenario.Check == "" {
			scenario.Check, _ = NormalizeRequirementName(scenario.Name)
		}
	}
	return scan.reading
}

func specKitFile(input SpecInput) (SpecFile, bool) {
	inDirectory := input.Reference.Path + "/" + specKitFileName
	for _, file := range input.Files {
		if file.Path == inDirectory || (file.Path == input.Reference.Path && path.Base(file.Path) == specKitFileName) {
			return file, true
		}
	}
	return SpecFile{}, false
}

// specKitAnchor is an anchor waiting for the line below it.
type specKitAnchor struct {
	id       string
	position SpecPosition
}

// specKitScan is the state of one pass over a spec.md.
type specKitScan struct {
	file    string
	reading SpecReading

	inComment       bool
	commentStart    SpecPosition
	fence           string
	fenceStart      SpecPosition
	section         specKitSection
	sectionLevel    int
	criteriaHeading SpecPosition
	pastHeader      bool
	afterBlank      bool
	anchor          *specKitAnchor
	// extend appends a continuation line to the list item above, nil when the
	// line above ended it.
	extend func(text string)
}

func (scan *specKitScan) line(raw string, number int) {
	position := SpecPosition{File: scan.file, Line: number}
	if scan.fenced(raw, position) {
		scan.endItem()
		return
	}
	if !scan.inComment && specKitHTMLComment.MatchString(raw) {
		if id, isAnchor, err := ParseSpecAnchor(raw); isAnchor {
			scan.settleAnchor()
			scan.endItem()
			if err != nil {
				scan.fail(position, "%v", err)
				return
			}
			scan.anchor = &specKitAnchor{id: id, position: position}
			return
		}
	}
	text := scan.stripComments(raw, position)
	if strings.TrimSpace(text) == "" {
		scan.blank()
		return
	}
	indented := specKitIndented(text)
	if scan.afterBlank && !indented {
		scan.endItem()
	}
	scan.afterBlank = false
	for _, marker := range specKitMarker.FindAllString(specKitInlineCode.ReplaceAllString(text, ""), -1) {
		scan.reading.EnforceFindings = append(scan.reading.EnforceFindings, diag.Warningf(FindingCodeSpecNeedsClarification, position.Field(),
			"%s is open: answer it before this project can enforce specs", marker))
	}
	if heading := specKitHeading.FindStringSubmatch(text); heading != nil {
		scan.settleAnchor()
		scan.endItem()
		scan.heading(len(heading[1]), heading[2], position)
		return
	}
	if !scan.pastHeader {
		if match := specKitMetadata.FindStringSubmatch(text); match != nil {
			scan.derivedFact(position, match[1])
		}
	}
	item := specKitListItem.FindStringSubmatch(text)
	switch {
	case scan.section == specKitRequirements && scan.requirement(text, position):
	case scan.section == specKitSuccessCriteria && scan.outcome(text, position):
	case scan.section == specKitUserStory && item != nil && specKitScenario.MatchString(item[1]):
		scan.scenario(item[1], position)
	case item != nil && indented && scan.extend != nil:
		scan.settleAnchor()
		scan.extend(strings.TrimSpace(text))
	case scan.section == specKitOutOfScope && item != nil:
		scan.ignoredLabel(text, item, position, "this requirement reads as a non-goal here: move it under Functional Requirements")
		scan.nonGoal(item[1], position)
	default:
		scan.settleAnchor()
		scan.ignoredLabel(text, item, position, "this requirement projects nothing: write it as **FR-xxx**: under Functional Requirements")
		if item != nil || specKitBreak.MatchString(text) {
			scan.endItem()
		} else if scan.extend != nil {
			scan.extend(strings.TrimSpace(text))
		}
	}
}

// blank handles an empty line: it settles an anchor, and leaves a list item open
// for an indented continuation.
func (scan *specKitScan) blank() {
	scan.settleAnchor()
	scan.afterBlank = true
}

// specKitIndented reports whether a line starts with white space, which keeps
// it inside the list item above it.
func specKitIndented(text string) bool {
	return text[0] == ' ' || text[0] == '\t'
}

// ignoredLabel warns on a line that looks like a requirement and is not read as
// one, because the requirement would otherwise vanish in silence.
func (scan *specKitScan) ignoredLabel(text string, item []string, position SpecPosition, message string) {
	body := strings.TrimSpace(text)
	if item != nil {
		body = item[1]
	}
	if specKitLooseLabel.MatchString(body) {
		scan.reading.Findings = append(scan.reading.Findings, diag.Warningf(WarningCodeSpecRequirementIgnored, position.Field(), "%s", message))
	}
}

// finish refuses what would otherwise hide text in silence: an anchor still
// waiting, a comment or a fence that never closes, and a spec with no outcome.
func (scan *specKitScan) finish() {
	scan.settleAnchor()
	if scan.inComment {
		scan.fail(scan.commentStart, "this HTML comment never closes, and it hides the rest of the file")
	}
	if scan.fence != "" {
		scan.fail(scan.fenceStart, "this code fence never closes, and it hides the rest of the file")
	}
	if len(scan.reading.Spec.Outcomes) == 0 {
		position := scan.criteriaHeading
		if position.File == "" {
			position = SpecPosition{File: scan.file, Line: 1}
		}
		scan.fail(position, "a Spec Kit spec needs a **SC-xxx** item under Success Criteria: no outcome was found")
		scan.reading.Spec = nil
	}
}

// fenced reports whether the line opens, closes or lies inside a code fence. A
// comment that is still open hides a fence.
func (scan *specKitScan) fenced(raw string, position SpecPosition) bool {
	if scan.inComment {
		return false
	}
	match := specKitFence.FindStringSubmatch(raw)
	if scan.fence == "" {
		if match == nil {
			return false
		}
		scan.fence, scan.fenceStart = match[1], position
		scan.settleAnchor()
		return true
	}
	if match != nil && match[1][0] == scan.fence[0] && len(match[1]) >= len(scan.fence) {
		scan.fence = ""
	}
	return true
}

// stripComments removes HTML comments from one line and tracks one that spans
// lines. An opening inside inline code is text.
func (scan *specKitScan) stripComments(line string, position SpecPosition) string {
	var kept strings.Builder
	for line != "" {
		if scan.inComment {
			_, after, closed := strings.Cut(line, "-->")
			if !closed {
				return kept.String()
			}
			scan.inComment = false
			line = after
			continue
		}
		start := specKitCommentOpening(line)
		if start < 0 {
			kept.WriteString(line)
			break
		}
		kept.WriteString(line[:start])
		scan.inComment, scan.commentStart = true, position
		line = line[start+len("<!--"):]
	}
	return kept.String()
}

// specKitCommentOpening returns the offset of the first "<!--" outside inline
// code, or -1.
func specKitCommentOpening(line string) int {
	spans := specKitInlineCode.FindAllStringIndex(line, -1)
	from := 0
	for {
		found := strings.Index(line[from:], "<!--")
		if found < 0 {
			return -1
		}
		start := from + found
		inside := false
		for _, span := range spans {
			if start >= span[0] && start < span[1] {
				from, inside = span[1], true
				break
			}
		}
		if !inside {
			return start
		}
	}
}

func (scan *specKitScan) heading(level int, title string, position SpecPosition) {
	if level >= 2 {
		scan.pastHeader = true
	}
	section := specKitSectionOf(title)
	switch {
	case section == specKitRequirements && scan.section == specKitRequirements:
		// Functional Requirements below Requirements: the section keeps its level,
		// so a sibling subsection does not end it whatever the order.
	case section != specKitNoSection:
		scan.section, scan.sectionLevel = section, level
		switch section {
		case specKitKeyEntities:
			scan.derivedFact(position, "Key Entities")
		case specKitSuccessCriteria:
			scan.criteriaHeading = position
		}
	case level <= scan.sectionLevel:
		scan.section, scan.sectionLevel = specKitNoSection, 0
	}
}

// specKitSectionOf recognizes a section by its title, without a trailing mark
// such as "*(mandatory)*" or "(Priority: P1)".
func specKitSectionOf(title string) specKitSection {
	name := strings.ToLower(title)
	if end := strings.IndexAny(name, "*("); end >= 0 {
		name = name[:end]
	}
	name = strings.TrimSpace(name)
	switch {
	case name == "success criteria":
		return specKitSuccessCriteria
	case name == "out of scope":
		return specKitOutOfScope
	case name == "requirements", name == "functional requirements":
		return specKitRequirements
	case name == "key entities":
		return specKitKeyEntities
	case strings.HasPrefix(name, "user story"):
		return specKitUserStory
	}
	return specKitNoSection
}

// labeled returns the label and text of a line such as "- **FR-001**: Text",
// with or without its list marker.
func labeled(text, prefix string) (string, string, bool) {
	body := strings.TrimSpace(text)
	if item := specKitListItem.FindStringSubmatch(text); item != nil {
		body = item[1]
	}
	match := specKitLabel.FindStringSubmatch(body)
	if match == nil || !strings.HasPrefix(match[1], prefix) {
		return "", "", false
	}
	return match[1], strings.TrimSpace(match[2]), true
}

func (scan *specKitScan) requirement(text string, position SpecPosition) bool {
	label, body, found := labeled(text, "FR-")
	if !found {
		return false
	}
	id, unanchored := "", true
	if scan.anchor != nil {
		id, unanchored = scan.anchor.id, false
		scan.anchor = nil
	} else {
		id, _ = requirementIDFromName(label, len(scan.reading.Spec.Requirements)+1)
	}
	spec := scan.reading.Spec
	spec.Requirements = append(spec.Requirements, SpecRequirement{ID: id, Text: body})
	scan.reading.Requirements = append(scan.reading.Requirements, SpecRequirementRecord{Position: position, Unanchored: unanchored})
	index := len(spec.Requirements) - 1
	scan.extend = func(more string) {
		spec.Requirements[index].Text = strings.TrimSpace(spec.Requirements[index].Text + " " + more)
	}
	return true
}

func (scan *specKitScan) scenario(text string, position SpecPosition) {
	scenario := SpecScenario{Name: specKitPlain(text), Position: position}
	if scan.anchor != nil {
		scenario.Check = scan.anchor.id
		scan.anchor = nil
	}
	scan.reading.Scenarios = append(scan.reading.Scenarios, scenario)
	index := len(scan.reading.Scenarios) - 1
	scan.extend = func(more string) {
		scan.reading.Scenarios[index].Name += " " + specKitPlain(more)
	}
}

func (scan *specKitScan) outcome(text string, position SpecPosition) bool {
	_, body, found := labeled(text, "SC-")
	if !found {
		return false
	}
	spec := scan.reading.Spec
	spec.Outcomes = append(spec.Outcomes, body)
	index := len(spec.Outcomes) - 1
	scan.reading.OutcomePositions = append(scan.reading.OutcomePositions, position)
	scan.extend = func(more string) {
		spec.Outcomes[index] = strings.TrimSpace(spec.Outcomes[index] + " " + more)
	}
	scan.settleAnchor()
	return true
}

func (scan *specKitScan) nonGoal(text string, position SpecPosition) {
	scan.settleAnchor()
	spec := scan.reading.Spec
	spec.NonGoals = append(spec.NonGoals, strings.TrimSpace(text))
	index := len(spec.NonGoals) - 1
	scan.reading.NonGoalPositions = append(scan.reading.NonGoalPositions, position)
	scan.extend = func(more string) {
		spec.NonGoals[index] = strings.TrimSpace(spec.NonGoals[index] + " " + more)
	}
}

// specKitPlain removes the bold marks around Given, When and Then.
func specKitPlain(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "**", ""))
}

func (scan *specKitScan) endItem() { scan.extend = nil }

// settleAnchor refuses an anchor that no requirement or scenario consumed: left
// alone, it would pin nothing while its author believes it pins an ID.
func (scan *specKitScan) settleAnchor() {
	if scan.anchor == nil {
		return
	}
	scan.fail(scan.anchor.position, "anchor %q must stand on the line directly above a requirement or a scenario", scan.anchor.id)
	scan.anchor = nil
}

func (scan *specKitScan) fail(position SpecPosition, format string, args ...any) {
	scan.reading.Findings = append(scan.reading.Findings, diag.Errorf(ErrorCodeSpecRead, position.Field(), format, args...))
}

func (scan *specKitScan) derivedFact(position SpecPosition, name string) {
	scan.reading.Findings = append(scan.reading.Findings, diag.Warningf(WarningCodeSpecDerivedFact, position.Field(),
		"%s is a derived fact: the spec does not read it", name))
}
