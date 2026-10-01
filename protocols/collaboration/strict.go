package collaboration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Validation diagnostic codes. Every parser and validator in this package
// emits only these.
const (
	ErrorCodeParseError          = "collaboration.parse_error"
	ErrorCodeUnknownField        = "collaboration.unknown_field"
	ErrorCodeDuplicateMember     = "collaboration.duplicate_member"
	ErrorCodeRequired            = "collaboration.required"
	ErrorCodeInvalidValue        = "collaboration.invalid_value"
	ErrorCodeTooLarge            = "collaboration.too_large"
	ErrorCodeInvalidReference    = "collaboration.invalid_reference"
	ErrorCodeInvalidState        = "collaboration.invalid_state"
	ErrorCodeInvalidPage         = "collaboration.invalid_page"
	ErrorCodeInvalidPrecondition = "collaboration.invalid_precondition"
	ErrorCodeInvalidTimestamp    = "collaboration.invalid_timestamp"
	ErrorCodeInvalidURL          = "collaboration.invalid_url"
	ErrorCodeInvalidOutcome      = "collaboration.invalid_outcome"
	ErrorCodeUnknownContract     = "collaboration.unknown_contract"
	ErrorCodeUnknownOperation    = "collaboration.unknown_operation"
	ErrorCodeUnsupportedVersion  = "collaboration.unsupported_version"
	ErrorCodeCredentialSetting   = "collaboration.credential_setting"
	ErrorCodeInvalidDeclaration  = "collaboration.invalid_declaration"
)

// ValidErrorCodes is the closed validation-code taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:          true,
	ErrorCodeUnknownField:        true,
	ErrorCodeDuplicateMember:     true,
	ErrorCodeRequired:            true,
	ErrorCodeInvalidValue:        true,
	ErrorCodeTooLarge:            true,
	ErrorCodeInvalidReference:    true,
	ErrorCodeInvalidState:        true,
	ErrorCodeInvalidPage:         true,
	ErrorCodeInvalidPrecondition: true,
	ErrorCodeInvalidTimestamp:    true,
	ErrorCodeInvalidURL:          true,
	ErrorCodeInvalidOutcome:      true,
	ErrorCodeUnknownContract:     true,
	ErrorCodeUnknownOperation:    true,
	ErrorCodeUnsupportedVersion:  true,
	ErrorCodeCredentialSetting:   true,
	ErrorCodeInvalidDeclaration:  true,
}

// MaxDocumentBytes bounds every document this package parses. It holds every
// request, and every result of one item, whose members are within their own
// bounds, however their text encodes; only a list page can need more, and a
// provider returns fewer items instead (Serve does it for a Go provider).
const MaxDocumentBytes = 4 << 20

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// strictDecode decodes one JSON object into into. It refuses a document that
// is empty of an object, carries trailing data, repeats a member name inside
// one object, carries a member the Go type does not declare (compared
// case-sensitively, which encoding/json alone does not do), or whose member
// types do not decode. A json.RawMessage member is opaque and not walked.
func strictDecode(data []byte, into any) []diag.Diagnostic {
	if len(data) > MaxDocumentBytes {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeTooLarge, "", "the document exceeds %d bytes", MaxDocumentBytes)}
	}
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "invalid JSON: %v", err)}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "trailing data after the JSON document")}
	}
	if _, ok := raw.(map[string]any); !ok {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "the document must be a JSON object")}
	}
	if field, found := firstDuplicateMember(data); found {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeDuplicateMember, field,
			"member %q appears more than once; the document is ambiguous", field)}
	}
	if field, ok := firstUndeclaredMember(raw, reflect.TypeOf(into), ""); !ok {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownField, field, "unknown member %q", field)}
	}
	typed := json.NewDecoder(bytes.NewReader(data))
	typed.DisallowUnknownFields()
	if err := typed.Decode(into); err != nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "invalid member type: %v", err)}
	}
	return nil
}

// firstUndeclaredMember walks value against typ and returns the dotted path
// of the first member typ does not declare, visiting members in sorted order.
func firstUndeclaredMember(value any, typ reflect.Type, path string) (string, bool) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == rawMessageType {
		return "", true
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return "", true
		}
		fields := jsonFields(typ)
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := joinPath(path, key)
			fieldType, declared := fields[key]
			if !declared {
				return child, false
			}
			if field, ok := firstUndeclaredMember(object[key], fieldType, child); !ok {
				return field, false
			}
		}
	case reflect.Slice:
		items, ok := value.([]any)
		if !ok {
			return "", true
		}
		for i, item := range items {
			if field, ok := firstUndeclaredMember(item, typ.Elem(), path+"["+strconv.Itoa(i)+"]"); !ok {
				return field, false
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return "", true
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if field, ok := firstUndeclaredMember(object[key], typ.Elem(), joinPath(path, key)); !ok {
				return field, false
			}
		}
	}
	return "", true
}

// jsonFields maps the JSON member names a struct type declares to their types.
func jsonFields(typ reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" || !field.IsExported() {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}

// firstDuplicateMember reports the dotted path of the first member name that
// appears twice inside one object. encoding/json keeps the last occurrence
// silently, so a document that names a member twice would otherwise mean
// whatever its last spelling says.
func firstDuplicateMember(data []byte) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	field, found, err := scanDuplicates(decoder, "")
	if err != nil {
		return "", false
	}
	return field, found
}

func scanDuplicates(decoder *json.Decoder, path string) (string, bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", false, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return "", false, nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return "", false, err
			}
			key, _ := keyToken.(string)
			child := joinPath(path, key)
			if seen[key] {
				return child, true, nil
			}
			seen[key] = true
			if field, found, err := scanDuplicates(decoder, child); err != nil || found {
				return field, found, err
			}
		}
		_, err := decoder.Token()
		return "", false, err
	case '[':
		for i := 0; decoder.More(); i++ {
			if field, found, err := scanDuplicates(decoder, path+"["+strconv.Itoa(i)+"]"); err != nil || found {
				return field, found, err
			}
		}
		_, err := decoder.Token()
		return "", false, err
	}
	return "", false, fmt.Errorf("unexpected delimiter %v", delim)
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
