package cli

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

const resultSchemaPath = "schemas/result.json"

type resultSchema struct {
	ID         string                     `json:"$id"`
	Properties map[string]json.RawMessage `json:"properties"`
	Defs       map[string]struct {
		Properties map[string]json.RawMessage `json:"properties"`
	} `json:"$defs"`
}

func loadResultSchema(t *testing.T) resultSchema {
	t.Helper()
	data, err := os.ReadFile(resultSchemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", resultSchemaPath, err)
	}
	var s resultSchema
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse %s: %v", resultSchemaPath, err)
	}
	return s
}

// jsonFieldNames returns the json tag names of a struct's fields (dropping
// ",omitempty" and skipping "-").
func jsonFieldNames(t *testing.T, v any) []string {
	t.Helper()
	rt := reflect.TypeOf(v)
	names := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		if comma := indexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		names = append(names, tag)
	}
	sort.Strings(names)
	return names
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSchemaIDMatchesConstant keeps ResultSchemaID and the schema's $id in
// lock-step.
func TestSchemaIDMatchesConstant(t *testing.T) {
	if got := loadResultSchema(t).ID; got != ResultSchemaID {
		t.Errorf("schema $id = %q, ResultSchemaID = %q", got, ResultSchemaID)
	}
}

// TestResultSchemaDrift fails if the Result / ResultError structs and the JSON
// schema disagree on their field sets — the two must be edited together.
func TestResultSchemaDrift(t *testing.T) {
	s := loadResultSchema(t)

	if got, want := keys(s.Properties), jsonFieldNames(t, Result{}); !reflect.DeepEqual(got, want) {
		t.Errorf("Result fields drift:\n schema: %v\n struct: %v", got, want)
	}

	errDef, ok := s.Defs["resultError"]
	if !ok {
		t.Fatalf("schema has no $defs/resultError")
	}
	if got, want := keys(errDef.Properties), jsonFieldNames(t, ResultError{}); !reflect.DeepEqual(got, want) {
		t.Errorf("ResultError fields drift:\n schema: %v\n struct: %v", got, want)
	}
}
