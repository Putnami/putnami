package features

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// SpecGapKind names one way an external spec gives less than a native spec. A
// value is a public wire value: it can be added to, not changed.
type SpecGapKind string

const (
	// SpecGapUnanchoredRequirement is a requirement whose ID comes from its
	// position, such as FR-001, with no anchor to pin it.
	SpecGapUnanchoredRequirement SpecGapKind = "unanchored_requirement"
	// SpecGapDerivedSection is a section that restates a fact Putnami derives.
	SpecGapDerivedSection SpecGapKind = "derived_section"
	// SpecGapContextLink is a link in the projected prose that is not a
	// doc/adr/*.md decision record.
	SpecGapContextLink SpecGapKind = "context_link"
	// SpecGapScenarioMismatch is a scenario with no declared check, or a
	// declared check with no scenario.
	SpecGapScenarioMismatch SpecGapKind = "scenario_mismatch"
)

// SpecGap is one thing a native rewrite of an external spec adds. It is
// information, never a finding: no verification mode turns it into one.
type SpecGap struct {
	Kind SpecGapKind `json:"kind"`
	// Field is the position in the "<path>#L<line>" form findings use, or the
	// reference path when the reader gave the text no position.
	Field   string `json:"field"`
	Message string `json:"message"`
	// Step is the edit that removes the gap.
	Step string `json:"step"`
}

const (
	specGapDerivedStep  = "delete the section: Putnami derives this fact, and a native spec has no place for it"
	specGapLinkStep     = "record the decision in a doc/adr/*.md file and link that file; any other link stays context, and a native spec drops it from its decisions"
	specGapScenarioStep = "write an anchor that names the check above the scenario, or change verification.checks in the feature declaration"
)

// SpecNativeGaps lists what a native rewrite of feature's external spec adds,
// sorted by position. A feature with no external reference, or a projection
// with no spec, has no gap. The result reads only the projection and the
// feature, so the same inputs give the same gaps.
func SpecNativeGaps(projection SpecProjection, feature Feature) []SpecGap {
	if projection.Spec == nil || feature.Spec == nil {
		return nil
	}
	reference := *feature.Spec
	// A native spec has no section, no context link and no scenario, so the
	// rewrite removes those gaps. specs init --from refuses a source enforce
	// refuses, such as an unanchored requirement or a text over the 512-byte
	// bound (D-043, D-056), so until those are fixed the step says so.
	command := fmt.Sprintf("`putnami specs init %s --from %s:%s`", feature.ID, reference.Format, reference.Path)
	rewrite := "; or draft a native spec with " + command
	if diag.HasErrors(projection.EnforceOnly(VerificationModeEnforce)) {
		rewrite = "; a native spec with " + command + " waits until the findings enforce refuses are fixed"
	}
	var gaps []SpecGap
	for i, record := range projection.Requirements {
		if record.Unanchored {
			id := projection.Spec.Requirements[i].ID
			gaps = append(gaps, SpecGap{
				Kind: SpecGapUnanchoredRequirement, Field: record.Position.Field(),
				Message: fmt.Sprintf("requirement %q takes its ID from its position, so reordering the file changes it", id),
				Step:    "write " + specAnchorExample(reference.Format, id) + " on the line above it",
			})
		}
	}
	for _, finding := range projection.Findings {
		if finding.Code == WarningCodeSpecDerivedFact {
			gaps = append(gaps, SpecGap{Kind: SpecGapDerivedSection, Field: finding.Field, Message: finding.Message, Step: specGapDerivedStep + rewrite})
		}
	}
	for _, gap := range specContextLinkGaps(projection, reference) {
		gap.Step += rewrite
		gaps = append(gaps, gap)
	}
	for _, finding := range CompareSpecScenarios(projection, feature) {
		gaps = append(gaps, SpecGap{Kind: SpecGapScenarioMismatch, Field: finding.Field, Message: finding.Message, Step: specGapScenarioStep + rewrite})
	}
	sortSpecGaps(gaps)
	return gaps
}

// specAnchorExample is the anchor an author writes in this format.
func specAnchorExample(format SpecFormat, id string) string {
	if format == SpecFormatGherkin {
		return "`@putnami-id:" + id + "`"
	}
	return "`<!-- putnami:id " + id + " -->`"
}

// specContextLinkGaps finds each link of the projected prose that is not a
// decision record. A link that starts with a dot is relative to its file.
func specContextLinkGaps(projection SpecProjection, reference SpecReference) []SpecGap {
	type located struct {
		text     string
		position *SpecPosition
	}
	texts := make([]located, 0, len(projection.Spec.Outcomes)+len(projection.Spec.NonGoals)+len(projection.Spec.Requirements))
	at := func(positions []SpecPosition, index int) *SpecPosition {
		if index < len(positions) {
			return &positions[index]
		}
		return nil
	}
	for i, text := range projection.Spec.Outcomes {
		texts = append(texts, located{text, at(projection.outcomePositions, i)})
	}
	for i, text := range projection.Spec.NonGoals {
		texts = append(texts, located{text, at(projection.nonGoalPositions, i)})
	}
	for i, requirement := range projection.Spec.Requirements {
		texts = append(texts, located{requirement.Text.String(), &projection.Requirements[i].Position})
	}
	var gaps []SpecGap
	seen := make(map[string]bool)
	for _, entry := range texts {
		field := reference.Path
		if entry.position != nil {
			field = entry.position.Field()
		}
		for _, match := range markdownLink.FindAllStringSubmatch(entry.text, -1) {
			link, _, _ := strings.Cut(match[1], "#")
			if link == "" {
				continue
			}
			if specDecisionLink(link, entry.position, reference) || seen[field+"\x00"+link] {
				continue
			}
			seen[field+"\x00"+link] = true
			gaps = append(gaps, SpecGap{
				Kind: SpecGapContextLink, Field: field,
				Message: fmt.Sprintf("link %q is context, not a decision: a native spec links only doc/adr/*.md records", link),
				Step:    specGapLinkStep,
			})
		}
	}
	return gaps
}

// specDecisionLink reports whether a link names a decision record. A link that
// starts with a dot is relative to its file; without a position it is tried
// against the reference, as a file and as a directory.
func specDecisionLink(link string, position *SpecPosition, reference SpecReference) bool {
	if !strings.HasPrefix(link, ".") {
		return IsDecisionLink(link)
	}
	bases := []string{reference.Path, path.Dir(reference.Path)}
	if position != nil {
		bases = []string{path.Dir(position.File)}
	}
	for _, base := range bases {
		if IsDecisionLink(path.Join(base, link)) {
			return true
		}
	}
	return false
}

// sortSpecGaps orders gaps by file, then line as a number, then kind and
// message, so line 9 comes before line 10.
func sortSpecGaps(gaps []SpecGap) {
	sort.SliceStable(gaps, func(i, j int) bool {
		leftFile, leftLine := splitSpecGapField(gaps[i].Field)
		rightFile, rightLine := splitSpecGapField(gaps[j].Field)
		switch {
		case leftFile != rightFile:
			return leftFile < rightFile
		case leftLine != rightLine:
			return leftLine < rightLine
		case gaps[i].Kind != gaps[j].Kind:
			return gaps[i].Kind < gaps[j].Kind
		default:
			return gaps[i].Message < gaps[j].Message
		}
	})
}

func splitSpecGapField(field string) (string, int) {
	file, rest, found := strings.Cut(field, "#L")
	if !found {
		return field, 0
	}
	line, err := strconv.Atoi(rest)
	if err != nil {
		return field, 0
	}
	return file, line
}
