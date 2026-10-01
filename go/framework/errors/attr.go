package errors

import "time"

// Attr is a typed key-value pair for structured error context.
type Attr struct {
	Key   string `json:"key"`
	Value any    `json:"value,omitempty"`
}

// String creates a string attribute.
func String(key, value string) Attr { return Attr{Key: key, Value: value} }

// Int creates an integer attribute.
func Int(key string, value int) Attr { return Attr{Key: key, Value: value} }

// Int64 creates an int64 attribute.
func Int64(key string, value int64) Attr { return Attr{Key: key, Value: value} }

// Float64 creates a float64 attribute.
func Float64(key string, value float64) Attr { return Attr{Key: key, Value: value} }

// Bool creates a boolean attribute.
func Bool(key string, value bool) Attr { return Attr{Key: key, Value: value} }

// Any creates an attribute with an arbitrary value.
func Any(key string, value any) Attr { return Attr{Key: key, Value: value} }

// Duration creates a duration attribute.
func Duration(key string, d time.Duration) Attr { return Attr{Key: key, Value: d} }
