package conformance

import (
	"bytes"
	"os"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features"
)

// MinimalSpecReader reads a line format that exists only to exercise the
// reader contract without a market format. Each line is one of:
//
//	outcome <text>
//	requirement <name> | <text>
//	positional <label> | <text>
//	scenario <name>
//	<!-- putnami:id <segment-id> -->
//
// A format no product ships keeps the contract's tests apart from any reader's.
type MinimalSpecReader struct{}

// Read implements features.SpecReader.
func (MinimalSpecReader) Read(input features.SpecInput) features.SpecReading {
	reading := features.SpecReading{Spec: &features.Spec{}}
	for _, file := range input.Files {
		anchor := ""
		for index, line := range strings.Split(string(file.Data), "\n") {
			position := features.SpecPosition{File: file.Path, Line: index + 1}
			if id, isAnchor, err := features.ParseSpecAnchor(line); isAnchor {
				if err != nil {
					reading.Findings = append(reading.Findings, diag.Errorf(features.ErrorCodeSpecRead, position.Field(), "%v", err))
				}
				anchor = id
				continue
			}
			keyword, rest, _ := strings.Cut(line, " ")
			switch keyword {
			case "outcome":
				reading.Spec.Outcomes = append(reading.Spec.Outcomes, rest)
			case "requirement", "positional":
				name, text, _ := strings.Cut(rest, " | ")
				id, ok := anchor, anchor != ""
				unanchored := false
				if !ok {
					id, ok = features.NormalizeRequirementName(name)
					unanchored = keyword == "positional"
				}
				if !ok {
					reading.Findings = append(reading.Findings, diag.Errorf(features.ErrorCodeSpecRead, position.Field(), "requirement %q has no ID: add an anchor", name))
					continue
				}
				reading.Spec.Requirements = append(reading.Spec.Requirements, features.SpecRequirement{ID: id, Text: features.SpecText{text}})
				reading.Requirements = append(reading.Requirements, features.SpecRequirementRecord{Position: position, Unanchored: unanchored, Scenarios: []features.SpecScenario{}})
				anchor = ""
			case "scenario":
				if last := len(reading.Requirements) - 1; last >= 0 {
					check, _ := features.NormalizeRequirementName(rest)
					reading.Requirements[last].Scenarios = append(reading.Requirements[last].Scenarios,
						features.SpecScenario{Name: rest, Check: check, Position: position})
				}
			}
		}
	}
	return reading
}

// RunSpecSource asserts that reader projects input to the committed golden spec
// with no error, and that a second read gives the same bytes and findings.
func RunSpecSource(t *testing.T, reader features.SpecReader, input features.SpecInput, goldenFile string) {
	t.Helper()
	golden, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf("read golden spec: %v", err)
	}
	first := features.ReadSpecSource(reader, input)
	if first.Spec == nil || diag.HasErrors(first.Findings) {
		t.Fatalf("spec source did not project: %v", first.Findings)
	}
	canonical, err := features.MarshalSpec(first.Spec)
	if err != nil {
		t.Fatalf("marshal projected spec: %v", err)
	}
	if !bytes.Equal(canonical, golden) {
		t.Fatalf("projected spec differs\n--- golden ---\n%s\n--- projected ---\n%s", golden, canonical)
	}
	second := features.ReadSpecSource(reader, input)
	again, err := features.MarshalSpec(second.Spec)
	if err != nil || !bytes.Equal(again, canonical) {
		t.Fatalf("a second read gave other bytes: %v", err)
	}
	if len(first.Findings) != len(second.Findings) {
		t.Fatalf("a second read gave other findings: %v, %v", first.Findings, second.Findings)
	}
}
