package contracts

import (
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// findCode returns true if diags contains a diagnostic with the given code.
func findCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestParseManifest_Valid(t *testing.T) {
	data := []byte(`{"protocolVersion": 1, "name": "c"}`)
	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil || m.Name != "c" {
		t.Fatalf("manifest = %v, want name c", m)
	}
}

func TestParseManifest_UnknownField(t *testing.T) {
	data := []byte(`{"protocolVersion": 1, "name": "c", "bogus": true}`)
	m, diags := ParseManifest(data)
	if m != nil {
		t.Error("manifest should be nil on parse error")
	}
	if !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestParseManifest_UnknownNestedField(t *testing.T) {
	// Strict parsing must reject unknown fields inside nested nodes too.
	data := []byte(`{"protocolVersion": 1, "name": "c", "enums": [{"name": "E", "values": [{"name": "A", "value": "a", "extra": 1}]}]}`)
	if _, diags := ParseManifest(data); !findCode(diags, ErrorCodeUnknownField) {
		t.Errorf("want %s for nested unknown field, got %v", ErrorCodeUnknownField, diags)
	}
}

func TestParseManifest_InvalidJSON(t *testing.T) {
	_, diags := ParseManifest([]byte("{not json"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
	if diags[0].Code != ErrorCodeParseError {
		t.Errorf("Code = %q, want %s", diags[0].Code, ErrorCodeParseError)
	}
}

func TestValidateManifest_InvalidProtocolVersion(t *testing.T) {
	m := &Manifest{ProtocolVersion: 99, Name: "c"}
	if !findCode(ValidateManifest(m), ErrorCodeInvalidProtocolVersion) {
		t.Errorf("want %s", ErrorCodeInvalidProtocolVersion)
	}
}

func TestValidateManifest_MissingName(t *testing.T) {
	m := &Manifest{ProtocolVersion: 1}
	if !findCode(ValidateManifest(m), ErrorCodeMissingName) {
		t.Errorf("want %s", ErrorCodeMissingName)
	}
}

func TestValidateManifest_Sample(t *testing.T) {
	if diags := ValidateManifest(sampleManifest()); diag.HasErrors(diags) {
		t.Fatalf("representative sample produced errors: %v", diags)
	}
}

func TestValidateManifest_EnumMissingValues(t *testing.T) {
	m := &Manifest{ProtocolVersion: 1, Name: "c", Enums: []Enum{{Name: "E"}}}
	if !findCode(ValidateManifest(m), ErrorCodeMissingEnumValues) {
		t.Errorf("want %s", ErrorCodeMissingEnumValues)
	}
}

func TestValidateManifest_EnumValueConflict(t *testing.T) {
	dupName := &Manifest{ProtocolVersion: 1, Name: "c", Enums: []Enum{{
		Name:   "E",
		Values: []EnumValue{{Name: "A", Value: "a"}, {Name: "A", Value: "b"}},
	}}}
	if !findCode(ValidateManifest(dupName), ErrorCodeEnumValueConflict) {
		t.Errorf("want %s for duplicate value name", ErrorCodeEnumValueConflict)
	}
	dupWire := &Manifest{ProtocolVersion: 1, Name: "c", Enums: []Enum{{
		Name:   "E",
		Values: []EnumValue{{Name: "A", Value: "x"}, {Name: "B", Value: "x"}},
	}}}
	if !findCode(ValidateManifest(dupWire), ErrorCodeEnumValueConflict) {
		t.Errorf("want %s for duplicate wire value", ErrorCodeEnumValueConflict)
	}
}

func TestValidateManifest_InvalidDiscriminator(t *testing.T) {
	m := &Manifest{ProtocolVersion: 1, Name: "c", Unions: []Union{{
		Name:     "U",
		Variants: []UnionVariant{{Tag: "a", Fields: []Field{{Name: "x", Type: "string"}}}},
	}}}
	if !findCode(ValidateManifest(m), ErrorCodeInvalidDiscriminator) {
		t.Errorf("want %s for empty discriminator", ErrorCodeInvalidDiscriminator)
	}
}

func TestValidateManifest_MissingVariants(t *testing.T) {
	m := &Manifest{ProtocolVersion: 1, Name: "c", Unions: []Union{{Name: "U", Discriminator: "kind"}}}
	if !findCode(ValidateManifest(m), ErrorCodeMissingVariants) {
		t.Errorf("want %s", ErrorCodeMissingVariants)
	}
}

func TestValidateManifest_DuplicateVariant(t *testing.T) {
	m := &Manifest{ProtocolVersion: 1, Name: "c", Structs: []Struct{{Name: "S", Fields: []Field{{Name: "x", Type: "string"}}}},
		Unions: []Union{{
			Name:          "U",
			Discriminator: "kind",
			Variants: []UnionVariant{
				{Tag: "a", Struct: "S"},
				{Tag: "a", Struct: "S"},
			},
		}}}
	if !findCode(ValidateManifest(m), ErrorCodeDuplicateVariant) {
		t.Errorf("want %s for duplicate tag", ErrorCodeDuplicateVariant)
	}
}

func TestValidateManifest_VariantMutualExclusivity(t *testing.T) {
	// A variant that sets both struct and fields is invalid.
	both := &Manifest{ProtocolVersion: 1, Name: "c", Structs: []Struct{{Name: "S", Fields: []Field{{Name: "x", Type: "string"}}}},
		Unions: []Union{{
			Name: "U", Discriminator: "kind",
			Variants: []UnionVariant{{Tag: "a", Struct: "S", Fields: []Field{{Name: "y", Type: "string"}}}},
		}}}
	if !findCode(ValidateManifest(both), ErrorCodeInvalidVariant) {
		t.Errorf("want %s when a variant sets both struct and fields", ErrorCodeInvalidVariant)
	}
	// A variant that sets neither is invalid.
	neither := &Manifest{ProtocolVersion: 1, Name: "c",
		Unions: []Union{{
			Name: "U", Discriminator: "kind",
			Variants: []UnionVariant{{Tag: "a"}},
		}}}
	if !findCode(ValidateManifest(neither), ErrorCodeInvalidVariant) {
		t.Errorf("want %s when a variant sets neither struct nor fields", ErrorCodeInvalidVariant)
	}
}

func TestValidateManifest_UnknownReference(t *testing.T) {
	cases := []struct {
		name string
		m    *Manifest
	}{
		{"variant struct", &Manifest{ProtocolVersion: 1, Name: "c", Unions: []Union{{
			Name: "U", Discriminator: "kind", Variants: []UnionVariant{{Tag: "a", Struct: "Missing"}},
		}}}},
		{"struct field type", &Manifest{ProtocolVersion: 1, Name: "c", Structs: []Struct{{
			Name: "S", Fields: []Field{{Name: "x", Type: "Nope"}},
		}}}},
		{"capability scope", &Manifest{ProtocolVersion: 1, Name: "c", Capabilities: []Capability{{
			Name: "cap", Scopes: []string{"missing:scope"},
		}}}},
		{"grant capability", &Manifest{ProtocolVersion: 1, Name: "c", Grants: []Grant{{
			Name: "g", Capability: "missing",
		}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !findCode(ValidateManifest(tc.m), ErrorCodeUnknownReference) {
				t.Errorf("want %s, got %v", ErrorCodeUnknownReference, ValidateManifest(tc.m))
			}
		})
	}
}

func TestValidateManifest_DuplicateNode(t *testing.T) {
	// Type names share one namespace across enums, unions, and structs.
	typeClash := &Manifest{ProtocolVersion: 1, Name: "c",
		Enums:   []Enum{{Name: "T", Values: []EnumValue{{Name: "A", Value: "a"}}}},
		Structs: []Struct{{Name: "T", Fields: []Field{{Name: "x", Type: "string"}}}},
	}
	if !findCode(ValidateManifest(typeClash), ErrorCodeDuplicateNode) {
		t.Errorf("want %s for enum/struct name clash", ErrorCodeDuplicateNode)
	}
	scopeClash := &Manifest{ProtocolVersion: 1, Name: "c",
		Scopes: []Scope{{Name: "s"}, {Name: "s"}},
	}
	if !findCode(ValidateManifest(scopeClash), ErrorCodeDuplicateNode) {
		t.Errorf("want %s for duplicate scope", ErrorCodeDuplicateNode)
	}
}

func TestValidateManifest_DuplicateField(t *testing.T) {
	// Two fields sharing a name within one struct would emit a duplicate key.
	structDup := &Manifest{ProtocolVersion: 1, Name: "c", Structs: []Struct{{
		Name:   "S",
		Fields: []Field{{Name: "x", Type: "string"}, {Name: "x", Type: "int"}},
	}}}
	if !findCode(ValidateManifest(structDup), ErrorCodeDuplicateNode) {
		t.Errorf("want %s for duplicate struct field, got %v", ErrorCodeDuplicateNode, ValidateManifest(structDup))
	}
	// The same rule applies to an inline union variant's fields.
	variantDup := &Manifest{ProtocolVersion: 1, Name: "c", Unions: []Union{{
		Name: "U", Discriminator: "kind",
		Variants: []UnionVariant{{Tag: "a", Fields: []Field{{Name: "x", Type: "string"}, {Name: "x", Type: "int"}}}},
	}}}
	if !findCode(ValidateManifest(variantDup), ErrorCodeDuplicateNode) {
		t.Errorf("want %s for duplicate inline variant field, got %v", ErrorCodeDuplicateNode, ValidateManifest(variantDup))
	}
}

func TestValidateManifest_InvalidFieldType(t *testing.T) {
	configField := &Manifest{ProtocolVersion: 1, Name: "c", ConfigFields: []ConfigField{{Name: "n", Type: "widget"}}}
	if !findCode(ValidateManifest(configField), ErrorCodeInvalidFieldType) {
		t.Errorf("want %s for config field, got %v", ErrorCodeInvalidFieldType, ValidateManifest(configField))
	}
	claim := &Manifest{ProtocolVersion: 1, Name: "c", Claims: []Claim{{Name: "n", Type: "widget"}}}
	if !findCode(ValidateManifest(claim), ErrorCodeInvalidFieldType) {
		t.Errorf("want %s for claim, got %v", ErrorCodeInvalidFieldType, ValidateManifest(claim))
	}
}

func TestValidateManifest_InvalidDefault(t *testing.T) {
	cases := []struct {
		name string
		cf   ConfigField
	}{
		{"string wants string", ConfigField{Name: "n", Type: "string", Default: 1}},
		{"int wants number", ConfigField{Name: "n", Type: "int", Default: "nope"}},
		{"int rejects fractional", ConfigField{Name: "n", Type: "int", Default: 1.5}},
		{"bool wants boolean", ConfigField{Name: "n", Type: "bool", Default: "true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{ProtocolVersion: 1, Name: "c", ConfigFields: []ConfigField{tc.cf}}
			if !findCode(ValidateManifest(m), ErrorCodeInvalidDefault) {
				t.Errorf("want %s, got %v", ErrorCodeInvalidDefault, ValidateManifest(m))
			}
		})
	}
}

func TestValidateManifest_TypedDefaultsAccepted(t *testing.T) {
	// The typed defaults on the representative sample must validate clean, and
	// stay clean after a JSON round-trip (numbers decode to float64).
	data := []byte(`{"protocolVersion":1,"name":"c","configFields":[` +
		`{"name":"a","type":"int","default":7},` +
		`{"name":"b","type":"string","default":"x"},` +
		`{"name":"d","type":"bool","default":false},` +
		`{"name":"e","type":"float","default":1.5}]}`)
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("typed defaults rejected after round-trip: %v", diags)
	}
	if m == nil {
		t.Fatal("nil manifest")
	}
}

func TestValidErrorCodes_Membership(t *testing.T) {
	required := []string{
		ErrorCodeParseError,
		ErrorCodeUnknownField,
		ErrorCodeInvalidProtocolVersion,
		ErrorCodeMissingName,
		ErrorCodeInvalidName,
		ErrorCodeDuplicateNode,
		ErrorCodeDuplicateVariant,
		ErrorCodeMissingEnumValues,
		ErrorCodeEnumValueConflict,
		ErrorCodeInvalidDiscriminator,
		ErrorCodeMissingVariants,
		ErrorCodeInvalidVariant,
		ErrorCodeUnknownReference,
		ErrorCodeInvalidFieldType,
		ErrorCodeInvalidDefault,
	}
	for _, code := range required {
		if !ValidErrorCodes[code] {
			t.Errorf("ValidErrorCodes missing %q", code)
		}
	}
	// Every code in the taxonomy must carry the contracts. namespace.
	for code := range ValidErrorCodes {
		if !strings.HasPrefix(code, "contracts.") {
			t.Errorf("error code %q is not namespaced under contracts.", code)
		}
	}
}
