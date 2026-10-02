package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// maxCredentialDepth bounds the nesting of one credential-v1 line. The
// deepest document is a credential answer: envelope, payload, credential,
// hosts, one host string.
const maxCredentialDepth = 5

// maxPublicationDepth bounds the nesting of one publication-v1 line. The
// deepest document is a resolve answer, eleven levels: envelope, payload,
// response, heads, one head, releaseSet, members, one member, dependencies,
// one dependency, one of its strings.
const maxPublicationDepth = 12

// lineBounds is the size and nesting one JSON document may reach. Depth counts
// levels below the top-level value, which is level 0.
type lineBounds struct {
	maxBytes int
	maxDepth int
}

var (
	credentialBounds  = lineBounds{maxBytes: MaxCredentialLineBytes, maxDepth: maxCredentialDepth}
	publicationBounds = lineBounds{maxBytes: MaxPublicationLineBytes, maxDepth: maxPublicationDepth}
)

// jsonScan reports what a strict scan measured.
type jsonScan struct {
	// depth is the deepest level the document reached.
	depth int
	// delegatedNull reports a null inside a delegated member.
	delegatedNull bool
}

// strictLine checks one credential-v1 JSONL line before any typed decode:
// bounded size, valid UTF-8, one JSON value with no trailing data, no null, no
// duplicate object member, and bounded nesting.
func strictLine(line []byte) error {
	_, err := strictScan(line, credentialBounds, nil)
	return err
}

// strictScan checks one JSON document before any typed decode: at most
// bounds.maxBytes, valid UTF-8, one JSON value with no trailing data, no
// duplicate object member, no U+FFFD, at most bounds.maxDepth levels, and no
// null outside a delegated member.
//
// A delegated member is named by its chain of member names from the top-level
// object. Its value is a distribution/release-set/v2 document whose own strict
// decoder decides where null is admitted, so a null inside it is reported in
// the result instead of refused here.
func strictScan(data []byte, bounds lineBounds, delegated [][]string) (jsonScan, error) {
	if len(data) == 0 || len(data) > bounds.maxBytes {
		return jsonScan{}, fmt.Errorf("registry: credential line is empty or exceeds %d bytes", bounds.maxBytes)
	}
	if !utf8.Valid(data) {
		return jsonScan{}, errors.New("registry: credential line is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	scanner := &strictScanner{decoder: decoder, maxDepth: bounds.maxDepth, delegated: delegated}
	if err := scanner.value(0, []string{}, false); err != nil {
		return jsonScan{}, fmt.Errorf("registry: invalid credential JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return jsonScan{}, errors.New("registry: trailing data after the credential JSON value")
	}
	return scanner.scan, nil
}

type strictScanner struct {
	decoder   *json.Decoder
	maxDepth  int
	delegated [][]string
	scan      jsonScan
}

// value scans one JSON value at depth. path is the member chain from the
// top-level object while it can still reach a delegated member, nil otherwise;
// inDelegated reports that the value lies inside a delegated member.
func (s *strictScanner) value(depth int, path []string, inDelegated bool) error {
	if depth > s.maxDepth {
		return fmt.Errorf("nesting exceeds %d levels", s.maxDepth)
	}
	s.scan.depth = max(s.scan.depth, depth)
	token, err := s.decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		if inDelegated {
			s.scan.delegatedNull = true
			return nil
		}
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
		for s.decoder.More() {
			key, err := s.decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid object member %q", key)
			}
			seen[name] = true
			childPath, childDelegated := s.member(path, name, inDelegated)
			if err := s.value(depth+1, childPath, childDelegated); err != nil {
				return err
			}
		}
	case '[':
		for s.decoder.More() {
			if err := s.value(depth+1, nil, inDelegated); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = s.decoder.Token()
	return err
}

// member returns the path and delegation of member name of the object at
// path. A delegated member is reached only through object members.
func (s *strictScanner) member(path []string, name string, inDelegated bool) ([]string, bool) {
	if inDelegated || path == nil {
		return nil, inDelegated
	}
	next := slices.Concat(path, []string{name})
	prefix := false
	for _, candidate := range s.delegated {
		if len(candidate) < len(next) || !slices.Equal(candidate[:len(next)], next) {
			continue
		}
		if len(candidate) == len(next) {
			return nil, true
		}
		prefix = true
	}
	if prefix {
		return next, false
	}
	return nil, false
}

// strictDecode decodes one object into target. Every member name, at every
// depth, is the exact json name of a field of the value it decodes into, and
// every name in required is present. The caller has already run strictScan
// over the enclosing line, so nulls, duplicates and trailing data are gone.
func strictDecode(data []byte, target any, required ...string) error {
	return decodeMembers(data, target, false, required...)
}

// strictDecodeComplete is strictDecode where, at every depth, a member whose
// json tag has no omitempty is required, and a member whose tag has omitempty
// is absent rather than empty. Decoding and re-encoding the value then
// reproduces the members it read.
func strictDecodeComplete(data []byte, target any) error {
	return decodeMembers(data, target, true)
}

func decodeMembers(data []byte, target any, complete bool, required ...string) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		return errors.New("registry: expected a JSON object")
	}
	for _, name := range required {
		if _, ok := members[name]; !ok {
			return fmt.Errorf("registry: missing member %q", name)
		}
	}
	if err := exactMembers(data, reflect.TypeOf(target), complete); err != nil {
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
// decode. When complete is true, it also refuses an absent member whose tag
// has no omitempty and an empty member whose tag has omitempty.
func exactMembers(data []byte, kind reflect.Type, complete bool) error {
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
			if complete && field.optional && emptyJSON(value) {
				return fmt.Errorf("member %q is empty; an absent value is omitted", name)
			}
			if err := exactMembers(value, field.kind, complete); err != nil {
				return err
			}
		}
		if complete {
			for _, name := range slices.Sorted(maps.Keys(fields)) {
				if _, present := members[name]; !present && !fields[name].optional {
					return fmt.Errorf("missing member %q", name)
				}
			}
		}
	case kind.Kind() == reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return nil
		}
		for _, item := range items {
			if err := exactMembers(item, kind.Elem(), complete); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsonField is one exported field of a wire struct: its type, and whether its
// json tag has omitempty.
type jsonField struct {
	kind     reflect.Type
	optional bool
}

// jsonFields maps the json name of each exported field of kind to its field.
func jsonFields(kind reflect.Type) map[string]jsonField {
	fields := make(map[string]jsonField, kind.NumField())
	for index := 0; index < kind.NumField(); index++ {
		field := kind.Field(index)
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = jsonField{kind: field.Type, optional: slices.Contains(strings.Split(options, ","), "omitempty")}
	}
	return fields
}

// emptyJSON reports whether value is a JSON value encoding/json omits under
// omitempty: "", false, 0, [] or {}.
func emptyJSON(value json.RawMessage) bool {
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return false
	}
	switch typed := decoded.(type) {
	case string:
		return typed == ""
	case bool:
		return !typed
	case float64:
		return typed == 0
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	}
	return false
}
