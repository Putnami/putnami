package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// MaxReviewInstructions bounds source-controlled guidance.
	MaxReviewInstructions = 32
	// MaxReviewInstructionLength bounds one instruction in Unicode characters.
	MaxReviewInstructionLength = 2_000
)

// ReviewProfile is target-revision semantic review intent. The execution plane
// owns enrollment, model availability, secrets, images, tools and publication.
// Omission of Document.Review requests no review.
type ReviewProfile struct {
	Enabled        bool     `json:"enabled"`
	FallbackEngine string   `json:"fallbackEngine"`
	Focus          []string `json:"focus"`
	Instructions   []string `json:"instructions"`
}

// UnmarshalJSON preserves required false/empty values while rejecting missing,
// null, unknown and case-variant properties at this authority boundary.
func (profile *ReviewProfile) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	required := []string{"enabled", "fallbackEngine", "focus", "instructions"}
	if len(fields) != len(required) {
		return fmt.Errorf("review requires exactly enabled, fallbackEngine, focus and instructions")
	}
	for _, name := range required {
		raw, exists := fields[name]
		if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("review.%s is required and must not be null", name)
		}
	}
	type plain ReviewProfile
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*profile = ReviewProfile(decoded)
	return nil
}

func validateReview(add func(diag.Diagnostic), profile *ReviewProfile) {
	if profile == nil {
		return
	}
	if profile.FallbackEngine != "codex" && profile.FallbackEngine != "claude-code" {
		add(diag.Errorf("ci.invalid_review", "review.fallbackEngine", "must be codex or claude-code"))
	}
	allowed := map[string]bool{"correctness": true, "security": true, "architecture": true, "performance": true, "maintainability": true, "tests": true}
	if len(profile.Focus) == 0 || len(profile.Focus) > len(allowed) {
		add(diag.Errorf("ci.invalid_review", "review.focus", "must contain one to six distinct review areas"))
	}
	seen := map[string]bool{}
	for index, focus := range profile.Focus {
		if !allowed[focus] || seen[focus] {
			add(diag.Errorf("ci.invalid_review", fmt.Sprintf("review.focus[%d]", index), "must be a distinct supported review area"))
		}
		seen[focus] = true
	}
	if profile.Instructions == nil || len(profile.Instructions) > MaxReviewInstructions {
		add(diag.Errorf("ci.invalid_review", "review.instructions", "must be an array with at most %d instructions", MaxReviewInstructions))
	}
	for index, instruction := range profile.Instructions {
		if instruction == "" || !utf8.ValidString(instruction) || utf8.RuneCountInString(instruction) > MaxReviewInstructionLength ||
			instruction != strings.TrimSpace(instruction) || strings.IndexFunc(instruction, unicode.IsControl) >= 0 {
			add(diag.Errorf("ci.invalid_review", fmt.Sprintf("review.instructions[%d]", index), "must contain 1..%d characters with no controls or surrounding whitespace", MaxReviewInstructionLength))
		}
	}
}
