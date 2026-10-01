package features

import (
	"fmt"
	"sort"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// SpecVerificationRecordProtocolVersion is the exact integer version
	// accepted by persisted spec-verification records.
	SpecVerificationRecordProtocolVersion = 1

	// SpecVerificationRecordFilename is the canonical filename the record is
	// persisted under, beside a session's other documents, so `specs verify
	// --session` can reproduce what the gate decided without reinterpreting
	// deleted temporary files.
	SpecVerificationRecordFilename = "spec-verification.json"

	// specVerificationMaxGroups bounds one persisted record.
	specVerificationMaxGroups = 4096
)

// SpecVerificationRecord is the persisted verdict projection of one engine
// session's executable-spec verification pass: which spec-owning projects were
// judged, under which committed mode, what every textual requirement resolved
// to, and which observation reports (by digest) the decision read. It is
// derived history — an audit surface, never an input to current maturity.
type SpecVerificationRecord struct {
	// Schema is the optional URI of the JSON schema describing the record.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the record wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// SessionID names the engine session this record was decided in.
	SessionID string `json:"sessionId,omitempty"`
	// GeneratedAt is the RFC 3339 evaluation instant rolling freshness used.
	GeneratedAt string `json:"generatedAt"`
	// Groups holds one entry per (spec-owning project, feature), sorted by
	// project then feature.
	Groups []SpecVerificationGroup `json:"groups"`
	// Diagnostics carries collection findings that belong to no single group:
	// an unreadable report artifact, a duplicate projection, a dropped
	// observation. Sorted by the shared diagnostic order.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// SpecVerificationGroup is one spec-owning project's verdict for one feature.
type SpecVerificationGroup struct {
	// Project is the spec-hosting project's canonical ID. Its committed policy
	// — never the implementing or observing project's — owns Mode.
	Project string `json:"project"`
	// Feature is the feature identity the spec details. Empty only when the
	// project's criteria projection could not be read at all.
	Feature string `json:"feature,omitempty"`
	// Spec is the workspace-relative spec document path.
	Spec string `json:"spec,omitempty"`
	// Mode is the effective verification mode and ModeSource its provenance.
	Mode       VerificationMode       `json:"mode"`
	ModeSource VerificationModeSource `json:"modeSource"`
	// AutomaticEvaluation is false when the effective mode is off: the group
	// was listed for the audit surface but nothing was collected or evaluated.
	AutomaticEvaluation bool `json:"automaticEvaluation"`
	// Blocked is the one decision core made for this group: true exactly when
	// the mode is enforce and at least one textual requirement did not resolve
	// verified (or the group's inputs were unreadable). Rendering surfaces
	// reproduce it; they never recompute a different one.
	Blocked bool `json:"blocked"`
	// Requirements is every textual requirement's verdict, sorted by ID.
	Requirements []SpecRequirementVerdict `json:"requirements,omitempty"`
	// Reports lists the observation reports the evaluation read, with digests.
	Reports []VerificationReportRef `json:"reports,omitempty"`
	// Counts is the bounded roll-up of Requirements.
	Counts SpecVerificationCounts `json:"counts"`
	// Diagnostics carries group-scoped collection findings.
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// SpecRequirementVerdict is one textual requirement's decided state.
type SpecRequirementVerdict struct {
	// Requirement is the shared spec-local and feature-local identity.
	Requirement string `json:"requirement"`
	// State is the closed executable-verification state.
	State RequirementVerificationState `json:"state"`
	// Checks classifies every declared check, sorted by check identity. Empty
	// for unmapped and unexecutable requirements.
	Checks []SpecCheckVerdict `json:"checks,omitempty"`
}

// SpecCheckVerdict is one declared check's decided state with its bounded
// provenance.
type SpecCheckVerdict struct {
	Check string `json:"check"`
	// State is the closed per-check classification and Reason its stable code.
	State  CheckState `json:"state"`
	Reason string     `json:"reason,omitempty"`
	// Path and Symbol echo the observed declaration, relative to the reporting
	// project's root. Empty when the check was never observed.
	Path   string `json:"path,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	// Measurement echoes a threshold observation's aggregate.
	Measurement *ObservationMeasurement `json:"measurement,omitempty"`
}

// VerificationReportRef is bounded provenance for one observation report the
// gate read: which task produced it, where the bytes were read from, and the
// exact content digest recorded at read time.
type VerificationReportRef struct {
	// Task is the producing task's canonical key.
	Task string `json:"task"`
	// Project is the reporting project's canonical ID.
	Project string `json:"project"`
	// Path is the workspace-relative artifact path the bytes were read from.
	Path string `json:"path"`
	// Digest is the lower-case sha256:<hex> content address of those bytes.
	Digest string `json:"digest"`
	// Restored is true when Task did not run in the session that decided this
	// record: the bytes came from that task's own cache entry, read at the key
	// the current inputs derive. Recording it keeps Reports honest — a reader
	// must be able to tell evidence a run produced from evidence a run
	// recovered, even though both are the same bytes under the same key.
	Restored bool `json:"restored,omitempty"`
}

// SpecVerificationCounts is the bounded per-group roll-up.
type SpecVerificationCounts struct {
	SpecRequirements int `json:"specRequirements"`
	Executable       int `json:"executable"`
	Verified         int `json:"verified"`
	Unmapped         int `json:"unmapped"`
	Unexecutable     int `json:"unexecutable"`
	Missing          int `json:"missing"`
	Stale            int `json:"stale"`
	Contradicted     int `json:"contradicted"`
	// Unobserved counts the requirements whose unresolved checks had no
	// observation source in scope at all. They are executable and they are not
	// verified, but they are reported and warned rather than sanctioned — see
	// RequirementUnobserved and GroupBlocks.
	Unobserved int `json:"unobserved,omitempty"`
}

// CountSpecVerdicts folds requirement verdicts into the closed roll-up, so
// every surface derives counts one way.
func CountSpecVerdicts(verdicts []SpecRequirementVerdict) SpecVerificationCounts {
	counts := SpecVerificationCounts{SpecRequirements: len(verdicts)}
	for _, verdict := range verdicts {
		switch verdict.State {
		case RequirementVerified:
			counts.Verified++
			counts.Executable++
		case RequirementUnmapped:
			counts.Unmapped++
		case RequirementUnexecutable:
			counts.Unexecutable++
		case RequirementMissing:
			counts.Missing++
			counts.Executable++
		case RequirementStale:
			counts.Stale++
			counts.Executable++
		case RequirementContradicted:
			counts.Contradicted++
			counts.Executable++
		case RequirementUnobserved:
			counts.Unobserved++
			counts.Executable++
		}
	}
	return counts
}

// GroupBlocks is the one blocking rule every surface shares: a group blocks
// exactly when its committed mode is enforce and either a textual requirement
// resolved to a state other than verified or unobserved, or the group's own
// inputs produced an error finding. Report and off never block, whatever was
// found.
//
// RequirementUnobserved is the ONE non-verified state that does not block, and
// it is not a weakening of the rule: it means no producer that could have
// reported the check was in scope, so the run has no evidence either way and
// sanctioning it would fail a build for work it was never asked to observe.
// Every state that rests on an observation that DID arrive — contradicted, a
// skipped check, an uninterpretable one, a stale one — still blocks, so a
// failing or skipped attestation can never reach this exemption.
func GroupBlocks(mode VerificationMode, verdicts []SpecRequirementVerdict, diagnostics []diag.Diagnostic) bool {
	if mode != VerificationModeEnforce {
		return false
	}
	if diag.HasErrors(diagnostics) {
		return true
	}
	for _, verdict := range verdicts {
		if verdict.State != RequirementVerified && verdict.State != RequirementUnobserved {
			return true
		}
	}
	return false
}

// ParseSpecVerificationRecord strictly parses one persisted record.
func ParseSpecVerificationRecord(data []byte) (*SpecVerificationRecord, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, SpecVerificationRecordProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var record SpecVerificationRecord
	if err := decodeStrictJSON(data, &record); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &record, nil
}

// ValidateSpecVerificationRecord validates one record in isolation.
func ValidateSpecVerificationRecord(input *SpecVerificationRecord) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "spec verification record is nil")}
	}
	var diagnostics []diag.Diagnostic
	if input.ProtocolVersion != SpecVerificationRecordProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported (want exactly %d)", input.ProtocolVersion, SpecVerificationRecordProtocolVersion))
	}
	if _, err := time.Parse(time.RFC3339, input.GeneratedAt); err != nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "generatedAt",
			"generatedAt must be an RFC 3339 instant"))
	}
	if input.Groups == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "groups", "groups is required and must be an array"))
	}
	if len(input.Groups) > specVerificationMaxGroups {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeSensitiveContent, "groups",
			"record exceeds the bounded group limit of %d", specVerificationMaxGroups))
		sortDiagnostics(diagnostics)
		return diagnostics
	}
	seen := make(map[string]bool, len(input.Groups))
	for i, group := range input.Groups {
		field := fmt.Sprintf("groups[%d]", i)
		if group.Project == "" {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".project", "group project is required"))
		}
		if !validVerificationModes[group.Mode] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".mode",
				"mode %q is not a verification mode", group.Mode))
		}
		for j, verdict := range group.Requirements {
			if !ValidRequirementVerificationStates[verdict.State] {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification,
					fmt.Sprintf("%s.requirements[%d].state", field, j),
					"state %q is not an executable-verification state", verdict.State))
			}
		}
		key := group.Project + "\x00" + group.Feature
		if seen[key] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateSpec, field,
				"group (%q, %q) is duplicated", group.Project, group.Feature))
		}
		seen[key] = true
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

// CanonicalSpecVerificationRecord returns a deterministically ordered copy.
func CanonicalSpecVerificationRecord(input *SpecVerificationRecord) *SpecVerificationRecord {
	if input == nil {
		return nil
	}
	out := *input
	// Groups stays a non-nil array on the wire: appending zero elements onto a
	// nil slice would turn an empty record's groups into JSON null, which the
	// record's own strict validator refuses — a test session with no
	// spec-owning projection in scope must still persist a readable record.
	out.Groups = append(make([]SpecVerificationGroup, 0, len(input.Groups)), input.Groups...)
	for i := range out.Groups {
		group := &out.Groups[i]
		group.Requirements = append([]SpecRequirementVerdict(nil), group.Requirements...)
		for j := range group.Requirements {
			group.Requirements[j].Checks = append([]SpecCheckVerdict(nil), group.Requirements[j].Checks...)
			sort.Slice(group.Requirements[j].Checks, func(a, b int) bool {
				return group.Requirements[j].Checks[a].Check < group.Requirements[j].Checks[b].Check
			})
		}
		sort.Slice(group.Requirements, func(a, b int) bool {
			return group.Requirements[a].Requirement < group.Requirements[b].Requirement
		})
		group.Reports = append([]VerificationReportRef(nil), group.Reports...)
		sort.Slice(group.Reports, func(a, b int) bool {
			if group.Reports[a].Task != group.Reports[b].Task {
				return group.Reports[a].Task < group.Reports[b].Task
			}
			return group.Reports[a].Path < group.Reports[b].Path
		})
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		if out.Groups[i].Project != out.Groups[j].Project {
			return out.Groups[i].Project < out.Groups[j].Project
		}
		return out.Groups[i].Feature < out.Groups[j].Feature
	})
	return &out
}

// MarshalSpecVerificationRecord encodes the canonical wire form.
func MarshalSpecVerificationRecord(record *SpecVerificationRecord) ([]byte, error) {
	return marshalCanonical(CanonicalSpecVerificationRecord(record))
}
