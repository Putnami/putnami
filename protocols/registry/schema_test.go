package registry

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// schemaOnlyInexpressible lists the invalid fixtures whose violation is a rule
// the schema description names as one it cannot express. Every other invalid
// fixture fails the schema, and each one listed here passes it.
var schemaOnlyInexpressible = map[string]string{
	"request-duplicate-member.json":                      "no duplicate member",
	"request-trailing-data.json":                         "no trailing data",
	"request-initialize-run-credential-long-utf8.json":   "a runCredential of at most 16384 bytes",
	"response-credential-host-port-range.json":           "a port between 1 and 65535",
	"response-credential-refusal-long-utf8-message.json": "a refusal message of at most 512 bytes",
	"response-credential-unsorted-hosts.json":            "sorted hosts",
	"request-resolve-invalid-channel.json":               "distribution/release-set/v2 documents",
	"request-release-null-in-request.json":               "distribution/release-set/v2 documents",
	"response-release-unknown-outcome.json":              "distribution/release-set/v2 documents",
	"response-resolve-release-form.json":                 "the payload shape of a response, which depends on the op of the request it answers",
	"request-open-digest-mismatch.json":                  "a planDigest equal to the digest of the plan",
	"request-open-unsorted-members.json":                 "plan members unique, in (ecosystem, coordinate) order",
	"request-open-member-source-mismatch.json":           "that share the plan's sourceRevision",
	"request-open-immutable-channel-not-listed.json":     "an immutableChannel listed in channels",
	"request-open-ancestry-channel-order.json":           "ancestry channels that name the plan's or the release's channels in order",
	"request-release-ancestry-channel-mismatch.json":     "ancestry channels that name the plan's or the release's channels in order",
	"request-open-ancestry-source-mismatch.json":         "read from the plan's sourceRevision",
	"request-open-own-head-not-ancestor.json":            "ancestor true when headSourceRevision equals sourceRevision",
	"request-release-head-for-empty-channel.json":        "no headSourceRevision for a release channel expected to have no head",
	"request-release-unsorted-images.json":               "evidence images unique, in project order",
}

// TestCredentialFixturesAgainstTheSchema evaluates every fixture against
// schemas/credential-provider-v1.json, so a second implementation that
// validates with the schema accepts every valid line and refuses every
// invalid line except those whose rule the schema names as inexpressible.
func TestCredentialFixturesAgainstTheSchema(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("schemas", "credential-provider-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := decodeNumbers(data, &root); err != nil {
		t.Fatal(err)
	}
	description, _ := root["description"].(string)
	for name, rule := range schemaOnlyInexpressible {
		if !strings.Contains(description, rule) {
			t.Errorf("%s: the schema description does not name %q", name, rule)
		}
	}
	evaluator := &schemaEvaluator{t: t, root: root}
	for _, validity := range []string{"valid", "invalid"} {
		paths, err := filepath.Glob(filepath.Join("fixtures", "credential-provider", validity, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("fixture corpus: %v, %v", paths, err)
		}
		for _, path := range paths {
			line, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// The first JSON value of the line: a schema judges one value, so
			// trailing data is outside what it can see.
			var value any
			decoder := json.NewDecoder(bytes.NewReader(line))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			name := filepath.Base(path)
			valid := evaluator.valid(root, value)
			_, inexpressible := schemaOnlyInexpressible[name]
			switch {
			case validity == "valid" && !valid:
				t.Errorf("valid fixture %s fails the schema", name)
			case validity == "invalid" && valid && !inexpressible:
				t.Errorf("invalid fixture %s passes the schema", name)
			case validity == "invalid" && !valid && inexpressible:
				t.Errorf("invalid fixture %s fails the schema; drop it from schemaOnlyInexpressible", name)
			}
		}
	}
}

func decodeNumbers(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}

// schemaEvaluator evaluates the draft-07 keywords this schema uses. A keyword
// it does not know fails the test, so the schema cannot grow a rule this
// evaluator silently ignores.
type schemaEvaluator struct {
	t    *testing.T
	root map[string]any
}

var schemaAnnotations = map[string]bool{"$schema": true, "$id": true, "title": true, "description": true, "definitions": true, "format": true}

func (e *schemaEvaluator) valid(schema, value any) bool {
	node, ok := schema.(map[string]any)
	if !ok {
		e.t.Fatalf("schema node %v is not an object", schema)
	}
	keywords := make([]string, 0, len(node))
	for keyword := range node {
		keywords = append(keywords, keyword)
	}
	sort.Strings(keywords)
	for _, keyword := range keywords {
		if schemaAnnotations[keyword] || keyword == "then" || keyword == "else" {
			continue
		}
		if !e.keyword(node, keyword, node[keyword], value) {
			return false
		}
	}
	return true
}

func (e *schemaEvaluator) keyword(node map[string]any, keyword string, argument, value any) bool {
	switch keyword {
	case "$ref":
		return e.valid(e.resolve(argument.(string)), value)
	case "type":
		return schemaType(argument.(string), value)
	case "const":
		return schemaEqual(argument, value)
	case "enum":
		for _, candidate := range argument.([]any) {
			if schemaEqual(candidate, value) {
				return true
			}
		}
		return false
	case "if":
		branch, ok := node["else"]
		if e.valid(argument, value) {
			branch, ok = node["then"]
		}
		return !ok || e.valid(branch, value)
	case "not":
		return !e.valid(argument, value)
	case "allOf", "anyOf", "oneOf":
		return e.combine(keyword, argument.([]any), value)
	}
	switch value := value.(type) {
	case map[string]any:
		return e.object(node, keyword, argument, value)
	case []any:
		return e.array(keyword, argument, value)
	case string:
		return e.text(keyword, argument, value)
	case json.Number:
		return e.number(keyword, argument, value)
	}
	return e.known(keyword)
}

func (e *schemaEvaluator) combine(keyword string, schemas []any, value any) bool {
	matches := 0
	for _, schema := range schemas {
		if e.valid(schema, value) {
			matches++
		}
	}
	switch keyword {
	case "allOf":
		return matches == len(schemas)
	case "anyOf":
		return matches > 0
	}
	return matches == 1
}

func (e *schemaEvaluator) object(node map[string]any, keyword string, argument any, value map[string]any) bool {
	switch keyword {
	case "required":
		for _, name := range argument.([]any) {
			if _, ok := value[name.(string)]; !ok {
				return false
			}
		}
		return true
	case "properties":
		for name, schema := range argument.(map[string]any) {
			if member, ok := value[name]; ok && !e.valid(schema, member) {
				return false
			}
		}
		return true
	case "additionalProperties":
		declared, _ := node["properties"].(map[string]any)
		for name := range value {
			if _, ok := declared[name]; !ok && !argument.(bool) {
				return false
			}
		}
		return true
	}
	return e.known(keyword)
}

func (e *schemaEvaluator) array(keyword string, argument any, value []any) bool {
	switch keyword {
	case "minItems":
		return float64(len(value)) >= schemaNumber(argument)
	case "maxItems":
		return float64(len(value)) <= schemaNumber(argument)
	case "uniqueItems":
		for i := range value {
			for j := i + 1; j < len(value); j++ {
				if schemaEqual(value[i], value[j]) {
					return !argument.(bool)
				}
			}
		}
		return true
	case "items":
		for _, item := range value {
			if !e.valid(argument, item) {
				return false
			}
		}
		return true
	}
	return e.known(keyword)
}

func (e *schemaEvaluator) text(keyword string, argument any, value string) bool {
	switch keyword {
	case "minLength":
		return float64(utf8.RuneCountInString(value)) >= schemaNumber(argument)
	case "maxLength":
		return float64(utf8.RuneCountInString(value)) <= schemaNumber(argument)
	case "pattern":
		return regexp.MustCompile(argument.(string)).MatchString(value)
	}
	return e.known(keyword)
}

func (e *schemaEvaluator) number(keyword string, argument any, value json.Number) bool {
	switch keyword {
	case "minimum":
		number, err := value.Float64()
		return err == nil && number >= schemaNumber(argument)
	case "maximum":
		number, err := value.Float64()
		return err == nil && number <= schemaNumber(argument)
	}
	return e.known(keyword)
}

// known accepts a keyword that does not apply to the value's type, and fails
// the test on a keyword this evaluator does not implement.
func (e *schemaEvaluator) known(keyword string) bool {
	switch keyword {
	case "required", "properties", "additionalProperties", "minItems", "maxItems", "uniqueItems", "items",
		"minLength", "maxLength", "pattern", "minimum", "maximum":
		return true
	}
	e.t.Fatalf("the schema uses %q, which this evaluator does not implement", keyword)
	return false
}

func (e *schemaEvaluator) resolve(ref string) any {
	name, ok := strings.CutPrefix(ref, "#/definitions/")
	definitions, _ := e.root["definitions"].(map[string]any)
	if !ok || definitions[name] == nil {
		e.t.Fatalf("unresolvable $ref %q", ref)
	}
	return definitions[name]
}

func schemaType(kind string, value any) bool {
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
		if kind == "number" {
			return true
		}
		number, err := value.Float64()
		return kind == "integer" && err == nil && number == math.Trunc(number)
	}
	return false
}

func schemaNumber(value any) float64 {
	number, _ := value.(json.Number).Float64()
	return number
}

func schemaEqual(left, right any) bool {
	leftNumber, leftIsNumber := left.(json.Number)
	rightNumber, rightIsNumber := right.(json.Number)
	if leftIsNumber && rightIsNumber {
		a, errA := leftNumber.Float64()
		b, errB := rightNumber.Float64()
		return errA == nil && errB == nil && a == b
	}
	return reflect.DeepEqual(left, right)
}
