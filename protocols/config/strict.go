package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ParseSchemaManifest decodes JSON data into a SchemaManifest using strict mode.
// Unknown fields are rejected and structured diagnostics are returned.
func ParseSchemaManifest(data []byte) (*SchemaManifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m SchemaManifest
	if err := dec.Decode(&m); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse config schema manifest: %v", err),
		}
	}
	return &m, nil
}

// ValidateSchemaManifest checks structural invariants on a parsed manifest.
func ValidateSchemaManifest(m *SchemaManifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-manifest", "", "config schema manifest is nil"),
		}
	}

	var diags []diag.Diagnostic

	diags = append(diags, ValidateAppName(m.AppName, "appName")...)

	if len(m.Configs) == 0 {
		diags = append(diags, diag.Errorf("required-field", "configs", "at least one config block is required"))
	}

	if m.SchemaHash != "" && !strings.HasPrefix(m.SchemaHash, HashPrefix) {
		diags = append(diags, diag.Errorf("invalid-hash", "schemaHash",
			"schemaHash must use prefix %q, got %q", HashPrefix, m.SchemaHash))
	}

	// Validate each config block in sorted order for deterministic diagnostics.
	paths := make([]string, 0, len(m.Configs))
	pathIndex := make(map[string]int)
	for i, c := range m.Configs {
		paths = append(paths, c.Path)
		pathIndex[c.Path] = i
	}
	sort.Strings(paths)

	seen := make(map[string]bool)
	for _, path := range paths {
		idx := pathIndex[path]
		block := m.Configs[idx]
		blockField := fmt.Sprintf("configs[%d]", idx)

		diags = append(diags, ValidateName(block.Path, blockField+".path")...)

		if seen[block.Path] {
			diags = append(diags, diag.Errorf("duplicate-path", blockField+".path",
				"duplicate config block path: %q", block.Path))
		}
		seen[block.Path] = true

		diags = append(diags, validateFields(block.Fields, blockField)...)
	}

	return diags
}

func validateFields(fields []FieldSchema, parentField string) []diag.Diagnostic {
	var diags []diag.Diagnostic

	names := make([]string, 0, len(fields))
	nameIndex := make(map[string]int)
	for i, f := range fields {
		names = append(names, f.Name)
		nameIndex[f.Name] = i
	}
	sort.Strings(names)

	seen := make(map[string]bool)
	for _, name := range names {
		idx := nameIndex[name]
		f := fields[idx]
		fieldPath := fmt.Sprintf("%s.fields[%d]", parentField, idx)

		if f.Name == "" {
			diags = append(diags, diag.Errorf("required-field", fieldPath+".name", "field name is required"))
		}

		if seen[f.Name] {
			diags = append(diags, diag.Errorf("duplicate-field", fieldPath+".name",
				"duplicate field name: %q", f.Name))
		}
		seen[f.Name] = true

		if f.Type == "" {
			diags = append(diags, diag.Errorf("required-field", fieldPath+".type", "field type is required"))
		} else if !ValidFieldTypes[f.Type] {
			diags = append(diags, diag.Errorf("invalid-type", fieldPath+".type",
				"unknown field type %q; valid types are: %s", f.Type, strings.Join(FieldTypeValues(), ", ")))
		}

		diags = append(diags, validateCompositeSlots(f, fieldPath)...)
	}

	return diags
}

// validateCompositeSlots checks that the Fields/Items/Keys/Values slots are
// populated only for the matching Type and recurses into nested shapes.
func validateCompositeSlots(f FieldSchema, fieldPath string) []diag.Diagnostic {
	var diags []diag.Diagnostic

	// Slots that don't match the declared Type must be empty so the canonical
	// JSON form stays minimal and the hash stays deterministic across
	// extractors.
	if f.Type != FieldTypeObject && len(f.Fields) > 0 {
		diags = append(diags, diag.Errorf("misplaced-slot", fieldPath+".fields",
			"fields is only valid when type is %q", FieldTypeObject))
	}
	if f.Type != FieldTypeArray && f.Items != nil {
		diags = append(diags, diag.Errorf("misplaced-slot", fieldPath+".items",
			"items is only valid when type is %q", FieldTypeArray))
	}
	if f.Type != FieldTypeMap && (f.Keys != "" || f.Values != nil) {
		diags = append(diags, diag.Errorf("misplaced-slot", fieldPath+".keys",
			"keys/values are only valid when type is %q", FieldTypeMap))
	}

	switch f.Type {
	case FieldTypeObject:
		if len(f.Fields) > 0 {
			diags = append(diags, validateFields(f.Fields, fieldPath)...)
		}
	case FieldTypeArray:
		if f.Items == nil {
			diags = append(diags, diag.Errorf("required-field", fieldPath+".items",
				"items is required when type is %q", FieldTypeArray))
		} else {
			diags = append(diags, validateCompositeChild(*f.Items, fieldPath+".items")...)
		}
	case FieldTypeMap:
		if f.Keys == "" {
			diags = append(diags, diag.Errorf("required-field", fieldPath+".keys",
				"keys is required when type is %q", FieldTypeMap))
		} else if !ValidMapKeyTypes[f.Keys] {
			diags = append(diags, diag.Errorf("invalid-map-key", fieldPath+".keys",
				"map key type %q is not a primitive; valid map key types are: %s", f.Keys, strings.Join(MapKeyTypeValues(), ", ")))
		}
		if f.Values == nil {
			diags = append(diags, diag.Errorf("required-field", fieldPath+".values",
				"values is required when type is %q", FieldTypeMap))
		} else {
			diags = append(diags, validateCompositeChild(*f.Values, fieldPath+".values")...)
		}
	}

	return diags
}

// validateCompositeChild validates an unnamed child schema (array item or
// map value). The child's Name is not used in the manifest; only Type and
// nested composite slots are meaningful.
func validateCompositeChild(child FieldSchema, fieldPath string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if child.Type == "" {
		diags = append(diags, diag.Errorf("required-field", fieldPath+".type", "field type is required"))
	} else if !ValidFieldTypes[child.Type] {
		diags = append(diags, diag.Errorf("invalid-type", fieldPath+".type",
			"unknown field type %q; valid types are: %s", child.Type, strings.Join(FieldTypeValues(), ", ")))
	}
	diags = append(diags, validateCompositeSlots(child, fieldPath)...)
	return diags
}

// NormalizeSchemaManifest applies canonical defaults to a parsed manifest.
// It modifies the manifest in place. Only top-level slices are normalized to
// empty: nested Fields/Items/Values stay nil when absent to preserve the
// minimal canonical JSON form used for hashing.
func NormalizeSchemaManifest(m *SchemaManifest) {
	if m == nil {
		return
	}
	if m.Configs == nil {
		m.Configs = []Block{}
	}
	for i := range m.Configs {
		if m.Configs[i].Fields == nil {
			m.Configs[i].Fields = []FieldSchema{}
		}
	}
}

// ParseAndValidateSchemaManifest combines strict parsing, validation, and normalization.
func ParseAndValidateSchemaManifest(data []byte) (*SchemaManifest, []diag.Diagnostic) {
	m, diags := ParseSchemaManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	vDiags := ValidateSchemaManifest(m)
	diags = append(diags, vDiags...)

	NormalizeSchemaManifest(m)

	return m, diags
}

// ParseRegisteredSchemaManifest decodes JSON data into a RegisteredSchemaManifest
// using strict mode. This accepts server-owned metadata such as registeredAt.
func ParseRegisteredSchemaManifest(data []byte) (*RegisteredSchemaManifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m RegisteredSchemaManifest
	if err := dec.Decode(&m); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse registered config schema manifest: %v", err),
		}
	}
	return &m, nil
}

// ValidateRegisteredSchemaManifest checks structural invariants on a parsed
// registered schema manifest.
func ValidateRegisteredSchemaManifest(m *RegisteredSchemaManifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-manifest", "", "registered config schema manifest is nil"),
		}
	}

	return ValidateSchemaManifest(&m.SchemaManifest)
}

// ParseAndValidateRegisteredSchemaManifest combines strict parsing,
// validation, and normalization for server-returned schema manifests.
func ParseAndValidateRegisteredSchemaManifest(data []byte) (*RegisteredSchemaManifest, []diag.Diagnostic) {
	m, diags := ParseRegisteredSchemaManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}

	diags = append(diags, ValidateRegisteredSchemaManifest(m)...)
	NormalizeSchemaManifest(&m.SchemaManifest)

	return m, diags
}

// ParseConfigEntry decodes JSON data into a ConfigEntry using strict mode.
func ParseConfigEntry(data []byte) (*ConfigEntry, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var e ConfigEntry
	if err := dec.Decode(&e); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse config entry: %v", err),
		}
	}
	return &e, nil
}

// ParseAndValidateConfigEntry combines strict parsing and validation.
func ParseAndValidateConfigEntry(data []byte) (*ConfigEntry, []diag.Diagnostic) {
	e, diags := ParseConfigEntry(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	diags = append(diags, ValidateConfigEntry(e)...)
	return e, diags
}

// ParseSecretEntry decodes JSON data into a SecretEntry using strict mode.
func ParseSecretEntry(data []byte) (*SecretEntry, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var e SecretEntry
	if err := dec.Decode(&e); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse secret entry: %v", err),
		}
	}
	return &e, nil
}

// ParseAndValidateSecretEntry combines strict parsing and validation.
func ParseAndValidateSecretEntry(data []byte) (*SecretEntry, []diag.Diagnostic) {
	e, diags := ParseSecretEntry(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	diags = append(diags, ValidateSecretEntry(e)...)
	return e, diags
}

// ParseDeleteRequest decodes JSON data into a DeleteRequest using strict mode.
func ParseDeleteRequest(data []byte) (*DeleteRequest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var r DeleteRequest
	if err := dec.Decode(&r); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse delete request: %v", err),
		}
	}
	return &r, nil
}

// ParseAndValidateDeleteRequest combines strict parsing and validation.
func ParseAndValidateDeleteRequest(data []byte) (*DeleteRequest, []diag.Diagnostic) {
	r, diags := ParseDeleteRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	diags = append(diags, ValidateDeleteRequest(r)...)
	return r, diags
}

// ParseResolveSecretsRequest decodes JSON data into a ResolveSecretsRequest using strict mode.
func ParseResolveSecretsRequest(data []byte) (*ResolveSecretsRequest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var r ResolveSecretsRequest
	if err := dec.Decode(&r); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse resolve secrets request: %v", err),
		}
	}
	return &r, nil
}

// ParseAndValidateResolveSecretsRequest combines strict parsing and validation.
func ParseAndValidateResolveSecretsRequest(data []byte) (*ResolveSecretsRequest, []diag.Diagnostic) {
	r, diags := ParseResolveSecretsRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	diags = append(diags, ValidateResolveSecretsRequest(r)...)
	return r, diags
}

// ValidateResolveRequest checks a resolve request for required fields and naming constraints.
func ValidateResolveRequest(r *ResolveRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{
			diag.Errorf("nil-request", "", "resolve request is nil"),
		}
	}
	diags := make([]diag.Diagnostic, 0, 3)
	diags = append(diags, ValidateAppName(r.AppName, "appName")...)
	diags = append(diags, ValidateName(r.Environment, "environment")...)
	diags = append(diags, ValidateOptionalName(r.Version, "version")...)
	return diags
}
