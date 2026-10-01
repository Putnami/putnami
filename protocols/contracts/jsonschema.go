package contracts

import (
	"encoding/json"
	"sort"

	"go.putnami.dev/protocol/config"
)

// JSONSchemaDraft is the dialect the contract JSON Schema is emitted in. It
// matches protocols/config's RenderJSONSchema so the whole framework speaks one
// JSON Schema dialect.
const JSONSchemaDraft = "https://json-schema.org/draft/2020-12/schema"

// RenderJSONSchema lowers the DTO/enum/union vocabulary of m into a JSON Schema
// (Draft 2020-12) document whose $defs carry one entry per enum, tagged union,
// and struct. Config fields, scopes, capabilities, grants, claims, and principal
// kinds are intentionally excluded: the JSON Schema describes the serializable
// type vocabulary, mirroring protocols/config's jsonschema approach.
//
// The result is a map[string]any so json.Marshal orders keys deterministically
// (encoding/json sorts map keys). The TypeScript twin (contract-emit.ts) builds
// the identical structure and sorts keys byte-wise before serializing, so both
// runtimes emit byte-identical schema bytes (see MarshalJSONSchema).
func RenderJSONSchema(m *Manifest) map[string]any {
	if m == nil {
		return nil
	}
	structsByName := make(map[string]Struct, len(m.Structs))
	for _, s := range m.Structs {
		structsByName[s.Name] = s
	}
	defs := make(map[string]any)
	for _, e := range m.Enums {
		defs[e.Name] = schemaForEnum(e)
	}
	for _, u := range m.Unions {
		defs[u.Name] = schemaForUnion(u, structsByName)
	}
	for _, s := range m.Structs {
		defs[s.Name] = schemaForStruct(s)
	}
	out := map[string]any{
		"$schema": JSONSchemaDraft,
		"$defs":   defs,
	}
	if m.Name != "" {
		out["title"] = m.Name + " contracts"
	}
	return out
}

// MarshalJSONSchema renders m's JSON Schema in the canonical wire form:
// json.MarshalIndent with two-space indentation plus a trailing newline. This is
// the byte-exact artifact the TypeScript emitter must reproduce; the equivalence
// golden testdata/contracts.schema.json pins it, and the TS emitter's parity
// test compares against the same bytes.
func MarshalJSONSchema(m *Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(RenderJSONSchema(m), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func schemaForEnum(e Enum) map[string]any {
	values := make([]any, len(e.Values))
	for i, v := range e.Values {
		values[i] = v.Value
	}
	out := map[string]any{
		"type": "string",
		"enum": values,
	}
	if e.Description != "" {
		out["description"] = e.Description
	}
	return out
}

func schemaForStruct(s Struct) map[string]any {
	props := make(map[string]any, len(s.Fields))
	var required []string
	for _, f := range s.Fields {
		props[f.Name] = schemaForField(f)
		if !f.Optional {
			required = append(required, f.Name)
		}
	}
	sort.Strings(required)
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if s.Description != "" {
		out["description"] = s.Description
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func schemaForField(f Field) map[string]any {
	out := schemaForType(f.Type)
	if f.Repeated {
		out = map[string]any{
			"type":  "array",
			"items": out,
		}
	}
	if f.Description != "" {
		out["description"] = f.Description
	}
	return out
}

// schemaForType maps a field type token to its schema fragment: a primitive
// gets an inline type (duration also carries format:"duration", matching
// protocols/config), and any other token is a declared enum/union/struct
// referenced through #/$defs.
func schemaForType(token string) map[string]any {
	switch token {
	case config.FieldTypeString:
		return map[string]any{"type": "string"}
	case config.FieldTypeInt:
		return map[string]any{"type": "integer"}
	case config.FieldTypeFloat:
		return map[string]any{"type": "number"}
	case config.FieldTypeBool:
		return map[string]any{"type": "boolean"}
	case config.FieldTypeDuration:
		return map[string]any{"type": "string", "format": "duration"}
	default:
		return map[string]any{"$ref": defRef(token)}
	}
}

func schemaForUnion(u Union, structs map[string]Struct) map[string]any {
	variants := make([]any, len(u.Variants))
	for i, v := range u.Variants {
		variants[i] = schemaForVariant(u.Discriminator, v, structs)
	}
	out := map[string]any{
		"oneOf": variants,
	}
	if u.Description != "" {
		out["description"] = u.Description
	}
	return out
}

// schemaForVariant renders one union arm as a single closed object schema that
// pins the discriminator to the variant tag (const) and inlines the variant's
// fields. A struct-ref variant contributes the referenced struct's fields.
//
// The fields are inlined rather than composed with allOf $ref on purpose: a
// referenced struct carries additionalProperties:false, and a JSON Schema allOf
// branch evaluates that constraint independently against the whole object, so
// $ref-ing the struct would reject the discriminator this arm adds — making the
// arm (and thus every instance of the variant) unsatisfiable.
func schemaForVariant(discriminator string, v UnionVariant, structs map[string]Struct) map[string]any {
	fields := v.Fields
	if v.Struct != "" {
		if s, ok := structs[v.Struct]; ok {
			fields = s.Fields
		}
	}
	props := map[string]any{discriminator: map[string]any{"const": v.Tag}}
	required := []string{discriminator}
	for _, f := range fields {
		props[f.Name] = schemaForField(f)
		if !f.Optional {
			required = append(required, f.Name)
		}
	}
	sort.Strings(required)
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
		"required":             required,
	}
	if v.Description != "" {
		out["description"] = v.Description
	}
	return out
}

func defRef(name string) string {
	return "#/$defs/" + name
}
