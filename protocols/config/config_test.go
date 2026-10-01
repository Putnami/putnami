package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestResolutionLayers(t *testing.T) {
	layers := ResolutionLayers("my-app", "production", "1.2.0")
	if len(layers) != 5 {
		t.Fatalf("expected 5 layers, got %d", len(layers))
	}

	expected := []struct {
		app, env, ver string
		priority      int
	}{
		{"*", "*", "", 10},
		{"*", "production", "", 20},
		{"my-app", "*", "", 30},
		{"my-app", "production", "", 40},
		{"my-app", "production", "1.2.0", 50},
	}

	for i, e := range expected {
		if layers[i].AppName != e.app || layers[i].Environment != e.env || layers[i].Version != e.ver || layers[i].Priority != e.priority {
			t.Errorf("layer[%d] = %+v, want app=%s env=%s ver=%s pri=%d", i, layers[i], e.app, e.env, e.ver, e.priority)
		}
	}
}

func TestResolutionLayersNoVersion(t *testing.T) {
	layers := ResolutionLayers("my-app", "staging", "")
	if len(layers) != 4 {
		t.Fatalf("expected 4 layers without version, got %d", len(layers))
	}
}

func TestDeepMerge(t *testing.T) {
	dst := map[string]any{
		"server": map[string]any{
			"host": "localhost",
			"port": 8080,
		},
		"keep": "this",
	}
	src := map[string]any{
		"server": map[string]any{
			"host": "0.0.0.0",
			"tls":  true,
		},
		"new": "value",
	}

	DeepMerge(dst, src)

	server := dst["server"].(map[string]any)
	if server["host"] != "0.0.0.0" {
		t.Errorf("host should be overridden, got %v", server["host"])
	}
	if server["port"] != 8080 {
		t.Errorf("port should be preserved, got %v", server["port"])
	}
	if server["tls"] != true {
		t.Errorf("tls should be added")
	}
	if dst["keep"] != "this" {
		t.Errorf("keep should be preserved")
	}
	if dst["new"] != "value" {
		t.Errorf("new should be added")
	}
}

func TestValidFieldTypes(t *testing.T) {
	for _, ft := range []string{"string", "int", "float", "bool", "duration", "object", "array", "map"} {
		if !ValidFieldTypes[ft] {
			t.Errorf("%q should be a valid field type", ft)
		}
	}
	for _, ft := range []string{"number", "integer", "double", "text", ""} {
		if ValidFieldTypes[ft] {
			t.Errorf("%q should not be a valid field type", ft)
		}
	}
}

func TestSchemaManifestJSONSchemaVocabularyMatchesProtocol(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(SchemaManifestJSONSchema(), &doc); err != nil {
		t.Fatalf("SchemaManifestJSONSchema invalid JSON: %v", err)
	}
	defs := doc["$defs"].(map[string]any)
	fieldType := defs["fieldType"].(map[string]any)
	mapKeyType := defs["mapKeyType"].(map[string]any)

	if got := stringsFromJSON(fieldType["enum"]); !reflect.DeepEqual(got, FieldTypeValues()) {
		t.Fatalf("field type schema enum = %v, want %v", got, FieldTypeValues())
	}
	if got := stringsFromJSON(mapKeyType["enum"]); !reflect.DeepEqual(got, MapKeyTypeValues()) {
		t.Fatalf("map key schema enum = %v, want %v", got, MapKeyTypeValues())
	}
}

func TestSchemaManifestJSONSchemaCompositeSlotRules(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(SchemaManifestJSONSchema(), &doc); err != nil {
		t.Fatalf("SchemaManifestJSONSchema invalid JSON: %v", err)
	}
	defs := doc["$defs"].(map[string]any)
	rules := defs["compositeSlotRules"].(map[string]any)["allOf"].([]any)

	objectRule := schemaRuleByTypeConst(t, rules, FieldTypeObject)
	if !schemaRuleProhibitsRequired(objectRule, "fields") {
		t.Fatalf("object rule should prohibit fields for non-object schemas: %v", objectRule)
	}

	arrayRule := schemaRuleByTypeConst(t, rules, FieldTypeArray)
	if got := stringsFromJSON(arrayRule["then"].(map[string]any)["required"]); !reflect.DeepEqual(got, []string{"items"}) {
		t.Fatalf("array rule required = %v, want [items]", got)
	}
	if !schemaRuleProhibitsRequired(arrayRule, "items") {
		t.Fatalf("array rule should prohibit items for non-array schemas: %v", arrayRule)
	}

	mapRule := schemaRuleByTypeConst(t, rules, FieldTypeMap)
	if got := stringsFromJSON(mapRule["then"].(map[string]any)["required"]); !reflect.DeepEqual(got, []string{"keys", "values"}) {
		t.Fatalf("map rule required = %v, want [keys values]", got)
	}
	if !schemaRuleProhibitsRequired(mapRule, "keys") || !schemaRuleProhibitsRequired(mapRule, "values") {
		t.Fatalf("map rule should prohibit keys and values for non-map schemas: %v", mapRule)
	}
}

func schemaRuleByTypeConst(t *testing.T, rules []any, fieldType string) map[string]any {
	t.Helper()
	for _, raw := range rules {
		rule := raw.(map[string]any)
		ifClause := rule["if"].(map[string]any)
		properties := ifClause["properties"].(map[string]any)
		typeProperty := properties["type"].(map[string]any)
		if typeProperty["const"] == fieldType {
			return rule
		}
	}
	t.Fatalf("missing composite slot rule for %q", fieldType)
	return nil
}

func schemaRuleProhibitsRequired(rule map[string]any, field string) bool {
	elseClause, ok := rule["else"].(map[string]any)
	if !ok {
		return false
	}
	notClause, ok := elseClause["not"].(map[string]any)
	if !ok {
		return false
	}
	if required, ok := notClause["required"]; ok {
		return containsString(stringsFromJSON(required), field)
	}
	for _, raw := range notClause["anyOf"].([]any) {
		item := raw.(map[string]any)
		if containsString(stringsFromJSON(item["required"]), field) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func stringsFromJSON(v any) []string {
	raw := v.([]any)
	out := make([]string, len(raw))
	for i, item := range raw {
		out[i] = item.(string)
	}
	return out
}
