// Package shared holds command-layer helpers used by 2+ verticals of
// tooling/cli/internal/commands. A symbol needed by only one vertical moves
// with that vertical instead.
package shared

import "errors"

// resultDataError carries a machine-readable payload alongside a classified
// command error so the structured failure envelope can surface it in
// Result.Data. It is transparent to message printing and errors.Is: the message
// is the wrapped error's, and Unwrap exposes the classified cause.
type resultDataError struct {
	err  error
	data any
}

func (e *resultDataError) Error() string { return e.err.Error() }
func (e *resultDataError) Unwrap() error { return e.err }

// WithResultData attaches data to err so a structured failure envelope can carry
// it in Result.Data. Returns nil when err is nil.
func WithResultData(err error, data any) error {
	if err == nil {
		return nil
	}
	return &resultDataError{err: err, data: data}
}

// ResultData returns the payload attached to err via WithResultData, or nil.
func ResultData(err error) any {
	var e *resultDataError
	if errors.As(err, &e) {
		return e.data
	}
	return nil
}
