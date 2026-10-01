package storage

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	manifestSchemaPath = "schemas/storage.json"
	bindingSchemaPath  = "schemas/storage-binding.json"
)

func readSchemaBytes(t *testing.T, schemaPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	return data
}

// schemaRootProperties extracts the top-level property names of a schema file.
func schemaRootProperties(t *testing.T, schemaPath string) []string {
	t.Helper()
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(readSchemaBytes(t, schemaPath), &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	return sortedKeys(schema.Properties)
}

// schemaDefProperties extracts property names from a schema $defs entry.
func schemaDefProperties(t *testing.T, schemaPath, defName string) []string {
	t.Helper()
	var schema struct {
		Defs map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(readSchemaBytes(t, schemaPath), &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	def, ok := schema.Defs[defName]
	if !ok {
		t.Fatalf("no $defs/%s in %s", defName, schemaPath)
	}
	return sortedKeys(def.Properties)
}

func sortedKeys(m map[string]any) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// goTypeJSONFields extracts json tag names from the struct underlying v.
func goTypeJSONFields(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		names = append(names, strings.Split(tag, ",")[0])
	}
	sort.Strings(names)
	return names
}

func assertFieldParity(t *testing.T, typeName string, schemaFields, goFields []string) {
	t.Helper()
	schemaSet := make(map[string]bool, len(schemaFields))
	for _, f := range schemaFields {
		schemaSet[f] = true
	}
	goSet := make(map[string]bool, len(goFields))
	for _, f := range goFields {
		goSet[f] = true
	}
	for _, f := range schemaFields {
		if !goSet[f] {
			t.Errorf("%s: field %q exists in schema but not in Go type", typeName, f)
		}
	}
	for _, f := range goFields {
		if !schemaSet[f] {
			t.Errorf("%s: field %q exists in Go type but not in schema", typeName, f)
		}
	}
}

func TestDrift_Manifest(t *testing.T) {
	assertFieldParity(t, "Manifest",
		schemaRootProperties(t, manifestSchemaPath),
		goTypeJSONFields(t, Manifest{}))
}

func TestDrift_Resource(t *testing.T) {
	assertFieldParity(t, "Resource",
		schemaDefProperties(t, manifestSchemaPath, "resource"),
		goTypeJSONFields(t, Resource{}))
}

func TestDrift_Binding(t *testing.T) {
	assertFieldParity(t, "Binding",
		schemaRootProperties(t, bindingSchemaPath),
		goTypeJSONFields(t, Binding{}))
}

// TestDrift_SchemaProtocolVersionPinned asserts each schema pins
// protocolVersion to the current ProtocolVersion, so an editor validating an
// authored manifest/binding agrees with the Go parser.
func TestDrift_SchemaProtocolVersionPinned(t *testing.T) {
	for _, path := range []string{manifestSchemaPath, bindingSchemaPath} {
		t.Run(path, func(t *testing.T) {
			var schema struct {
				Properties struct {
					ProtocolVersion struct {
						Const *int `json:"const"`
					} `json:"protocolVersion"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(readSchemaBytes(t, path), &schema); err != nil {
				t.Fatalf("parse schema %s: %v", path, err)
			}
			got := schema.Properties.ProtocolVersion.Const
			if got == nil {
				t.Fatalf("%s: properties.protocolVersion has no const", path)
			}
			if *got != ProtocolVersion {
				t.Errorf("%s: protocolVersion const = %d, want %d", path, *got, ProtocolVersion)
			}
		})
	}
}
