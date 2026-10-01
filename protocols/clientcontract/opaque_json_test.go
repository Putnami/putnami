package clientcontract

import (
	"bytes"
	"encoding/json"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const opaqueJSONRequirement = "opaque-json-values"

// TestOpaqueJSONHasOneClosedSpelling pins the two declarations a provider may
// publish for a value it does not interpret, and every near-miss the strict
// reader refuses. The empty schema stays refused: "any JSON value" is a
// declaration, never the absence of one.
func TestOpaqueJSONHasOneClosedSpelling(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", opaqueJSONRequirement,
		"an-opaque-json-value-and-a-free-form-object-are-accepted-in-their-one-closed-spelling")
	spectest.Proves(t, "client-contract/first-party-generated-clients", opaqueJSONRequirement,
		"the-empty-schema-and-every-other-opaque-spelling-are-refused")

	accepted := map[string]string{
		"opaque value":              `{"x-putnami-json":"any"}`,
		"documented opaque value":   `{"x-putnami-json":"any","title":"Payload","description":"carried as sent"}`,
		"free-form object":          `{"type":"object","additionalProperties":true}`,
		"nullable free-form object": `{"type":"object","additionalProperties":true,"nullable":true}`,
		"array of opaque values":    `{"type":"array","items":{"x-putnami-json":"any"}}`,
		"opaque property":           `{"type":"object","properties":{"value":{"x-putnami-json":"any"}},"required":["value"],"additionalProperties":false}`,
	}
	for name, raw := range accepted {
		t.Run("accepts/"+name, func(t *testing.T) {
			if _, diags := ParseAndValidateSchema([]byte(raw)); len(diags) != 0 {
				t.Fatalf("%s refused: %v", raw, diags)
			}
		})
	}

	refused := []struct {
		name string
		raw  string
		code string
	}{
		{"empty schema", `{}`, ErrorCodeInvalidSchema},
		{"empty property schema", `{"type":"object","properties":{"value":{}},"additionalProperties":false}`, ErrorCodeInvalidSchema},
		{"another keyword value", `{"x-putnami-json":"object"}`, ErrorCodeInvalidEnum},
		{"a type beside it", `{"x-putnami-json":"any","type":"object"}`, ErrorCodeInvalidSchema},
		{"nullable beside it", `{"x-putnami-json":"any","nullable":true}`, ErrorCodeInvalidSchema},
		{"a reference beside it", `{"x-putnami-json":"any","$ref":"#/components/schemas/Other"}`, ErrorCodeInvalidSchema},
		{"a default beside it", `{"x-putnami-json":"any","default":{}}`, ErrorCodeInvalidSchema},
		{"a union beside it", `{"x-putnami-json":"any","oneOf":[{"type":"string"},{"type":"integer","format":"int32"}]}`, ErrorCodeInvalidSchema},
		{"an opaque map value", `{"type":"object","additionalProperties":{"x-putnami-json":"any"}}`, ErrorCodeInvalidSchema},
		{"a boolean keyword", `{"x-putnami-json":true}`, ErrorCodeParseError},
		{"a null keyword", `{"x-putnami-json":null}`, ErrorCodeParseError},
	}
	for _, tc := range refused {
		t.Run("refuses/"+tc.name, func(t *testing.T) {
			_, diags := ParseAndValidateSchema([]byte(tc.raw))
			if !diag.HasErrors(diags) {
				t.Fatalf("%s accepted", tc.raw)
			}
			for _, diagnostic := range diags {
				if diagnostic.Code == tc.code {
					return
				}
			}
			t.Fatalf("%s: diagnostics %v do not carry %s", tc.raw, diags, tc.code)
		})
	}
}

// TestOpaqueJSONRoundTripsThroughTheWireType proves the keyword survives the
// strict reader and the writer byte for byte, so a provider document re-read by
// a consumer publishes the same declaration.
func TestOpaqueJSONRoundTripsThroughTheWireType(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", opaqueJSONRequirement,
		"the-opaque-json-keyword-round-trips-through-the-wire-type")

	raw := []byte(`{"type":"object","properties":{"attributes":{"type":"object","additionalProperties":true},"value":{"description":"carried as sent","x-putnami-json":"any"}},"required":["attributes","value"],"additionalProperties":false}`)
	schema, diags := ParseAndValidateSchema(raw)
	if len(diags) != 0 {
		t.Fatalf("diagnostics: %v", diags)
	}
	value := schema.Properties["value"]
	if !value.IsOpaqueJSON() || value.IsFreeFormObject() {
		t.Fatalf("value = %+v, want an opaque JSON value", value)
	}
	attributes := schema.Properties["attributes"]
	if !attributes.IsFreeFormObject() || attributes.IsOpaqueJSON() {
		t.Fatalf("attributes = %+v, want a free-form object", attributes)
	}
	written, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, raw) {
		t.Fatalf("round trip changed the declaration:\n got: %s\nwant: %s", written, raw)
	}
}
