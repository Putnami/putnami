package transaction

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	unitOfWorkSchemaPath = "schemas/transaction.json"
	resultSchemaPath     = "schemas/transaction-result.json"
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

// schemaRootRequired extracts the top-level required property names of a schema.
func schemaRootRequired(t *testing.T, schemaPath string) []string {
	t.Helper()
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(readSchemaBytes(t, schemaPath), &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	return sortedStrings(schema.Required)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
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
	return names
}

func assertRequiredFields(t *testing.T, typeName string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s required fields = %v, want %v", typeName, got, want)
	}
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

func TestDrift_UnitOfWork(t *testing.T) {
	assertFieldParity(t, "UnitOfWork",
		schemaRootProperties(t, unitOfWorkSchemaPath),
		goTypeJSONFields(t, UnitOfWork{}))
}

func TestDrift_Result(t *testing.T) {
	assertFieldParity(t, "Result",
		schemaRootProperties(t, resultSchemaPath),
		goTypeJSONFields(t, Result{}))
}

func TestDrift_SchemaRequiredFields(t *testing.T) {
	assertRequiredFields(t, "UnitOfWork",
		schemaRootRequired(t, unitOfWorkSchemaPath),
		[]string{"propagation", "protocolVersion"})
	assertRequiredFields(t, "Result",
		schemaRootRequired(t, resultSchemaPath),
		[]string{"outcome", "protocolVersion"})
}

// TestDrift_SchemaProtocolVersionPinned asserts each schema pins protocolVersion
// to the current ProtocolVersion, so an editor validating an authored document
// agrees with the Go parser.
func TestDrift_SchemaProtocolVersionPinned(t *testing.T) {
	for _, path := range []string{unitOfWorkSchemaPath, resultSchemaPath} {
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

// TestDrift_SchemaEnumParity asserts the closed enums in the schemas match the
// Go enum values, so a schema edit that drops or adds a value fails the guard.
func TestDrift_SchemaEnumParity(t *testing.T) {
	t.Run("propagation", func(t *testing.T) {
		assertEnumParity(t, unitOfWorkSchemaPath, "propagation",
			[]string{string(PropagationRequired), string(PropagationRequiresNew), string(PropagationNested)})
	})
	t.Run("isolation", func(t *testing.T) {
		assertEnumParity(t, unitOfWorkSchemaPath, "isolation",
			[]string{string(IsolationReadCommitted), string(IsolationRepeatableRead), string(IsolationSerializable)})
	})
	t.Run("outcome", func(t *testing.T) {
		assertEnumParity(t, resultSchemaPath, "outcome",
			[]string{string(OutcomeApplied), string(OutcomeAlreadyConsumedConflict), string(OutcomeNotFound), string(OutcomeRetryableSerializationFailure)})
	})
}

func assertEnumParity(t *testing.T, schemaPath, defName string, want []string) {
	t.Helper()
	var schema struct {
		Defs map[string]struct {
			Enum []string `json:"enum"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(readSchemaBytes(t, schemaPath), &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	def, ok := schema.Defs[defName]
	if !ok {
		t.Fatalf("no $defs/%s in %s", defName, schemaPath)
	}
	if !reflect.DeepEqual(sortedStrings(def.Enum), sortedStrings(want)) {
		t.Errorf("%s enum = %v, want %v", defName, def.Enum, want)
	}
}
