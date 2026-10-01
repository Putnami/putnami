package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Nesting budgets. A bare document (manifest, request, bundle) nests at most
// MaxDocumentDepth levels; a provider RPC envelope wraps the deepest of them
// (a submit payload's request, whose task resources sit at depth 7) two
// levels deeper, so the envelope parser owns its own budget.
const (
	MaxDocumentDepth = 8
	MaxEnvelopeDepth = 12
)

func strictJSON(data []byte) error {
	return strictJSONDepth(data, MaxDocumentDepth)
}

func strictJSONDepth(data []byte, maxDepth int) error {
	if len(data) > MaxManifestBytes || !utf8.Valid(data) {
		return fmt.Errorf("runner: manifest exceeds byte limit or contains invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := strictValue(decoder, 0, maxDepth); err != nil {
		return fmt.Errorf("runner: invalid JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("runner: trailing JSON data")
	}
	return nil
}

func strictValue(decoder *json.Decoder, depth, maxDepth int) error {
	if depth > maxDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels", maxDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return fmt.Errorf("null is not permitted")
	}
	if value, ok := token.(string); ok && strings.ContainsRune(value, utf8.RuneError) {
		return fmt.Errorf("replacement characters or malformed Unicode escapes are not permitted")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' {
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid object key %q", key)
			}
			seen[name] = true
			if err := strictValue(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
	} else if delim == '[' {
		for decoder.More() {
			if err := strictValue(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = decoder.Token()
	return err
}

// strictObject checks that data is a JSON object whose members are exactly the
// required keys plus a subset of the optional keys, and returns them raw.
func strictObject(data []byte, required, optional []string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = true
		if len(bytes.TrimSpace(fields[key])) == 0 {
			return nil, fmt.Errorf("runner: missing field %q", key)
		}
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range fields {
		if !allowed[key] {
			return nil, fmt.Errorf("runner: unknown field %q", key)
		}
	}
	return fields, nil
}

// strictArray checks that data is a JSON array and returns its raw elements.
func strictArray(data []byte, limit int) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, err
	}
	if items == nil {
		return nil, fmt.Errorf("runner: expected an array")
	}
	if len(items) > limit {
		return nil, fmt.Errorf("runner: array exceeds %d elements", limit)
	}
	return items, nil
}
