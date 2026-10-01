package ci

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const validReview = `{"enabled":true,"fallbackEngine":"codex","focus":["security"],"instructions":[]}`

func reviewDocument(profile string) []byte {
	return []byte(`{"version":3,"commands":["test"],"review":` + profile + `}`)
}

func TestReviewRejectsAuthorityAndMalformedInput(t *testing.T) {
	cases := map[string]string{
		"null":              "null",
		"missing-enabled":   `{"fallbackEngine":"codex","focus":["security"],"instructions":[]}`,
		"null-enabled":      strings.Replace(validReview, "true", "null", 1),
		"case-variant":      strings.Replace(validReview, "enabled", "Enabled", 1),
		"unknown-engine":    strings.Replace(validReview, "codex", "arbitrary", 1),
		"empty-focus":       strings.Replace(validReview, `["security"]`, `[]`, 1),
		"duplicate-focus":   strings.Replace(validReview, `["security"]`, `["security","security"]`, 1),
		"unknown-focus":     strings.Replace(validReview, "security", "run-commands", 1),
		"null-instructions": strings.Replace(validReview, `"instructions":[]`, `"instructions":null`, 1),
		"duplicate-key":     strings.Replace(validReview, `"enabled":true`, `"enabled":true,"enabled":false`, 1),
		"null-element":      strings.Replace(validReview, `"instructions":[]`, `"instructions":[null]`, 1),
	}
	for _, field := range []string{"credential", "image", "command", "mcp", "provider", "approval"} {
		cases[field] = strings.TrimSuffix(validReview, "}") + `,"` + field + `":"untrusted"}`
	}
	for name, profile := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(reviewDocument(profile)); err == nil {
				t.Fatal("unsafe profile accepted")
			}
		})
	}
}

func TestReviewBoundsAreFieldAddressable(t *testing.T) {
	profile := ReviewProfile{Enabled: true, FallbackEngine: "codex", Focus: []string{"security"}, Instructions: []string{}}
	cases := []struct {
		name, field string
		mutate      func(*ReviewProfile)
	}{
		{"too-many", "review.instructions", func(p *ReviewProfile) { p.Instructions = make([]string, MaxReviewInstructions+1) }},
		{"too-long", "review.instructions[0]", func(p *ReviewProfile) { p.Instructions = []string{strings.Repeat("é", MaxReviewInstructionLength+1)} }},
		{"leading-space", "review.instructions[0]", func(p *ReviewProfile) { p.Instructions = []string{" check"} }},
		{"control", "review.instructions[0]", func(p *ReviewProfile) { p.Instructions = []string{"check\npermissions"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := profile
			tc.mutate(&p)
			findings := Validate(Document{Version: Version, Commands: []CommandEntry{{Name: "test"}}, Review: &p})
			for _, finding := range findings {
				if finding.Code == "ci.invalid_review" && finding.Field == tc.field {
					return
				}
			}
			t.Fatalf("missing field %s: %v", tc.field, findings)
		})
	}
	profile.Instructions = []string{strings.Repeat("é", MaxReviewInstructionLength)}
	if findings := Validate(Document{Version: Version, Commands: []CommandEntry{{Name: "test"}}, Review: &profile}); diag.HasErrors(findings) {
		t.Fatalf("character limit rejected: %v", findings)
	}
}

func TestReviewCanonicalizationPreservesGuidanceAndIsolation(t *testing.T) {
	document := mustParse(t, readFixture(t, "valid", "review.json"))
	before, _ := CanonicalBytes(document)
	normalized := Normalize(document)
	normalized.Review.Focus[0] = "tests"
	normalized.Review.Instructions[0] = "changed"
	after, _ := CanonicalBytes(document)
	if !bytes.Equal(before, after) {
		t.Fatal("normalization aliases source slices")
	}
	digest, _ := Digest(document)
	changed, _ := Digest(normalized)
	if digest == changed {
		t.Fatal("guidance missing from digest")
	}
	if HasProviderSections(document) {
		t.Fatal("review falsely requires a release-set provider")
	}
	disabled, err := Parse(reviewDocument(strings.Replace(validReview, "true", "false", 1)))
	if err != nil || disabled.Review == nil || disabled.Review.Enabled {
		t.Fatalf("disabled: %v", err)
	}
	formatted, err := Format(reviewDocument(validReview))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip := mustParse(t, formatted)
	if roundTrip.Review.Instructions == nil {
		t.Fatal("empty instructions became null")
	}
}

func TestReviewAbsentPreservesExistingCanonicalBytes(t *testing.T) {
	data := []byte(`{"version":3,"commands":["test"]}`)
	document := mustParse(t, data)
	got, _ := CanonicalBytes(document)
	want := `{"$schema":"https://putnami.dev/schemas/putnami-ci.json","version":3,"commands":["test"]}`
	if string(got) != want || document.Review != nil {
		t.Fatalf("existing document changed: %s", got)
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	var defs map[string]json.RawMessage
	if err := json.Unmarshal(schema["$defs"], &defs); err != nil {
		t.Fatal(err)
	}
	var profile struct {
		AdditionalProperties bool     `json:"additionalProperties"`
		Required             []string `json:"required"`
	}
	if err := json.Unmarshal(defs["reviewProfile"], &profile); err != nil {
		t.Fatal(err)
	}
	if profile.AdditionalProperties || !reflect.DeepEqual(profile.Required, []string{"enabled", "fallbackEngine", "focus", "instructions"}) {
		t.Fatal("review schema boundary drift")
	}
}
