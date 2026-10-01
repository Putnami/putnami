package errors

import (
	"fmt"
	"time"
)

// New creates a structured error with stack capture.
// Stack is captured ONCE here — Wrap never captures stack.
func New(code Code, msg string, attrs ...Attr) *Error {
	e := &Error{
		code:    code,
		message: msg,
		stack:   captureStack(),
		attrs:   attrs,
		time:    time.Now(),
	}
	fireHooks(e)
	return e
}

// Newf creates a structured error with a formatted message and stack capture.
func Newf(code Code, format string, args ...any) *Error {
	e := &Error{
		code:    code,
		message: fmt.Sprintf(format, args...),
		stack:   captureStack(),
		time:    time.Now(),
	}
	fireHooks(e)
	return e
}

// Wrap wraps an existing error with a code and optional attrs.
// NO stack is captured — the original stack (if any) is preserved via Unwrap.
// Returns nil if err is nil.
func Wrap(err error, code Code, attrs ...Attr) *Error {
	if err == nil {
		return nil
	}
	// No message is set: Wrap adds a code, not text. Error() falls back to the
	// cause so the underlying message still renders (without duplicating it).
	return &Error{
		code:  code,
		cause: err,
		attrs: attrs,
		time:  time.Now(),
	}
}

// Wrapf wraps an existing error with a code, custom message, and optional attrs.
// NO stack is captured. Returns nil if err is nil.
func Wrapf(err error, code Code, msg string, attrs ...Attr) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		code:    code,
		message: msg,
		cause:   err,
		attrs:   attrs,
		time:    time.Now(),
	}
}

// Bug creates an error classified as a bug (invariant violation).
// Captures a stack trace. Returns nil if err is nil.
func Bug(err error) *Error {
	if err == nil {
		return nil
	}
	e := &Error{
		code:     CodeInternal,
		cause:    err,
		stack:    captureStack(),
		category: CategoryBug,
		time:     time.Now(),
	}
	fireHooks(e)
	return e
}

// Bugf creates a bug error with a formatted message and stack capture.
func Bugf(format string, args ...any) *Error {
	e := &Error{
		code:     CodeInternal,
		message:  fmt.Sprintf(format, args...),
		stack:    captureStack(),
		category: CategoryBug,
		time:     time.Now(),
	}
	fireHooks(e)
	return e
}

// User creates a safe, user-facing error. No stack is captured.
func User(code Code, msg string, attrs ...Attr) *Error {
	return &Error{
		code:     code,
		message:  msg,
		attrs:    attrs,
		category: CategoryUser,
		time:     time.Now(),
	}
}

// Retryable wraps an error and marks it as retryable.
// If err is already an *Error, returns a shallow copy with retryable set to true.
// Returns nil if err is nil.
func Retryable(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if As(err, &e) {
		cp := *e
		cp.retryable = true
		return &cp
	}
	return &Error{
		code:      CodeUnknown,
		cause:     err,
		retryable: true,
		time:      time.Now(),
	}
}
