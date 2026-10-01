package config

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestRenderJSONSchema_NestedFixtureConformance is the Go side of the
// cross-language JSON Schema anchor: rendering the shared manifest
// fixture must produce semantically equivalent JSON to the saved
// .jsonschema.json companion. The TS extractor's test renders the same
// manifest and compares against the same companion file.
func TestRenderJSONSchema_NestedFixtureConformance(t *testing.T) {
	manifestData, err := os.ReadFile("fixtures/valid/nested-hash.json")
	if err != nil {
		t.Fatal(err)
	}
	var m SchemaManifest
	if err := json.Unmarshal(manifestData, &m); err != nil {
		t.Fatal(err)
	}

	expectedData, err := os.ReadFile("fixtures/jsonschema/nested-hash.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(expectedData, &expected); err != nil {
		t.Fatal(err)
	}

	// Round-trip through JSON so map[string]any values compare cleanly
	// (the renderer constructs typed Go structures; expected was parsed
	// from JSON and is purely map[string]any).
	gotData, err := json.Marshal(RenderJSONSchema(&m))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("JSON Schema mismatch:\n  got:  %s\n  want: %s", gotData, expectedData)
	}
}

// TestRenderJSONSchema_TypedDefaultsFixtureConformance is the Go side of the
// cross-language anchor for typed defaults: rendering the shared manifest
// fixture must reproduce the saved .jsonschema companion, in which each default
// is emitted as the JSON scalar matching its declared field type (bool→boolean,
// int→integer, float→number) rather than a transport string. The TS extractor's
// test renders the same manifest and compares against the same companion file.
func TestRenderJSONSchema_TypedDefaultsFixtureConformance(t *testing.T) {
	manifestData, err := os.ReadFile("fixtures/valid/typed-defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	var m SchemaManifest
	if err := json.Unmarshal(manifestData, &m); err != nil {
		t.Fatal(err)
	}

	expectedData, err := os.ReadFile("fixtures/jsonschema/typed-defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(expectedData, &expected); err != nil {
		t.Fatal(err)
	}

	gotData, err := json.Marshal(RenderJSONSchema(&m))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("JSON Schema mismatch:\n  got:  %s\n  want: %s", gotData, expectedData)
	}
}

// TestRenderJSONSchema_TypedDefaultDeclaredTypeParity asserts each rendered
// default is the JSON type its own field schema declares, so the default would
// validate against that schema (a boolean default under type:boolean, an
// integer default under type:integer, a number default under type:number).
func TestRenderJSONSchema_TypedDefaultDeclaredTypeParity(t *testing.T) {
	m := &SchemaManifest{
		AppName: "app",
		Configs: []Block{{
			Path: "session",
			Fields: []FieldSchema{
				{Name: "secure", Type: FieldTypeBool, Default: "true"},
				{Name: "ttl", Type: FieldTypeInt, Default: "604800"},
				{Name: "sampleRate", Type: FieldTypeFloat, Default: "0.5"},
			},
		}},
	}
	props := RenderJSONSchema(m)["properties"].(map[string]any)["session"].(map[string]any)["properties"].(map[string]any)

	secure := props["secure"].(map[string]any)
	if secure["type"] != "boolean" {
		t.Fatalf("secure type = %v, want boolean", secure["type"])
	}
	if _, ok := secure["default"].(bool); !ok {
		t.Errorf("type:boolean default must be a JSON boolean, got %T (%v)", secure["default"], secure["default"])
	}

	ttl := props["ttl"].(map[string]any)
	if ttl["type"] != "integer" {
		t.Fatalf("ttl type = %v, want integer", ttl["type"])
	}
	if _, ok := ttl["default"].(int64); !ok {
		t.Errorf("type:integer default must be a JSON integer, got %T (%v)", ttl["default"], ttl["default"])
	}

	sampleRate := props["sampleRate"].(map[string]any)
	if _, ok := sampleRate["default"].(float64); !ok {
		t.Errorf("type:number default must be a JSON number, got %T (%v)", sampleRate["default"], sampleRate["default"])
	}
}

// TestRenderJSONSchema_EmptyStringDefaultOmitted pins the reconciled Go/TS rule:
// an empty-string default is indistinguishable from "no default declared" at the
// transport layer (Default is serialized with omitempty), so the emitter omits
// `default`. This previously diverged — TS emitted "" while Go dropped it.
func TestRenderJSONSchema_EmptyStringDefaultOmitted(t *testing.T) {
	got := renderField(FieldSchema{Name: "label", Type: FieldTypeString, Default: ""})
	if _, present := got["default"]; present {
		t.Errorf("empty-string default must be omitted, got default=%v", got["default"])
	}
}

// TestCoerceDefault locks the narrow, cross-language parse semantics shared with
// the TS twin: bool→JSON boolean, int→JSON integer (within the JS safe-integer
// range), float→JSON number (finite JSON-number tokens), duration/string
// unchanged, and any parse failure keeps the raw string.
func TestCoerceDefault(t *testing.T) {
	tests := []struct {
		name      string
		fieldType string
		raw       string
		want      any
	}{
		{"bool true", FieldTypeBool, "true", true},
		{"bool false", FieldTypeBool, "false", false},
		{"bool other keeps raw", FieldTypeBool, "yes", "yes"},
		{"int", FieldTypeInt, "604800", int64(604800)},
		{"int signed", FieldTypeInt, "-5", int64(-5)},
		{"int non-numeric keeps raw", FieldTypeInt, "not-a-number", "not-a-number"},
		{"int at safe-integer max", FieldTypeInt, "9007199254740991", int64(9007199254740991)},
		{"int beyond safe integer keeps raw", FieldTypeInt, "9007199254740993", "9007199254740993"},
		{"int overflow keeps raw", FieldTypeInt, "99999999999999999999", "99999999999999999999"},
		{"float", FieldTypeFloat, "0.5", 0.5},
		{"float exponent", FieldTypeFloat, "1e3", float64(1000)},
		{"float inf keeps raw", FieldTypeFloat, "Inf", "Inf"},
		{"float hex keeps raw", FieldTypeFloat, "0x1p4", "0x1p4"},
		// Underflow rounds to a finite 0 and MUST emit as the number 0 (valid
		// against type:number), matching TS Number("1e-400")===0. Overflow
		// becomes ±Inf and MUST fall back to the raw string on both sides.
		// Locks the finiteness gate so Go never diverges from the TS twin
		// regardless of how a Go release flags underflow as ErrRange.
		{"float underflow emits zero", FieldTypeFloat, "1e-400", float64(0)},
		{"float overflow keeps raw", FieldTypeFloat, "1e400", "1e400"},
		{"float negative overflow keeps raw", FieldTypeFloat, "-1e400", "-1e400"},
		{"duration stays string", FieldTypeDuration, "30s", "30s"},
		{"string stays string", FieldTypeString, "cookie", "cookie"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coerceDefault(tt.fieldType, tt.raw); got != tt.want {
				t.Errorf("coerceDefault(%q, %q) = %#v (%T), want %#v (%T)",
					tt.fieldType, tt.raw, got, got, tt.want, tt.want)
			}
		})
	}
}

// TestRenderJSONSchema_ProductionUnsafeMarker asserts the production-unsafe
// marker flows into the rendered JSON Schema as the custom `x-production-unsafe-default`
// extension when set, and is omitted otherwise — mirroring the x-sensitive path
// and the TS twin (config-schema-json.ts).
func TestRenderJSONSchema_ProductionUnsafeMarker(t *testing.T) {
	marked := renderField(FieldSchema{Name: "driver", Type: FieldTypeString, ProductionUnsafeDefault: true})
	if marked["x-production-unsafe-default"] != true {
		t.Errorf("marked field must render x-production-unsafe-default=true, got %v", marked["x-production-unsafe-default"])
	}

	unmarked := renderField(FieldSchema{Name: "driver", Type: FieldTypeString})
	if _, present := unmarked["x-production-unsafe-default"]; present {
		t.Errorf("unmarked field must omit x-production-unsafe-default, got %v", unmarked["x-production-unsafe-default"])
	}
}

func TestRenderJSONSchema_NilManifest(t *testing.T) {
	if got := RenderJSONSchema(nil); got != nil {
		t.Fatalf("expected nil for nil manifest, got %v", got)
	}
}

func TestRenderJSONSchema_RequiredOnlyOptIn(t *testing.T) {
	m := &SchemaManifest{
		AppName: "app",
		Configs: []Block{{
			Path: "server",
			Fields: []FieldSchema{
				{Name: "host", Type: FieldTypeString},
				{Name: "port", Type: FieldTypeInt, Required: true},
			},
		}},
	}
	out := RenderJSONSchema(m)
	server := out["properties"].(map[string]any)["server"].(map[string]any)
	required, ok := server["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "port" {
		t.Errorf("required should contain only opt-in fields, got: %v", server["required"])
	}
}

func TestRenderJSONSchema_MapKeyConstraints(t *testing.T) {
	tests := []struct {
		key  string
		want map[string]any
	}{
		{FieldTypeString, map[string]any{"type": "string"}},
		{FieldTypeInt, map[string]any{"type": "string", "pattern": "^-?[0-9]+$"}},
		{FieldTypeBool, map[string]any{"type": "string", "enum": []string{"true", "false"}}},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := mapKeyConstraint(tt.key)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mapKeyConstraint(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}
