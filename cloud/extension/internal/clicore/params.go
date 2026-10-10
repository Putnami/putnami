package clicore

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Param returns the first present value among names, or nil when none is set.
func Param(params map[string]any, names ...string) any {
	for _, name := range names {
		if v, ok := params[name]; ok {
			return v
		}
	}
	return nil
}

// StringParam returns the first present value among names as a trimmed string.
func StringParam(params map[string]any, names ...string) string {
	return strings.TrimSpace(StringValue(Param(params, names...)))
}

// ScalarStringParam is StringParam but rejects composite (map/slice) values,
// rendering only scalar types.
func ScalarStringParam(params map[string]any, names ...string) string {
	return strings.TrimSpace(ScalarStringValue(Param(params, names...)))
}

// ScalarStringValue renders a scalar value as a string, returning "" for maps,
// slices, and other composite types.
func ScalarStringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	case fmt.Stringer:
		return v.String()
	default:
		switch v := value.(type) {
		case bool:
			return strconv.FormatBool(v)
		case int:
			return strconv.Itoa(v)
		case int64:
			return strconv.FormatInt(v, 10)
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case float32:
			return strconv.FormatFloat(float64(v), 'f', -1, 32)
		default:
			return ""
		}
	}
}

// NumberParam returns the first present value among names as a *float64, or nil
// when none is set or parseable.
func NumberParam(params map[string]any, names ...string) *float64 {
	value := Param(params, names...)
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case float64:
		return &v
	case float32:
		n := float64(v)
		return &n
	case int:
		n := float64(v)
		return &n
	case int64:
		n := float64(v)
		return &n
	case json.Number:
		if n, err := v.Float64(); err == nil {
			return &n
		}
	case string:
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return &n
		}
	}
	return nil
}

// Truthy reports whether value represents a true-ish flag.
func Truthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case float64:
		return v != 0
	case string:
		switch strings.ToLower(v) {
		case "true", "1", "yes", "on":
			return true
		}
	}
	return false
}

// BoolParam returns the first present value among names as a bool, falling back
// to defaultValue when none is set. A present-but-unparseable value is true.
func BoolParam(params map[string]any, defaultValue bool, names ...string) bool {
	value := Param(params, names...)
	if value == nil {
		return defaultValue
	}
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case float64:
		return v != 0
	case string:
		switch strings.ToLower(v) {
		case "false", "0", "no", "off":
			return false
		default:
			return true
		}
	}
	return true
}

// ValueString returns m[key] rendered as a string, or "" when m is nil.
func ValueString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	return StringValue(m[key])
}

// StringValue renders any value as a string.
func StringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	case fmt.Stringer:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

// ValueFloat returns m[key] as a float64, or 0 when m is nil or non-numeric.
func ValueFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		n, _ := v.Float64()
		return n
	case string:
		n, _ := strconv.ParseFloat(v, 64)
		return n
	default:
		return 0
	}
}

// FirstString returns the first non-empty value among values.
func FirstString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// MaxFloat returns the larger of a and b.
func MaxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
