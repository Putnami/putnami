package features

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Stable feature-protocol diagnostic codes. Derived assessment warnings use
// the same vocabulary, but snapshot computation remains outside this package.
const (
	ErrorCodeParseError                   = "features.parse_error"
	ErrorCodeUnknownField                 = "features.unknown_field"
	ErrorCodeInvalidProtocolVersion       = "features.invalid_protocol_version"
	ErrorCodeInvalidID                    = "features.invalid_id"
	ErrorCodeDuplicateFeature             = "features.duplicate_feature"
	ErrorCodeInvalidType                  = "features.invalid_type"
	ErrorCodeInvalidStage                 = "features.invalid_stage"
	ErrorCodeInvalidRelation              = "features.invalid_relation"
	ErrorCodeDanglingRelation             = "features.dangling_relation"
	ErrorCodeRelationCycle                = "features.relation_cycle"
	ErrorCodeInvalidRequirement           = "features.invalid_requirement"
	ErrorCodeInvalidDecision              = "features.invalid_decision"
	ErrorCodeDuplicateEvidence            = "features.duplicate_evidence"
	ErrorCodeDuplicateSpec                = "features.duplicate_spec"
	ErrorCodeInvalidVerification          = "features.invalid_verification"
	ErrorCodeInvalidObservation           = "features.invalid_observation"
	ErrorCodeDuplicateObservation         = "features.duplicate_observation"
	ErrorCodeUnknownFeature               = "features.unknown_feature"
	ErrorCodeUnknownRequirement           = "features.unknown_requirement"
	ErrorCodeStageMismatch                = "features.stage_mismatch"
	ErrorCodeInvalidSubject               = "features.invalid_subject"
	ErrorCodeInvalidAuthority             = "features.invalid_authority"
	ErrorCodeInvalidSourceBinding         = "features.invalid_source_binding"
	ErrorCodeSourceBindingUnavailable     = "features.source_binding_unavailable"
	ErrorCodeInvalidPath                  = "features.invalid_path"
	ErrorCodePathEscape                   = "features.path_escape"
	ErrorCodeSymlinkEscape                = "features.symlink_escape"
	ErrorCodeOutsideDiscoveryRoot         = "features.outside_discovery_root"
	ErrorCodeSensitiveContent             = "features.sensitive_content"
	ErrorCodeRatchetRegression            = "features.ratchet_regression"
	ErrorCodeInvalidDecisionRegistry      = "features.invalid_decision_registry"
	ErrorCodeDecisionViolated             = "features.decision_violated"
	ErrorCodeDecisionsUnreadable          = "features.decisions_unreadable"
	WarningCodeRatchetGrowth              = "features.ratchet_growth"
	WarningCodeMisplacedBaseline          = "features.misplaced_baseline"
	WarningCodeMissingEvidence            = "features.missing_evidence"
	WarningCodeStaleEvidence              = "features.stale_evidence"
	WarningCodeContradictedEvidence       = "features.contradicted_evidence"
	WarningCodeMissingHumanAuthority      = "features.missing_human_authority"
	WarningCodeUnclassifiedContribution   = "features.unclassified_contribution"
	WarningCodeUnresolvedFeatureAuthority = "features.unresolved_feature_authority"
	WarningCodeUnobservedRequirement      = "features.unobserved_requirement"
)

// ValidDiagnosticCodes enumerates the reserved automation vocabulary.
var ValidDiagnosticCodes = map[string]bool{
	ErrorCodeParseError: true, ErrorCodeUnknownField: true, ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeInvalidID: true, ErrorCodeDuplicateFeature: true, ErrorCodeInvalidType: true,
	ErrorCodeInvalidStage: true, ErrorCodeInvalidRelation: true, ErrorCodeDanglingRelation: true,
	ErrorCodeRelationCycle: true, ErrorCodeInvalidRequirement: true, ErrorCodeInvalidDecision: true,
	ErrorCodeDuplicateEvidence: true, ErrorCodeDuplicateSpec: true,
	ErrorCodeInvalidVerification: true, ErrorCodeInvalidObservation: true,
	ErrorCodeDuplicateObservation: true,
	ErrorCodeUnknownFeature:       true, ErrorCodeUnknownRequirement: true, ErrorCodeStageMismatch: true,
	ErrorCodeInvalidSubject: true, ErrorCodeInvalidAuthority: true, ErrorCodeInvalidSourceBinding: true,
	ErrorCodeSourceBindingUnavailable: true, ErrorCodeInvalidPath: true, ErrorCodePathEscape: true,
	ErrorCodeSymlinkEscape: true, ErrorCodeOutsideDiscoveryRoot: true, ErrorCodeSensitiveContent: true,
	ErrorCodeRatchetRegression: true, WarningCodeRatchetGrowth: true,
	ErrorCodeInvalidDecisionRegistry: true, ErrorCodeDecisionViolated: true,
	ErrorCodeDecisionsUnreadable: true,
	WarningCodeMissingEvidence:   true, WarningCodeStaleEvidence: true,
	WarningCodeContradictedEvidence: true, WarningCodeMissingHumanAuthority: true,
	WarningCodeUnclassifiedContribution: true, WarningCodeUnresolvedFeatureAuthority: true,
	WarningCodeUnobservedRequirement: true,
}

type protocolVersionError struct{ message string }

func (err protocolVersionError) Error() string { return err.message }

// ParseManifest strictly parses an authored feature manifest. Both manifest
// versions are accepted; a version 1 document may not carry the version 2
// verification criterion, which is refused on the wire rather than ignored.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, MinimumManifestProtocolVersion, ManifestProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var manifest Manifest
	if err := decodeStrictJSON(data, &manifest); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if finding := rejectForwardManifestFields(&manifest); finding != nil {
		return nil, []diag.Diagnostic{*finding}
	}
	return &manifest, nil
}

// ParseEvidenceDocument strictly parses one evidence fragment.
func ParseEvidenceDocument(data []byte) (*EvidenceDocument, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, EvidenceProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var document EvidenceDocument
	if err := decodeStrictJSON(data, &document); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &document, nil
}

// ParseSpec strictly parses one durable specification document.
func ParseSpec(data []byte) (*Spec, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, SpecProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var spec Spec
	if err := decodeStrictJSON(data, &spec); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &spec, nil
}

// ParseVerificationReport strictly parses one run-scoped verification report.
func ParseVerificationReport(data []byte) (*VerificationReport, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, VerificationReportProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var report VerificationReport
	if err := decodeStrictJSON(data, &report); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &report, nil
}

// rejectForwardManifestFields refuses a version 1 manifest that carries a field
// only the current version defines. The strict decoder cannot see this on its
// own, because both versions share one Go wire type; reporting the exact
// authored location keeps the failure as actionable as an unknown field.
func rejectForwardManifestFields(manifest *Manifest) *diag.Diagnostic {
	if manifest.ProtocolVersion >= ManifestProtocolVersion {
		return nil
	}
	for i, feature := range manifest.Features {
		for j, requirement := range feature.Requirements {
			if requirement.Verification == nil {
				continue
			}
			field := fmt.Sprintf("features[%d].requirements[%d].verification", i, j)
			finding := diag.Errorf(ErrorCodeUnknownField, field,
				"verification requires feature manifest protocolVersion %d", ManifestProtocolVersion)
			return &finding
		}
	}
	return nil
}

// ParseAndValidateManifest performs strict parsing followed by semantic
// validation.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	manifest, diagnostics := ParseManifest(data)
	if manifest == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateManifest(manifest)...)
	sortDiagnostics(diagnostics)
	return manifest, diagnostics
}

// ParseAndValidateEvidenceDocument performs strict parsing followed by local
// evidence validation. Cross-document references are checked by
// ValidateRepository.
func ParseAndValidateEvidenceDocument(data []byte) (*EvidenceDocument, []diag.Diagnostic) {
	document, diagnostics := ParseEvidenceDocument(data)
	if document == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateEvidenceDocument(document)...)
	sortDiagnostics(diagnostics)
	return document, diagnostics
}

// ParseAndValidateSpec performs strict parsing followed by local spec
// validation. Feature resolution and one-spec-per-feature uniqueness are
// checked by ValidateSpecRepository, because a single document cannot decide
// either.
func ParseAndValidateSpec(data []byte) (*Spec, []diag.Diagnostic) {
	spec, diagnostics := ParseSpec(data)
	if spec == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateSpec(spec)...)
	sortDiagnostics(diagnostics)
	return spec, diagnostics
}

// ParseAndValidateVerificationReport performs strict parsing followed by local
// report validation. Whether an observed check is declared by a criterion, and
// whether the reporting task may claim it, are workspace facts checked by the
// evaluator and by core.
func ParseAndValidateVerificationReport(data []byte) (*VerificationReport, []diag.Diagnostic) {
	report, diagnostics := ParseVerificationReport(data)
	if report == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateVerificationReport(report)...)
	sortDiagnostics(diagnostics)
	return report, diagnostics
}

func requireExactProtocolVersion(data []byte, supported ...int) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("document must be a JSON object")
	}
	var raw json.RawMessage
	found := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("document object key is not a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		if key != "protocolVersion" {
			continue
		}
		if found {
			return protocolVersionError{message: "protocolVersion must appear exactly once"}
		}
		found = true
		raw = bytes.TrimSpace(value)
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if !found {
		return protocolVersionError{message: "protocolVersion is required"}
	}
	for _, version := range supported {
		if string(raw) == strconv.Itoa(version) {
			return nil
		}
	}
	return protocolVersionError{message: fmt.Sprintf("protocolVersion token %q is not supported (want exact integer token %s)", raw, supportedVersionList(supported))}
}

// supportedVersionList renders the accepted exact integer tokens so a version
// diagnostic states the whole contract rather than only the newest wire.
func supportedVersionList(supported []int) string {
	tokens := make([]string, len(supported))
	for i, version := range supported {
		tokens[i] = strconv.Itoa(version)
	}
	if len(tokens) < 2 {
		return strings.Join(tokens, "")
	}
	return strings.Join(tokens[:len(tokens)-1], ", ") + " or " + tokens[len(tokens)-1]
}

func decodeStrictJSON(data []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

type nullValueError struct{ field string }

func (err nullValueError) Error() string {
	return "explicit null is not allowed; omit optional fields or provide their concrete wire type"
}

func rejectExplicitNulls(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return findExplicitNull(value, "")
}

func findExplicitNull(value any, field string) error {
	switch typed := value.(type) {
	case nil:
		return nullValueError{field: field}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := typed[key]
			childField := key
			if field != "" {
				childField = field + "." + key
			}
			if err := findExplicitNull(child, childField); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := findExplicitNull(child, fmt.Sprintf("%s[%d]", field, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func versionOrParseDiagnostic(err error) diag.Diagnostic {
	var versionErr protocolVersionError
	if errors.As(err, &versionErr) {
		return diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "%s", versionErr.message)
	}
	return parseDiagnostic(err)
}

func parseDiagnostic(err error) diag.Diagnostic {
	var nullErr nullValueError
	if errors.As(err, &nullErr) {
		return diag.Errorf(ErrorCodeParseError, nullErr.field, "%s", nullErr.Error())
	}
	message := err.Error()
	if strings.HasPrefix(message, "json: unknown field ") {
		field := strings.Trim(strings.TrimPrefix(message, "json: unknown field "), `"`)
		return diag.Errorf(ErrorCodeUnknownField, field, "%s", message)
	}
	return diag.Errorf(ErrorCodeParseError, "", "%s", message)
}
