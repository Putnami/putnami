package features

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// kiroFileName is the one file of a Kiro spec directory the reader uses.
// design.md and tasks.md are context and process state.
const kiroFileName = "requirements.md"

var (
	kiroHeading    = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	kiroFence      = regexp.MustCompile("^[ \\t]*(`{3,}|~{3,})")
	kiroBreak      = regexp.MustCompile(`^ {0,3}(?:-{3,}|\*{3,}|_{3,})[ \t]*$`)
	kiroListItem   = regexp.MustCompile(`^([ \t]*)(?:[-*+]|[0-9]+(?:\.[0-9]+)+[.)]?|[0-9]+[.)])[ \t]+(?:\[[ xX]\][ \t]+)?(.*)$`)
	kiroGroup      = regexp.MustCompile(`(?i)^requirement[ \t]+(r?[0-9]+(?:\.[0-9]+)*)(?:[^0-9A-Za-z.]|$)`)
	kiroCriteria   = regexp.MustCompile(`(?i)^acceptance criteria\b`)
	kiroBoldLabel  = regexp.MustCompile(`(?i)^[*_]*acceptance criteria[*_:]*[ \t]*$`)
	kiroNumbering  = regexp.MustCompile(`^[0-9]+[.)][ \t]+`)
	kiroInlineCode = regexp.MustCompile("`[^`]*`")
	kiroOpener     = regexp.MustCompile(`(?i)^(when|while|where|if|the|for all)\b`)
	kiroShall      = regexp.MustCompile(`(?i)\bshall\b`)
	kiroThen       = regexp.MustCompile(`(?i)\bthen\b`)
	// kiroSentenceEnd matches the space that ends a sentence of folded prose.
	kiroSentenceEnd = regexp.MustCompile(`[.!?] `)
)

// kiroDerivedSections names the sections that restate what the repository
// already derives.
var kiroDerivedSections = map[string]bool{
	"status": true, "date": true, "dates": true, "branch": true, "routes": true, "entities": true,
}

func init() {
	specReaders[SpecFormatKiro] = kiroReader{}
}

// kiroReader projects the requirements.md of one Kiro spec: each Introduction
// paragraph to an outcome and each acceptance criterion to one requirement, so
// each criterion keeps its own bound and proof. A Kiro criterion has no
// scenarios, so its record leaves Scenarios nil.
type kiroReader struct{}

// Read implements SpecReader.
func (kiroReader) Read(input SpecInput) SpecReading {
	file, found := kiroFile(input)
	if !found {
		return SpecReading{Findings: []diag.Diagnostic{diag.Errorf(ErrorCodeSpecRead, input.Reference.Path,
			"a kiro reference names a spec directory or its %s, and this one holds none", kiroFileName)}}
	}
	scan := kiroScan{file: file.Path, reading: SpecReading{Spec: &Spec{}}}
	text := strings.TrimPrefix(string(file.Data), "\ufeff")
	for index, line := range strings.Split(text, "\n") {
		scan.line(strings.TrimRight(line, "\r"), index+1)
	}
	scan.finish()
	return scan.reading
}

func kiroFile(input SpecInput) (SpecFile, bool) {
	inDirectory := input.Reference.Path + "/" + kiroFileName
	for _, file := range input.Files {
		if file.Path == inDirectory || (file.Path == input.Reference.Path && path.Base(file.Path) == kiroFileName) {
			return file, true
		}
	}
	return SpecFile{}, false
}

// kiroAnchor is an anchor waiting for the line below it.
type kiroAnchor struct {
	id       string
	position SpecPosition
}

// kiroRequirementGroup is one Requirement N heading. It is not a native
// requirement: its ID only names the criteria that have no anchor.
type kiroRequirementGroup struct {
	id       string
	level    int
	position SpecPosition
	criteria int
	// reading says the lines below belong to its acceptance criteria.
	reading bool
}

// kiroParagraph is one block of Introduction prose.
type kiroParagraph struct {
	lines    []string
	position SpecPosition
}

// kiroScan is the state of one pass over a requirements.md.
type kiroScan struct {
	file    string
	reading SpecReading

	inComment    bool
	commentStart SpecPosition
	fence        string
	fenceStart   SpecPosition
	anchor       *kiroAnchor
	afterBlank   bool

	inIntroduction bool
	introduction   SpecPosition
	paragraphs     []kiroParagraph
	inParagraph    bool

	group  *kiroRequirementGroup
	groups int
	// extend appends a continuation line to the criterion above, nil when the
	// line above ended it.
	extend func(text string)
	indent int
}

func (scan *kiroScan) line(raw string, number int) {
	position := SpecPosition{File: scan.file, Line: number}
	if scan.fenced(raw, position) {
		scan.endBlock()
		return
	}
	if !scan.inComment && strings.HasPrefix(strings.TrimSpace(raw), "<!--") {
		if id, isAnchor, err := ParseSpecAnchor(raw); isAnchor {
			scan.settleAnchor()
			scan.endBlock()
			if err != nil {
				scan.fail(position, "%v", err)
				return
			}
			scan.anchor = &kiroAnchor{id: id, position: position}
			return
		}
	}
	text := scan.stripComments(raw, position)
	if strings.TrimSpace(text) == "" {
		scan.settleAnchor()
		scan.inParagraph = false
		scan.afterBlank = true
		return
	}
	item := kiroListItem.FindStringSubmatch(text)
	indented := text[0] == ' ' || text[0] == '\t'
	if scan.afterBlank && !indented {
		scan.extend = nil
	}
	scan.afterBlank = false
	if heading := kiroHeading.FindStringSubmatch(text); heading != nil {
		scan.endBlock()
		scan.heading(len(heading[1]), heading[2], position)
		return
	}
	switch {
	case scan.group != nil && kiroBoldLabel.MatchString(strings.TrimSpace(text)):
		scan.settleAnchor()
		scan.extend = nil
		scan.group.reading = true
	case scan.inIntroduction:
		scan.settleAnchor()
		scan.prose(text, item != nil, position)
	case scan.group != nil && scan.group.reading:
		scan.criterionLine(text, item, position)
	default:
		scan.settleAnchor()
	}
}

// criterionLine reads one line of an acceptance criteria list. A list item
// opens a criterion unless it is nested under the open one. Any other line
// continues the open criterion.
func (scan *kiroScan) criterionLine(text string, item []string, position SpecPosition) {
	switch {
	case item != nil && (scan.extend == nil || len(item[1]) <= scan.indent):
		scan.criterion(item[2], len(item[1]), position)
	case scan.extend != nil && !kiroBreak.MatchString(text):
		scan.settleAnchor()
		scan.extend(strings.TrimSpace(text))
	default:
		scan.settleAnchor()
		scan.extend = nil
	}
}

func (scan *kiroScan) criterion(text string, indent int, position SpecPosition) {
	scan.group.criteria++
	id, unanchored := scan.group.id+"-"+strconv.Itoa(scan.group.criteria), true
	if scan.anchor != nil {
		id, unanchored = scan.anchor.id, false
		scan.anchor = nil
	}
	spec := scan.reading.Spec
	spec.Requirements = append(spec.Requirements, SpecRequirement{ID: id, Text: strings.TrimSpace(text)})
	scan.reading.Requirements = append(scan.reading.Requirements, SpecRequirementRecord{Position: position, Unanchored: unanchored})
	index := len(spec.Requirements) - 1
	scan.indent = indent
	scan.extend = func(more string) {
		spec.Requirements[index].Text = strings.TrimSpace(spec.Requirements[index].Text + " " + more)
	}
}

func (scan *kiroScan) heading(level int, title string, position SpecPosition) {
	plain := strings.Trim(title, "*_ \t")
	if match := kiroGroup.FindStringSubmatch(plain); match != nil {
		scan.closeGroup()
		scan.inIntroduction = false
		scan.openGroup(match[1], level, position)
		return
	}
	scan.settleAnchor()
	if scan.group != nil {
		if level <= scan.group.level {
			scan.closeGroup()
		} else {
			scan.group.reading = kiroCriteria.MatchString(plain)
			if scan.group.reading {
				return
			}
		}
	}
	name := strings.ToLower(kiroNumbering.ReplaceAllString(plain, ""))
	scan.inIntroduction = name == "introduction"
	if scan.inIntroduction && scan.introduction.File == "" {
		scan.introduction = position
	}
	if kiroDerivedSections[name] {
		scan.reading.Findings = append(scan.reading.Findings, diag.Warningf(WarningCodeSpecDerivedFact, position.Field(),
			"%s is a derived fact: the spec does not read it", title))
	}
}

func (scan *kiroScan) openGroup(label string, level int, position SpecPosition) {
	id := ""
	if scan.anchor != nil {
		id = scan.anchor.id
		scan.anchor = nil
	} else {
		normalized, _ := NormalizeRequirementName(label)
		id = "requirement-" + normalized
	}
	scan.group = &kiroRequirementGroup{id: id, level: level, position: position}
	scan.groups++
}

// closeGroup warns on a group with no criterion, because it would otherwise
// vanish from the projection in silence.
func (scan *kiroScan) closeGroup() {
	if scan.group != nil && scan.group.criteria == 0 {
		scan.reading.Findings = append(scan.reading.Findings, diag.Warningf(WarningCodeSpecRequirementIgnored, scan.group.position.Field(),
			"this requirement has no acceptance criteria, so it projects nothing: list them under Acceptance Criteria"))
	}
	scan.group = nil
}

// prose adds a line to the Introduction paragraph it continues. A list item
// opens its own paragraph.
func (scan *kiroScan) prose(text string, item bool, position SpecPosition) {
	if !scan.inParagraph || item {
		scan.inParagraph = true
		scan.paragraphs = append(scan.paragraphs, kiroParagraph{position: position})
	}
	if match := kiroListItem.FindStringSubmatch(text); match != nil {
		text = match[2]
	}
	last := &scan.paragraphs[len(scan.paragraphs)-1]
	last.lines = append(last.lines, text)
}

// finish refuses what would otherwise hide text in silence: an anchor still
// waiting, a comment or a fence that never closes, and a file that states no
// outcome or no requirement.
func (scan *kiroScan) finish() {
	scan.settleAnchor()
	scan.closeGroup()
	if scan.inComment {
		scan.fail(scan.commentStart, "this HTML comment never closes, and it hides the rest of the file")
	}
	if scan.fence != "" {
		scan.fail(scan.fenceStart, "this code fence never closes, and it hides the rest of the file")
	}
	spec := scan.reading.Spec
	for _, paragraph := range scan.paragraphs {
		text := strings.Join(strings.Fields(strings.Join(paragraph.lines, " ")), " ")
		for _, outcome := range kiroOutcomes(text) {
			if len(outcome) > specTextMaxLength {
				scan.fail(paragraph.position, "this Introduction sentence has %d bytes, over the %d-byte bound: split it into two sentences", len(outcome), specTextMaxLength)
				continue
			}
			spec.Outcomes = append(spec.Outcomes, outcome)
		}
	}
	top := SpecPosition{File: scan.file, Line: 1}
	if len(scan.paragraphs) == 0 {
		position := top
		if scan.introduction.File != "" {
			position = scan.introduction
		}
		scan.fail(position, "a Kiro spec needs an Introduction paragraph: no outcome was found")
	}
	if scan.groups == 0 {
		scan.fail(top, "a Kiro spec needs a Requirement N heading: no requirement was found")
	}
	if len(spec.Outcomes) == 0 || scan.groups == 0 {
		scan.reading.Spec, scan.reading.Requirements = nil, nil
		return
	}
	for index, requirement := range spec.Requirements {
		if !isEARSCriterion(requirement.Text) {
			scan.reading.Findings = append(scan.reading.Findings, diag.Warningf(WarningCodeSpecCriterionMalformed,
				scan.reading.Requirements[index].Position.Field(),
				"this criterion is in no EARS form, such as WHEN <trigger> THE <system> SHALL <response>: it stays a requirement"))
		}
	}
}

// kiroOutcomes cuts a paragraph over the bound at sentence ends, keeping as many
// sentences together as the bound allows. An Introduction is free prose that no
// other tool bounds, so refusing a long one would fail a first report run. A
// single sentence over the bound comes back whole, for the caller to refuse.
func kiroOutcomes(text string) []string {
	if len(text) <= specTextMaxLength {
		return []string{text}
	}
	var outcomes []string
	current, start := "", 0
	add := func(sentence string) {
		switch {
		case current == "":
			current = sentence
		case len(current)+1+len(sentence) <= specTextMaxLength:
			current += " " + sentence
		default:
			outcomes = append(outcomes, current)
			current = sentence
		}
	}
	for _, end := range kiroSentenceEnd.FindAllStringIndex(text, -1) {
		add(text[start : end[1]-1])
		start = end[1]
	}
	add(text[start:])
	return append(outcomes, current)
}

// isEARSCriterion reports whether a criterion opens like an EARS form and
// states a response: ubiquitous (THE), event-driven (WHEN), state-driven
// (WHILE), unwanted behavior (IF ... THEN), optional feature (WHERE), or the
// FOR ALL property Kiro writes beside them.
func isEARSCriterion(text string) bool {
	plain := strings.TrimSpace(strings.ReplaceAll(text, "*", ""))
	opener := kiroOpener.FindString(plain)
	shall := kiroShall.FindStringIndex(plain)
	if opener == "" || shall == nil {
		return false
	}
	return !strings.EqualFold(opener, "if") || kiroThen.MatchString(plain[:shall[0]])
}

// fenced reports whether the line opens, closes or lies inside a code fence. A
// comment that is still open hides a fence.
func (scan *kiroScan) fenced(raw string, position SpecPosition) bool {
	if scan.inComment {
		return false
	}
	match := kiroFence.FindStringSubmatch(raw)
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
func (scan *kiroScan) stripComments(line string, position SpecPosition) string {
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
		start := kiroCommentOpening(line)
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

// kiroCommentOpening returns the offset of the first "<!--" outside inline
// code, or -1.
func kiroCommentOpening(line string) int {
	spans := kiroInlineCode.FindAllStringIndex(line, -1)
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

// endBlock ends the open criterion and paragraph.
func (scan *kiroScan) endBlock() {
	scan.extend = nil
	scan.inParagraph = false
}

// settleAnchor refuses an anchor that no criterion or group consumed: left
// alone, it would pin nothing while its author believes it pins an ID.
func (scan *kiroScan) settleAnchor() {
	if scan.anchor == nil {
		return
	}
	scan.fail(scan.anchor.position, "anchor %q must stand on the line directly above a requirement heading or a criterion", scan.anchor.id)
	scan.anchor = nil
}

func (scan *kiroScan) fail(position SpecPosition, format string, args ...any) {
	scan.reading.Findings = append(scan.reading.Findings, diag.Errorf(ErrorCodeSpecRead, position.Field(), format, args...))
}
