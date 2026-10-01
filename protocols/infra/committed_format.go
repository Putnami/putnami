package infra

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// committedLineWidth mirrors json.formatter.lineWidth in
// typescript/extension/config/biome.json. The committed infra/requirements.json
// is rendered to be a fixed point under Biome's formatter (expand:"auto"), so a
// scalar array is collapsed onto one line only when that line fits this width.
// Keep the two values in sync: if the Biome line width changes, this constant
// must change with it, or generated files will churn against `putnami lint`.
const committedLineWidth = 120

// committedIndent is one indentation level. It matches Biome's
// json.formatter.indentStyle="space" / indentWidth=2.
const committedIndent = "  "

// marshalCommitted renders m as the committed infra/requirements.json in the
// exact shape Biome's JSON formatter (expand:"auto") would leave untouched, so
// the file is a fixed point: the generator and the language toolchain's
// formatter agree on one layout and the file only changes when a requirement
// changes.
//
// Biome's "auto" mode keeps objects and arrays that contain objects expanded —
// one element per line, like encoding/json.MarshalIndent — but collapses a
// scalar-only array (strings, numbers, booleans) onto a single line when that
// line fits the print width, counting the leading indentation, the "<key>":
// prefix, and any trailing comma. An array that does not fit is expanded one
// element per line, identical to MarshalIndent. The result is newline
// terminated.
//
// Infra resource names are ASCII (schemas/infra.json restricts them to
// [a-z0-9_./-]) and the only other strings are the schema URL and engine
// enum, so byte length equals display width for the fit check.
func marshalCommitted(m PerProjectManifest) ([]byte, error) {
	// Encode compact first so every value carries its canonical JSON form
	// (preserving struct field order); HTML escaping is disabled to match
	// Biome, which leaves <, >, & unescaped.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	compact := bytes.TrimRight(buf.Bytes(), "\n")

	out, err := formatValue(compact, "", 0, false)
	if err != nil {
		return nil, err
	}
	return append([]byte(out), '\n'), nil
}

// formatValue formats a single compact JSON value. indent is the leading
// whitespace for the lines this value owns; startCol is the column the value
// opens at (the indentation length plus any "<key>": prefix the caller already
// emitted); trailingComma reports whether a comma follows the value on its
// closing line, which counts toward the scalar-array fit check.
func formatValue(raw []byte, indent string, startCol int, trailingComma bool) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("infra: empty JSON value")
	}
	switch trimmed[0] {
	case '{':
		return formatObject(trimmed, indent)
	case '[':
		return formatArray(trimmed, indent, startCol, trailingComma)
	default:
		// Scalar: the compact bytes are already its canonical encoding.
		return string(trimmed), nil
	}
}

func formatObject(raw []byte, indent string) (string, error) {
	type member struct {
		key string
		val json.RawMessage
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // consume '{'
		return "", err
	}
	var members []member
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", err
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", fmt.Errorf("infra: object key is not a string: %v", keyTok)
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return "", err
		}
		members = append(members, member{key: key, val: val})
	}
	if _, err := dec.Token(); err != nil { // consume '}'
		return "", err
	}
	if len(members) == 0 {
		return "{}", nil
	}

	childIndent := indent + committedIndent
	var b strings.Builder
	b.WriteString("{\n")
	for i, m := range members {
		last := i == len(members)-1
		prefix := encodeString(m.key) + ": "
		valStr, err := formatValue(m.val, childIndent, len(childIndent)+len(prefix), !last)
		if err != nil {
			return "", err
		}
		b.WriteString(childIndent)
		b.WriteString(prefix)
		b.WriteString(valStr)
		if !last {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString(indent)
	b.WriteByte('}')
	return b.String(), nil
}

func formatArray(raw []byte, indent string, startCol int, trailingComma bool) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // consume '['
		return "", err
	}
	var elems []json.RawMessage
	for dec.More() {
		var e json.RawMessage
		if err := dec.Decode(&e); err != nil {
			return "", err
		}
		elems = append(elems, e)
	}
	if _, err := dec.Token(); err != nil { // consume ']'
		return "", err
	}
	if len(elems) == 0 {
		return "[]", nil
	}

	// A scalar-only array is a candidate for the collapsed single-line form.
	scalar := true
	parts := make([]string, len(elems))
	for i, e := range elems {
		t := bytes.TrimSpace(e)
		if len(t) > 0 && (t[0] == '{' || t[0] == '[') {
			scalar = false
			break
		}
		parts[i] = string(t)
	}
	if scalar {
		collapsed := "[" + strings.Join(parts, ", ") + "]"
		lineLen := startCol + len(collapsed)
		if trailingComma {
			lineLen++
		}
		if lineLen <= committedLineWidth {
			return collapsed, nil
		}
	}

	// Expanded: one element per line, like encoding/json.MarshalIndent.
	childIndent := indent + committedIndent
	var b strings.Builder
	b.WriteString("[\n")
	for i, e := range elems {
		last := i == len(elems)-1
		valStr, err := formatValue(e, childIndent, len(childIndent), !last)
		if err != nil {
			return "", err
		}
		b.WriteString(childIndent)
		b.WriteString(valStr)
		if !last {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString(indent)
	b.WriteByte(']')
	return b.String(), nil
}

// encodeString returns the canonical JSON encoding of s (quoted, minimally
// escaped, no HTML escaping), matching how Biome and encoding/json render it.
func encodeString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) //nolint:errcheck // encoding a string to a bytes.Buffer cannot fail
	return strings.TrimRight(buf.String(), "\n")
}
