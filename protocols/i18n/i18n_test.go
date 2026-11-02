package i18n

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const feature = "i18n/catalog-format"

func TestSchemasNameTheirPublishedID(t *testing.T) {
	spectest.Proves(t, feature, "the-schemas-are-published", "each-schema-names-its-published-id")
	for name, tc := range map[string]struct {
		schema []byte
		id     string
	}{
		"catalog":  {CatalogSchema(), CatalogSchemaID},
		"glossary": {GlossarySchema(), GlossarySchemaID},
	} {
		var document struct {
			Dialect string `json:"$schema"`
			ID      string `json:"$id"`
		}
		if err := json.Unmarshal(tc.schema, &document); err != nil {
			t.Fatalf("%s schema is not JSON: %v", name, err)
		}
		if document.ID != tc.id {
			t.Errorf("%s schema $id = %q, want %q", name, document.ID, tc.id)
		}
		if document.Dialect != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s schema dialect = %q, want draft 2020-12", name, document.Dialect)
		}
	}
}

func TestEachSchemaAcceptsTheValidFixturesAndRefusesTheInvalid(t *testing.T) {
	spectest.Proves(t, feature, "the-fixture-corpus-is-the-contract", "each-schema-accepts-the-valid-fixtures-and-refuses-the-invalid")
	for format, raw := range map[string][]byte{"catalog": CatalogSchema(), "glossary": GlossarySchema()} {
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s schema is not JSON: %v", format, err)
		}
		for _, kind := range []string{"valid", "invalid"} {
			files, err := filepath.Glob(filepath.Join("fixtures", format, kind, "*.json"))
			if err != nil || len(files) == 0 {
				t.Fatalf("fixtures/%s/%s holds no fixture", format, kind)
			}
			for _, file := range files {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var value any
				if err := json.Unmarshal(data, &value); err != nil {
					t.Fatalf("%s is not JSON: %v", file, err)
				}
				problems := (&subsetValidator{t: t, root: schema}).check(schema, value, "")
				switch {
				case kind == "valid" && len(problems) > 0:
					t.Errorf("%s schema refuses %s: %s", format, file, strings.Join(problems, "; "))
				case kind == "invalid" && len(problems) == 0:
					t.Errorf("%s schema accepts %s", format, file)
				}
			}
		}
	}
}

// subsetValidator applies the JSON Schema 2020-12 keywords the i18n schemas
// use. It fails the test on any other keyword, so a schema edit that needs a
// keyword it does not apply cannot pass unchecked. format is an annotation in
// 2020-12: the schemas pair it with pattern, which asserts.
type subsetValidator struct {
	t    *testing.T
	root map[string]any
}

var annotations = map[string]bool{"$schema": true, "$id": true, "$defs": true, "title": true, "description": true, "format": true}

func (v *subsetValidator) check(schema map[string]any, value any, at string) []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, at+": "+fmt.Sprintf(format, args...)) }
	object, isObject := value.(map[string]any)
	for keyword, rule := range schema {
		switch keyword {
		case "$ref":
			ref, _ := rule.(string)
			name, ok := strings.CutPrefix(ref, "#/$defs/")
			defs, _ := v.root["$defs"].(map[string]any)
			target, found := defs[name].(map[string]any)
			if !ok || !found {
				v.t.Fatalf("unresolved $ref %q", ref)
			}
			problems = append(problems, v.check(target, value, at)...)
		case "type":
			if !hasType(rule, value) {
				fail("%v is not of type %v", value, rule)
			}
		case "enum":
			if !slices.Contains(rule.([]any), value) {
				fail("%v is not one of %v", value, rule)
			}
		case "const":
			if value != rule {
				fail("%v is not %v", value, rule)
			}
		case "minLength":
			if text, ok := value.(string); ok && float64(len([]rune(text))) < rule.(float64) {
				fail("%q is shorter than %v", text, rule)
			}
		case "pattern":
			if text, ok := value.(string); ok && !regexp.MustCompile(rule.(string)).MatchString(text) {
				fail("%q does not match %s", text, rule)
			}
		case "required":
			for _, name := range rule.([]any) {
				if _, ok := object[name.(string)]; isObject && !ok {
					fail("%s is required", name)
				}
			}
		case "properties":
			for name, sub := range rule.(map[string]any) {
				if field, ok := object[name]; isObject && ok {
					problems = append(problems, v.check(sub.(map[string]any), field, at+"/"+name)...)
				}
			}
		case "additionalProperties":
			if !isObject {
				continue
			}
			declared, _ := schema["properties"].(map[string]any)
			for name, field := range object {
				if _, ok := declared[name]; ok {
					continue
				}
				switch sub := rule.(type) {
				case bool:
					if !sub {
						fail("%s is not allowed", name)
					}
				case map[string]any:
					problems = append(problems, v.check(sub, field, at+"/"+name)...)
				}
			}
		case "if":
			if len(v.check(rule.(map[string]any), value, at)) == 0 {
				if then, ok := schema["then"].(map[string]any); ok {
					problems = append(problems, v.check(then, value, at)...)
				}
			}
		case "then":
		default:
			if !annotations[keyword] {
				v.t.Fatalf("schema keyword %q at %q is outside the subset this test applies", keyword, at)
			}
		}
	}
	return problems
}

func hasType(rule, value any) bool {
	names, ok := rule.([]any)
	if !ok {
		names = []any{rule}
	}
	for _, name := range names {
		switch name {
		case "object":
			if _, ok := value.(map[string]any); ok {
				return true
			}
		case "string":
			if _, ok := value.(string); ok {
				return true
			}
		case "null":
			if value == nil {
				return true
			}
		}
	}
	return false
}
