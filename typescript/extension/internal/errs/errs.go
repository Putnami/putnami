// Package errs provides a small structured error type for the TypeScript
// extension. Each error carries a machine-readable Code (and optional
// Category) that travels on the error value itself, so diagnostic codes can be
// derived from the error via CodeOf at the emit site.
//
// The codes are the same UPPER_SNAKE_CASE identifiers the extension already
// emits to DiagnosticWithCode; this package is the single place they are
// defined. It has no dependencies outside the standard library.
package errs

import (
	stderrors "errors"
	"fmt"
)

// Code is a stable, machine-readable diagnostic identifier. It mirrors the
// codes the extension emits via jsonl.Emitter.DiagnosticWithCode.
type Code string

// String returns the code as a plain string, suitable for passing to
// DiagnosticWithCode.
func (c Code) String() string { return string(c) }

const (
	// CodeUnknown is the zero value returned by CodeOf for a plain error that
	// carries no structured code.
	CodeUnknown Code = ""

	// Build / generate phase codes.
	CodeGenerateFailed Code = "GENERATE_FAILED"
	CodeTranspileError Code = "TRANSPILE_ERROR"
	CodeCompileError   Code = "COMPILE_ERROR"

	// Run / serve resolution codes.
	CodeNoRunEntrypoint Code = "NO_RUN_ENTRYPOINT"
	CodeNoServeExport   Code = "NO_SERVE_EXPORT"

	// Test phase codes.
	CodeTestsFailed             Code = "TESTS_FAILED"
	CodeTestFailed              Code = "TEST_FAILED"
	CodeUnhandledError          Code = "UNHANDLED_ERROR"
	CodeCoverageThresholdNotMet Code = "COVERAGE_THRESHOLD_NOT_MET"

	// Tooling / subprocess codes.
	CodeGitFailed Code = "GIT_FAILED"
)

// Category classifies the nature of an error for operational decisions
// (log level, user visibility). It mirrors go.putnami.dev/errors.Category so
// the two vocabularies stay aligned, without taking a dependency on it.
type Category string

// Error categories.
const (
	CategoryNone      Category = ""
	CategoryInfra     Category = "infra"     // infrastructure failures (process, disk, network)
	CategoryUser      Category = "user"      // user input / project configuration errors
	CategoryTransient Category = "transient" // temporary, retryable failures
	CategoryBug       Category = "bug"       // invariant violations, programmer errors
)

// Error is a structured error carrying a machine-readable Code, an optional
// Category, and a wrapped cause. It implements error and Unwrap so it composes
// with errors.Is / errors.As / errors.Unwrap.
type Error struct {
	code     Code
	category Category
	message  string
	cause    error
}

// Error implements the error interface. It renders as the message (or the
// cause when no message is set), so existing callers that surface err.Error()
// to users keep producing the same human-readable text.
func (e *Error) Error() string {
	switch {
	case e.message != "" && e.cause != nil:
		return e.message + ": " + e.cause.Error()
	case e.message != "":
		return e.message
	case e.cause != nil:
		return e.cause.Error()
	default:
		return e.code.String()
	}
}

// Unwrap returns the underlying cause for errors.Is/errors.As compatibility.
func (e *Error) Unwrap() error { return e.cause }

// Code returns the error's machine-readable code.
func (e *Error) Code() Code { return e.code }

// Category returns the error's operational classification (CategoryNone if
// unset).
func (e *Error) Category() Category { return e.category }

// WithCategory sets the category and returns the error for chaining.
func (e *Error) WithCategory(cat Category) *Error {
	e.category = cat
	return e
}

// New creates a structured error with the given code and message.
func New(code Code, message string) *Error {
	return &Error{code: code, message: message}
}

// Newf creates a structured error with the given code and a formatted message.
func Newf(code Code, format string, args ...any) *Error {
	return &Error{code: code, message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a code to an existing error, preserving it as the cause so the
// original message and any errors.Is/As targets remain reachable. Returns nil
// if err is nil.
func Wrap(err error, code Code) *Error {
	if err == nil {
		return nil
	}
	return &Error{code: code, cause: err}
}

// Wrapf attaches a code and an additional message to an existing error,
// preserving it as the cause. Returns nil if err is nil.
func Wrapf(err error, code Code, format string, args ...any) *Error {
	if err == nil {
		return nil
	}
	return &Error{code: code, message: fmt.Sprintf(format, args...), cause: err}
}

// CodeOf extracts the Code from the first *Error in the chain. It returns
// CodeUnknown ("") for a nil error or any error that carries no structured
// code, so emit sites can fall back to a default when needed.
func CodeOf(err error) Code {
	var e *Error
	if stderrors.As(err, &e) {
		return e.code
	}
	return CodeUnknown
}
