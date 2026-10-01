package schema

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestValidateDecodedPreservesPresenceNullAndWideIntegers(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "decoded-json-fidelity", "raw-json-presence-distinguishes-absent-null-and-present-zero-values")
	spectest.Proves(t, "go/struct-validation", "decoded-json-fidelity", "nested-json-values-are-validated-recursively-with-exact-numbers")
	spectest.Proves(t, "go/struct-validation", "decoded-json-fidelity", "optional-defaults-are-applied-to-the-decoded-value")
	type child struct {
		Count int64  `json:"count" validate:"required,min=9007199254740993"`
		Label string `json:"label" default:"ready"`
	}
	type input struct {
		Enabled         bool             `json:"enabled" validate:"required"`
		Count           int64            `json:"count" validate:"required"`
		Label           string           `json:"label" validate:"required"`
		Wide            uint64           `json:"wide" validate:"required,min=9007199254740993,max=9007199254740993"`
		Nullable        *string          `json:"nullable" validate:"required"`
		Nested          child            `json:"nested" validate:"required"`
		Rows            []child          `json:"rows" validate:"required"`
		ByName          map[string]child `json:"byName" validate:"required"`
		Defaulted       int64            `json:"defaulted" default:"9007199254740993"`
		RequiredDefault int64            `json:"requiredDefault" default:"7" validate:"required"`
	}
	decoded := input{
		Wide:   9007199254740993,
		Nested: child{Count: 9007199254740993},
		Rows:   []child{{Count: 9007199254740993}},
		ByName: map[string]child{"primary": {Count: 9007199254740993}},
	}
	raw := []byte(`{"enabled":false,"count":0,"label":"","wide":9007199254740993,"nullable":null,"nested":{"count":9007199254740993},"rows":[{"count":9007199254740993}],"byName":{"primary":{"count":9007199254740993}},"requiredDefault":0}`)
	result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, raw, WithLabel("body"))
	if result.HasErrors() {
		t.Fatalf("present zero values, exact wide integer, and nullable null were rejected: %v", result.Errors)
	}
	if got, ok := result.Data["wide"].(uint64); !ok || got != 9007199254740993 {
		t.Fatalf("wide = %T(%v), want exact uint64", result.Data["wide"], result.Data["wide"])
	}

	if decoded.Defaulted != 9007199254740993 {
		t.Fatalf("decoded default = %d, want exact int64", decoded.Defaulted)
	}
	if decoded.RequiredDefault != 0 {
		t.Fatalf("present required zero was replaced by its default: %d", decoded.RequiredDefault)
	}
	if decoded.Nested.Label != "ready" || decoded.Rows[0].Label != "ready" || decoded.ByName["primary"].Label != "ready" {
		t.Fatalf("nested defaults were not applied to decoded body: %#v", decoded)
	}

	missing := []byte(`{"enabled":false,"label":"","wide":9007199254740993,"nullable":null,"nested":{"count":9007199254740993},"rows":[],"byName":{}}`)
	if result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, missing); !result.HasErrors() {
		t.Fatal("missing required field was accepted")
	}
	missingRequiredDefault := []byte(`{"enabled":false,"count":0,"label":"","wide":9007199254740993,"nullable":null,"nested":{"count":9007199254740993},"rows":[],"byName":{}}`)
	if result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, missingRequiredDefault); !result.HasErrors() {
		t.Fatal("required field with a default was accepted while absent")
	}

	tooLarge := decoded
	tooLarge.Wide++
	if result := ValidateDecoded(reflect.TypeOf(input{}), &tooLarge, raw); !result.HasErrors() {
		t.Fatal("wide integer immediately above the exact maximum was accepted")
	}

	nullNonNullable := []byte(`{"enabled":false,"count":null,"label":"","wide":9007199254740993,"nullable":null,"nested":{"count":9007199254740993},"rows":[],"byName":{}}`)
	if result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, nullNonNullable); !result.HasErrors() {
		t.Fatal("null for a non-nullable field was accepted")
	}

	for name, invalid := range map[string][]byte{
		"nested missing required": []byte(`{"enabled":false,"count":0,"label":"","wide":9007199254740993,"nullable":null,"nested":{},"rows":[],"byName":{}}`),
		"nested scalar null":      []byte(`{"enabled":false,"count":0,"label":"","wide":9007199254740993,"nullable":null,"nested":{"count":null},"rows":[],"byName":{}}`),
		"slice nested missing":    []byte(`{"enabled":false,"count":0,"label":"","wide":9007199254740993,"nullable":null,"nested":{"count":9007199254740993},"rows":[{}],"byName":{}}`),
		"map nested missing":      []byte(`{"enabled":false,"count":0,"label":"","wide":9007199254740993,"nullable":null,"nested":{"count":9007199254740993},"rows":[],"byName":{"primary":{}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, invalid); !result.HasErrors() {
				t.Fatal("invalid nested body was accepted")
			}
		})
	}
}

func TestValidateDecodedUsesJSONPromotionDominanceAndOmission(t *testing.T) {
	type Promoted struct {
		Required string `json:"required" validate:"required"`
		Hidden   string `json:"-" validate:"required"`
	}
	type Untagged struct {
		Value string
	}
	type Tagged struct {
		Other string `json:"Value"`
	}
	type input struct {
		*Promoted
		Untagged
		Tagged
	}

	raw := []byte(`{"required":"present","Value":"tagged"}`)
	var decoded input
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, raw)
	if result.HasErrors() {
		t.Fatalf("promoted field, tagged winner, or omitted field was mishandled: %v", result.Errors)
	}
	if decoded.Promoted == nil || decoded.Required != "present" || decoded.Tagged.Other != "tagged" || decoded.Untagged.Value != "" {
		t.Fatalf("decoded promotion/dominance = %#v", decoded)
	}

	missing := []byte(`{"Value":"tagged"}`)
	var missingDecoded input
	if err := json.Unmarshal(missing, &missingDecoded); err != nil {
		t.Fatal(err)
	}
	if result := ValidateDecoded(reflect.TypeOf(input{}), &missingDecoded, missing); !result.HasErrors() {
		t.Fatal("missing required promoted property was accepted")
	}
}

func TestJSONFieldsOmitsAmbiguousDiamondAndPreservesLegacyUntaggedName(t *testing.T) {
	type X struct {
		Count int `validate:"required"`
	}
	type A struct{ X }
	type B struct{ X }
	type diamond struct {
		A
		B
	}

	encoded, err := json.Marshal(diamond{A: A{X: X{Count: 1}}, B: B{X: X{Count: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("encoding/json diamond = %s, want ambiguous Count omitted", encoded)
	}
	if fields := JSONFields(reflect.TypeOf(diamond{})); len(fields) != 0 {
		t.Fatalf("shared JSON fields retained ambiguous diamond: %+v", fields)
	}
	if result := ValidateDecoded(reflect.TypeOf(diamond{}), new(diamond), encoded); result.HasErrors() {
		t.Fatalf("omitted ambiguous field was validated: %v", result.Errors)
	}

	type legacy struct {
		Count int `validate:"required"`
	}
	if result := Validate(reflect.TypeOf(legacy{}), map[string]any{"Count": 1}); result.HasErrors() {
		t.Fatalf("legacy untagged Go field name changed: %v", result.Errors)
	}
}

func TestValidatePreservesLegacyMapFieldSelectionAndPointerValues(t *testing.T) {
	type Embedded struct {
		Promoted string `json:"promoted"`
	}
	type legacy struct {
		Embedded `validate:"required"`
		Ignored  string `json:"-" validate:"required"`
		Pointer  *int   `json:"pointer"`
	}

	pointer := 7
	embedded := Embedded{Promoted: "caller-map-value"}
	result := Validate(reflect.TypeOf(legacy{}), map[string]any{
		"Embedded": embedded,
		"-":        "historical-key",
		"pointer":  &pointer,
	})
	if result.HasErrors() {
		t.Fatalf("historical map field selection was rejected: %v", result.Errors)
	}
	if got, ok := result.Data["Embedded"].(Embedded); !ok || got != embedded {
		t.Fatalf("embedded field = %T(%v), want caller-supplied %#v", result.Data["Embedded"], result.Data["Embedded"], embedded)
	}
	if got := result.Data["-"]; got != "historical-key" {
		t.Fatalf("json-dash map key = %v, want historical-key", got)
	}
	if got, ok := result.Data["pointer"].(*int); !ok || got != &pointer {
		t.Fatalf("pointer = %T(%v), want original pointer", result.Data["pointer"], result.Data["pointer"])
	}
	if _, promoted := result.Data["promoted"]; promoted {
		t.Fatalf("Validate promoted an embedded JSON field in its caller-authored map: %#v", result.Data)
	}
}

func TestValidateDecodedDoesNotAllocateAbsentEmbeddedPointer(t *testing.T) {
	type Embedded struct {
		Optional string `json:"optional"`
	}
	type input struct{ *Embedded }
	var decoded input
	if result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, []byte(`{}`)); result.HasErrors() {
		t.Fatal(result.Errors)
	}
	if decoded.Embedded != nil {
		t.Fatalf("absent optional embedded object was allocated: %#v", decoded)
	}
}

func TestJSONFieldsMatchesEncodingJSONForUnexportedEmbedding(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "decoded-json-fidelity", "json-field-selection-matches-encoding-json")
	type hidden struct {
		Visible string `json:"visible" validate:"required"`
	}
	type input struct {
		hidden
		Fallback string `json:"fallback" validate:"required"`
	}
	decoded := input{hidden: hidden{Visible: "yes"}, Fallback: "exact"}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"visible":"yes","fallback":"exact"}` {
		t.Fatalf("encoding/json fields = %s", encoded)
	}
	fields := JSONFields(reflect.TypeOf(input{}))
	names := make(map[string]bool, len(fields))
	for _, field := range fields {
		names[field.Name] = true
	}
	if len(fields) != 2 || !names["visible"] || !names["fallback"] {
		t.Fatalf("shared JSON fields differ from encoding/json: %+v", fields)
	}
	if result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, encoded); result.HasErrors() {
		t.Fatalf("wire-valid promoted and fallback fields were rejected: %v", result.Errors)
	}

	missing := []byte(`{"fallback":"exact"}`)
	var missingDecoded input
	if err := json.Unmarshal(missing, &missingDecoded); err != nil {
		t.Fatal(err)
	}
	if result := ValidateDecoded(reflect.TypeOf(input{}), &missingDecoded, missing); !result.HasErrors() {
		t.Fatal("required field promoted from unexported anonymous struct was ignored")
	}
}

func TestJSONFieldsInvalidTagFallsBackToGoFieldName(t *testing.T) {
	invalidTag := reflect.StructTag("json:" + strconv.Quote("bad\\name"))
	dynamicType := reflect.StructOf([]reflect.StructField{{
		Name: "Fallback",
		Type: reflect.TypeOf(""),
		Tag:  invalidTag,
	}})
	value := reflect.New(dynamicType).Elem()
	value.Field(0).SetString("exact")
	encoded, err := json.Marshal(value.Interface())
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"Fallback":"exact"}` {
		t.Fatalf("encoding/json invalid tag fallback = %s", encoded)
	}
	fields := JSONFields(dynamicType)
	if len(fields) != 1 || fields[0].Name != "Fallback" {
		t.Fatalf("shared JSON fields kept an invalid tag name: %+v", fields)
	}
}

func TestJSONFieldsPreservesExplicitDashFieldName(t *testing.T) {
	tag := reflect.StructTag("json:" + strconv.Quote("-,omitempty") + " validate:" + strconv.Quote("required"))
	inputType := reflect.StructOf([]reflect.StructField{{
		Name: "Value",
		Type: reflect.TypeOf(""),
		Tag:  tag,
	}})
	value := reflect.New(inputType).Elem()
	value.Field(0).SetString("exact")
	encoded, err := json.Marshal(value.Interface())
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"-":"exact"}` {
		t.Fatalf("encoding/json explicit dash name = %s", encoded)
	}
	fields := JSONFields(inputType)
	if len(fields) != 1 || fields[0].Name != "-" {
		t.Fatalf("shared JSON fields lost explicit dash name: %+v", fields)
	}
	decoded := reflect.New(inputType)
	if err := json.Unmarshal(encoded, decoded.Interface()); err != nil {
		t.Fatal(err)
	}
	if result := ValidateDecoded(inputType, decoded.Interface(), encoded); result.HasErrors() {
		t.Fatalf("explicit dash field was rejected: %v", result.Errors)
	}
}

func TestValidateDecodedHandlesNamedAndPromotedUnexportedEmbedsWithoutUnsafe(t *testing.T) {
	type private struct {
		Required string `json:"required" validate:"required"`
		Count    int    `json:"count,omitempty" default:"7"`
	}
	type namedValue struct {
		private `json:"nested"`
	}
	type promotedValue struct{ private }
	type namedPointer struct {
		*private `json:"nested"`
	}
	type promotedPointer struct{ *private }

	tests := []struct {
		name        string
		wireValue   any
		newTarget   func() any
		wantDataKey string
		count       func(any) int
	}{
		{
			name: "named value", wireValue: namedValue{private: private{Required: "yes"}},
			newTarget: func() any { return new(namedValue) }, wantDataKey: "nested",
			count: func(value any) int { return value.(*namedValue).Count },
		},
		{
			name: "promoted value", wireValue: promotedValue{private: private{Required: "yes"}},
			newTarget: func() any { return new(promotedValue) }, wantDataKey: "count",
			count: func(value any) int { return value.(*promotedValue).Count },
		},
		{
			name: "named pointer", wireValue: namedPointer{private: &private{Required: "yes"}},
			newTarget: func() any { return &namedPointer{private: &private{}} }, wantDataKey: "nested",
			count: func(value any) int { return value.(*namedPointer).Count },
		},
		{
			name: "promoted pointer", wireValue: promotedPointer{private: &private{Required: "yes"}},
			newTarget: func() any { return &promotedPointer{private: &private{}} }, wantDataKey: "count",
			count: func(value any) int { return value.(*promotedPointer).Count },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.wireValue)
			if err != nil {
				t.Fatal(err)
			}
			target := test.newTarget()
			if err := json.Unmarshal(raw, target); err != nil {
				t.Fatalf("encoding/json could not decode its own representation %s: %v", raw, err)
			}
			result := ValidateDecoded(reflect.TypeOf(target).Elem(), target, raw)
			if result.HasErrors() {
				t.Fatalf("wire-valid body was rejected: %v", result.Errors)
			}
			if test.count(target) != 7 {
				t.Fatalf("nested default was not applied to the decoded value: %#v", target)
			}
			data, ok := result.Data[test.wantDataKey]
			if !ok {
				t.Fatalf("validated data omitted %q: %#v", test.wantDataKey, result.Data)
			}
			if test.wantDataKey == "nested" {
				nested, ok := data.(map[string]any)
				if !ok || nested["count"] != 7 {
					t.Fatalf("named embed data lost its validated default: %#v", data)
				}
			}
		})
	}
}

func TestValidateDecodedRejectsUnrepresentableUnexportedEmbedRulesSafely(t *testing.T) {
	type constrained struct {
		Value string `json:"value"`
	}
	type withConstraint struct {
		constrained `json:"nested" validate:"minlen=1"`
	}
	type withDefault struct {
		constrained `json:"nested" default:"{}"`
	}

	constraintTarget := &withConstraint{constrained: constrained{Value: "yes"}}
	constraintResult := ValidateDecoded(reflect.TypeOf(*constraintTarget), constraintTarget, []byte(`{"nested":{"value":"yes"}}`))
	if !constraintResult.HasErrors() || !strings.Contains(constraintResult.Errors[0].Message, "unexported field") {
		t.Fatalf("unrepresentable value constraint did not fail safely: %v", constraintResult.Errors)
	}

	defaultTarget := new(withDefault)
	defaultResult := ValidateDecoded(reflect.TypeOf(*defaultTarget), defaultTarget, []byte(`{}`))
	if !defaultResult.HasErrors() || !strings.Contains(defaultResult.Errors[0].Message, "not settable") {
		t.Fatalf("unrepresentable default did not fail safely: %v", defaultResult.Errors)
	}
}

func TestDefaultJSONRejectsInvalidBytesAndNonNullableNullWithoutEchoingValues(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "decoded-json-fidelity", "invalid-defaults-fail-without-exposing-authored-values")
	for _, test := range []struct {
		name  string
		value string
		typ   reflect.Type
	}{
		{name: "bytes", value: "not base64 !", typ: reflect.TypeOf([]byte(nil))},
		{name: "non-nullable null", value: "null", typ: reflect.TypeOf(int64(0))},
		{name: "nil target type", value: "sensitive-default", typ: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DefaultJSON(test.value, test.typ)
			if err == nil {
				t.Fatal("invalid default was accepted")
			}
			if strings.Contains(err.Error(), test.value) {
				t.Fatalf("default error exposed the authored value: %v", err)
			}
		})
	}
}

func TestValidateDecodedDistinguishesEmptyDefaultFromNoDefault(t *testing.T) {
	type decodedInput struct {
		Value *string `json:"value,omitempty" default:""`
	}
	decoded := decodedInput{}
	result := ValidateDecoded(reflect.TypeOf(decodedInput{}), &decoded, []byte(`{}`))
	if result.HasErrors() {
		t.Fatalf("empty string default was rejected: %v", result.Errors)
	}
	if decoded.Value == nil || *decoded.Value != "" {
		t.Fatalf("empty string default was not applied: %#v", decoded.Value)
	}

	type invalidDecodedInput struct {
		Value int `json:"value,omitempty" default:""`
	}
	invalid := invalidDecodedInput{}
	invalidResult := ValidateDecoded(reflect.TypeOf(invalidDecodedInput{}), &invalid, []byte(`{}`))
	if !invalidResult.HasErrors() || invalidResult.Errors[0].Constraint != "default" {
		t.Fatalf("empty non-string default did not fail explicitly: %v", invalidResult.Errors)
	}

	// Validate's historical map contract treated an empty tag value as no
	// default. Keep that behavior while the JSON-aware API uses tag presence.
	type legacyInput struct {
		Value string `json:"value" default:""`
	}
	legacyResult := Validate(reflect.TypeOf(legacyInput{}), map[string]any{})
	if legacyResult.HasErrors() {
		t.Fatalf("legacy empty default produced an error: %v", legacyResult.Errors)
	}
	if _, exists := legacyResult.Data["value"]; exists {
		t.Fatalf("legacy Validate started materializing an empty default: %#v", legacyResult.Data)
	}
}

func TestValidateDecodedMapDiagnosticsAreDeterministic(t *testing.T) {
	type child struct {
		Required string `json:"required" validate:"required"`
	}
	type input struct {
		Items map[string]child `json:"items" validate:"required"`
	}
	decoded := input{Items: map[string]child{"zeta": {}, "alpha": {}}}
	raw := []byte(`{"items":{"zeta":{},"alpha":{}}}`)

	result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, raw)
	if len(result.Errors) != 2 {
		t.Fatalf("map validation errors = %v, want two required-field errors", result.Errors)
	}
	if result.Errors[0].Field != "items.alpha.required" || result.Errors[1].Field != "items.zeta.required" {
		t.Fatalf("map validation errors are not key-sorted: %v", result.Errors)
	}
}

func TestValidateDecodedAppliesNumericBoundsToNamedGoTypes(t *testing.T) {
	type Amount uint64
	type Ratio float64
	type input struct {
		Amount Amount `json:"amount" validate:"min=9007199254740993,max=9007199254740993"`
		Ratio  Ratio  `json:"ratio" validate:"min=1.25,max=1.25"`
	}
	decoded := input{Amount: 9007199254740993, Ratio: 1.25}
	raw := []byte(`{"amount":9007199254740993,"ratio":1.25}`)
	if result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, raw); result.HasErrors() {
		t.Fatalf("named numeric types were rejected at their exact bounds: %v", result.Errors)
	}

	decoded.Amount++
	if result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, raw); !result.HasErrors() {
		t.Fatal("named uint64 above its exact maximum was accepted")
	}
	decoded.Amount = 9007199254740993
	decoded.Ratio = 1.5
	if result := ValidateDecoded(reflect.TypeOf(input{}), &decoded, raw); !result.HasErrors() {
		t.Fatal("named float above its maximum was accepted")
	}
}

type UserInput struct {
	Name  string `json:"name" validate:"required,minlen=2,maxlen=50"`
	Email string `json:"email" validate:"required,email"`
	Age   int    `json:"age" validate:"min=0,max=150"`
}

type OptionalInput struct {
	Label   string `json:"label"`
	Count   int    `json:"count" default:"10"`
	Verbose bool   `json:"verbose" default:"true"`
}

type UUIDInput struct {
	ID string `json:"id" validate:"required,uuid"`
}

type URLInput struct {
	Website string `json:"website" validate:"url"`
}

type OneOfInput struct {
	Status string `json:"status" validate:"required,oneof=active|inactive|pending"`
}

type PatternInput struct {
	Code string `json:"code" validate:"pattern=^[A-Z]{3}-[0-9]{4}$"`
}

func TestValidateRequired(t *testing.T) {
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{})

	if !result.HasErrors() {
		t.Fatal("expected validation errors")
	}

	// Name and email are required
	if len(result.Errors) != 2 {
		t.Errorf("expected 2 errors, got %d: %v", len(result.Errors), result.Errors)
	}
}

func TestValidateNonStructReturnsErrorNotPanic(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "no-panic-on-bad-type", "a-non-struct-type-returns-a-schema-error-instead-of-panicking")
	// Non-struct and multi-pointer types must yield a structured error, not a
	// panic across the public API boundary.
	cases := []reflect.Type{
		reflect.TypeOf(0),                  // basic
		reflect.TypeOf([]string{}),         // slice
		reflect.TypeOf(map[string]int{}),   // map
		reflect.TypeOf((**UserInput)(nil)), // double pointer
		nil,                                // nil type
	}
	for _, ty := range cases {
		result := Validate(ty, map[string]any{})
		if !result.HasErrors() {
			t.Errorf("Validate(%v) should report a schema-type error, got none", ty)
		}
	}

	// A single-pointer struct still validates normally (deref loop reaches it).
	if result := Validate(reflect.TypeOf(&UserInput{}), map[string]any{
		"name":  "Alice",
		"email": "alice@example.com",
		"age":   30,
	}); result.HasErrors() {
		t.Errorf("single-pointer struct should validate, got errors: %v", result.Errors)
	}
}

func TestValidateValid(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "validated-data-only", "a-field-that-passed-every-constraint-reaches-data")
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{
		"name":  "Alice",
		"email": "alice@example.com",
		"age":   30,
	})

	if result.HasErrors() {
		t.Fatalf("expected no errors, got: %v", result.Errors)
	}
	if result.Data["name"] != "Alice" {
		t.Errorf("expected 'Alice', got %v", result.Data["name"])
	}
}

// TestValidateExcludesConstraintFailedFieldsFromData pins the documented
// contract that Data holds only fields that passed validation: a field that is
// present but fails a constraint must not leak its invalid value into Data.
func TestValidateExcludesConstraintFailedFieldsFromData(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "validated-data-only", "a-field-that-failed-a-constraint-is-excluded-from-data")
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{
		"name":  "Alice",        // valid
		"email": "not-an-email", // present but fails the email constraint
		"age":   30,             // valid
	})

	if !result.HasErrors() {
		t.Fatal("expected validation errors for the invalid email")
	}
	if v, ok := result.Data["email"]; ok {
		t.Errorf("constraint-failed field 'email' must be excluded from Data, got %v", v)
	}
	// Fields that passed must still be present in Data.
	if result.Data["name"] != "Alice" {
		t.Errorf("valid field 'name' missing from Data: %v", result.Data["name"])
	}
	if result.Data["age"] != 30 {
		t.Errorf("valid field 'age' missing from Data: %v", result.Data["age"])
	}
}

func TestValidateMinLen(t *testing.T) {
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{
		"name":  "A",
		"email": "a@b.com",
	})

	if !result.HasErrors() {
		t.Fatal("expected minlen error")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "name" && e.Constraint == "minlen" {
			found = true
		}
	}
	if !found {
		t.Error("expected minlen error on name field")
	}
}

func TestValidateEmail(t *testing.T) {
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{
		"name":  "Alice",
		"email": "not-an-email",
	})

	if !result.HasErrors() {
		t.Fatal("expected email error")
	}
}

func TestValidateMin(t *testing.T) {
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{
		"name":  "Alice",
		"email": "a@b.com",
		"age":   -1,
	})

	found := false
	for _, e := range result.Errors {
		if e.Field == "age" && e.Constraint == "min" {
			found = true
		}
	}
	if !found {
		t.Error("expected min error on age field")
	}
}

func TestValidateMax(t *testing.T) {
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{
		"name":  "Alice",
		"email": "a@b.com",
		"age":   200,
	})

	found := false
	for _, e := range result.Errors {
		if e.Field == "age" && e.Constraint == "max" {
			found = true
		}
	}
	if !found {
		t.Error("expected max error on age field")
	}
}

func TestValidateDefaults(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "absent-fields", "a-default-tag-fills-an-absent-field")
	result := Validate(reflect.TypeOf(OptionalInput{}), map[string]any{})

	if result.HasErrors() {
		t.Fatalf("expected no errors, got: %v", result.Errors)
	}
	if result.Data["count"] != 10 {
		t.Errorf("expected default count=10, got %v", result.Data["count"])
	}
	if result.Data["verbose"] != true {
		t.Errorf("expected default verbose=true, got %v", result.Data["verbose"])
	}
}

func TestValidateUUID(t *testing.T) {
	// Valid UUID
	result := Validate(reflect.TypeOf(UUIDInput{}), map[string]any{
		"id": "550e8400-e29b-41d4-a716-446655440000",
	})
	if result.HasErrors() {
		t.Fatalf("valid UUID should pass: %v", result.Errors)
	}

	// Invalid UUID
	result = Validate(reflect.TypeOf(UUIDInput{}), map[string]any{
		"id": "not-a-uuid",
	})
	if !result.HasErrors() {
		t.Error("invalid UUID should fail")
	}
}

func TestValidateURL(t *testing.T) {
	result := Validate(reflect.TypeOf(URLInput{}), map[string]any{
		"website": "https://example.com",
	})
	if result.HasErrors() {
		t.Fatalf("valid URL should pass: %v", result.Errors)
	}
}

func TestValidateURL_SchemeAllowlist(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "url-scheme-allowlist", "only-http-and-https-are-accepted")
	valid := []string{
		"https://example.com",
		"http://example.com/path?q=1",
		"HTTPS://EXAMPLE.COM", // scheme is case-insensitive
	}
	for _, u := range valid {
		result := Validate(reflect.TypeOf(URLInput{}), map[string]any{"website": u})
		if result.HasErrors() {
			t.Errorf("%q should be a valid URL: %v", u, result.Errors)
		}
	}

	dangerous := []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"ftp://host/file",
		"gopher://host/1",
		"/relative/path", // no scheme: not an absolute http(s) URL
	}
	for _, u := range dangerous {
		result := Validate(reflect.TypeOf(URLInput{}), map[string]any{"website": u})
		if !result.HasErrors() {
			t.Errorf("%q should be rejected by the url constraint", u)
		}
	}
}

func TestValidateOneOf(t *testing.T) {
	result := Validate(reflect.TypeOf(OneOfInput{}), map[string]any{
		"status": "active",
	})
	if result.HasErrors() {
		t.Fatalf("valid oneof should pass: %v", result.Errors)
	}

	result = Validate(reflect.TypeOf(OneOfInput{}), map[string]any{
		"status": "deleted",
	})
	if !result.HasErrors() {
		t.Error("invalid oneof value should fail")
	}
}

func TestValidatePattern(t *testing.T) {
	result := Validate(reflect.TypeOf(PatternInput{}), map[string]any{
		"code": "ABC-1234",
	})
	if result.HasErrors() {
		t.Fatalf("valid pattern should pass: %v", result.Errors)
	}

	result = Validate(reflect.TypeOf(PatternInput{}), map[string]any{
		"code": "abc-1234",
	})
	if !result.HasErrors() {
		t.Error("invalid pattern should fail")
	}
}

func TestValidateCoerce(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "opt-in-coercion", "coercion-converts-only-when-it-is-opted-into")
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{
		"name":  "Alice",
		"email": "a@b.com",
		"age":   "25",
	}, WithCoerce())

	if result.HasErrors() {
		t.Fatalf("coerced value should pass: %v", result.Errors)
	}
	if result.Data["age"] != 25 {
		t.Errorf("expected coerced age=25, got %v (%T)", result.Data["age"], result.Data["age"])
	}
}

type TypoRequiredInput struct {
	Name string `json:"name" validate:"requierd"`
}

type BadPatternInput struct {
	Code string `json:"code" validate:"pattern=[unclosed"`
}

func TestValidateTypoedRequiredIsSchemaError(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "declaration-faults", "a-typoed-required-does-not-silently-disable-the-check")
	// Absent field: a misspelled "required" must not silently pass — the schema
	// fault is reported instead of the presence check being dropped.
	result := Validate(reflect.TypeOf(TypoRequiredInput{}), map[string]any{})
	if !result.HasErrors() {
		t.Fatal("expected a schema-definition error for a typo'd constraint when the field is absent")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "name" && e.Constraint == "requierd" && strings.Contains(e.Message, "unknown constraint") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an unknown-constraint error naming the field, got: %v", result.Errors)
	}

	// Present field: the same fault is reported once, attributed to the schema,
	// not misreported per-value against the user's data.
	result = Validate(reflect.TypeOf(TypoRequiredInput{}), map[string]any{"name": "x"})
	count := 0
	for _, e := range result.Errors {
		if e.Constraint == "requierd" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one schema error for the typo'd constraint, got %d: %v", count, result.Errors)
	}
}

func TestValidateInvalidPatternIsSchemaError(t *testing.T) {
	// Even when the value is absent, an uncompilable pattern is a schema bug and
	// must be reported rather than silently swallowed.
	result := Validate(reflect.TypeOf(BadPatternInput{}), map[string]any{})
	if !result.HasErrors() {
		t.Fatal("expected a schema-definition error for an uncompilable pattern")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "code" && e.Constraint == "pattern" && strings.Contains(e.Message, "invalid pattern") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an invalid-pattern schema error naming the field, got: %v", result.Errors)
	}
}

func TestValidateWithLabel(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "error-labels", "a-label-prefixes-every-reported-field-name")
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{}, WithLabel("body"))

	if !result.HasErrors() {
		t.Fatal("expected errors")
	}
	for _, e := range result.Errors {
		if e.Field != "body.name" && e.Field != "body.email" {
			t.Errorf("expected prefixed field, got %q", e.Field)
		}
	}
}

// A partial update supplies only some fields. The absent ones must not be
// invented: their constraints do not run, they produce no error, and they stay
// out of Data so the caller can tell "not supplied" from "supplied as empty".
// A default tag is the one exception — it fills the field before the required
// check, so required+default never reports the field as missing.
func TestAbsentFieldsAreSkippedAndDefaultsPrecedeRequired(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "absent-fields", "absent-and-nil-fields-are-skipped-and-defaults-precede-required")
	type partialUpdate struct {
		Name  string `json:"name" validate:"minlen=2,maxlen=50"`
		Email string `json:"email" validate:"email"`
		Tier  string `json:"tier" validate:"required,oneof=free|pro" default:"free"`
	}

	result := Validate(reflect.TypeOf(partialUpdate{}), map[string]any{
		"name": "Ada",
	})

	if result.HasErrors() {
		t.Fatalf("a partial update must validate only the fields it supplies, got: %v", result.Errors)
	}
	if result.Data["name"] != "Ada" {
		t.Errorf("expected the supplied name to survive, got %v", result.Data["name"])
	}
	if _, present := result.Data["email"]; present {
		t.Errorf("an absent field must stay out of Data, got %v", result.Data["email"])
	}
	if result.Data["tier"] != "free" {
		t.Errorf("a default must satisfy the required check, got %v", result.Data["tier"])
	}

	// An explicit nil is absence too, not an empty value to run constraints on.
	nilled := Validate(reflect.TypeOf(partialUpdate{}), map[string]any{
		"name":  "Ada",
		"email": nil,
	})
	if nilled.HasErrors() {
		t.Fatalf("an explicitly nil field must be skipped like an absent one, got: %v", nilled.Errors)
	}
	if _, present := nilled.Data["email"]; present {
		t.Errorf("a nil field must stay out of Data, got %v", nilled.Data["email"])
	}
}

// A declared query parameter reaches the handler at its declared width. Before
// this, only int, int64, float64 and bool were converted: an int32 arrived as an
// unconverted string, which the endpoint's struct binding can neither assign nor
// convert, so the handler read a zero and the declared min/max never applied.
func TestValidateCoerceConvertsEveryDeclarableIntegerWidth(t *testing.T) {
	type widths struct {
		Small    int32  `json:"small"`
		Large    int64  `json:"large"`
		Unsigned uint32 `json:"unsigned"`
		Wide     uint64 `json:"wide"`
	}
	result := Validate(reflect.TypeOf(widths{}), map[string]any{
		"small":    "2147483647",
		"large":    "9223372036854775807",
		"unsigned": "4294967295",
		"wide":     "18446744073709551615",
	}, WithCoerce())
	if result.HasErrors() {
		t.Fatalf("declared widths should coerce: %v", result.Errors)
	}
	for name, want := range map[string]any{
		"small":    int32(2147483647),
		"large":    int64(9223372036854775807),
		"unsigned": uint32(4294967295),
		"wide":     uint64(18446744073709551615),
	} {
		if result.Data[name] != want {
			t.Errorf("%s = %v (%T), want %v (%T)", name, result.Data[name], result.Data[name], want, want)
		}
	}
}

func TestValidateCoerceRefusesAValueOutsideTheDeclaredWidth(t *testing.T) {
	type widths struct {
		Small    int32  `json:"small"`
		Unsigned uint32 `json:"unsigned"`
		Wide     uint64 `json:"wide"`
	}
	for name, value := range map[string]string{
		// One past int32, one past uint32, one negative where unsigned is
		// declared, and one past uint64. Truncating any of them into the
		// declared field would wrap silently.
		"small":    "2147483648",
		"unsigned": "-1",
		"wide":     "18446744073709551616",
	} {
		t.Run(name, func(t *testing.T) {
			result := Validate(reflect.TypeOf(widths{}), map[string]any{name: value}, WithCoerce())
			if !result.HasErrors() {
				t.Fatalf("%s=%s is outside the declared width and must be refused; data = %v", name, value, result.Data)
			}
			if _, present := result.Data[name]; present {
				t.Errorf("a refused value must stay out of Data, got %v", result.Data[name])
			}
		})
	}
}

// A declared bound is enforced at the declared width, not at the width the
// parser happened to use.
func TestValidateCoerceAppliesDeclaredBoundsToANarrowInteger(t *testing.T) {
	type page struct {
		Limit int32 `json:"limit" validate:"min=1,max=100"`
	}
	if result := Validate(reflect.TypeOf(page{}), map[string]any{"limit": "50"}, WithCoerce()); result.HasErrors() {
		t.Fatalf("50 is within [1,100]: %v", result.Errors)
	}
	result := Validate(reflect.TypeOf(page{}), map[string]any{"limit": "101"}, WithCoerce())
	if !result.HasErrors() {
		t.Fatalf("101 is outside [1,100] and must be refused; data = %v", result.Data)
	}
}

// Coercion is opt-in. Without WithCoerce the value reaches the caller with the
// type it was supplied with, so a transport that already decoded the payload is
// not second-guessed; TestValidateCoerce covers the opted-in half.
func TestValidateWithoutCoerceKeepsTheCallerSuppliedType(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "opt-in-coercion", "without-opt-in-the-caller-supplied-type-survives")
	result := Validate(reflect.TypeOf(UserInput{}), map[string]any{
		"name":  "Alice",
		"email": "a@b.com",
		"age":   "25",
	})

	if result.HasErrors() {
		t.Fatalf("expected no errors, got: %v", result.Errors)
	}
	age, ok := result.Data["age"].(string)
	if !ok {
		t.Fatalf("without WithCoerce the supplied type must survive, got %T (%v)", result.Data["age"], result.Data["age"])
	}
	if age != "25" {
		t.Errorf("expected the supplied \"25\", got %q", age)
	}
}
