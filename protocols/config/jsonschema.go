package config

import (
	"maps"
	"math"
	"regexp"
	"sort"
	"strconv"
)

// JSONSchemaDraft is the dialect emitted by RenderJSONSchema. Draft
// 2020-12 was picked because it has wide tool support (VS Code's YAML
// extension, ajv, jsonschema CLI) and a stable `$schema` URL.
const JSONSchemaDraft = "https://json-schema.org/draft/2020-12/schema"

// RenderJSONSchema converts a SchemaManifest into a JSON Schema document
// (Draft 2020-12) describing the resolved config tree.
//
// Output shape:
//
//   - The root is an object whose properties are the config block paths
//     (one entry per Block).
//   - Each block is an object schema whose properties mirror its field
//     schemas, with a `required` array built from fields that carry an
//     explicit `required: true` signal.
//   - Field metadata flows through: Description → `description`; Default
//     → `default`; Sensitive → `x-sensitive` (custom extension);
//     Env → `x-env` (custom extension); Constraints → `x-constraints`.
//   - Composite shapes translate naturally: object → nested `properties`,
//     array → `items`, map → `additionalProperties` with `propertyNames`.
//
// Both Go and TS implementations must emit semantically equivalent JSON
// for equivalent manifests — the shared conformance fixture in
// fixtures/jsonschema/*.json locks this invariant.
func RenderJSONSchema(m *SchemaManifest) map[string]any {
	if m == nil {
		return nil
	}
	properties := make(map[string]any, len(m.Configs))
	requiredBlocks := make([]string, 0, len(m.Configs))
	for _, block := range m.Configs {
		properties[block.Path] = renderObjectFields(block.Fields)
		// Blocks describe the contract every config file is expected to
		// populate; the JSON Schema marks them required unless the
		// schema author explicitly opted out via Block.Optional. Fields
		// inside a block are required only when they carry the signal.
		if !block.Optional {
			requiredBlocks = append(requiredBlocks, block.Path)
		}
	}
	sort.Strings(requiredBlocks)

	out := map[string]any{
		"$schema":              JSONSchemaDraft,
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(requiredBlocks) > 0 {
		out["required"] = requiredBlocks
	}
	if m.AppName != "" {
		out["title"] = m.AppName + " config"
	}
	return out
}

func renderObjectFields(fields []FieldSchema) map[string]any {
	properties := make(map[string]any, len(fields))
	var required []string
	for _, f := range fields {
		properties[f.Name] = renderField(f)
		if f.Required {
			required = append(required, f.Name)
		}
	}
	sort.Strings(required)
	out := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func renderField(f FieldSchema) map[string]any {
	out := map[string]any{}
	switch f.Type {
	case FieldTypeString, FieldTypeDuration:
		out["type"] = "string"
		if f.Type == FieldTypeDuration {
			// Duration values are stringly-typed but follow a known shape;
			// signal the semantic to consumers without imposing a regex
			// that would reject otherwise-valid Go durations.
			out["format"] = "duration"
		}
	case FieldTypeInt:
		out["type"] = "integer"
	case FieldTypeFloat:
		out["type"] = "number"
	case FieldTypeBool:
		out["type"] = "boolean"
	case FieldTypeObject:
		maps.Copy(out, renderObjectFields(f.Fields))
	case FieldTypeArray:
		out["type"] = "array"
		if f.Items != nil {
			out["items"] = renderField(*f.Items)
		}
	case FieldTypeMap:
		out["type"] = "object"
		if f.Values != nil {
			out["additionalProperties"] = renderField(*f.Values)
		}
		if f.Keys != "" {
			// propertyNames constrains the keys: JSON Schema can only express
			// string-keyed maps, so non-string primitives (int/bool) translate
			// to string patterns matching their canonical encodings.
			out["propertyNames"] = mapKeyConstraint(f.Keys)
		}
	default:
		out["type"] = "object"
	}

	if f.Description != "" {
		out["description"] = f.Description
	}
	// Default is a transport string at every layer (see FieldSchema.Default,
	// serialized with omitempty). An empty string is therefore
	// indistinguishable from "no default declared", so both Go and TS skip it —
	// including an empty-string default on a string field. When a default is
	// present, coerceDefault renders it as the JSON scalar matching the declared
	// type so the emitted `default` validates against its own typed schema. This
	// rule and the coercion must stay identical to renderJsonSchemaField in the
	// TS twin (config-schema-json.ts).
	if f.Default != "" {
		out["default"] = coerceDefault(f.Type, f.Default)
	}
	if f.Sensitive {
		out["x-sensitive"] = true
	}
	if f.ProductionUnsafeDefault {
		out["x-production-unsafe-default"] = true
	}
	if f.Env != "" {
		out["x-env"] = f.Env
	}
	if len(f.Constraints) > 0 {
		// Constraints retain their original putnami form under a custom
		// extension. Translating each constraint to a strict JSON Schema
		// keyword is intentionally out of scope here — consumers that
		// need it can layer their own translation on top of x-constraints.
		out["x-constraints"] = append([]string(nil), f.Constraints...)
	}
	return out
}

// floatDefaultPattern gates which transport strings are coerced to a JSON
// number for float fields. It admits exactly the JSON-number shape (optional
// sign, integer/fraction, optional exponent) and rejects Go/JS-specific
// spellings that would parse differently across languages (hex floats,
// "Inf"/"NaN", surrounding whitespace). The identical pattern is applied on the
// TS side (config-schema-json.ts) so both runtimes accept and reject the same
// tokens.
var floatDefaultPattern = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

// Safe-integer bounds mirror JavaScript's Number.MAX_SAFE_INTEGER /
// Number.MIN_SAFE_INTEGER. Integer defaults outside this range cannot be
// represented as a JSON number identically in both runtimes, so they fall back
// to the raw string rather than diverge.
const (
	maxSafeInteger int64 = 1<<53 - 1
	minSafeInteger int64 = -(1<<53 - 1)
)

// coerceDefault parses a transport-string default into the JSON scalar that
// matches the field's declared type, so a generated default validates against
// its own typed schema (e.g. an int field emits the number 604800, not the
// string "604800"). Duration stays a string (it already carries
// format:"duration"); string/object/array/map pass through unchanged.
//
// Parsing is intentionally narrow and byte-for-byte identical to the TS twin
// coerceDefault in config-schema-json.ts: bool accepts only "true"/"false";
// int accepts an optionally-signed run of digits within the JS safe-integer
// range; float accepts a JSON-number-shaped, finite token. On any parse failure
// the raw string is returned unchanged — malformed defaults are surfaced by
// validation, not repaired here.
func coerceDefault(fieldType, raw string) any {
	switch fieldType {
	case FieldTypeBool:
		switch raw {
		case "true":
			return true
		case "false":
			return false
		}
	case FieldTypeInt:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= minSafeInteger && n <= maxSafeInteger {
			return n
		}
	case FieldTypeFloat:
		if floatDefaultPattern.MatchString(raw) {
			// The regex already guaranteed a syntactically valid decimal float,
			// so ParseFloat can only fail with ErrRange. Gate on the parsed
			// value being finite rather than on err == nil: overflow yields
			// ±Inf (fall back to the raw string), while underflow yields a
			// finite 0/subnormal that must still be emitted as a JSON number.
			// This mirrors the TS twin's Number.isFinite(Number(raw)) exactly,
			// so e.g. "1e-400" renders 0 on both sides regardless of whether a
			// given Go release reports underflow as ErrRange.
			if n, _ := strconv.ParseFloat(raw, 64); !math.IsInf(n, 0) && !math.IsNaN(n) {
				return n
			}
		}
	}
	return raw
}

// mapKeyConstraint produces the JSON Schema fragment that constrains a
// map's property names. Strings are unrestricted; int/bool keys constrain
// to their canonical decimal/boolean string forms.
func mapKeyConstraint(keyType string) map[string]any {
	switch keyType {
	case FieldTypeInt:
		return map[string]any{"type": "string", "pattern": "^-?[0-9]+$"}
	case FieldTypeBool:
		return map[string]any{"type": "string", "enum": []string{"true", "false"}}
	default:
		return map[string]any{"type": "string"}
	}
}
