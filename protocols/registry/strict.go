package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// maxCredentialDepth bounds the nesting of one credential-provider line. The
// deepest document is a credential answer: envelope, payload, credential,
// hosts, one host string.
const maxCredentialDepth = 5

// strictLine checks one JSONL line before any typed decode: bounded size,
// valid UTF-8, one JSON value with no trailing data, no null, no duplicate
// object member, and bounded nesting.
func strictLine(line []byte) error {
	if len(line) == 0 || len(line) > MaxCredentialLineBytes {
		return fmt.Errorf("registry: credential line is empty or exceeds %d bytes", MaxCredentialLineBytes)
	}
	if !utf8.Valid(line) {
		return errors.New("registry: credential line is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := strictValue(decoder, 0); err != nil {
		return fmt.Errorf("registry: invalid credential JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("registry: trailing data after the credential JSON value")
	}
	return nil
}

func strictValue(decoder *json.Decoder, depth int) error {
	if depth > maxCredentialDepth {
		return fmt.Errorf("nesting exceeds %d levels", maxCredentialDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("null is not permitted")
	}
	if value, ok := token.(string); ok && strings.ContainsRune(value, utf8.RuneError) {
		return errors.New("replacement characters are not permitted")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid object member %q", key)
			}
			seen[name] = true
			if err := strictValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := strictValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = decoder.Token()
	return err
}

// strictDecode decodes one object into target. Every member name, at every
// depth, is the exact json name of a field of the value it decodes into, and
// every name in required is present. The caller has already run strictLine
// over the enclosing line, so nulls, duplicates and trailing data are gone.
func strictDecode(data []byte, target any, required ...string) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		return errors.New("registry: expected a JSON object")
	}
	for _, name := range required {
		if _, ok := members[name]; !ok {
			return fmt.Errorf("registry: missing member %q", name)
		}
	}
	if err := exactMembers(data, reflect.TypeOf(target)); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	return nil
}

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// exactMembers refuses an object member that is not the exact json name of a
// field of kind, through struct, pointer and slice fields. encoding/json
// matches names without regard to case; the wire does not. A json.RawMessage
// is left to the parser that decodes it, and a shape error to the typed
// decode.
func exactMembers(data []byte, kind reflect.Type) error {
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	switch {
	case kind == rawMessageType:
		return nil
	case kind.Kind() == reflect.Struct:
		var members map[string]json.RawMessage
		if json.Unmarshal(data, &members) != nil {
			return nil
		}
		fields := jsonFields(kind)
		for name, value := range members {
			field, ok := fields[name]
			if !ok {
				return fmt.Errorf("unknown member %q", name)
			}
			if err := exactMembers(value, field); err != nil {
				return err
			}
		}
	case kind.Kind() == reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return nil
		}
		for _, item := range items {
			if err := exactMembers(item, kind.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsonFields maps the json name of each exported field of kind to its type.
func jsonFields(kind reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, kind.NumField())
	for index := 0; index < kind.NumField(); index++ {
		field := kind.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}
