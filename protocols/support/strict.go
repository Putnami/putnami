package support

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Support protocol diagnostic codes are stable automation vocabulary.
const (
	ErrorCodeParseError             = "support.parse_error"
	ErrorCodeUnknownField           = "support.unknown_field"
	ErrorCodeDuplicateField         = "support.duplicate_field"
	ErrorCodeInvalidProtocolVersion = "support.invalid_protocol_version"
	ErrorCodeMissingEntries         = "support.missing_entries"
	ErrorCodeInvalidKind            = "support.invalid_kind"
	ErrorCodeInvalidID              = "support.invalid_id"
	ErrorCodeInvalidStatus          = "support.invalid_status"
	ErrorCodeInvalidDefault         = "support.invalid_default"
	ErrorCodeInvalidParity          = "support.invalid_parity"
	ErrorCodeDuplicateEntry         = "support.duplicate_entry"
)

// ValidDiagnosticCodes enumerates the stable support-protocol taxonomy.
var ValidDiagnosticCodes = map[string]bool{
	ErrorCodeParseError:             true,
	ErrorCodeUnknownField:           true,
	ErrorCodeDuplicateField:         true,
	ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeMissingEntries:         true,
	ErrorCodeInvalidKind:            true,
	ErrorCodeInvalidID:              true,
	ErrorCodeInvalidStatus:          true,
	ErrorCodeInvalidDefault:         true,
	ErrorCodeInvalidParity:          true,
	ErrorCodeDuplicateEntry:         true,
}

var subjectIDPattern = regexp.MustCompile(`^[a-z0-9@][a-z0-9@._/+:-]{0,255}$`)

func validSubjectID(value string) bool {
	if !subjectIDPattern.MatchString(value) {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

type protocolVersionError struct{ message string }

func (err protocolVersionError) Error() string { return err.message }

type duplicateFieldError struct{ field string }

func (err duplicateFieldError) Error() string {
	return fmt.Sprintf("field %q appears more than once", err.field)
}

type nullValueError struct{ field string }

func (err nullValueError) Error() string { return "explicit null is not allowed" }

// ParseCatalog strictly parses a support catalog. It rejects duplicate object
// fields, explicit nulls, unknown fields, trailing JSON, and any version token
// other than the exact integer 1.
func ParseCatalog(data []byte) (*Catalog, []diag.Diagnostic) {
	if err := inspectJSONStructure(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := requireExactProtocolVersion(data); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var catalog Catalog
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := requireEOF(decoder); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &catalog, nil
}

// ParseAndValidateCatalog performs strict parsing followed by semantic
// validation.
func ParseAndValidateCatalog(data []byte) (*Catalog, []diag.Diagnostic) {
	catalog, diagnostics := ParseCatalog(data)
	if catalog == nil {
		return nil, diagnostics
	}
	return catalog, append(diagnostics, ValidateCatalog(catalog)...)
}

// ValidateCatalog validates the closed vocabularies, entry identity, and the
// one cross-field rule v1 needs: an experimental subject cannot be default-on.
func ValidateCatalog(catalog *Catalog) []diag.Diagnostic {
	if catalog == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "catalog is nil")}
	}
	var diagnostics []diag.Diagnostic
	if catalog.ProtocolVersion != ProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocolVersion %d is not supported (want 1)", catalog.ProtocolVersion))
	}
	if len(catalog.Entries) == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeMissingEntries, "entries", "entries must contain at least one reviewed support declaration"))
	}
	seen := make(map[string]string, len(catalog.Entries))
	for i, entry := range catalog.Entries {
		field := fmt.Sprintf("entries[%d]", i)
		if !ValidSubjectKinds[entry.Kind] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidKind, field+".kind", "subject kind %q is not in the v1 set: protocol, package, feature", entry.Kind))
		}
		if !validSubjectID(entry.ID) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".id", "subject id %q is not a canonical bounded id", entry.ID))
		}
		if !ValidStatuses[entry.Status] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidStatus, field+".status", "support status %q is not in the v1 set: stable, preview, experimental", entry.Status))
		}
		if entry.Default != nil && *entry.Default && entry.Status == StatusExperimental {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDefault, field+".default", "experimental subjects cannot participate in the default experience"))
		}
		if entry.Parity != "" && !ValidParityStatuses[entry.Parity] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidParity, field+".parity", "parity %q is not supported by v1; omit it or use unsupported", entry.Parity))
		}
		key := string(entry.Kind) + "\x00" + entry.ID
		if first, exists := seen[key]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateEntry, field, "support entry (%q, %q) duplicates %s", entry.Kind, entry.ID, first))
		} else {
			seen[key] = field
		}
	}
	return diagnostics
}

func inspectJSONStructure(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return fmt.Errorf("catalog must be a JSON object")
	}
	if err := inspectJSONObject(decoder, ""); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("trailing JSON value %v is not allowed", token)
	}
	return nil
}

func inspectJSONObject(decoder *json.Decoder, path string) error {
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		field := key
		if path != "" {
			field = path + "." + key
		}
		if seen[key] {
			return duplicateFieldError{field: field}
		}
		seen[key] = true
		if err := inspectJSONValue(decoder, field); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func inspectJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return nullValueError{field: path}
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return inspectJSONObject(decoder, path)
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := inspectJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
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
		return fmt.Errorf("catalog must be a JSON object")
	}
	found := false
	var raw json.RawMessage
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		if key == "protocolVersion" {
			found = true
			raw = bytes.TrimSpace(value)
		}
	}
	if !found {
		return protocolVersionError{message: "protocolVersion is required"}
	}
	if string(raw) != "1" {
		return protocolVersionError{message: fmt.Sprintf("protocolVersion token %q is not supported (want exact integer token 1)", raw)}
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
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
