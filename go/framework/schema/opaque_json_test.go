package schema

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// A json.RawMessage or an empty-interface field is published as an opaque JSON
// value, and null is one of the values that declaration admits: the validator
// must keep it instead of refusing it as a missing value. Every other
// non-pointer type still refuses null.
func TestValidateDecodedAdmitsNullForAnOpaqueValue(t *testing.T) {
	type input struct {
		Raw        json.RawMessage            `json:"raw" validate:"required"`
		Value      any                        `json:"value" validate:"required"`
		Attributes map[string]any             `json:"attributes" validate:"required"`
		Frames     map[string]json.RawMessage `json:"frames"`
		Trail      []any                      `json:"trail"`
		Count      int64                      `json:"count"`
	}
	raw := []byte(`{"raw":null,"value":null,"attributes":{"a":null,"b":{"c":null}},"frames":{"f":null},"trail":[null,1]}`)
	var decoded input
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, raw, WithLabel("body"))
	if result.HasErrors() {
		t.Fatalf("null was refused for an opaque value: %v", result.Errors)
	}
	if !bytes.Equal(decoded.Raw, []byte("null")) || !bytes.Equal(decoded.Frames["f"], []byte("null")) {
		t.Fatalf("an explicit null did not survive as the bytes null: raw=%q frames=%q", decoded.Raw, decoded.Frames["f"])
	}
	if value, present := decoded.Attributes["a"]; !present || value != nil {
		t.Fatalf("attributes[a] = %v (present %v), want an explicit nil", value, present)
	}

	for name, invalid := range map[string][]byte{
		"a free-form object is not nullable": []byte(`{"raw":1,"value":1,"attributes":null}`),
		"a scalar is still not nullable":     []byte(`{"raw":1,"value":1,"attributes":{},"count":null}`),
		"a required opaque value is present": []byte(`{"value":1,"attributes":{}}`),
	} {
		var target input
		_ = json.Unmarshal(invalid, &target)
		if result := ValidateDecoded(reflect.TypeOf(input{}), &target, invalid); !result.HasErrors() {
			t.Errorf("%s: %s was accepted", name, invalid)
		}
	}
}
