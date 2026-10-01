package analytics

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// unknownFieldPrefix is the fixed prefix encoding/json puts on the error a
// decoder with DisallowUnknownFields returns. The message is the only place the
// offending name appears, so the contract's unknown_attribute code is recovered
// from it rather than from a typed error the standard library does not define.
const unknownFieldPrefix = "json: unknown field "

// ParseBatchStrict decodes an analytics batch and rejects unknown fields. It
// returns only decode diagnostics; use ParseAndValidateBatch (or call
// ValidateBatch separately) for the contract's structural rules.
//
// A decode failure is classified rather than flattened: a JSON type mismatch
// becomes ErrorCodeAttributeKind on the offending field, an undefined field
// becomes ErrorCodeUnknownAttribute naming it, and anything else becomes
// ErrorCodeParseError with an empty field, because the document as a whole is
// what failed.
func ParseBatchStrict(data []byte) (*Batch, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var batch Batch
	if err := dec.Decode(&batch); err != nil {
		return nil, []diag.Diagnostic{decodeDiagnostic(err)}
	}
	return &batch, nil
}

// ParseAndValidateBatch decodes an analytics batch strictly and, when it decodes
// cleanly, validates it against the wire contract.
func ParseAndValidateBatch(data []byte) (*Batch, []diag.Diagnostic) {
	batch, diags := ParseBatchStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return batch, append(diags, ValidateBatch(batch)...)
}

// decodeDiagnostic maps a decode failure onto the closed error vocabulary.
func decodeDiagnostic(err error) diag.Diagnostic {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return diag.Errorf(ErrorCodeAttributeKind, typeErr.Field,
			"field carries JSON %s, the contract declares %s", typeErr.Value, typeErr.Type)
	}
	if name, ok := unknownFieldName(err); ok {
		return diag.Errorf(ErrorCodeUnknownAttribute, name,
			"the analytics wire contract defines no field %q", name)
	}
	return diag.Errorf(ErrorCodeParseError, "", "failed to parse analytics batch: %v", err)
}

// unknownFieldName extracts the field name from a DisallowUnknownFields error.
func unknownFieldName(err error) (string, bool) {
	quoted, found := strings.CutPrefix(err.Error(), unknownFieldPrefix)
	if !found {
		return "", false
	}
	name, unquoteErr := strconv.Unquote(quoted)
	if unquoteErr != nil {
		return "", false
	}
	return name, true
}
