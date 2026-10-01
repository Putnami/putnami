package errors

import stderrors "errors"

// walkErrors traverses the entire error tree rooted at err — handling both the
// single-child Unwrap() error and multi-child Unwrap() []error forms — and
// invokes fn for every *Error it encounters. Traversal stops as soon as fn
// returns true (and walkErrors then also returns true). This reproduces the
// tree descent that the stdlib errors.Is/errors.As perform internally, which a
// single-level stderrors.Unwrap loop cannot do for multi-error trees such as
// AggregateError (it exposes Unwrap() []error, so Unwrap() returns nil).
func walkErrors(err error, fn func(*Error) bool) bool {
	for err != nil {
		// A per-node type switch is required to run fn on every *Error in the
		// tree; errors.As would only surface the first match and stop.
		switch x := err.(type) {
		case *Error:
			if fn(x) {
				return true
			}
			err = x.Unwrap()
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		case interface{ Unwrap() []error }:
			for _, child := range x.Unwrap() {
				if walkErrors(child, fn) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}

// Is checks if any error in the tree has the given code.
func Is(err error, code Code) bool {
	return walkErrors(err, func(e *Error) bool { return e.code == code })
}

// GetCode extracts the Code from the first *Error in the chain.
// Returns CodeUnknown if no *Error is found.
func GetCode(err error) Code {
	var e *Error
	if stderrors.As(err, &e) {
		return e.code
	}
	return CodeUnknown
}

// GetError extracts the first *Error from the chain.
// Returns nil if no *Error is found.
func GetError(err error) *Error {
	var e *Error
	if stderrors.As(err, &e) {
		return e
	}
	return nil
}

// GetCategory extracts the Category from the first *Error in the chain.
// Returns empty string if no *Error is found.
func GetCategory(err error) Category {
	var e *Error
	if stderrors.As(err, &e) {
		return e.category
	}
	return ""
}

// GetAttrs collects all attrs from the entire error tree.
func GetAttrs(err error) []Attr {
	var result []Attr
	walkErrors(err, func(e *Error) bool {
		result = append(result, e.attrs...)
		return false // visit every *Error in the tree
	})
	return result
}

// IsRetryable checks if any error in the tree is marked retryable.
func IsRetryable(err error) bool {
	return walkErrors(err, func(e *Error) bool { return e.retryable })
}

// Re-export stdlib errors functions so callers can use a single import.
var (
	As     = stderrors.As
	Unwrap = stderrors.Unwrap
	Join   = stderrors.Join
)
