package features

import (
	"fmt"
	"sort"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// SpecCriteriaProjectionProtocolVersion is the exact integer version accepted
	// by executable-criteria projections.
	SpecCriteriaProjectionProtocolVersion = 1

	// SpecCriteriaProjectionArtifactID is the reserved declared-artifact identity
	// a spec-validation task uses to publish one project's executable-criteria
	// projection. The projection is derived only from durable manifests and
	// specs; it carries what a run is EXPECTED to prove, never what it observed,
	// so the party that authors criteria and the party that sanctions a run stay
	// two different parties.
	SpecCriteriaProjectionArtifactID = "putnami-spec-criteria"

	// SpecCriteriaProjectionFilename is the canonical projection artifact
	// filename inside a task's command-output directory.
	SpecCriteriaProjectionFilename = "spec-criteria.json"

	// SpecCriteriaProjectionMaxGroups bounds one projection so a consumer never
	// reads an unbounded artifact. It matches the bounded spec-directory limit:
	// one group exists per specified feature.
	SpecCriteriaProjectionMaxGroups = 1024

	// specCriteriaMaxRequirements bounds the textual requirement identities one
	// group may carry.
	specCriteriaMaxRequirements = 4096
)

// SpecCriteriaProjection is the bounded, deterministic executable-criteria
// projection one spec-owning project's validation task emits: for every durable
// spec the project hosts, the textual requirement identities it states and the
// same-ID authored feature requirements that could execute them.
//
// It is a derivation, not an authority: every byte is recomputable from the
// committed manifests and specs, and a consumer joining it with run
// observations must do so through the pure evaluator. It deliberately carries
// requirement identities rather than requirement prose, so the artifact stays
// bounded and the spec document remains the only home of the agreed sentence.
type SpecCriteriaProjection struct {
	// Schema is the optional URI of the JSON schema describing the projection.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the projection wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Groups lists one entry per specified feature, sorted by feature identity.
	Groups []SpecCriteriaGroup `json:"groups"`
}

// SpecCriteriaGroup joins one durable spec's textual requirements to the
// same-ID authored feature requirements. Requirements without a matching
// textual identity are deliberately absent: they express maturity evidence the
// spec never states, and projecting them would widen the gate beyond the
// sentences the team agreed to.
type SpecCriteriaGroup struct {
	// Feature is the exact feature identity the spec details.
	Feature string `json:"feature"`
	// Spec is the canonical workspace-relative path of the spec document.
	Spec string `json:"spec"`
	// SpecRequirements lists the spec's textual requirement identities, sorted.
	SpecRequirements []string `json:"specRequirements"`
	// Requirements carries the authored feature requirements whose IDs match a
	// textual requirement, sorted by ID. A matching requirement without a
	// verification criterion stays here so a consumer can tell an unexecutable
	// requirement from an unmapped one.
	Requirements []Requirement `json:"requirements,omitempty"`
}

// ParseSpecCriteriaProjection strictly parses one executable-criteria
// projection.
func ParseSpecCriteriaProjection(data []byte) (*SpecCriteriaProjection, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, SpecCriteriaProjectionProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var projection SpecCriteriaProjection
	if err := decodeStrictJSON(data, &projection); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &projection, nil
}

// ParseAndValidateSpecCriteriaProjection performs strict parsing followed by
// local validation. Whether the projected criteria match the committed
// manifests is the producing task's contract; a consumer only checks that the
// document is closed, bounded, and internally consistent.
func ParseAndValidateSpecCriteriaProjection(data []byte) (*SpecCriteriaProjection, []diag.Diagnostic) {
	projection, diagnostics := ParseSpecCriteriaProjection(data)
	if projection == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateSpecCriteriaProjection(projection)...)
	sortDiagnostics(diagnostics)
	return projection, diagnostics
}

// ValidateSpecCriteriaProjection validates one projection in isolation.
func ValidateSpecCriteriaProjection(input *SpecCriteriaProjection) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "spec criteria projection is nil")}
	}
	var diagnostics []diag.Diagnostic
	if input.ProtocolVersion != SpecCriteriaProjectionProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported (want exactly %d)", input.ProtocolVersion, SpecCriteriaProjectionProtocolVersion))
	}
	if input.Groups == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "groups", "groups is required and must be an array"))
	}
	if len(input.Groups) > SpecCriteriaProjectionMaxGroups {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeSensitiveContent, "groups",
			"projection exceeds the bounded group limit of %d", SpecCriteriaProjectionMaxGroups))
		sortDiagnostics(diagnostics)
		return diagnostics
	}
	seenFeatures := make(map[string]bool, len(input.Groups))
	for i, group := range input.Groups {
		field := fmt.Sprintf("groups[%d]", i)
		diagnostics = append(diagnostics, validateSpecCriteriaGroup(field, group)...)
		if seenFeatures[group.Feature] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateSpec, field+".feature",
				"feature %q appears in more than one group", group.Feature))
		}
		seenFeatures[group.Feature] = true
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateSpecCriteriaGroup(field string, group SpecCriteriaGroup) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !featureIDPattern.MatchString(group.Feature) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".feature",
			"feature ID %q is not a canonical slash-separated identity", group.Feature))
	}
	if !specSourcePattern.MatchString(group.Spec) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidPath, field+".spec",
			"spec path %q is not a canonical specs/ document path", group.Spec))
	} else {
		diagnostics = append(diagnostics, validateProtocolPath(field+".spec", group.Spec)...)
	}
	if len(group.SpecRequirements) == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, field+".specRequirements",
			"a group must carry at least one textual requirement identity"))
	}
	if len(group.SpecRequirements) > specCriteriaMaxRequirements {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeSensitiveContent, field+".specRequirements",
			"group exceeds the bounded requirement limit of %d", specCriteriaMaxRequirements))
		return diagnostics
	}
	textual := make(map[string]bool, len(group.SpecRequirements))
	for i, id := range group.SpecRequirements {
		idField := fmt.Sprintf("%s.specRequirements[%d]", field, i)
		if !segmentIDPattern.MatchString(id) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, idField,
				"requirement ID %q must be one lower-case ASCII ID segment", id))
		}
		if textual[id] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, idField,
				"requirement ID %q is duplicated", id))
		}
		textual[id] = true
	}
	seen := make(map[string]bool, len(group.Requirements))
	for i, requirement := range group.Requirements {
		requirementField := fmt.Sprintf("%s.requirements[%d]", field, i)
		if !textual[requirement.ID] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownRequirement, requirementField+".id",
				"requirement %q matches no textual requirement in this group", requirement.ID))
		}
		if seen[requirement.ID] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, requirementField+".id",
				"requirement ID %q is duplicated", requirement.ID))
		}
		seen[requirement.ID] = true
		if _, valid := StageRank(requirement.Stage); !valid {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidStage, requirementField+".stage",
				"requirement stage %q is not in the maturity ladder", requirement.Stage))
		}
		kinds := make(map[EvidenceKind]bool, len(requirement.EvidenceKinds))
		for j, kind := range requirement.EvidenceKinds {
			if !validEvidenceKinds[kind] {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement,
					fmt.Sprintf("%s.evidenceKinds[%d]", requirementField, j), "evidence kind %q is not supported", kind))
			}
			kinds[kind] = true
		}
		// The criterion is re-validated with the current manifest version: a
		// projection is derived from a v2 manifest by definition, so a shape the
		// manifest validator would refuse is refused here identically.
		diagnostics = append(diagnostics, validateVerificationCriterion(requirementField+".verification",
			ManifestProtocolVersion, requirement, kinds)...)
	}
	return diagnostics
}

// CanonicalSpecCriteriaProjection returns a deterministically ordered copy
// without mutating caller-owned slices: groups sort by feature, textual
// identities and requirements sort by ID, and every criterion's checks sort.
func CanonicalSpecCriteriaProjection(input *SpecCriteriaProjection) *SpecCriteriaProjection {
	if input == nil {
		return nil
	}
	out := *input
	out.Groups = append([]SpecCriteriaGroup(nil), input.Groups...)
	for i := range out.Groups {
		out.Groups[i].SpecRequirements = cloneStrings(out.Groups[i].SpecRequirements)
		sort.Strings(out.Groups[i].SpecRequirements)
		out.Groups[i].Requirements = append([]Requirement(nil), out.Groups[i].Requirements...)
		for j := range out.Groups[i].Requirements {
			requirement := &out.Groups[i].Requirements[j]
			requirement.EvidenceKinds = append([]EvidenceKind(nil), requirement.EvidenceKinds...)
			sort.Slice(requirement.EvidenceKinds, func(a, b int) bool {
				return requirement.EvidenceKinds[a] < requirement.EvidenceKinds[b]
			})
			requirement.Verification = canonicalVerificationCriterion(requirement.Verification)
		}
		sort.Slice(out.Groups[i].Requirements, func(a, b int) bool {
			return out.Groups[i].Requirements[a].ID < out.Groups[i].Requirements[b].ID
		})
	}
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].Feature < out.Groups[j].Feature })
	return &out
}

// MarshalSpecCriteriaProjection encodes the canonical wire form.
func MarshalSpecCriteriaProjection(projection *SpecCriteriaProjection) ([]byte, error) {
	return marshalCanonical(CanonicalSpecCriteriaProjection(projection))
}
