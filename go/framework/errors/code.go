// Package errors provides the canonical structured error type for the
// Putnami Go framework. Every error carries a typed Code, a human message,
// optional structured Attrs, a classification (Category / Retryable), and
// an auto-captured stack trace (at creation only, never at wrap).
//
// This package has zero external dependencies — telemetry integration is
// handled via the OnError hook (see hooks.go).
package errors

// Code is a typed, stable, machine-readable error identifier.
// Codes use dotted namespaces: "inject.not_registered", "db.connection".
type Code string

// Framework-wide error codes. Packages define their own domain codes.
const (
	// Generic
	CodeUnknown        Code = "unknown"
	CodeInternal       Code = "internal"
	CodeUnavailable    Code = "unavailable"
	CodeCancelled      Code = "canceled"
	CodeTimeout        Code = "timeout"
	CodeNotFound       Code = "not_found"
	CodeNotImplemented Code = "not_implemented"

	// Validation
	CodeValidation Code = "validation"
	CodeInvalidArg Code = "invalid_argument"

	// Auth
	CodeUnauthorized Code = "unauthorized"
	CodeForbidden    Code = "forbidden"

	// State
	CodeConflict      Code = "conflict"
	CodePrecondition  Code = "precondition_failed"
	CodeAlreadyExists Code = "already_exists"

	// Infra
	CodeConnection Code = "connection"
	CodeRateLimit  Code = "rate_limit"
)

// String returns the code as a string.
func (c Code) String() string { return string(c) }
