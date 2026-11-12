package features

import (
	"embed"
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"sync"

	diag "go.putnami.dev/protocol/diagnostic"
)

// gherkinDefaultLanguage is the language of a file with no language header.
const gherkinDefaultLanguage = "en"

// gherkinCatalogs holds one keyword catalog per language, from
// gherkin-languages.json of cucumber/gherkin, whose version gherkin/README.md
// names.
//
//go:embed gherkin/locales/*.json
var gherkinCatalogs embed.FS

type gherkinKeyword int

const (
	gherkinNoKeyword gherkinKeyword = iota
	gherkinFeature
	gherkinRule
	gherkinScenario
	gherkinBackground
	gherkinExamples
)

// gherkinLanguage is the keywords of one language. Scenario and Scenario
// Outline share a meaning here: each is one scenario.
type gherkinLanguage struct {
	keywords map[string]gherkinKeyword
	// steps are the step keywords, each with the space that follows it, if any.
	steps []string
}

// gherkinLanguages maps each language code to its keywords.
var gherkinLanguages = sync.OnceValue(func() map[string]gherkinLanguage {
	entries, err := gherkinCatalogs.ReadDir("gherkin/locales")
	if err != nil {
		panic("features: the embedded Gherkin catalogs are missing: " + err.Error())
	}
	languages := make(map[string]gherkinLanguage, len(entries))
	for _, entry := range entries {
		var words struct {
			Feature         []string `json:"feature"`
			Rule            []string `json:"rule"`
			Background      []string `json:"background"`
			Scenario        []string `json:"scenario"`
			ScenarioOutline []string `json:"scenarioOutline"`
			Examples        []string `json:"examples"`
			Given           []string `json:"given"`
			When            []string `json:"when"`
			Then            []string `json:"then"`
			And             []string `json:"and"`
			But             []string `json:"but"`
		}
		data, err := gherkinCatalogs.ReadFile("gherkin/locales/" + entry.Name())
		if err == nil {
			err = json.Unmarshal(data, &words)
		}
		if err != nil {
			panic("features: the embedded Gherkin catalog " + entry.Name() + " is invalid: " + err.Error())
		}
		language := strings.TrimSuffix(entry.Name(), path.Ext(entry.Name()))
		keywords := map[string]gherkinKeyword{}
		for _, group := range []struct {
			words   []string
			keyword gherkinKeyword
		}{
			{words.Feature, gherkinFeature},
			{words.Rule, gherkinRule},
			{words.Background, gherkinBackground},
			{words.Scenario, gherkinScenario},
			{words.ScenarioOutline, gherkinScenario},
			{words.Examples, gherkinExamples},
		} {
			for _, word := range group.words {
				keywords[word] = group.keyword
			}
		}
		groups := [][]string{words.Given, words.When, words.Then, words.And, words.But}
		steps := make([]string, 0, len(words.Given)*len(groups))
		for _, group := range groups {
			steps = append(steps, group...)
		}
		languages[language] = gherkinLanguage{keywords: keywords, steps: steps}
	}
	return languages
})

var (
	gherkinLanguageHeader = regexp.MustCompile(`^#\s*language\s*:\s*([a-zA-Z0-9_-]+)\s*$`)
	gherkinTagComment     = regexp.MustCompile(`\s#.*$`)
)

// gherkinReader projects the .feature files of one reference.
//
// Each file holds one Feature. A Rule, and a Feature that has no Rule or holds
// scenarios outside its rules, is a requirement that owns its scenarios.
type gherkinReader struct{}

// Read implements SpecReader.
func (gherkinReader) Read(input SpecInput) SpecReading {
	reading := SpecReading{Spec: &Spec{}}
	for _, file := range input.Files {
		scan := gherkinScan{file: file.Path, language: gherkinLanguages()[gherkinDefaultLanguage], reading: &reading}
		scan.read(strings.TrimPrefix(string(file.Data), "\ufeff"))
	}
	// An error leaves part of a file unread, so a projection would hide it.
	if diag.HasErrors(reading.Findings) {
		reading.Spec, reading.Requirements = nil, nil
		reading.OutcomePositions = nil
	}
	return reading
}

// gherkinParagraph is one paragraph of a description.
type gherkinParagraph struct {
	text     string
	position SpecPosition
}

// gherkinAnchor is one anchor tag of a keyword.
type gherkinAnchor struct {
	id       string
	position SpecPosition
}

// gherkinTags holds the anchor tags waiting for their keyword.
type gherkinTags struct {
	id    *gherkinAnchor
	check *gherkinAnchor
}

// gherkinBlock is a Feature or a Rule.
type gherkinBlock struct {
	name        string
	position    SpecPosition
	anchor      *gherkinAnchor
	description []gherkinParagraph
	scenarios   []SpecScenario
}

// gherkinScan is the state of one pass over a file. It keeps the keywords in
// Cucumber's sense: a description runs up to the next keyword or tag, and only
// the line after a step can open a doc string.
type gherkinScan struct {
	file     string
	language gherkinLanguage
	reading  *SpecReading

	feature *gherkinBlock
	rules   []*gherkinBlock
	// owner receives the scenarios, and describing the description lines.
	owner      *gherkinBlock
	describing *gherkinBlock
	blank      bool
	// steps says a Scenario, Background or Examples line opened a step section,
	// and afterStep that the last line with content was a step.
	steps     bool
	afterStep bool
	tags      gherkinTags
	docString string
	docStart  SpecPosition
}

func (scan *gherkinScan) read(text string) {
	for index, line := range strings.Split(text, "\n") {
		if !scan.line(strings.TrimSpace(line), SpecPosition{File: scan.file, Line: index + 1}) {
			return
		}
	}
	scan.finish()
}

// line reads one trimmed line, and reports whether the scan can go on.
func (scan *gherkinScan) line(text string, position SpecPosition) bool {
	switch {
	case scan.docString != "":
		if strings.HasPrefix(text, scan.docString) {
			scan.docString = ""
		}
	case text == "":
		scan.blank = true
	case strings.HasPrefix(text, "#"):
		return scan.comment(text, position)
	case strings.HasPrefix(text, "@"):
		scan.describing, scan.afterStep = nil, false
		scan.tagLine(text, position)
	default:
		return scan.content(text, position)
	}
	return true
}

// comment selects the language: a header counts only above the Feature.
func (scan *gherkinScan) comment(text string, position SpecPosition) bool {
	match := gherkinLanguageHeader.FindStringSubmatch(text)
	if match == nil || scan.feature != nil {
		return true
	}
	language, known := gherkinLanguages()[match[1]]
	if !known {
		scan.fail(position, "Gherkin has no language %q, so this file's keywords cannot be read", match[1])
		return false
	}
	scan.language = language
	return true
}

// content reads a line that is no comment, tag or blank, and reports whether
// the scan can go on.
func (scan *gherkinScan) content(text string, position SpecPosition) bool {
	keyword, name := scan.keywordOf(text)
	afterStep := scan.afterStep
	scan.afterStep = false
	if keyword == gherkinFeature {
		scan.openFeature(name, position)
		return true
	}
	if scan.feature == nil {
		scan.fail(position, "this line comes before the Feature line, and Cucumber refuses it: start the file with Feature:")
		return false
	}
	switch keyword {
	case gherkinRule:
		block := &gherkinBlock{name: name, position: position}
		block.anchor = scan.takeRequirementTag()
		scan.rules = append(scan.rules, block)
		scan.owner, scan.describing, scan.blank, scan.steps = block, block, false, false
	case gherkinScenario:
		scan.openScenario(name, position)
	case gherkinBackground, gherkinExamples:
		scan.refuseTags("a Background or an Examples line")
		scan.describing, scan.steps = nil, true
	default:
		scan.other(text, position, afterStep)
	}
	return true
}

// keywordOf splits a line at the first colon, which no keyword contains.
func (scan *gherkinScan) keywordOf(text string) (gherkinKeyword, string) {
	word, name, found := strings.Cut(text, ":")
	if !found {
		return gherkinNoKeyword, ""
	}
	return scan.language.keywords[word], strings.TrimSpace(name)
}

func (scan *gherkinScan) openFeature(name string, position SpecPosition) {
	if scan.feature != nil {
		scan.fail(position, "a Gherkin file holds one Feature, and this is its second: move it to its own file")
		return
	}
	block := &gherkinBlock{name: name, position: position}
	block.anchor = scan.takeRequirementTag()
	scan.feature, scan.owner, scan.describing = block, block, block
}

func (scan *gherkinScan) openScenario(name string, position SpecPosition) {
	check := ""
	if anchor := scan.tags.check; anchor != nil {
		check = anchor.id
		scan.tags.check = nil
	}
	if anchor := scan.tags.id; anchor != nil {
		scan.fail(anchor.position, "@putnami-id:%s names a requirement: above a scenario, write @putnami-check:<id>", anchor.id)
		scan.tags.id = nil
	}
	if check == "" {
		check, _ = NormalizeRequirementName(name)
	}
	scan.owner.scenarios = append(scan.owner.scenarios, SpecScenario{Name: name, Check: check, Position: position})
	scan.describing, scan.steps = nil, true
}

// other handles a line that is no keyword: description text, a doc string
// opening, or a step and its table, which project nothing.
func (scan *gherkinScan) other(text string, position SpecPosition, afterStep bool) {
	scan.refuseTags("a line that is no Gherkin keyword")
	if scan.describing != nil {
		scan.describe(text, position)
		return
	}
	if !scan.steps {
		return
	}
	for _, separator := range []string{`"""`, "```"} {
		if afterStep && strings.HasPrefix(text, separator) {
			scan.docString, scan.docStart = separator, position
			return
		}
	}
	for _, step := range scan.language.steps {
		if strings.HasPrefix(text, step) || text == strings.TrimSpace(step) {
			scan.afterStep = true
			return
		}
	}
}

func (scan *gherkinScan) describe(text string, position SpecPosition) {
	block := scan.describing
	if last := len(block.description) - 1; last >= 0 && !scan.blank {
		block.description[last].text += " " + text
	} else {
		block.description = append(block.description, gherkinParagraph{text: text, position: position})
	}
	scan.blank = false
}

// tagLine reads the tags of one line as Cucumber does: at each @, up to a
// comment. Other tags are the project's own.
func (scan *gherkinScan) tagLine(text string, position SpecPosition) {
	for _, tag := range strings.Split(gherkinTagComment.ReplaceAllString(text, ""), "@") {
		tag = "@" + strings.TrimSpace(tag)
		name, _, _ := strings.Cut(tag, ":")
		switch name {
		case "@putnami-id":
			scan.readIDTag(tag, position)
		case "@putnami-check":
			scan.readCheckTag(tag, position)
		}
	}
}

func (scan *gherkinScan) readIDTag(tag string, position SpecPosition) {
	id, isAnchor, err := ParseSpecAnchor(tag)
	switch {
	case !isAnchor:
		scan.fail(position, "%s needs an ID: write @putnami-id:<id>", tag)
	case err != nil:
		scan.fail(position, "%v", err)
	case scan.tags.id != nil:
		scan.fail(position, "@putnami-id:%s repeats the tag at line %d", id, scan.tags.id.position.Line)
	default:
		scan.tags.id = &gherkinAnchor{id: id, position: position}
	}
}

func (scan *gherkinScan) readCheckTag(tag string, position SpecPosition) {
	id, _ := strings.CutPrefix(tag, "@putnami-check:")
	switch {
	case !strings.HasPrefix(tag, "@putnami-check:") || len(id) > checkIDMaxLength || !segmentIDPattern.MatchString(id):
		scan.fail(position, "%s must be @putnami-check:<id>, with one lower-case kebab segment of at most %d bytes", tag, checkIDMaxLength)
	case scan.tags.check != nil:
		scan.fail(position, "@putnami-check:%s repeats the tag at line %d", id, scan.tags.check.position.Line)
	default:
		scan.tags.check = &gherkinAnchor{id: id, position: position}
	}
}

// takeRequirementTag gives a Feature or a Rule its @putnami-id tag.
func (scan *gherkinScan) takeRequirementTag() *gherkinAnchor {
	anchor := scan.tags.id
	scan.tags.id = nil
	if check := scan.tags.check; check != nil {
		scan.fail(check.position, "@putnami-check:%s names a scenario's check, and no scenario follows", check.id)
		scan.tags.check = nil
	}
	return anchor
}

// refuseTags fails each anchor tag still waiting: it would otherwise do nothing
// while it looks like it works.
func (scan *gherkinScan) refuseTags(place string) {
	for _, anchor := range []*gherkinAnchor{scan.tags.id, scan.tags.check} {
		if anchor != nil {
			scan.fail(anchor.position, "an anchor tag belongs above a Rule, a Feature or a Scenario, and this one stands above %s", place)
		}
	}
	scan.tags = gherkinTags{}
}

// finish adds the file's outcomes and requirements to the reading.
func (scan *gherkinScan) finish() {
	if scan.feature == nil {
		scan.fail(SpecPosition{File: scan.file, Line: 1}, "this file holds no Feature line in its language")
		return
	}
	scan.refuseTags("nothing")
	if scan.docString != "" {
		scan.fail(scan.docStart, "this doc string never closes, and it hides the rest of the file")
	}
	scan.outcomes()
	feature := scan.feature
	if len(scan.rules) == 0 || len(feature.scenarios) > 0 {
		scan.requirement(feature, feature.name)
	} else if feature.anchor != nil {
		scan.fail(feature.anchor.position, "@putnami-id:%s has no requirement here: this Feature's requirements are its rules", feature.anchor.id)
	}
	for _, rule := range scan.rules {
		scan.requirement(rule, ruleText(rule))
	}
}

func (scan *gherkinScan) outcomes() {
	spec, reading := scan.reading.Spec, scan.reading
	for _, paragraph := range scan.feature.description {
		spec.Outcomes = append(spec.Outcomes, paragraph.text)
		reading.OutcomePositions = append(reading.OutcomePositions, paragraph.position)
	}
	if len(scan.feature.description) > 0 {
		return
	}
	if scan.feature.name == "" {
		scan.fail(scan.feature.position, "this Feature has no name and no description, so it states no outcome")
		return
	}
	spec.Outcomes = append(spec.Outcomes, scan.feature.name)
	reading.OutcomePositions = append(reading.OutcomePositions, scan.feature.position)
}

// ruleText joins a Rule's name and its description, all paragraphs included.
func ruleText(rule *gherkinBlock) string {
	paragraphs := make([]string, 0, len(rule.description))
	for _, paragraph := range rule.description {
		paragraphs = append(paragraphs, paragraph.text)
	}
	switch {
	case len(paragraphs) == 0:
		return rule.name
	case rule.name == "":
		return strings.Join(paragraphs, " ")
	}
	return strings.TrimRight(rule.name, ".:") + ": " + strings.Join(paragraphs, " ")
}

func (scan *gherkinScan) requirement(block *gherkinBlock, text string) {
	id, unanchored := "", false
	if block.anchor != nil {
		id = block.anchor.id
	} else {
		id, unanchored = requirementIDFromName(block.name, len(scan.reading.Spec.Requirements)+1)
	}
	scenarios := block.scenarios
	if scenarios == nil {
		scenarios = []SpecScenario{}
	}
	scan.reading.Spec.Requirements = append(scan.reading.Spec.Requirements, SpecRequirement{ID: id, Text: SpecText{text}})
	scan.reading.Requirements = append(scan.reading.Requirements, SpecRequirementRecord{Position: block.position, Unanchored: unanchored, Scenarios: scenarios})
}

func (scan *gherkinScan) fail(position SpecPosition, format string, args ...any) {
	scan.reading.Findings = append(scan.reading.Findings, diag.Errorf(ErrorCodeSpecRead, position.Field(), format, args...))
}
