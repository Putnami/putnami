package errors

import (
	"fmt"
	"strings"
)

// AggregateError wraps multiple errors into a single error.
// Compatible with errors.Is/errors.As via Unwrap() []error.
type AggregateError struct {
	Message string  `json:"message"`
	Errors  []error `json:"errors"`
}

// Error implements the error interface.
func (e *AggregateError) Error() string {
	if len(e.Errors) == 0 {
		return e.Message
	}
	msgs := make([]string, len(e.Errors))
	for i, err := range e.Errors {
		msgs[i] = err.Error()
	}
	return fmt.Sprintf("%s: %s", e.Message, strings.Join(msgs, "; "))
}

// Unwrap returns the list of wrapped errors (for errors.Is/As compatibility).
func (e *AggregateError) Unwrap() []error {
	return e.Errors
}

// NewAggregate creates an AggregateError if there are any errors.
// Returns nil if the slice is empty.
func NewAggregate(message string, errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return &AggregateError{Message: message, Errors: errs}
}
