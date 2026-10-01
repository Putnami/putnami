package errors

import (
	"strings"
	"time"
)

// Error is the canonical structured error type for the Putnami Go framework.
type Error struct {
	code      Code
	message   string
	cause     error
	stack     Stack
	attrs     []Attr
	retryable bool
	category  Category
	source    string
	time      time.Time
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(string(e.code))
	if e.message != "" {
		b.WriteString(": ")
		b.WriteString(e.message)
	}
	// Append the cause text. When message is empty (Wrap/Bug/Retryable) this is
	// the only descriptive text; when message is set it adds the wrapped detail.
	if e.cause != nil {
		b.WriteString(": ")
		b.WriteString(e.cause.Error())
	}
	return b.String()
}

// Unwrap returns the underlying cause for errors.Is/errors.As compatibility.
func (e *Error) Unwrap() error { return e.cause }

// Code returns the error code.
func (e *Error) Code() Code { return e.code }

// Message returns the human-readable message.
func (e *Error) Message() string { return e.message }

// Cause returns the underlying error.
func (e *Error) Cause() error { return e.cause }

// Stack returns the captured stack trace (nil for wrapped errors).
func (e *Error) Stack() Stack { return e.stack }

// Attrs returns the structured attributes.
func (e *Error) Attrs() []Attr { return e.attrs }

// IsRetryable returns whether this error is retryable.
func (e *Error) IsRetryable() bool { return e.retryable }

// Category returns the error classification.
func (e *Error) Category() Category { return e.category }

// Source returns the component/module that produced the error.
func (e *Error) Source() string { return e.source }

// Time returns when the error was created.
func (e *Error) Time() time.Time { return e.time }

// WithSource sets the source component and returns the error for chaining.
func (e *Error) WithSource(source string) *Error {
	e.source = source
	return e
}

// WithCategory sets the error category and returns the error for chaining.
func (e *Error) WithCategory(cat Category) *Error {
	e.category = cat
	return e
}

// WithRetryable marks the error as retryable and returns the error for chaining.
func (e *Error) WithRetryable(r bool) *Error {
	e.retryable = r
	return e
}

// WithAttr appends a structured attribute and returns the error for chaining.
func (e *Error) WithAttr(attr Attr) *Error {
	e.attrs = append(e.attrs, attr)
	return e
}
