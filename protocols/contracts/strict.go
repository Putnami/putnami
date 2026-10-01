package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"go.putnami.dev/protocol/config"
	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict parsing and validation. Tooling and runtime
// compliance suites key off these — keep them in sync with ValidErrorCodes
// below.
const (
	ErrorCodeParseError             = "contracts.parse_error"
	ErrorCodeUnknownField           = "contracts.unknown_field"
	ErrorCodeInvalidProtocolVersion = "contracts.invalid_protocol_version"
	ErrorCodeMissingName            = "contracts.missing_name"
	ErrorCodeInvalidName            = "contracts.invalid_name"
	ErrorCodeDuplicateNode          = "contracts.duplicate_node"
	ErrorCodeDuplicateVariant       = "contracts.duplicate_variant"
	ErrorCodeMissingEnumValues      = "contracts.missing_enum_values"
	ErrorCodeEnumValueConflict      = "contracts.enum_value_conflict"
	ErrorCodeInvalidDiscriminator   = "contracts.invalid_discriminator"
	ErrorCodeMissingVariants        = "contracts.missing_variants"
	ErrorCodeInvalidVariant         = "contracts.invalid_variant"
	ErrorCodeUnknownReference       = "contracts.unknown_reference"
	ErrorCodeInvalidFieldType       = "contracts.invalid_field_type"
	ErrorCodeInvalidDefault         = "contracts.invalid_default"
)

// ValidErrorCodes enumerates the canonical contracts-protocol error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:             true,
	ErrorCodeUnknownField:           true,
	ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeMissingName:            true,
	ErrorCodeInvalidName:            true,
	ErrorCodeDuplicateNode:          true,
	ErrorCodeDuplicateVariant:       true,
	ErrorCodeMissingEnumValues:      true,
	ErrorCodeEnumValueConflict:      true,
	ErrorCodeInvalidDiscriminator:   true,
	ErrorCodeMissingVariants:        true,
	ErrorCodeInvalidVariant:         true,
	ErrorCodeUnknownReference:       true,
	ErrorCodeInvalidFieldType:       true,
	ErrorCodeInvalidDefault:         true,
}

// fieldPrimitives is the subset of the config field-type vocabulary a struct or
// union-variant field may use directly; any other type token must reference a
// declared enum, union, or struct. Composite kinds (object/array/map) are
// expressed through named references and the Repeated flag, so they are not
// accepted as bare field types.
var fieldPrimitives = map[string]bool{
	config.FieldTypeString:   true,
	config.FieldTypeInt:      true,
	config.FieldTypeFloat:    true,
	config.FieldTypeBool:     true,
	config.FieldTypeDuration: true,
}

// ParseManifest decodes a contract IR from JSON in strict mode (unknown fields
// rejected). It returns a non-nil manifest only when parsing produced no
// errors.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		code := ErrorCodeParseError
		field := ""
		msg := err.Error()
		if strings.HasPrefix(msg, "json: unknown field ") {
			code = ErrorCodeUnknownField
			field = strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
		}
		return nil, []diag.Diagnostic{diag.Errorf(code, field, "%s", msg)}
	}
	return &m, nil
}

// ValidateManifest checks structural and semantic invariants on a parsed
// manifest. It returns one diagnostic per finding in a stable, source-order
// sequence: per-node structural checks first, then deterministic duplicate and
// reference findings.
func ValidateManifest(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}

	var diags []diag.Diagnostic
	diags = append(diags, validateProtocolVersion(m.ProtocolVersion)...)

	if strings.TrimSpace(m.Name) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingName, "name",
			"manifest is missing the contract name"))
	}

	// Structural pass, in source order.
	for i, e := range m.Enums {
		field := fmt.Sprintf("enums[%d]", i)
		diags = append(diags, validateName(field+".name", e.Name)...)
		diags = append(diags, validateEnumValues(field, e.Values)...)
	}
	for i, u := range m.Unions {
		diags = append(diags, validateUnion(fmt.Sprintf("unions[%d]", i), u)...)
	}
	for i, s := range m.Structs {
		field := fmt.Sprintf("structs[%d]", i)
		diags = append(diags, validateName(field+".name", s.Name)...)
		diags = append(diags, validateFieldNames(field, s.Fields)...)
	}
	for i, c := range m.ConfigFields {
		diags = append(diags, validateConfigField(fmt.Sprintf("configFields[%d]", i), c)...)
	}
	for i, s := range m.Scopes {
		diags = append(diags, validateName(fmt.Sprintf("scopes[%d].name", i), s.Name)...)
	}
	for i, c := range m.Capabilities {
		diags = append(diags, validateName(fmt.Sprintf("capabilities[%d].name", i), c.Name)...)
	}
	for i, g := range m.Grants {
		diags = append(diags, validateName(fmt.Sprintf("grants[%d].name", i), g.Name)...)
	}
	for i, c := range m.Claims {
		diags = append(diags, validateClaim(fmt.Sprintf("claims[%d]", i), c)...)
	}
	for i, p := range m.PrincipalKinds {
		diags = append(diags, validateName(fmt.Sprintf("principalKinds[%d].name", i), p.Name)...)
	}

	// Semantic pass: duplicates then cross-references. Running it after the
	// source-order structural pass keeps malformed entries surfaced first,
	// followed by deterministic collisions and unresolved references.
	diags = append(diags, validateTypeNamespace(m)...)
	diags = append(diags, findDuplicateNames("configFields", m.ConfigFields, func(c ConfigField) string { return c.Name })...)
	diags = append(diags, findDuplicateNames("scopes", m.Scopes, func(s Scope) string { return s.Name })...)
	diags = append(diags, findDuplicateNames("capabilities", m.Capabilities, func(c Capability) string { return c.Name })...)
	diags = append(diags, findDuplicateNames("grants", m.Grants, func(g Grant) string { return g.Name })...)
	diags = append(diags, findDuplicateNames("claims", m.Claims, func(c Claim) string { return c.Name })...)
	diags = append(diags, findDuplicateNames("principalKinds", m.PrincipalKinds, func(p PrincipalKind) string { return p.Name })...)
	diags = append(diags, validateReferences(m)...)
	return diags
}

// validateEnumValues checks that an enum declares a non-empty, internally
// unique set of members: each member carries a name and a wire value, and no
// two members share either.
func validateEnumValues(field string, values []EnumValue) []diag.Diagnostic {
	if len(values) == 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingEnumValues, field+".values",
			"enum must declare at least one value; add a value or remove the enum")}
	}
	var diags []diag.Diagnostic
	names := make(map[string]bool, len(values))
	wire := make(map[string]bool, len(values))
	for i, v := range values {
		vf := fmt.Sprintf("%s.values[%d]", field, i)
		diags = append(diags, validateName(vf+".name", v.Name)...)
		if strings.TrimSpace(v.Value) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidName, vf+".value",
				"enum value is required"))
		}
		if strings.TrimSpace(v.Name) != "" {
			if names[v.Name] {
				diags = append(diags, diag.Errorf(ErrorCodeEnumValueConflict, vf+".name",
					"enum value name %q is declared more than once; give each member a unique name", v.Name))
			}
			names[v.Name] = true
		}
		if strings.TrimSpace(v.Value) != "" {
			if wire[v.Value] {
				diags = append(diags, diag.Errorf(ErrorCodeEnumValueConflict, vf+".value",
					"enum wire value %q is declared more than once; give each member a unique value", v.Value))
			}
			wire[v.Value] = true
		}
	}
	return diags
}

// validateUnion checks a union's discriminator, that it declares at least one
// variant, and that every variant carries a unique tag and sets exactly one of
// struct or fields (the mutually exclusive shape). Variant reference resolution
// runs in the semantic pass.
func validateUnion(field string, u Union) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, validateName(field+".name", u.Name)...)
	if strings.TrimSpace(u.Discriminator) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDiscriminator, field+".discriminator",
			"union %q must declare a non-empty discriminator; name the wire field that selects the variant", u.Name))
	}
	if len(u.Variants) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeMissingVariants, field+".variants",
			"union %q must declare at least one variant; add a variant or remove the union", u.Name))
	}
	tags := make(map[string]bool, len(u.Variants))
	for i, v := range u.Variants {
		vf := fmt.Sprintf("%s.variants[%d]", field, i)
		if strings.TrimSpace(v.Tag) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidVariant, vf+".tag",
				"union variant must declare a non-empty tag; the tag is the discriminator value that selects it"))
		} else {
			if tags[v.Tag] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicateVariant, vf+".tag",
					"union %q declares variant tag %q more than once; give each variant a unique tag", u.Name, v.Tag))
			}
			tags[v.Tag] = true
		}
		hasStruct := strings.TrimSpace(v.Struct) != ""
		hasFields := len(v.Fields) > 0
		if hasStruct == hasFields {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidVariant, vf,
				"union variant %q must set exactly one of \"struct\" or \"fields\"; they are mutually exclusive", v.Tag))
		}
		diags = append(diags, validateFieldNames(vf, v.Fields)...)
	}
	return diags
}

// validateFieldNames checks every field in a struct or inline union variant
// declares a non-empty name and that no two fields in the same shape share a
// name — a duplicate would emit two identically-keyed fields in the generated
// Go/TS types and a duplicate JSON key.
func validateFieldNames(field string, fields []Field) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := make(map[string]bool, len(fields))
	for j, f := range fields {
		fn := fmt.Sprintf("%s.fields[%d].name", field, j)
		diags = append(diags, validateName(fn, f.Name)...)
		if strings.TrimSpace(f.Name) == "" {
			continue
		}
		if seen[f.Name] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateNode, fn,
				"field %q is declared more than once; remove the duplicate or give each field a unique name", f.Name))
			continue
		}
		seen[f.Name] = true
	}
	return diags
}

// validateConfigField checks a config field's name, that its type is in the
// shared config vocabulary, and that any typed default agrees with the type.
func validateConfigField(field string, c ConfigField) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, validateName(field+".name", c.Name)...)
	if !config.ValidFieldTypes[c.Type] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidFieldType, field+".type",
			"config field type %q is not in the config vocabulary: %s", c.Type, fieldTypeList()))
		return diags
	}
	diags = append(diags, validateDefault(field, c.Type, c.Default)...)
	return diags
}

// validateClaim checks a claim's name and that its type is in the shared config
// vocabulary.
func validateClaim(field string, c Claim) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, validateName(field+".name", c.Name)...)
	if !config.ValidFieldTypes[c.Type] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidFieldType, field+".type",
			"claim type %q is not in the config vocabulary: %s", c.Type, fieldTypeList()))
	}
	return diags
}

// validateDefault checks that a config field's typed default has a JSON kind
// consistent with its declared type. Numeric JSON values decode to float64 but
// hand-built manifests may carry native Go integers, so both are accepted.
func validateDefault(field, typ string, def any) []diag.Diagnostic {
	if def == nil {
		return nil
	}
	if defaultMatchesType(typ, def) {
		return nil
	}
	return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDefault, field+".default",
		"default for a %q field must be a %s value; fix the default or the field type", typ, jsonKindFor(typ))}
}

func defaultMatchesType(typ string, def any) bool {
	switch typ {
	case config.FieldTypeString, config.FieldTypeDuration:
		_, ok := def.(string)
		return ok
	case config.FieldTypeInt:
		// An int default must be a whole number: a JSON 7 decodes to
		// float64(7) and is accepted, but a fractional 1.5 is not. This mirrors
		// the config protocol's isIntegralNumber check.
		return isIntegralNumber(def)
	case config.FieldTypeFloat:
		return isJSONNumber(def)
	case config.FieldTypeBool:
		_, ok := def.(bool)
		return ok
	case config.FieldTypeObject, config.FieldTypeMap:
		_, ok := def.(map[string]any)
		return ok
	case config.FieldTypeArray:
		_, ok := def.([]any)
		return ok
	default:
		// Unknown types are reported separately as invalid_field_type; do not
		// double-report here.
		return true
	}
}

func isJSONNumber(def any) bool {
	switch def.(type) {
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return true
	default:
		return false
	}
}

// isIntegralNumber reports whether def is a JSON number with no fractional
// part, so it is a valid default for an "int" field. A JSON default like 7
// decodes to float64(7) and is accepted; 1.5 is rejected.
func isIntegralNumber(def any) bool {
	switch v := def.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		f := float64(v)
		return !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f
	case float64:
		return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Trunc(v) == v
	case json.Number:
		_, err := v.Int64()
		return err == nil
	default:
		return false
	}
}

func jsonKindFor(typ string) string {
	switch typ {
	case config.FieldTypeString, config.FieldTypeDuration:
		return "string"
	case config.FieldTypeInt, config.FieldTypeFloat:
		return "number"
	case config.FieldTypeBool:
		return "boolean"
	case config.FieldTypeObject, config.FieldTypeMap:
		return "object"
	case config.FieldTypeArray:
		return "array"
	default:
		return "compatible"
	}
}

// validateTypeNamespace rejects a type name declared more than once across the
// shared enum/union/struct namespace. Field type references resolve against
// this single namespace, so a collision would make a reference ambiguous.
func validateTypeNamespace(m *Manifest) []diag.Diagnostic {
	seen := make(map[string]bool, len(m.Enums)+len(m.Unions)+len(m.Structs))
	var diags []diag.Diagnostic
	check := func(field string, i int, name string) {
		if strings.TrimSpace(name) == "" {
			return
		}
		if seen[name] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateNode, fmt.Sprintf("%s[%d]", field, i),
				"type name %q is declared more than once across enums, unions, and structs; type names must be unique", name))
			return
		}
		seen[name] = true
	}
	for i, e := range m.Enums {
		check("enums", i, e.Name)
	}
	for i, u := range m.Unions {
		check("unions", i, u.Name)
	}
	for i, s := range m.Structs {
		check("structs", i, s.Name)
	}
	return diags
}

// validateReferences resolves every cross-reference against the declared nodes:
// struct and inline-variant field types against the type namespace, union
// variant struct refs against declared structs, capability scopes against
// declared scopes, and grant capabilities against declared capabilities.
func validateReferences(m *Manifest) []diag.Diagnostic {
	typeNames := declaredTypeNames(m)
	structNames := nameSet(m.Structs, func(s Struct) string { return s.Name })
	scopeNames := nameSet(m.Scopes, func(s Scope) string { return s.Name })
	capabilityNames := nameSet(m.Capabilities, func(c Capability) string { return c.Name })

	var diags []diag.Diagnostic
	for i, s := range m.Structs {
		for j, f := range s.Fields {
			diags = append(diags, validateFieldTypeRef(fmt.Sprintf("structs[%d].fields[%d]", i, j), f, typeNames)...)
		}
	}
	for i, u := range m.Unions {
		for j, v := range u.Variants {
			vf := fmt.Sprintf("unions[%d].variants[%d]", i, j)
			if strings.TrimSpace(v.Struct) != "" && !structNames[v.Struct] {
				diags = append(diags, diag.Errorf(ErrorCodeUnknownReference, vf+".struct",
					"union variant references struct %q, which is not declared; declare the struct or fix the reference", v.Struct))
			}
			for k, f := range v.Fields {
				diags = append(diags, validateFieldTypeRef(fmt.Sprintf("%s.fields[%d]", vf, k), f, typeNames)...)
			}
		}
	}
	for i, c := range m.Capabilities {
		for j, sc := range c.Scopes {
			if !scopeNames[sc] {
				diags = append(diags, diag.Errorf(ErrorCodeUnknownReference, fmt.Sprintf("capabilities[%d].scopes[%d]", i, j),
					"capability %q references scope %q, which is not declared; declare the scope or fix the reference", c.Name, sc))
			}
		}
	}
	for i, g := range m.Grants {
		if strings.TrimSpace(g.Capability) != "" && !capabilityNames[g.Capability] {
			diags = append(diags, diag.Errorf(ErrorCodeUnknownReference, fmt.Sprintf("grants[%d].capability", i),
				"grant %q references capability %q, which is not declared; declare the capability or fix the reference", g.Name, g.Capability))
		}
	}
	return diags
}

// validateFieldTypeRef checks that a field's type is a primitive or resolves to
// a declared enum, union, or struct.
func validateFieldTypeRef(field string, f Field, typeNames map[string]bool) []diag.Diagnostic {
	if fieldPrimitives[f.Type] || typeNames[f.Type] {
		return nil
	}
	if strings.TrimSpace(f.Type) == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownReference, field+".type",
			"field %q must declare a type; use a primitive (string, int, float, bool, duration) or a declared enum/union/struct", f.Name)}
	}
	return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownReference, field+".type",
		"field %q type %q is not a primitive (string, int, float, bool, duration) or a declared enum/union/struct", f.Name, f.Type)}
}

// findDuplicateNames rejects a name declared more than once within a single
// node collection.
func findDuplicateNames[T any](field string, items []T, name func(T) string) []diag.Diagnostic {
	seen := make(map[string]bool, len(items))
	var diags []diag.Diagnostic
	for i, item := range items {
		n := name(item)
		if strings.TrimSpace(n) == "" {
			continue
		}
		if seen[n] {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateNode, fmt.Sprintf("%s[%d]", field, i),
				"%q is declared more than once; remove the duplicate or give each entry a unique name", n))
			continue
		}
		seen[n] = true
	}
	return diags
}

func declaredTypeNames(m *Manifest) map[string]bool {
	out := make(map[string]bool, len(m.Enums)+len(m.Unions)+len(m.Structs))
	for _, e := range m.Enums {
		if strings.TrimSpace(e.Name) != "" {
			out[e.Name] = true
		}
	}
	for _, u := range m.Unions {
		if strings.TrimSpace(u.Name) != "" {
			out[u.Name] = true
		}
	}
	for _, s := range m.Structs {
		if strings.TrimSpace(s.Name) != "" {
			out[s.Name] = true
		}
	}
	return out
}

func nameSet[T any](items []T, name func(T) string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		if n := name(it); strings.TrimSpace(n) != "" {
			out[n] = true
		}
	}
	return out
}

func fieldTypeList() string {
	return strings.Join(config.FieldTypeValues(), ", ")
}

// validateProtocolVersion checks that v is the supported protocol version.
func validateProtocolVersion(v int) []diag.Diagnostic {
	if v != ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported by this parser (want %d)", v, ProtocolVersion)}
	}
	return nil
}

// validateName checks that a required identifier field is non-empty. Node names
// range from type names to scope and claim keys, so no single character pattern
// applies — the check is presence only.
func validateName(field, name string) []diag.Diagnostic {
	if strings.TrimSpace(name) == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, field, "name is required")}
	}
	return nil
}

// ParseAndValidateManifest is a convenience that runs strict parsing followed
// by structural and semantic validation.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return m, append(diags, ValidateManifest(m)...)
}
