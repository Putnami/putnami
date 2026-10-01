package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
)

// This file holds the canonical construction and serialization helpers every
// renderer uses. Building attributes through AttrsFromStrings (sorted by key)
// and serializing through MarshalCanonical is what makes two runtimes emit
// byte-identical OTLP/JSON for equivalent input.

// StringVal builds a string-valued AnyValue.
func StringVal(s string) AnyValue { return AnyValue{StringValue: &s} }

// BoolVal builds a bool-valued AnyValue.
func BoolVal(b bool) AnyValue { return AnyValue{BoolValue: &b} }

// IntVal builds an int64-valued AnyValue (encoded as a decimal string).
func IntVal(i int64) AnyValue {
	s := strconv.FormatInt(i, 10)
	return AnyValue{IntValue: &s}
}

// DoubleVal builds a double-valued AnyValue.
func DoubleVal(f float64) AnyValue { return AnyValue{DoubleValue: &f} }

// Attr builds one KeyValue attribute.
func Attr(key string, value AnyValue) KeyValue { return KeyValue{Key: key, Value: value} }

// AttrsFromStrings builds a KeyValue slice from a string map, sorted by key.
// Sorting is mandatory: attribute order is otherwise map-iteration-random in Go
// and insertion-ordered in TS, which would break byte-equivalence. Returns nil
// for an empty map so the attributes field is omitted entirely.
func AttrsFromStrings(m map[string]string) []KeyValue {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, Attr(k, StringVal(m[k])))
	}
	return out
}

// SortAttrs sorts a KeyValue slice in place by key, returning it for chaining.
// Use when attributes come from a source that is not already a string map.
func SortAttrs(attrs []KeyValue) []KeyValue {
	sort.SliceStable(attrs, func(i, j int) bool { return attrs[i].Key < attrs[j].Key })
	return attrs
}

// ResourceFromStrings builds a Resource with sorted attributes from a string
// map. Empty-valued keys are dropped so optional attributes (service.version)
// only appear when known.
func ResourceFromStrings(m map[string]string) Resource {
	filtered := make(map[string]string, len(m))
	for k, v := range m {
		if v != "" {
			filtered[k] = v
		}
	}
	return Resource{Attributes: AttrsFromStrings(filtered)}
}

// FormatUint encodes a uint64 (timestamp, count) as the decimal string OTLP/JSON
// requires for 64-bit integers.
func FormatUint(v uint64) string { return strconv.FormatUint(v, 10) }

// FormatInt encodes an int64 as a decimal string.
func FormatInt(v int64) string { return strconv.FormatInt(v, 10) }

// SeverityNumberFor maps a lowercase level name ("debug","info","warn","error",
// "trace","fatal") to its canonical OTLP severity number, or 0 when unknown.
func SeverityNumberFor(level string) SeverityNumber {
	switch level {
	case "trace":
		return SeverityTrace
	case "debug":
		return SeverityDebug
	case "info":
		return SeverityInfo
	case "warn", "warning":
		return SeverityWarn
	case "error":
		return SeverityError
	case "fatal":
		return SeverityFatal
	default:
		return 0
	}
}

// MarshalCanonical serializes a value to compact OTLP/JSON. With attributes
// already sorted (via AttrsFromStrings/SortAttrs) and metrics/spans/logs in a
// deterministic order, the field-order-stable encoders on both sides produce
// byte-identical output. encoding/json escapes HTML by default; we disable that
// so the bytes match JSON.stringify, which does not.
func MarshalCanonical(v any) ([]byte, error) {
	return marshalNoEscape(v)
}

// Digest returns the lowercase hex SHA-256 of b. Both runtimes hash the bytes
// of MarshalCanonical for the same input and must arrive at the same digest;
// the equivalence tests pin it.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// marshalNoEscape is json.Marshal with HTML escaping disabled. json.Marshal
// escapes <, >, & as < etc.; JSON.stringify does not, so we must turn it
// off for cross-language byte-equivalence.
func marshalNoEscape(v any) ([]byte, error) {
	var sb []byte
	buf := bytesBuffer{b: &sb}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a trailing newline; strip it.
	if n := len(sb); n > 0 && sb[n-1] == '\n' {
		sb = sb[:n-1]
	}
	return sb, nil
}

// bytesBuffer is a minimal io.Writer over a *[]byte so we avoid importing bytes
// for a single growable buffer.
type bytesBuffer struct{ b *[]byte }

func (w *bytesBuffer) Write(p []byte) (int, error) {
	*w.b = append(*w.b, p...)
	return len(p), nil
}
