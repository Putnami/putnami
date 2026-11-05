package cli

import "errors"

// resultDataError carries a machine-readable payload beside a classified
// command error, so the failure envelope can carry it as its data member. It
// is transparent to message printing and to errors.Is: the message is the
// wrapped error's, and Unwrap returns the classified cause.
type resultDataError struct {
	err  error
	data any
}

func (e *resultDataError) Error() string { return e.err.Error() }
func (e *resultDataError) Unwrap() error { return e.err }

// WithResultData attaches data to err, so a command that ran and found
// problems can fail and still report what it found: pass ResultData(err) as
// the data of NewResultV2. The exit code and the error class stay err's own.
// A nil err stays nil.
func WithResultData(err error, data any) error {
	if err == nil {
		return nil
	}
	return &resultDataError{err: err, data: data}
}

// ResultData returns the payload WithResultData attached to err or to an error
// it wraps, or nil.
func ResultData(err error) any {
	var e *resultDataError
	if errors.As(err, &e) {
		return e.data
	}
	return nil
}
