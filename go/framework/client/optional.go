package client

import (
	"bytes"
	"encoding/json"
)

// Optional preserves the three JSON states needed by generated contracts:
// absent, explicit null, and a concrete value.
type Optional[T any] struct {
	present bool
	null    bool
	value   T
}

// Some constructs an explicitly present optional value.
func Some[T any](value T) Optional[T] { return Optional[T]{present: true, value: value} }

// Null constructs an explicitly present JSON null.
func Null[T any]() Optional[T] { return Optional[T]{present: true, null: true} }

// Present reports whether the field appeared on the wire or was explicitly set.
func (optional Optional[T]) Present() bool { return optional.present }

// IsNull reports whether the present value is JSON null.
func (optional Optional[T]) IsNull() bool { return optional.present && optional.null }

// Value returns the concrete value and whether it is present and non-null.
func (optional Optional[T]) Value() (T, bool) {
	return optional.value, optional.present && !optional.null
}

// IsZero lets encoding/json's `omitzero` omit the absent state.
func (optional Optional[T]) IsZero() bool { return !optional.present }

// MarshalJSON implements json.Marshaler.
func (optional Optional[T]) MarshalJSON() ([]byte, error) {
	if optional.null || !optional.present {
		return []byte("null"), nil
	}
	return json.Marshal(optional.value)
}

// UnmarshalJSON implements json.Unmarshaler.
func (optional *Optional[T]) UnmarshalJSON(data []byte) error {
	optional.present = true
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		optional.null = true
		var zero T
		optional.value = zero
		return nil
	}
	optional.null = false
	return json.Unmarshal(data, &optional.value)
}
