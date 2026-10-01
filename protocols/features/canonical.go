package features

import (
	"encoding/json"
	"sort"
)

// CanonicalManifest returns a sorted copy without mutating caller-owned
// slices. Go string comparison is bytewise, matching the UTF-8 wire contract.
func CanonicalManifest(input *Manifest) *Manifest {
	if input == nil {
		return nil
	}
	out := *input
	out.Features = append([]Feature(nil), input.Features...)
	for i := range out.Features {
		out.Features[i].Relations = append([]Relation(nil), out.Features[i].Relations...)
		sort.Slice(out.Features[i].Relations, func(a, b int) bool {
			left, right := out.Features[i].Relations[a], out.Features[i].Relations[b]
			if left.Kind != right.Kind {
				return left.Kind < right.Kind
			}
			return left.Target < right.Target
		})
		out.Features[i].Requirements = append([]Requirement(nil), out.Features[i].Requirements...)
		for j := range out.Features[i].Requirements {
			out.Features[i].Requirements[j].EvidenceKinds = append([]EvidenceKind(nil), out.Features[i].Requirements[j].EvidenceKinds...)
			sort.Slice(out.Features[i].Requirements[j].EvidenceKinds, func(a, b int) bool {
				return out.Features[i].Requirements[j].EvidenceKinds[a] < out.Features[i].Requirements[j].EvidenceKinds[b]
			})
			out.Features[i].Requirements[j].Verification = canonicalVerificationCriterion(out.Features[i].Requirements[j].Verification)
		}
		sort.Slice(out.Features[i].Requirements, func(a, b int) bool {
			left, lok := StageRank(out.Features[i].Requirements[a].Stage)
			right, rok := StageRank(out.Features[i].Requirements[b].Stage)
			if lok != rok {
				return lok
			}
			if left != right {
				return left < right
			}
			return out.Features[i].Requirements[a].ID < out.Features[i].Requirements[b].ID
		})
	}
	sort.Slice(out.Features, func(i, j int) bool { return out.Features[i].ID < out.Features[j].ID })
	return &out
}

// canonicalVerificationCriterion deep-copies one criterion and sorts its
// expected checks. Checks are an unordered set of keyed identities, so sorting
// normalizes them; the pointer is copied so canonicalization can never reorder
// a caller-owned criterion in place.
func canonicalVerificationCriterion(input *VerificationCriterion) *VerificationCriterion {
	if input == nil {
		return nil
	}
	out := *input
	out.Checks = cloneStrings(input.Checks)
	sort.Strings(out.Checks)
	if input.Target != nil {
		target := *input.Target
		out.Target = &target
	}
	if input.Window != nil {
		window := *input.Window
		out.Window = &window
	}
	return &out
}

// CanonicalVerificationReport returns a sorted copy without mutating the input.
// Observations sort by the join key they are read through, so two runs that
// observe the same checks in different orders produce the same bytes.
func CanonicalVerificationReport(input *VerificationReport) *VerificationReport {
	if input == nil {
		return nil
	}
	out := *input
	out.Observations = append([]VerificationObservation(nil), input.Observations...)
	for i := range out.Observations {
		if measurement := out.Observations[i].Measurement; measurement != nil {
			copied := *measurement
			out.Observations[i].Measurement = &copied
		}
		if window := out.Observations[i].Window; window != nil {
			copied := *window
			out.Observations[i].Window = &copied
		}
	}
	sort.Slice(out.Observations, func(i, j int) bool {
		left, right := out.Observations[i], out.Observations[j]
		if left.Feature != right.Feature {
			return left.Feature < right.Feature
		}
		if left.Requirement != right.Requirement {
			return left.Requirement < right.Requirement
		}
		return left.Check < right.Check
	})
	return &out
}

// CanonicalEvidenceDocument returns a sorted copy without mutating the input.
func CanonicalEvidenceDocument(input *EvidenceDocument) *EvidenceDocument {
	if input == nil {
		return nil
	}
	out := *input
	out.Evidence = append([]EvidenceRecord(nil), input.Evidence...)
	sort.Slice(out.Evidence, func(i, j int) bool {
		left, right := out.Evidence[i], out.Evidence[j]
		if left.Feature != right.Feature {
			return left.Feature < right.Feature
		}
		leftRank, leftOK := StageRank(left.Stage)
		rightRank, rightOK := StageRank(right.Stage)
		if leftOK != rightOK {
			return leftOK
		}
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.Requirement != right.Requirement {
			return left.Requirement < right.Requirement
		}
		return left.ID < right.ID
	})
	return &out
}

// CanonicalSpec returns a sorted copy without mutating the input.
//
// Requirements sort by their stable ID and decision links sort by canonical
// path, because both are keyed identities whose authored order carries no
// meaning. Outcomes and non-goals keep their authored order: they are ordered
// prose, so reordering them would rewrite the statement rather than normalize
// it. Empty and absent collections stay distinct so canonical bytes round-trip.
func CanonicalSpec(input *Spec) *Spec {
	if input == nil {
		return nil
	}
	out := *input
	out.Outcomes = cloneStrings(input.Outcomes)
	out.NonGoals = cloneStrings(input.NonGoals)
	out.Requirements = cloneSpecRequirements(input.Requirements)
	sort.Slice(out.Requirements, func(i, j int) bool { return out.Requirements[i].ID < out.Requirements[j].ID })
	out.Decisions = cloneStrings(input.Decisions)
	sort.Strings(out.Decisions)
	return &out
}

func cloneStrings(input []string) []string {
	if input == nil {
		return nil
	}
	out := make([]string, len(input))
	copy(out, input)
	return out
}

func cloneSpecRequirements(input []SpecRequirement) []SpecRequirement {
	if input == nil {
		return nil
	}
	out := make([]SpecRequirement, len(input))
	copy(out, input)
	return out
}

// MarshalManifest returns canonical two-space-indented JSON with one trailing
// newline.
func MarshalManifest(manifest *Manifest) ([]byte, error) {
	return marshalCanonical(CanonicalManifest(manifest))
}

// MarshalEvidenceDocument returns canonical two-space-indented JSON with one
// trailing newline.
func MarshalEvidenceDocument(document *EvidenceDocument) ([]byte, error) {
	return marshalCanonical(CanonicalEvidenceDocument(document))
}

// MarshalSpec returns canonical two-space-indented JSON with one trailing
// newline.
func MarshalSpec(spec *Spec) ([]byte, error) {
	return marshalCanonical(CanonicalSpec(spec))
}

// MarshalVerificationReport returns canonical two-space-indented JSON with one
// trailing newline.
func MarshalVerificationReport(report *VerificationReport) ([]byte, error) {
	return marshalCanonical(CanonicalVerificationReport(report))
}

func marshalCanonical(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
