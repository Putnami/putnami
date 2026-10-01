package errors

import (
	"encoding/json"
	"time"
)

// jsonError is the JSON representation of *Error.
// Stack traces and cause chains are intentionally excluded from serialization
// to prevent leaking internal details (DB errors, file paths) to HTTP clients.
type jsonError struct {
	Code      Code     `json:"code"`
	Message   string   `json:"message"`
	Category  Category `json:"category,omitempty"`
	Retryable bool     `json:"retryable,omitempty"`
	Source    string   `json:"source,omitempty"`
	Attrs     []Attr   `json:"attrs,omitempty"`
	Time      string   `json:"time,omitempty"`
}

// genericErrorMessage is the client-facing message substituted for errors whose
// category is not client-safe. Shared by MarshalJSON and WriteHTTPError so the
// two serialization paths expose the same text.
const genericErrorMessage = "An internal error occurred"

// clientSafeCategory reports whether an error's message and attrs are safe to
// expose to clients. Defined once so MarshalJSON and WriteHTTPError gate
// identically and the category check cannot be bypassed via direct marshaling.
func clientSafeCategory(c Category) bool {
	return c == CategoryUser || c == CategorySecurity
}

// MarshalJSON implements json.Marshaler. Serialization is gated by category to
// stay consistent with WriteHTTPError: only client-safe errors (CategoryUser /
// CategorySecurity) expose their message and attrs. Internal/infra/bug/transient
// errors — and uncategorized ones — emit a generic message and omit attrs, so a
// direct json.Marshal(err), an *Error embedded in a response struct, or one
// returned as handler data cannot leak internal text or sensitive attributes
// (DSNs, internal IDs, query text). Stack traces and cause chains are never
// serialized either — they are for server-side logging only.
func (e *Error) MarshalJSON() ([]byte, error) {
	j := jsonError{
		Code:      e.code,
		Message:   e.message,
		Category:  e.category,
		Retryable: e.retryable,
		Source:    e.source,
		Attrs:     e.attrs,
	}
	if !clientSafeCategory(e.category) {
		j.Message = genericErrorMessage
		j.Attrs = nil
	}
	if !e.time.IsZero() {
		j.Time = e.time.Format(time.RFC3339Nano)
	}
	return json.Marshal(j)
}

// UnmarshalJSON implements json.Unmarshaler.
func (e *Error) UnmarshalJSON(data []byte) error {
	var j jsonError
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	e.code = j.Code
	e.message = j.Message
	e.category = j.Category
	e.retryable = j.Retryable
	e.source = j.Source
	e.attrs = j.Attrs
	if j.Time != "" {
		if t, err := time.Parse(time.RFC3339Nano, j.Time); err == nil {
			e.time = t
		}
	}
	return nil
}
