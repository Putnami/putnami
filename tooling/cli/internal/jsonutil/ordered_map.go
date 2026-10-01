// Package jsonutil provides order-preserving JSON utilities.
//
// Go's encoding/json marshals map keys in sorted order, which rewrites
// package.json property order on every round-trip. OrderedMap preserves
// the original key order from the source file, appending new keys at the end.
package jsonutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// OrderedMap is a JSON object that preserves insertion order of keys.
// Nested JSON objects are also represented as *OrderedMap.
type OrderedMap struct {
	keys   []string
	values map[string]any
}

// New creates an empty OrderedMap.
func New() *OrderedMap {
	return &OrderedMap{values: make(map[string]any)}
}

// Get returns the value for key and whether it exists.
func (m *OrderedMap) Get(key string) (any, bool) {
	v, ok := m.values[key]
	return v, ok
}

// GetMap returns the value for key as an *OrderedMap, or nil if missing or not an object.
func (m *OrderedMap) GetMap(key string) *OrderedMap {
	v, ok := m.values[key]
	if !ok {
		return nil
	}
	om, _ := v.(*OrderedMap)
	return om
}

// Set sets a key-value pair. New keys are appended at the end.
func (m *OrderedMap) Set(key string, value any) {
	if m.values == nil {
		m.values = make(map[string]any)
	}
	if _, exists := m.values[key]; !exists {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

// Delete removes a key, preserving order of remaining keys.
func (m *OrderedMap) Delete(key string) {
	if _, exists := m.values[key]; !exists {
		return
	}
	delete(m.values, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			return
		}
	}
}

// Len returns the number of keys.
func (m *OrderedMap) Len() int {
	return len(m.keys)
}

// UnmarshalJSON decodes JSON while preserving key order.
func (m *OrderedMap) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))

	t, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("expected '{', got %v", t)
	}

	m.keys = nil
	m.values = make(map[string]any)

	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string)
		m.keys = append(m.keys, key)

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}

		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			nested := &OrderedMap{}
			if err := json.Unmarshal(raw, nested); err != nil {
				return err
			}
			m.values[key] = nested
		} else {
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				return err
			}
			m.values[key] = v
		}
	}

	// Consume closing '}'.
	_, err = dec.Token()
	return err
}

// marshalNoEscape marshals v the way encoding/json.Marshal does, except that
// '&', '<', and '>' are left as themselves rather than escaped to their \uXXXX
// forms.
//
// json.Marshal's escaping exists to make JSON safe to embed inside an HTML
// <script> tag — not a concern for a file this package only ever writes to
// disk. Left on, it silently rewrites any value round-tripped through this
// package that happens to contain one of those bytes (a query string in a
// recorded URL, a tag with a literal "&", ...): ReadFile decodes the escape
// back to the plain byte, and the next WriteFile re-escapes it, so a file a
// human or another tool committed with the plain byte comes back modified even
// though nothing about its meaning changed. That turns a config rewrite (for
// example `projects tag`, or a workspace membership update) into a working-tree
// diff no one asked for.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder.Encode appends a trailing '\n' that json.Marshal does not;
	// callers here re-indent or otherwise consume the bytes directly, so drop
	// it to keep this a drop-in replacement for json.Marshal.
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// marshalIndent writes the map as indented JSON, preserving key order.
func (m *OrderedMap) marshalIndent(indent string, level int) ([]byte, error) {
	if len(m.keys) == 0 {
		return []byte("{}"), nil
	}

	var buf bytes.Buffer
	buf.WriteString("{\n")

	prefix := strings.Repeat(indent, level+1)
	for i, key := range m.keys {
		val := m.values[key]

		buf.WriteString(prefix)
		keyJSON, _ := marshalNoEscape(key)
		buf.Write(keyJSON)
		buf.WriteString(": ")

		if err := writeValue(&buf, val, indent, level+1); err != nil {
			return nil, err
		}

		if i < len(m.keys)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}

	buf.WriteString(strings.Repeat(indent, level))
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// writeValue writes a single JSON value, re-indenting arrays and plain objects.
func writeValue(buf *bytes.Buffer, val any, indent string, level int) error {
	if nested, ok := val.(*OrderedMap); ok {
		data, err := nested.marshalIndent(indent, level)
		if err != nil {
			return err
		}
		buf.Write(data)
		return nil
	}

	data, err := marshalNoEscape(val)
	if err != nil {
		return err
	}

	// Re-indent arrays and objects so they align with the current nesting.
	if len(data) > 0 && (data[0] == '[' || data[0] == '{') {
		var indented bytes.Buffer
		if err := json.Indent(&indented, data, strings.Repeat(indent, level), indent); err != nil {
			return err
		}
		buf.Write(indented.Bytes())
	} else {
		buf.Write(data)
	}
	return nil
}

// ReadFile reads a JSON file into an OrderedMap.
func ReadFile(path string) (*OrderedMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := &OrderedMap{}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, err
	}
	return m, nil
}

// WriteFile writes an OrderedMap as 2-space indented JSON with a trailing newline.
func WriteFile(path string, m *OrderedMap) error {
	data, err := m.Bytes()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Bytes renders an OrderedMap as two-space indented JSON with a trailing
// newline while preserving object-member order.
func (m *OrderedMap) Bytes() ([]byte, error) {
	data, err := m.marshalIndent("  ", 0)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
