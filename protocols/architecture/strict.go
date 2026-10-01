package architecture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Stable architecture-protocol diagnostic and finding codes.
const (
	ErrorCodeParseError                  = "architecture.parse_error"
	ErrorCodeUnknownField                = "architecture.unknown_field"
	ErrorCodeDuplicateField              = "architecture.duplicate_field"
	ErrorCodeInvalidProtocolVersion      = "architecture.invalid_protocol_version"
	ErrorCodeInvalidID                   = "architecture.invalid_id"
	ErrorCodeInvalidDomain               = "architecture.invalid_domain"
	ErrorCodeInvalidProject              = "architecture.invalid_project"
	ErrorCodeInvalidStatus               = "architecture.invalid_status"
	ErrorCodeInvalidMode                 = "architecture.invalid_mode"
	ErrorCodeInvalidTransport            = "architecture.invalid_transport"
	ErrorCodeInvalidFact                 = "architecture.invalid_fact"
	ErrorCodeInvalidConsistency          = "architecture.invalid_consistency"
	ErrorCodeInvalidProjection           = "architecture.invalid_projection"
	ErrorCodeInvalidDeletion             = "architecture.invalid_deletion"
	ErrorCodeInvalidBinding              = "architecture.invalid_binding"
	ErrorCodeDuplicateDomain             = "architecture.duplicate_domain"
	ErrorCodeDuplicateProject            = "architecture.duplicate_project"
	ErrorCodeDuplicateExport             = "architecture.duplicate_export"
	ErrorCodeDuplicateImport             = "architecture.duplicate_import"
	ErrorCodeDuplicateBinding            = "architecture.duplicate_binding"
	ErrorCodeUnknownDomain               = "architecture.unknown_domain"
	ErrorCodeUnknownExport               = "architecture.unknown_export"
	ErrorCodeUnknownProject              = "architecture.unknown_project"
	ErrorCodeIncompatibleImport          = "architecture.incompatible_import"
	ErrorCodeInvalidDebtRecord           = "architecture.invalid_debt_record"
	ErrorCodeDuplicateDebtRecord         = "architecture.duplicate_debt_record"
	ErrorCodeUndeclaredProjectDependency = "architecture.undeclared_project_dependency"
	ErrorCodeDeclaredBindingUnobserved   = "architecture.declared_binding_unobserved"
	ErrorCodeDeclaredWithoutEvidence     = "architecture.declared_without_evidence"
	ErrorCodeEvidenceWithoutDeclaration  = "architecture.evidence_without_declaration"
	ErrorCodeStaleBaseline               = "architecture.stale_baseline"
	ErrorCodeStaleWaiver                 = "architecture.stale_waiver"
	ErrorCodeBaselineGrowth              = "architecture.baseline_growth"
	ErrorCodeExpiredWaiver               = "architecture.expired_waiver"
)

// ValidDiagnosticCodes pins the architecture automation vocabulary.
var ValidDiagnosticCodes = map[string]bool{
	ErrorCodeParseError: true, ErrorCodeUnknownField: true, ErrorCodeDuplicateField: true, ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeInvalidID: true, ErrorCodeInvalidDomain: true, ErrorCodeInvalidProject: true,
	ErrorCodeInvalidStatus: true, ErrorCodeInvalidMode: true, ErrorCodeInvalidTransport: true,
	ErrorCodeInvalidFact: true, ErrorCodeInvalidConsistency: true, ErrorCodeInvalidProjection: true,
	ErrorCodeInvalidDeletion: true, ErrorCodeInvalidBinding: true, ErrorCodeDuplicateDomain: true,
	ErrorCodeDuplicateProject: true, ErrorCodeDuplicateExport: true, ErrorCodeDuplicateImport: true,
	ErrorCodeDuplicateBinding: true, ErrorCodeUnknownDomain: true, ErrorCodeUnknownExport: true,
	ErrorCodeUnknownProject: true, ErrorCodeIncompatibleImport: true, ErrorCodeInvalidDebtRecord: true,
	ErrorCodeDuplicateDebtRecord: true, ErrorCodeUndeclaredProjectDependency: true,
	ErrorCodeDeclaredBindingUnobserved: true, ErrorCodeDeclaredWithoutEvidence: true,
	ErrorCodeEvidenceWithoutDeclaration: true, ErrorCodeStaleBaseline: true,
	ErrorCodeStaleWaiver: true, ErrorCodeBaselineGrowth: true, ErrorCodeExpiredWaiver: true,
}

type protocolVersionError struct{ message string }

func (err protocolVersionError) Error() string { return err.message }

type duplicateFieldError struct{ field string }

func (err duplicateFieldError) Error() string {
	return fmt.Sprintf("field %q appears more than once", err.field)
}

// ParseManifest strictly decodes one domain manifest without semantic validation.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	var manifest Manifest
	if diagnostics := parseStrict(data, &manifest); diagnostics != nil {
		return nil, diagnostics
	}
	return &manifest, nil
}

// ParseBaseline strictly decodes the workspace architecture baseline.
func ParseBaseline(data []byte) (*Baseline, []diag.Diagnostic) {
	var baseline Baseline
	if diagnostics := parseStrict(data, &baseline); diagnostics != nil {
		return nil, diagnostics
	}
	return &baseline, nil
}

// ParseWaiverFile strictly decodes the workspace architecture waivers.
func ParseWaiverFile(data []byte) (*WaiverFile, []diag.Diagnostic) {
	var waivers WaiverFile
	if diagnostics := parseStrict(data, &waivers); diagnostics != nil {
		return nil, diagnostics
	}
	return &waivers, nil
}

// ParseSnapshot strictly decodes one deterministic architecture snapshot.
func ParseSnapshot(data []byte) (*Snapshot, []diag.Diagnostic) {
	var snapshot Snapshot
	if diagnostics := parseStrict(data, &snapshot); diagnostics != nil {
		return nil, diagnostics
	}
	return &snapshot, nil
}

// ParseAndValidateManifest strictly parses and semantically validates a manifest.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	manifest, diagnostics := ParseManifest(data)
	if manifest == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateManifest(manifest)...)
	sortDiagnostics(diagnostics)
	return manifest, diagnostics
}

// ParseAndValidateBaseline strictly parses and validates a baseline.
func ParseAndValidateBaseline(data []byte) (*Baseline, []diag.Diagnostic) {
	baseline, diagnostics := ParseBaseline(data)
	if baseline == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateBaseline(baseline)...)
	sortDiagnostics(diagnostics)
	return baseline, diagnostics
}

// ParseAndValidateWaiverFile strictly parses and validates a waiver file.
func ParseAndValidateWaiverFile(data []byte) (*WaiverFile, []diag.Diagnostic) {
	waivers, diagnostics := ParseWaiverFile(data)
	if waivers == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateWaiverFile(waivers)...)
	sortDiagnostics(diagnostics)
	return waivers, diagnostics
}

func parseStrict(data []byte, output any) []diag.Diagnostic {
	if err := rejectDuplicateFields(data); err != nil {
		return []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := requireExactProtocolVersion(data); err != nil {
		return []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	if err := decodeStrictJSON(data, output); err != nil {
		return []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return []diag.Diagnostic{parseDiagnostic(err)}
	}
	return nil
}

// rejectDuplicateFields walks the token stream because encoding/json accepts
// duplicate object members and silently keeps the last value. A strict
// architecture declaration cannot let authoring order decide authority.
func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder, ""); err != nil {
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

func inspectJSONValue(decoder *json.Decoder, field string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			childField := key
			if field != "" {
				childField = field + "." + key
			}
			if seen[key] {
				return duplicateFieldError{field: childField}
			}
			seen[key] = true
			if err := inspectJSONValue(decoder, childField); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := inspectJSONValue(decoder, fmt.Sprintf("%s[%d]", field, index)); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func requireExactProtocolVersion(data []byte) error {
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
	if string(raw) != "1" {
		return protocolVersionError{message: fmt.Sprintf("protocolVersion token %q is not supported (want exact integer token 1)", raw)}
	}
	return nil
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
			childField := key
			if field != "" {
				childField = field + "." + key
			}
			if err := findExplicitNull(typed[key], childField); err != nil {
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
	var duplicateErr duplicateFieldError
	if errors.As(err, &duplicateErr) {
		return diag.Errorf(ErrorCodeDuplicateField, duplicateErr.field, "%s", duplicateErr.Error())
	}
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

func sortDiagnostics(diagnostics []diag.Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if diag.SeverityRank(left.Severity) != diag.SeverityRank(right.Severity) {
			return diag.SeverityRank(left.Severity) < diag.SeverityRank(right.Severity)
		}
		if left.Severity != right.Severity {
			return left.Severity < right.Severity
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Message < right.Message
	})
}
