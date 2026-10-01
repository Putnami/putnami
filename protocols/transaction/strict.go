package transaction

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict parsing and validation. Tooling and adapters key off
// these — keep them in sync with ValidErrorCodes.
const (
	ErrorCodeParseError             = "transaction.parse_error"
	ErrorCodeUnknownField           = "transaction.unknown_field"
	ErrorCodeInvalidProtocolVersion = "transaction.invalid_protocol_version"
	ErrorCodeInvalidName            = "transaction.invalid_name"
	ErrorCodeMissingPropagation     = "transaction.missing_propagation"
	ErrorCodeInvalidPropagation     = "transaction.invalid_propagation"
	ErrorCodeInvalidIsolation       = "transaction.invalid_isolation"
	ErrorCodeMissingOutcome         = "transaction.missing_outcome"
	ErrorCodeInvalidOutcome         = "transaction.invalid_outcome"
	ErrorCodeRetryableMismatch      = "transaction.retryable_mismatch"
)

// ValidErrorCodes enumerates the canonical transaction-protocol error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:             true,
	ErrorCodeUnknownField:           true,
	ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeInvalidName:            true,
	ErrorCodeMissingPropagation:     true,
	ErrorCodeInvalidPropagation:     true,
	ErrorCodeInvalidIsolation:       true,
	ErrorCodeMissingOutcome:         true,
	ErrorCodeInvalidOutcome:         true,
	ErrorCodeRetryableMismatch:      true,
}

// unitOfWorkNamePattern matches a canonical unit-of-work name. We reuse the same
// shape as the other protocol/* modules (lowercase letters, digits, '-', '_',
// '.', '/'; 1–64 chars) so names look uniform across protocols.
var unitOfWorkNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_./-]{0,63}$`)

// ParseUnitOfWork decodes a UnitOfWork from JSON in strict mode (unknown fields
// rejected). A non-nil descriptor is returned only when parsing produced no
// errors.
func ParseUnitOfWork(data []byte) (*UnitOfWork, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var u UnitOfWork
	if err := dec.Decode(&u); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	return &u, nil
}

// ParseResult decodes a Result from JSON in strict mode.
func ParseResult(data []byte) (*Result, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Result
	if err := dec.Decode(&r); err != nil {
		return nil, []diag.Diagnostic{decodeError(err)}
	}
	return &r, nil
}

// decodeError maps a json decode failure to the matching diagnostic code,
// distinguishing an unknown-field rejection from a generic parse error.
func decodeError(err error) diag.Diagnostic {
	msg := err.Error()
	if field, ok := strings.CutPrefix(msg, "json: unknown field "); ok {
		return diag.Errorf(ErrorCodeUnknownField, strings.Trim(field, `"`), "%s", msg)
	}
	return diag.Errorf(ErrorCodeParseError, "", "%s", msg)
}

// ValidateUnitOfWork checks structural invariants: a supported protocol version,
// a canonical name when present, a required in-enum propagation, and an in-enum
// isolation when present.
func ValidateUnitOfWork(u *UnitOfWork) []diag.Diagnostic {
	if u == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "unit of work is nil")}
	}
	diags := validateProtocolVersion(u.ProtocolVersion)
	if u.Name != "" && !unitOfWorkNamePattern.MatchString(u.Name) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidName, "name",
			"name %q does not match canonical pattern %q", u.Name, unitOfWorkNamePattern.String()))
	}
	diags = append(diags, validatePropagation(u.Propagation)...)
	if u.Isolation != "" && !u.Isolation.Valid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidIsolation, "isolation",
			"isolation %q is not in the v1 set: read-committed, repeatable-read, serializable", u.Isolation))
	}
	return diags
}

// ValidateResult checks structural invariants: a supported protocol
// version, a required in-enum outcome, and a Retryable advisory that agrees with
// the outcome taxonomy (Outcome.Retryable()). The consistency check keeps the
// advisory from ever contradicting the code, cross-language.
func ValidateResult(r *Result) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "transaction result is nil")}
	}
	diags := validateProtocolVersion(r.ProtocolVersion)
	switch {
	case r.Outcome == "":
		diags = append(diags, diag.Errorf(ErrorCodeMissingOutcome, "outcome", "outcome is required"))
	case !r.Outcome.Valid():
		diags = append(diags, diag.Errorf(ErrorCodeInvalidOutcome, "outcome",
			"outcome %q is not in the v1 set: applied, already-consumed-conflict, not-found, retryable-serialization-failure", r.Outcome))
	case r.Retryable != r.Outcome.Retryable():
		diags = append(diags, diag.Errorf(ErrorCodeRetryableMismatch, "retryable",
			"retryable %t contradicts outcome %q (want %t)", r.Retryable, r.Outcome, r.Outcome.Retryable()))
	}
	return diags
}

// validatePropagation validates a unit-of-work propagation: required and in the
// closed enum.
func validatePropagation(p Propagation) []diag.Diagnostic {
	if p == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingPropagation, "propagation", "propagation is required")}
	}
	if !p.Valid() {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPropagation, "propagation",
			"propagation %q is not in the v1 set: required, requires-new, nested", p)}
	}
	return nil
}

// validateProtocolVersion checks that v is the supported protocol version.
func validateProtocolVersion(v int) []diag.Diagnostic {
	if v != ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported by this parser (want %d)", v, ProtocolVersion)}
	}
	return nil
}

// ParseAndValidateUnitOfWork runs strict parsing followed by structural
// validation.
func ParseAndValidateUnitOfWork(data []byte) (*UnitOfWork, []diag.Diagnostic) {
	u, diags := ParseUnitOfWork(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return u, append(diags, ValidateUnitOfWork(u)...)
}

// ParseAndValidateResult runs strict parsing followed by structural
// validation.
func ParseAndValidateResult(data []byte) (*Result, []diag.Diagnostic) {
	r, diags := ParseResult(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return r, append(diags, ValidateResult(r)...)
}
