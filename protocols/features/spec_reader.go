package features

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// requirementNameMaxLength bounds a normalized name, as checkIDMaxLength bounds
// a check ID, so a long heading cannot become a long baseline identity.
const requirementNameMaxLength = 128

// specReferencePathMaxLength bounds the path of one external spec reference.
const specReferencePathMaxLength = 4096

// specFileExtensions lists the only files each format's reader sees. The spec
// tasks of @putnami/sdd declare a static input for each extension, because a
// task input cannot depend on a manifest's path. A new extension here needs a
// new task input; a test of that extension pins the two together.
var specFileExtensions = map[SpecFormat][]string{
	SpecFormatOpenSpec: {".md"},
	SpecFormatSpecKit:  {".md"},
	SpecFormatKiro:     {".md"},
	SpecFormatGherkin:  {".feature"},
}

// specReaders holds the reader each format ships. A format with no entry is a
// valid wire value that no release can read yet.
var specReaders = map[SpecFormat]SpecReader{
	SpecFormatSpecKit: specKitReader{},
}

var (
	specAnchorComment = regexp.MustCompile(`^<!--\s*putnami:id\s+(\S+)\s*-->$`)
	specAnchorTag     = regexp.MustCompile(`^@putnami-id:(\S+)$`)
)

// SpecFormats returns the closed format vocabulary in sorted order.
func SpecFormats() []SpecFormat {
	formats := make([]SpecFormat, 0, len(validSpecFormats))
	for format := range validSpecFormats {
		formats = append(formats, format)
	}
	sort.Slice(formats, func(i, j int) bool { return formats[i] < formats[j] })
	return formats
}

// SpecFileExtensions returns a fresh copy of the file extensions one format's
// reader sees.
func SpecFileExtensions(format SpecFormat) []string {
	return append([]string(nil), specFileExtensions[format]...)
}

// SpecReaderFor returns the reader a format ships and whether one exists.
func SpecReaderFor(format SpecFormat) (SpecReader, bool) {
	reader, exists := specReaders[format]
	return reader, exists
}

// SpecFile is the bytes of one file of an external spec.
type SpecFile struct {
	// Path is the canonical workspace-relative file path.
	Path string
	// Data is the file's exact bytes.
	Data []byte
}

// SpecInput is everything a reader may look at.
type SpecInput struct {
	// Feature is the identity of the feature that names this spec.
	Feature string
	// Reference is the manifest entry's reference.
	Reference SpecReference
	// Files are the files under Reference.Path, sorted by path, restricted to
	// SpecFileExtensions(Reference.Format).
	Files []SpecFile
}

// SpecPosition locates one line of an external spec.
type SpecPosition struct {
	// File is the canonical workspace-relative file path.
	File string
	// Line is the 1-based line number.
	Line int
}

// Field renders the position in the "<path>#<field>" form every spec finding
// uses, so a finding points into the file.
func (position SpecPosition) Field() string {
	return position.File + "#L" + strconv.Itoa(position.Line)
}

func (position SpecPosition) String() string {
	return position.File + ":" + strconv.Itoa(position.Line)
}

// SpecScenario is one scenario of an external spec.
type SpecScenario struct {
	// Name is the scenario's text, as written.
	Name string
	// Check is the check ID the scenario maps to, empty when it maps to none.
	Check string
	// Position locates the scenario.
	Position SpecPosition
}

// SpecRequirementRecord carries what the native spec cannot about one
// requirement.
type SpecRequirementRecord struct {
	// Position locates the requirement.
	Position SpecPosition
	// Unanchored says the ID comes from the requirement's position, such as
	// FR-001, and no anchor pins it. A project in enforce refuses it.
	Unanchored bool
	// Scenarios lists the requirement's scenarios in file order. Nil says the
	// format attaches no scenarios to this requirement, so it is not compared;
	// an empty list says it can and has none.
	Scenarios []SpecScenario
}

// SpecReading is what a reader returns.
type SpecReading struct {
	// Spec is the projection, nil when the bytes cannot produce one. The reader
	// leaves Feature and ProtocolVersion to ReadSpecSource.
	Spec *Spec
	// Requirements holds one record per Spec.Requirements entry, in order.
	Requirements []SpecRequirementRecord
	// Scenarios lists, in file order, the scenarios that belong to the file and
	// not to one requirement. Nil says the format has none.
	Scenarios []SpecScenario
	// TextPositions locates a spec field the records do not, such as
	// "outcomes[0]" or "nonGoals[1]", so a finding on it points at its line.
	TextPositions map[string]SpecPosition
	// Findings are positioned diagnostics; build them with SpecPosition.Field.
	Findings []diag.Diagnostic
	// EnforceFindings are warnings that block only in enforce. A reader does not
	// know the mode: SpecProjection.EnforceOnly applies it.
	EnforceFindings []diag.Diagnostic
}

// SpecReader turns the bytes of one external spec into a native spec. An
// implementation is pure and deterministic: no network, no model, no clock, no
// filesystem. Call it through ReadSpecSource, which applies the contract.
type SpecReader interface {
	Read(input SpecInput) SpecReading
}

// SpecProjection is a contract-checked reading.
type SpecProjection struct {
	// Spec is the validated projection, nil when any finding is an error.
	Spec *Spec
	// Requirements is parallel to Spec.Requirements, nil when Spec is nil.
	Requirements []SpecRequirementRecord
	// Scenarios are the reading's file scenarios, nil when Spec is nil.
	Scenarios []SpecScenario
	// Findings are the sorted reader and contract findings.
	Findings []diag.Diagnostic
	// EnforceFindings are the reading's enforce findings, nil when Spec is nil.
	// Read them through EnforceOnly.
	EnforceFindings []diag.Diagnostic
}

// EnforceOnly returns what depends on the verification mode. In enforce, an
// enforce finding is an error and so is each unanchored requirement, because
// reordering the file would change the ID the baseline records. Otherwise an
// enforce finding stays a warning and an unanchored requirement has no finding.
func (projection SpecProjection) EnforceOnly(mode VerificationMode) []diag.Diagnostic {
	enforce := mode == VerificationModeEnforce
	findings := make([]diag.Diagnostic, 0, len(projection.EnforceFindings))
	for _, finding := range projection.EnforceFindings {
		if enforce {
			finding.Severity = diag.Error
		}
		findings = append(findings, finding)
	}
	if enforce && projection.Spec != nil {
		for index, record := range projection.Requirements {
			if record.Unanchored {
				findings = append(findings, diag.Errorf(ErrorCodeSpecRequirementUnanchored, record.Position.Field(),
					"requirement %q takes its ID from its position: write an anchor above it before this project can enforce specs",
					projection.Spec.Requirements[index].ID))
			}
		}
	}
	sortDiagnostics(findings)
	return findings
}

// ReadSpecSource runs reader under the contract every format shares: the
// projection is a valid native spec for input.Feature, a requirement over the
// native bound is an error that names a split, two requirements with one ID
// name both positions, and a validation finding points at the file.
func ReadSpecSource(reader SpecReader, input SpecInput) SpecProjection {
	input.Files = append([]SpecFile(nil), input.Files...)
	sort.Slice(input.Files, func(i, j int) bool { return input.Files[i].Path < input.Files[j].Path })

	reading := reader.Read(input)
	findings := append([]diag.Diagnostic(nil), reading.Findings...)
	if reading.Spec == nil {
		if !diag.HasErrors(findings) {
			findings = append(findings, diag.Errorf(ErrorCodeSpecRead, input.Reference.Path, "the %s reader found no spec here", input.Reference.Format))
		}
		return finishProjection(nil, SpecReading{}, findings)
	}
	spec := *reading.Spec
	spec.Feature = input.Feature
	spec.ProtocolVersion = SpecProtocolVersion
	if spec.Outcomes == nil {
		spec.Outcomes = []string{}
	}
	if spec.Requirements == nil {
		spec.Requirements = []SpecRequirement{}
	}
	if len(reading.Requirements) != len(spec.Requirements) {
		findings = append(findings, diag.Errorf(ErrorCodeSpecRead, input.Reference.Path,
			"the %s reader returned %d records for %d requirements", input.Reference.Format, len(reading.Requirements), len(spec.Requirements)))
		return finishProjection(nil, SpecReading{}, findings)
	}

	first := make(map[string]SpecPosition, len(spec.Requirements))
	refused := make(map[string]bool)
	for i, requirement := range spec.Requirements {
		position := reading.Requirements[i].Position
		if len(requirement.Text) > specTextMaxLength {
			refused[fmt.Sprintf("requirements[%d].text", i)] = true
			findings = append(findings, diag.Errorf(ErrorCodeSpecRequirementTooLong, position.Field(),
				"requirement %q has %d bytes, over the %d-byte bound: split it into two requirements", requirement.ID, len(requirement.Text), specTextMaxLength))
		}
		if earlier, seen := first[requirement.ID]; seen {
			refused[fmt.Sprintf("requirements[%d].id", i)] = true
			findings = append(findings, diag.Errorf(ErrorCodeInvalidRequirement, position.Field(),
				"requirement ID %q at %s is also the ID at %s", requirement.ID, position, earlier))
		} else {
			first[requirement.ID] = position
		}
	}
	for _, finding := range ValidateSpec(&spec) {
		if refused[finding.Field] {
			continue
		}
		finding.Field = specFindingField(finding.Field, input.Reference.Path, reading)
		findings = append(findings, finding)
	}
	if diag.HasErrors(findings) {
		return finishProjection(nil, SpecReading{}, findings)
	}
	return finishProjection(&spec, reading, findings)
}

func finishProjection(spec *Spec, reading SpecReading, findings []diag.Diagnostic) SpecProjection {
	sortDiagnostics(findings)
	return SpecProjection{
		Spec: spec, Requirements: reading.Requirements, Scenarios: reading.Scenarios,
		Findings: findings, EnforceFindings: reading.EnforceFindings,
	}
}

// specFindingField moves a field of the projected spec to the external file: a
// finding on a requirement or a located text to its line, and any other to the
// reference.
func specFindingField(field, referencePath string, reading SpecReading) string {
	if position, located := reading.TextPositions[field]; located {
		return position.Field()
	}
	records := reading.Requirements
	if rest, found := strings.CutPrefix(field, "requirements["); found {
		if digits, _, closed := strings.Cut(rest, "]"); closed {
			if index, err := strconv.Atoi(digits); err == nil && index >= 0 && index < len(records) {
				return records[index].Position.Field()
			}
		}
	}
	if field == "" {
		return referencePath
	}
	return referencePath + "#" + field
}

// CompareSpecScenarios compares a projection's scenarios with the checks its
// feature declares, for each requirement whose format attaches scenarios, and
// for the file's own scenarios against every check its requirements declare. A
// scenario with no declared check and a declared check with no scenario are
// warnings. The comparison only reads: scenarios never define or shrink the
// check set.
func CompareSpecScenarios(projection SpecProjection, feature Feature) []diag.Diagnostic {
	if projection.Spec == nil {
		return nil
	}
	declared := make(map[string][]string, len(feature.Requirements))
	for _, requirement := range feature.Requirements {
		if requirement.Verification != nil {
			declared[requirement.ID] = requirement.Verification.Checks
		}
	}
	var findings []diag.Diagnostic
	for i, requirement := range projection.Spec.Requirements {
		record := projection.Requirements[i]
		if record.Scenarios == nil {
			continue
		}
		checks := make(map[string]bool, len(declared[requirement.ID]))
		for _, check := range declared[requirement.ID] {
			checks[check] = true
		}
		matched := make(map[string]bool, len(record.Scenarios))
		for _, scenario := range record.Scenarios {
			if scenario.Check != "" && checks[scenario.Check] {
				matched[scenario.Check] = true
				continue
			}
			findings = append(findings, diag.Warningf(WarningCodeSpecScenarioMismatch, scenario.Position.Field(),
				"scenario %q of requirement %q matches no declared check", scenario.Name, requirement.ID))
		}
		for _, check := range declared[requirement.ID] {
			if !matched[check] {
				findings = append(findings, diag.Warningf(WarningCodeSpecScenarioMismatch, record.Position.Field(),
					"declared check %q of requirement %q has no scenario", check, requirement.ID))
			}
		}
	}
	if projection.Scenarios != nil {
		findings = append(findings, compareFileScenarios(projection, declared)...)
	}
	sortDiagnostics(findings)
	return findings
}

// compareFileScenarios compares scenarios that belong to no requirement with
// the union of the projected requirements' checks.
func compareFileScenarios(projection SpecProjection, declared map[string][]string) []diag.Diagnostic {
	checks := make(map[string]bool)
	for _, requirement := range projection.Spec.Requirements {
		for _, check := range declared[requirement.ID] {
			checks[check] = true
		}
	}
	var findings []diag.Diagnostic
	matched := make(map[string]bool, len(projection.Scenarios))
	for _, scenario := range projection.Scenarios {
		if scenario.Check != "" && checks[scenario.Check] {
			matched[scenario.Check] = true
			continue
		}
		findings = append(findings, diag.Warningf(WarningCodeSpecScenarioMismatch, scenario.Position.Field(),
			"scenario %q matches no declared check", scenario.Name))
	}
	for i, requirement := range projection.Spec.Requirements {
		for _, check := range declared[requirement.ID] {
			if !matched[check] {
				findings = append(findings, diag.Warningf(WarningCodeSpecScenarioMismatch, projection.Requirements[i].Position.Field(),
					"declared check %q of requirement %q has no scenario", check, requirement.ID))
			}
		}
	}
	return findings
}

// NormalizeRequirementName maps a name-based requirement to its ID. It
// lowercases ASCII letters, keeps ASCII digits, turns each other run of ASCII
// characters into one hyphen and trims the ends. A name with a non-ASCII
// character, with nothing left, or longer than the bound has no ID: its author
// writes an anchor.
func NormalizeRequirementName(name string) (string, bool) {
	var out strings.Builder
	pending := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 0x80:
			return "", false
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		default:
			pending = true
			continue
		}
		if pending && out.Len() > 0 {
			out.WriteByte('-')
		}
		pending = false
		out.WriteByte(c)
	}
	id := out.String()
	if id == "" || len(id) > requirementNameMaxLength {
		return "", false
	}
	return id, true
}

// ParseSpecAnchor reads one line. isAnchor says the line is an anchor, and err
// says its ID breaks the native segment grammar. An anchor stands alone on the
// line above its requirement: `<!-- putnami:id <segment-id> -->`, or the tag
// `@putnami-id:<segment-id>` in a Gherkin file.
func ParseSpecAnchor(line string) (id string, isAnchor bool, err error) {
	trimmed := strings.TrimSpace(line)
	match := specAnchorComment.FindStringSubmatch(trimmed)
	if match == nil {
		match = specAnchorTag.FindStringSubmatch(trimmed)
	}
	if match == nil {
		return "", false, nil
	}
	if !segmentIDPattern.MatchString(match[1]) {
		return "", true, fmt.Errorf("anchor %q must be one lower-case kebab segment", match[1])
	}
	return match[1], true, nil
}

// IsDecisionLink reports whether a link is a durable decision record. Only such
// a link may enter Spec.Decisions: design.md and plan.md are context.
func IsDecisionLink(link string) bool {
	return len(link) <= decisionLinkMaxLength && decisionLinkPattern.MatchString(link)
}
