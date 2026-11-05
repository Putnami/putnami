package features

import (
	"bytes"
	"path"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// openSpecFileName is the one file a capability folder holds as its spec.
const openSpecFileName = "spec.md"

// openSpecChangesPath marks a proposal in flight: deltas, not the current spec.
const openSpecChangesPath = "/openspec/changes/"

var (
	markdownHeading     = regexp.MustCompile(`^(#{1,6})[ \t]+(.*)$`)
	markdownClosingRun  = regexp.MustCompile(`[ \t]+#+[ \t]*$`)
	markdownFenceOpen   = regexp.MustCompile("^\\s*(`{3,}|~{3,})(.*)$")
	markdownFenceClose  = regexp.MustCompile("^\\s*(`{3,}|~{3,})\\s*$")
	markdownLink        = regexp.MustCompile(`\[[^\]]*\]\(\s*<?([^)\s>]+)`)
	openSpecRequirement = regexp.MustCompile(`(?i)^Requirement:[ \t]*(.*)$`)
	openSpecScenario    = regexp.MustCompile(`(?i)^Scenario:[ \t]*(.+)$`)
	openSpecMetadata    = regexp.MustCompile(`^\*\*[^*]+\*\*:`)
	// A placeholder is what `openspec archive` writes, or a TBD or TODO that
	// opens the paragraph. A marker inside a sentence is a real Purpose with an
	// open question in it.
	openSpecArchivedPlaceholder = regexp.MustCompile(`TBD - created by archiving change \S+\. Update Purpose after archive\.`)
	openSpecLeadingShouted      = regexp.MustCompile(`^(?:TBD|TODO)(?:[^\p{L}\p{N}\p{M}_]|$)`)
	openSpecLeadingPunctuated   = regexp.MustCompile(`(?i)^(?:tbd|todo)[ \t]*(?:$|[:\-–—.,;()\[\]{}])`)
)

// specDerivedSections names the sections that restate what the repository
// already derives. Their content has another home, so a reader warns on them.
var specDerivedSections = map[string]bool{
	"status": true, "date": true, "dates": true, "branch": true, "routes": true, "entities": true,
}

func init() {
	specReaders[SpecFormatOpenSpec] = openSpecReader{}
}

// openSpecReader projects one OpenSpec capability: `## Purpose` to outcomes,
// `### Requirement: <name>` to requirements and `#### Scenario: <name>` to the
// scenarios of the requirement above it.
type openSpecReader struct{}

// Read implements SpecReader.
func (openSpecReader) Read(input SpecInput) SpecReading {
	if strings.Contains("/"+input.Reference.Path+"/", openSpecChangesPath) {
		return SpecReading{Findings: []diag.Diagnostic{diag.Errorf(ErrorCodeSpecRead, input.Reference.Path,
			"%s is a change in flight, not a current spec: name the capability under openspec/specs", input.Reference.Path)}}
	}
	for _, file := range input.Files {
		if file.Path == input.Reference.Path || file.Path == input.Reference.Path+"/"+openSpecFileName {
			parser := openSpecParser{file: file.Path}
			parser.parse(file.Data)
			return parser.reading()
		}
	}
	return SpecReading{Findings: []diag.Diagnostic{diag.Errorf(ErrorCodeSpecRead, input.Reference.Path,
		"no %s here: name the folder of one capability", openSpecFileName)}}
}

// openSpecSection is the part of the file the current line belongs to.
type openSpecSection int

const (
	openSpecLeading openSpecSection = iota
	openSpecPurpose
	openSpecRequirements
	openSpecOther
)

// openSpecParagraph is one block of prose and the line it starts on.
type openSpecParagraph struct {
	lines []string
	line  int
}

type openSpecParser struct {
	file    string
	section openSpecSection
	// fence is the marker of the open code fence, empty outside one.
	fence     string
	inComment bool
	// anchor and anchorLine hold an anchor until the next line uses it.
	anchor     string
	anchorLine int

	leading     []openSpecParagraph
	purpose     []openSpecParagraph
	purposeLine int
	inParagraph bool

	spec          Spec
	records       []SpecRequirementRecord
	findings      []diag.Diagnostic
	body          []string
	metadata      []string
	inBody        bool
	inRequirement bool
}

func (parser *openSpecParser) position(line int) SpecPosition {
	return SpecPosition{File: parser.file, Line: line}
}

func (parser *openSpecParser) parse(data []byte) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(string(data))
	for index, line := range strings.Split(text, "\n") {
		// An anchor names the requirement on the very next line, or nothing.
		anchor, anchorLine := parser.anchor, parser.anchorLine
		parser.anchor, parser.anchorLine = "", 0
		if !parser.line(line, index+1, anchor) && anchorLine != 0 {
			parser.strayAnchor(anchorLine)
		}
	}
	if parser.anchorLine != 0 {
		parser.strayAnchor(parser.anchorLine)
	}
	parser.closeRequirement()
}

func (parser *openSpecParser) strayAnchor(line int) {
	parser.findings = append(parser.findings, diag.Errorf(ErrorCodeSpecRead, parser.position(line).Field(),
		"this anchor stands above no requirement: move it to the line above one"))
}

// line reads one line and reports whether it used the anchor above it.
func (parser *openSpecParser) line(line string, number int, anchor string) bool {
	trimmed := strings.TrimSpace(line)
	switch {
	case parser.fence != "":
		if match := markdownFenceClose.FindStringSubmatch(line); match != nil && match[1][0] == parser.fence[0] && len(match[1]) >= len(parser.fence) {
			parser.fence = ""
		}
		return false
	case parser.inComment:
		parser.inComment = !strings.Contains(trimmed, "-->")
		return false
	case strings.HasPrefix(trimmed, "<!--"):
		parser.comment(trimmed, number)
		return false
	}
	if match := markdownFenceOpen.FindStringSubmatch(line); match != nil && !(match[1][0] == '`' && strings.Contains(match[2], "`")) {
		parser.fence = match[1]
		return false
	}
	heading := markdownHeading.FindStringSubmatch(line)
	if heading == nil {
		parser.prose(trimmed, number)
		return false
	}
	return parser.heading(len(heading[1]), markdownClosingRun.ReplaceAllString(heading[2], ""), number, anchor)
}

// comment reads a line that opens an HTML comment, which an anchor is. Only the
// comment form counts here: the tag form of ParseSpecAnchor belongs to Gherkin.
func (parser *openSpecParser) comment(trimmed string, number int) {
	id, isAnchor, err := ParseSpecAnchor(trimmed)
	switch {
	case err != nil:
		parser.findings = append(parser.findings, diag.Errorf(ErrorCodeSpecRead, parser.position(number).Field(), "%v", err))
	case isAnchor:
		parser.anchor, parser.anchorLine = id, number
	default:
		parser.inComment = !strings.Contains(trimmed[len("<!--"):], "-->")
	}
}

// prose files one visible line under the section it belongs to.
func (parser *openSpecParser) prose(trimmed string, number int) {
	switch {
	case parser.section == openSpecRequirements:
		if !parser.inBody || trimmed == "" {
			return
		}
		if openSpecMetadata.MatchString(trimmed) {
			parser.metadata = append(parser.metadata, trimmed)
		} else {
			parser.body = append(parser.body, trimmed)
		}
	case parser.section == openSpecPurpose:
		parser.purpose = parser.appendProse(parser.purpose, trimmed, number)
	case parser.section == openSpecLeading:
		parser.leading = parser.appendProse(parser.leading, trimmed, number)
	}
}

// appendProse adds a line to the paragraph it continues. A blank line or a list
// item starts a new one, so each list item is its own outcome.
func (parser *openSpecParser) appendProse(paragraphs []openSpecParagraph, trimmed string, number int) []openSpecParagraph {
	if trimmed == "" {
		parser.inParagraph = false
		return paragraphs
	}
	item := strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ")
	if item {
		trimmed = strings.TrimSpace(trimmed[1:])
	}
	if !parser.inParagraph || item {
		parser.inParagraph = true
		return append(paragraphs, openSpecParagraph{lines: []string{trimmed}, line: number})
	}
	last := len(paragraphs) - 1
	paragraphs[last].lines = append(paragraphs[last].lines, trimmed)
	return paragraphs
}

// heading reads one heading and reports whether it used the anchor above it.
func (parser *openSpecParser) heading(level int, title string, number int, anchor string) bool {
	parser.inParagraph = false
	title = strings.TrimSpace(title)
	folded := strings.ToLower(title)
	if (level == 2 || level == 3) && specDerivedSections[folded] {
		parser.findings = append(parser.findings, diag.Warningf(WarningCodeSpecDerivedFact, parser.position(number).Field(),
			"section %q restates a fact Putnami derives: remove it", title))
	}
	switch {
	case level == 1:
		parser.closeRequirement()
	case level == 2:
		parser.closeRequirement()
		switch folded {
		case "purpose":
			parser.section, parser.purposeLine = openSpecPurpose, number
		case "requirements":
			parser.section = openSpecRequirements
		default:
			parser.section = openSpecOther
		}
	case parser.section != openSpecRequirements:
		parser.section = openSpecOther
	default:
		return parser.requirementHeading(level, title, number, anchor)
	}
	return false
}

// requirementHeading reads a heading below `## Requirements`. Any heading ends
// a requirement's body, and only a level 3 heading ends the requirement.
func (parser *openSpecParser) requirementHeading(level int, title string, number int, anchor string) bool {
	if match := openSpecRequirement.FindStringSubmatch(title); match != nil && level == 3 {
		parser.closeRequirement()
		parser.openRequirement(strings.TrimSpace(match[1]), number, anchor)
		return true
	}
	parser.closeBody()
	if level == 3 {
		parser.inRequirement = false
	}
	if match := openSpecScenario.FindStringSubmatch(title); match != nil && level == 4 && parser.inRequirement {
		name := strings.TrimSpace(match[1])
		check, _ := NormalizeRequirementName(name)
		last := len(parser.records) - 1
		parser.records[last].Scenarios = append(parser.records[last].Scenarios,
			SpecScenario{Name: name, Check: check, Position: parser.position(number)})
	}
	return false
}

func (parser *openSpecParser) openRequirement(name string, number int, anchor string) {
	id, ok := anchor, anchor != ""
	if !ok {
		id, ok = NormalizeRequirementName(name)
	}
	if !ok {
		parser.findings = append(parser.findings, diag.Errorf(ErrorCodeSpecRead, parser.position(number).Field(),
			"requirement %q has no ID: write an anchor on the line above it", name))
		return
	}
	parser.spec.Requirements = append(parser.spec.Requirements, SpecRequirement{ID: id})
	parser.records = append(parser.records, SpecRequirementRecord{Position: parser.position(number), Scenarios: []SpecScenario{}})
	parser.inBody, parser.inRequirement = true, true
}

// closeBody stores the requirement's text: its visible lines on one line.
// `**Priority**:` metadata stays out unless it is all the body says.
func (parser *openSpecParser) closeBody() {
	if parser.inBody {
		lines := parser.body
		if len(lines) == 0 {
			lines = parser.metadata
		}
		parser.spec.Requirements[len(parser.spec.Requirements)-1].Text = strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	}
	parser.body, parser.metadata, parser.inBody = nil, nil, false
}

func (parser *openSpecParser) closeRequirement() {
	parser.closeBody()
	parser.inRequirement = false
}

// outcomes bounds the prose here, where its line is known: a validation finding
// on an outcome would name only the reference.
func (parser *openSpecParser) outcomes() (outcomes []string, refused bool) {
	paragraphs := parser.purpose
	if len(paragraphs) == 0 {
		paragraphs = parser.leading
	}
	for _, paragraph := range paragraphs {
		position := parser.position(paragraph.line)
		text := strings.Join(strings.Fields(strings.Join(paragraph.lines, " ")), " ")
		if len(text) > specTextMaxLength {
			refused = true
			parser.findings = append(parser.findings, diag.Errorf(ErrorCodeSpecRead, position.Field(),
				"this Purpose paragraph has %d bytes, over the %d-byte bound: split it into two paragraphs", len(text), specTextMaxLength))
			continue
		}
		if isOpenSpecPlaceholder(text) {
			parser.findings = append(parser.findings, diag.Warningf(WarningCodeSpecPlaceholderOutcome, position.Field(),
				"this Purpose is a placeholder: write what the capability is for"))
		}
		outcomes = append(outcomes, text)
	}
	return outcomes, refused
}

func isOpenSpecPlaceholder(text string) bool {
	return openSpecArchivedPlaceholder.MatchString(text) || openSpecLeadingShouted.MatchString(text) || openSpecLeadingPunctuated.MatchString(text)
}

// decisions collects the links of the projected prose that name a decision
// record. A link that starts with a dot is relative to the spec file; any other
// is workspace-relative, the form Spec.Decisions holds.
func (parser *openSpecParser) decisions(texts []string) []string {
	var decisions []string
	seen := make(map[string]bool)
	for _, text := range texts {
		for _, match := range markdownLink.FindAllStringSubmatch(text, -1) {
			link, _, _ := strings.Cut(match[1], "#")
			if strings.HasPrefix(link, ".") {
				link = path.Join(path.Dir(parser.file), link)
			}
			if IsDecisionLink(link) && !seen[link] {
				seen[link] = true
				decisions = append(decisions, link)
			}
		}
	}
	return decisions
}

func (parser *openSpecParser) reading() SpecReading {
	outcomes, refused := parser.outcomes()
	if len(outcomes) == 0 {
		if !refused {
			parser.findings = append(parser.findings, diag.Errorf(ErrorCodeSpecRead, parser.position(max(parser.purposeLine, 1)).Field(),
				"this spec states no outcome: write its Purpose"))
		}
		return SpecReading{Findings: parser.findings}
	}
	texts := append([]string(nil), outcomes...)
	for _, requirement := range parser.spec.Requirements {
		texts = append(texts, requirement.Text)
	}
	spec := parser.spec
	spec.Outcomes = outcomes
	spec.Decisions = parser.decisions(texts)
	return SpecReading{Spec: &spec, Requirements: parser.records, Findings: parser.findings}
}
