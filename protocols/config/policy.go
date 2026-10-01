package config

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ValidateConfigEntryAgainstSchema enforces the publish-time policy for
// plaintext config writes against the registered schema. For a recognized block
// it rejects unknown fields, invalid value types, invalid map keys, and fields
// marked Sensitive anywhere in nested object/array/map values.
//
// Use this at publish/PUT time alongside ValidateConfigEntry, which only
// checks structural invariants.
func ValidateConfigEntryAgainstSchema(e *ConfigEntry, m *SchemaManifest) []diag.Diagnostic {
	if e == nil || m == nil || e.Values == nil {
		return nil
	}
	block, ok := blockAtPath(e.Path, m)
	if !ok {
		return nil
	}
	return validateObjectValues(block.Fields, e.Values, "values")
}

// ValidateSecretEntryAgainstSchema warns when a non-sensitive value is being
// stored in the secrets store. Returns warning diagnostics (never errors):
// the write is harmless but suggests the schema is stale or the caller is
// over-encrypting.
//
// Two path shapes are supported:
//   - block.field paths (e.g. "database.password") — warns when the named
//     field exists in the schema and is not marked Sensitive.
//   - block paths (e.g. "database") — warns when the block exists in the
//     schema and contains zero Sensitive fields. Storing the entire block
//     in the secrets store would over-encrypt only-plaintext config.
//
// Paths absent from the schema are not flagged here — that is
// ValidatePathInSchema's job and runs as an error, not a warning.
func ValidateSecretEntryAgainstSchema(e *SecretEntry, m *SchemaManifest) []diag.Diagnostic {
	if e == nil || m == nil {
		return nil
	}

	if block, ok := blockAtPath(e.Path, m); ok {
		if blockHasSensitive(block) {
			return nil
		}
		return []diag.Diagnostic{
			diag.Warningf("non-sensitive-in-secret", "path",
				"block %q contains no fields marked sensitive in the schema; storing the entire block in the secrets store over-encrypts plaintext config",
				e.Path),
		}
	}

	if f, ok := fieldAtSchemaPath(e.Path, m); ok && !fieldContainsSensitive(f) {
		return []diag.Diagnostic{
			diag.Warningf("non-sensitive-in-secret", "path",
				"field %q is not marked sensitive in the schema; storing it in the secrets store over-encrypts a plaintext value",
				e.Path),
		}
	}

	return nil
}

// ValidatePathInSchema enforces the schema-as-contract invariant that every
// written path must be present in the registered schema. No grandfathering:
// an entry written before its field joined the schema is rejected too, so the
// schema stays the manifest of every value the app reads. Returns an error
// diagnostic when the path is not represented.
//
// For ConfigEntry, the path is the block path; for SecretEntry, the path is
// either a block path or a block.field path. We accept both forms by
// matching against the block prefix when the full path is not a registered
// block.
func ValidatePathInSchema(path string, m *SchemaManifest) []diag.Diagnostic {
	if m == nil || path == "" {
		return nil
	}
	if pathInSchema(path, m) {
		return nil
	}
	return []diag.Diagnostic{
		diag.Errorf("path-not-in-schema", "path",
			"path %q is not present in the registered schema; add the field to the schema or delete the entry",
			path),
	}
}

func pathInSchema(path string, m *SchemaManifest) bool {
	if _, ok := blockAtPath(path, m); ok {
		return true
	}
	_, ok := fieldAtSchemaPath(path, m)
	return ok
}

func blockAtPath(path string, m *SchemaManifest) (Block, bool) {
	for _, b := range m.Configs {
		if b.Path == path {
			return b, true
		}
	}
	return Block{}, false
}

func fieldAtSchemaPath(path string, m *SchemaManifest) (FieldSchema, bool) {
	var (
		best     Block
		bestRest string
		found    bool
	)
	for _, b := range m.Configs {
		prefix := b.Path + "."
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		if !found || len(b.Path) > len(best.Path) {
			best = b
			bestRest = strings.TrimPrefix(path, prefix)
			found = true
		}
	}
	if !found {
		return FieldSchema{}, false
	}
	return fieldAtPath(best.Fields, strings.Split(bestRest, "."))
}

func fieldAtPath(fields []FieldSchema, parts []string) (FieldSchema, bool) {
	if len(parts) == 0 || parts[0] == "" {
		return FieldSchema{}, false
	}
	for _, f := range fields {
		if f.Name != parts[0] {
			continue
		}
		if len(parts) == 1 {
			return f, true
		}
		return childFieldAtPath(f, parts[1:])
	}
	return FieldSchema{}, false
}

func childFieldAtPath(f FieldSchema, parts []string) (FieldSchema, bool) {
	if len(parts) == 0 {
		return f, true
	}
	switch f.Type {
	case FieldTypeObject:
		return fieldAtPath(f.Fields, parts)
	case FieldTypeArray:
		if f.Items == nil {
			return FieldSchema{}, false
		}
		if isArrayIndex(parts[0]) {
			parts = parts[1:]
		}
		return childFieldAtPath(*f.Items, parts)
	case FieldTypeMap:
		if f.Values == nil {
			return FieldSchema{}, false
		}
		if !mapKeyMatches(f.Keys, parts[0]) {
			return FieldSchema{}, false
		}
		parts = parts[1:]
		return childFieldAtPath(*f.Values, parts)
	default:
		return FieldSchema{}, false
	}
}

func validateObjectValues(fields []FieldSchema, values map[string]any, parent string) []diag.Diagnostic {
	byName := make(map[string]FieldSchema, len(fields))
	for _, f := range fields {
		byName[f.Name] = f
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var diags []diag.Diagnostic
	for _, key := range keys {
		fieldPath := parent + "." + key
		f, ok := byName[key]
		if !ok {
			diags = append(diags, diag.Errorf("field-not-in-schema", fieldPath,
				"field %q is not present in the registered schema", displayConfigPath(fieldPath)))
			continue
		}
		diags = append(diags, validateConfigValue(f, values[key], fieldPath)...)
	}
	return diags
}

func validateConfigValue(f FieldSchema, value any, fieldPath string) []diag.Diagnostic {
	if f.Sensitive {
		return []diag.Diagnostic{
			diag.Errorf("sensitive-in-config", fieldPath,
				"field %q is marked sensitive in the schema; write it via PUT /api/secrets, not PUT /api/configs",
				displayConfigPath(fieldPath)),
		}
	}

	switch f.Type {
	case FieldTypeString, FieldTypeDuration:
		if _, ok := value.(string); ok {
			return nil
		}
		return []diag.Diagnostic{invalidConfigValue(fieldPath, "string", value)}
	case FieldTypeInt:
		if isIntegralNumber(value) {
			return nil
		}
		return []diag.Diagnostic{invalidConfigValue(fieldPath, "int", value)}
	case FieldTypeFloat:
		if isNumber(value) {
			return nil
		}
		return []diag.Diagnostic{invalidConfigValue(fieldPath, "float", value)}
	case FieldTypeBool:
		if _, ok := value.(bool); ok {
			return nil
		}
		return []diag.Diagnostic{invalidConfigValue(fieldPath, "bool", value)}
	case FieldTypeObject:
		obj, ok := value.(map[string]any)
		if !ok {
			return []diag.Diagnostic{invalidConfigValue(fieldPath, "object", value)}
		}
		if len(f.Fields) == 0 {
			return nil
		}
		return validateObjectValues(f.Fields, obj, fieldPath)
	case FieldTypeArray:
		items, ok := value.([]any)
		if !ok {
			return []diag.Diagnostic{invalidConfigValue(fieldPath, "array", value)}
		}
		if f.Items == nil {
			return nil
		}
		var diags []diag.Diagnostic
		for i, item := range items {
			diags = append(diags, validateConfigValue(*f.Items, item, fmt.Sprintf("%s[%d]", fieldPath, i))...)
		}
		return diags
	case FieldTypeMap:
		obj, ok := value.(map[string]any)
		if !ok {
			return []diag.Diagnostic{invalidConfigValue(fieldPath, "map", value)}
		}
		var diags []diag.Diagnostic
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			keyPath := fieldPath + "." + key
			if !mapKeyMatches(f.Keys, key) {
				diags = append(diags, diag.Errorf("invalid-map-key", keyPath,
					"map key %q does not match declared key type %q", key, f.Keys))
				continue
			}
			if f.Values != nil {
				diags = append(diags, validateConfigValue(*f.Values, obj[key], keyPath)...)
			}
		}
		return diags
	default:
		return []diag.Diagnostic{
			diag.Errorf("invalid-type", fieldPath,
				"unknown field type %q; valid types are: %s", f.Type, strings.Join(FieldTypeValues(), ", ")),
		}
	}
}

func invalidConfigValue(fieldPath, want string, value any) diag.Diagnostic {
	return diag.Errorf("invalid-config-value", fieldPath,
		"%s must be %s, got %s", displayConfigPath(fieldPath), want, valueKind(value))
}

func displayConfigPath(fieldPath string) string {
	return strings.TrimPrefix(fieldPath, "values.")
}

func valueKind(value any) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprintf("%T", value)
}

func isIntegralNumber(value any) bool {
	switch v := value.(type) {
	case int, int8, int16, int32, int64:
		return true
	case uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		f := float64(v)
		return !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f
	case float64:
		return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Trunc(v) == v
	case json.Number:
		if _, err := v.Int64(); err == nil {
			return true
		}
		f, err := v.Float64()
		return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f
	default:
		return false
	}
}

func isNumber(value any) bool {
	switch v := value.(type) {
	case int, int8, int16, int32, int64:
		return true
	case uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		f := float64(v)
		return !math.IsNaN(f) && !math.IsInf(f, 0)
	case float64:
		return !math.IsNaN(v) && !math.IsInf(v, 0)
	case json.Number:
		_, err := v.Float64()
		return err == nil
	default:
		return false
	}
}

func mapKeyMatches(keyType, key string) bool {
	switch keyType {
	case "", FieldTypeString:
		return true
	case FieldTypeInt:
		_, err := strconv.ParseInt(key, 10, 64)
		return err == nil
	case FieldTypeBool:
		return key == "true" || key == "false"
	default:
		return false
	}
}

func isArrayIndex(part string) bool {
	if part == "" {
		return false
	}
	for _, r := range part {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func blockHasSensitive(b Block) bool {
	for _, f := range b.Fields {
		if fieldContainsSensitive(f) {
			return true
		}
	}
	return false
}

func fieldContainsSensitive(f FieldSchema) bool {
	if f.Sensitive {
		return true
	}
	switch f.Type {
	case FieldTypeObject:
		for _, nested := range f.Fields {
			if fieldContainsSensitive(nested) {
				return true
			}
		}
	case FieldTypeArray:
		return f.Items != nil && fieldContainsSensitive(*f.Items)
	case FieldTypeMap:
		return f.Values != nil && fieldContainsSensitive(*f.Values)
	}
	return false
}
