// Package schematest judges a document against the published JSON Schema of
// the screenshots manifest, for the tests of a writer or of a reader. It
// evaluates the draft-07 keywords the schema uses and fails the test on a
// keyword it does not know, so the schema cannot grow a rule a test ignores.
//
// A schema does not see every rule of the contract: its description names
// the ones only the decoder applies. A consumer decodes with
// screenshots.Decode; this package is for tests.
package schematest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"go.putnami.dev/protocol/screenshots"
)

// Check returns the first reason the published schema of the document's
// version refuses document, or nil when that schema accepts it: the version 1
// schema for a document whose protocolVersion is 1, the current schema for
// any other. It fails the test when the schema itself cannot be evaluated.
func Check(tb testing.TB, document []byte) error {
	tb.Helper()
	var value any
	if err := decode(document, &value); err != nil {
		return fmt.Errorf("the document is not JSON: %w", err)
	}
	var root map[string]any
	if err := decode(SchemaOf(value), &root); err != nil {
		tb.Fatalf("the schema is not JSON: %v", err)
	}
	e := &evaluator{tb: tb, root: root}
	return e.check(root, value, "$")
}

// SchemaOf returns the published schema that judges a decoded document: the
// version 1 schema when its protocolVersion is 1, the current one otherwise.
func SchemaOf(document any) []byte {
	if object, ok := document.(map[string]any); ok {
		if version, ok := object["protocolVersion"].(json.Number); ok {
			if value, err := version.Float64(); err == nil && value == screenshots.ProtocolVersionV1 {
				return screenshots.SchemaV1()
			}
		}
	}
	return screenshots.Schema()
}

// decode reads the first JSON value of data, keeping each number as written.
func decode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}

type evaluator struct {
	tb   testing.TB
	root map[string]any
}

var annotations = map[string]bool{"$schema": true, "$id": true, "title": true, "description": true, "$defs": true}

func (e *evaluator) check(schema, value any, at string) error {
	node, ok := schema.(map[string]any)
	if !ok {
		e.tb.Fatalf("the schema node at %s is not an object", at)
	}
	keywords := make([]string, 0, len(node))
	for keyword := range node {
		keywords = append(keywords, keyword)
	}
	sort.Strings(keywords)
	for _, keyword := range keywords {
		if annotations[keyword] {
			continue
		}
		if err := e.keyword(node, keyword, node[keyword], value, at); err != nil {
			return err
		}
	}
	return nil
}

func (e *evaluator) keyword(node map[string]any, keyword string, argument, value any, at string) error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s", at, fmt.Sprintf(format, args...))
	}
	switch keyword {
	case "$ref":
		name, ok := strings.CutPrefix(e.text(argument), "#/$defs/")
		defs, _ := e.root["$defs"].(map[string]any)
		if !ok || defs[name] == nil {
			e.tb.Fatalf("the schema holds an unresolvable $ref %q", argument)
		}
		return e.check(defs[name], value, at)
	case "type":
		if !hasType(e.text(argument), value) {
			return refuse("is no %s", argument)
		}
		return nil
	case "const":
		if !equal(argument, value) {
			return refuse("is not %v", argument)
		}
		return nil
	case "enum":
		for _, candidate := range e.list(argument) {
			if equal(candidate, value) {
				return nil
			}
		}
		return refuse("is none of %v", argument)
	case "not":
		if e.check(argument, value, at) == nil {
			return refuse("matches a schema it must not match")
		}
		return nil
	case "anyOf":
		for _, schema := range e.list(argument) {
			if e.check(schema, value, at) == nil {
				return nil
			}
		}
		return refuse("matches none of the allowed schemas")
	}
	switch value := value.(type) {
	case map[string]any:
		switch keyword {
		case "required":
			for _, name := range e.list(argument) {
				if _, ok := value[e.text(name)]; !ok {
					return refuse("lacks the member %s", name)
				}
			}
			return nil
		case "properties":
			for name, schema := range e.object(argument) {
				if member, ok := value[name]; ok {
					if err := e.check(schema, member, at+"."+name); err != nil {
						return err
					}
				}
			}
			return nil
		case "additionalProperties":
			if allowed, ok := argument.(bool); !ok || allowed {
				e.tb.Fatalf("the schema uses additionalProperties with %v, which this evaluator does not implement", argument)
			}
			declared, _ := node["properties"].(map[string]any)
			for name := range value {
				if _, ok := declared[name]; !ok {
					return refuse("holds the unknown member %s", name)
				}
			}
			return nil
		}
	case []any:
		switch keyword {
		case "maxItems":
			if float64(len(value)) > number(argument) {
				return refuse("holds %d items, more than %v", len(value), argument)
			}
			return nil
		case "minItems":
			if float64(len(value)) < number(argument) {
				return refuse("holds %d items, fewer than %v", len(value), argument)
			}
			return nil
		case "uniqueItems":
			if unique, ok := argument.(bool); !ok || !unique {
				e.tb.Fatalf("the schema uses uniqueItems with %v, which this evaluator does not implement", argument)
			}
			for i := range value {
				for j := range i {
					if equal(value[i], value[j]) {
						return refuse("holds the item %d twice", j)
					}
				}
			}
			return nil
		case "items":
			for i, item := range value {
				if err := e.check(argument, item, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
			return nil
		}
	case string:
		switch keyword {
		case "maxLength":
			if float64(utf8.RuneCountInString(value)) > number(argument) {
				return refuse("is longer than %v characters", argument)
			}
			return nil
		case "pattern":
			if !regexp.MustCompile(e.text(argument)).MatchString(value) {
				return refuse("does not match %s", argument)
			}
			return nil
		}
	case json.Number:
		got, err := value.Float64()
		switch keyword {
		case "minimum":
			if err != nil || got < number(argument) {
				return refuse("is less than %v", argument)
			}
			return nil
		case "maximum":
			if err != nil || got > number(argument) {
				return refuse("is more than %v", argument)
			}
			return nil
		}
	}
	// A keyword that does not apply to the type of the value accepts it.
	switch keyword {
	case "required", "properties", "additionalProperties", "maxItems", "minItems", "uniqueItems", "items", "maxLength", "pattern", "minimum", "maximum":
		return nil
	}
	e.tb.Fatalf("the schema uses %q, which this evaluator does not implement", keyword)
	return nil
}

// text, list and object read the argument of a keyword, and fail the test on
// a schema that gives the keyword another type.
func (e *evaluator) text(argument any) string {
	value, ok := argument.(string)
	if !ok {
		e.tb.Fatalf("the schema gives %v where a keyword takes a string", argument)
	}
	return value
}

func (e *evaluator) list(argument any) []any {
	value, ok := argument.([]any)
	if !ok {
		e.tb.Fatalf("the schema gives %v where a keyword takes an array", argument)
	}
	return value
}

func (e *evaluator) object(argument any) map[string]any {
	value, ok := argument.(map[string]any)
	if !ok {
		e.tb.Fatalf("the schema gives %v where a keyword takes an object", argument)
	}
	return value
}

func hasType(kind string, value any) bool {
	switch value := value.(type) {
	case map[string]any:
		return kind == "object"
	case []any:
		return kind == "array"
	case string:
		return kind == "string"
	case bool:
		return kind == "boolean"
	case json.Number:
		got, err := value.Float64()
		return kind == "number" || kind == "integer" && err == nil && got == math.Trunc(got)
	}
	return false
}

func number(value any) float64 {
	text, _ := value.(json.Number)
	got, _ := text.Float64()
	return got
}

func equal(left, right any) bool {
	leftNumber, leftIsNumber := left.(json.Number)
	rightNumber, rightIsNumber := right.(json.Number)
	if leftIsNumber && rightIsNumber {
		a, errA := leftNumber.Float64()
		b, errB := rightNumber.Float64()
		return errA == nil && errB == nil && a == b
	}
	return reflect.DeepEqual(left, right)
}
