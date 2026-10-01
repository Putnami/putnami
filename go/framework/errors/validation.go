package errors

import (
	"fmt"
	"strings"
)

// FieldError represents a validation error on a specific field.
type FieldError struct {
	Field      string `json:"field"`
	Message    string `json:"message"`
	Constraint string `json:"constraint,omitempty"`
	Value      any    `json:"value,omitempty"`
	sensitive  bool
}

// Error implements the error interface.
func (e *FieldError) Error() string {
	if e.Value != nil && !e.sensitive {
		return fmt.Sprintf("%s: %s (got %v)", e.Field, e.Message, e.Value)
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationErrors is a collection of field validation errors.
type ValidationErrors struct {
	Errors []FieldError `json:"errors"`
}

// Error implements the error interface.
func (e *ValidationErrors) Error() string {
	if len(e.Errors) == 1 {
		return "validation failed: " + e.Errors[0].Error()
	}
	msgs := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		msgs[i] = fe.Error()
	}
	return fmt.Sprintf("validation failed (%d errors): %s",
		len(e.Errors), strings.Join(msgs, "; "))
}

// HasErrors returns true if there are any validation errors.
func (e *ValidationErrors) HasErrors() bool {
	return len(e.Errors) > 0
}

// Add adds a field error.
func (e *ValidationErrors) Add(field, message, constraint string, value any) {
	e.Errors = append(e.Errors, FieldError{
		Field:      field,
		Message:    message,
		Constraint: constraint,
		Value:      value,
	})
}

// AddSensitive adds a field error for a sensitive field (value excluded from output).
func (e *ValidationErrors) AddSensitive(field, message, constraint string) {
	e.Errors = append(e.Errors, FieldError{
		Field:      field,
		Message:    message,
		Constraint: constraint,
		sensitive:  true,
	})
}

// ToError converts validation errors to a structured *Error with details.
func (e *ValidationErrors) ToError() *Error {
	return BadRequest("Validation failed", Any("details", e.Errors))
}
