// Package schema — edge-case characterization tests.
// These tests pin the ACTUAL CURRENT behavior of the code. Where the behavior
// is surprising (noted inline), a separate issue tracks the defect; this file
// must not fix it — only ensure it cannot silently change.
package schema

import (
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// ---------------------------------------------------------------------------
// maxlen — ASCII and multi-byte (byte-counting) behavior
// ---------------------------------------------------------------------------

type maxlenInput struct {
	Name string `json:"name" validate:"maxlen=5"`
}

func TestMaxLen_ASCII(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "length-in-runes", "maxlen-holds-at-its-ascii-boundary")
	tests := []struct {
		value   string
		wantErr bool
	}{
		{"hello", false},  // exactly 5 bytes / chars — allowed
		{"hi", false},     // 2 chars — well under limit
		{"toolong", true}, // 7 bytes / chars — must fail
		{"123456", true},  // 6 bytes — must fail
	}
	schemaType := reflect.TypeOf(maxlenInput{})
	for _, tc := range tests {
		result := Validate(schemaType, map[string]any{"name": tc.value})
		if result.HasErrors() != tc.wantErr {
			t.Errorf("maxlen=5, value=%q: HasErrors=%v, want %v (errors: %v)",
				tc.value, result.HasErrors(), tc.wantErr, result.Errors)
		}
	}
}

func TestMaxLen_MultiByte(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "length-in-runes", "maxlen-counts-runes-not-bytes")
	// maxlen/minlen count RUNES, not bytes: the constraint
	// message says "characters", so a multi-byte UTF-8 value must be measured
	// the way a user counts it. These cases would have behaved differently under
	// the old byte-counting implementation and pin the corrected semantics.
	schemaType := reflect.TypeOf(maxlenInput{}) // maxlen=5

	// "héllo": 5 runes, 6 bytes (é = 2 bytes). Under byte-counting this WRONGLY
	// failed maxlen=5; under rune-counting it passes (5 ≤ 5).
	if result := Validate(schemaType, map[string]any{"name": "héllo"}); result.HasErrors() {
		t.Errorf("maxlen=5, 'héllo' (5 runes, 6 bytes): expected PASS, got errors: %v", result.Errors)
	}

	// "café!": 5 runes, 6 bytes. Same flip — must now PASS (5 ≤ 5).
	if result := Validate(schemaType, map[string]any{"name": "café!"}); result.HasErrors() {
		t.Errorf("maxlen=5, 'café!' (5 runes, 6 bytes): expected PASS, got errors: %v", result.Errors)
	}

	// "caféés": 6 runes — must FAIL maxlen=5 regardless of byte length.
	if result := Validate(schemaType, map[string]any{"name": "caféés"}); !result.HasErrors() {
		t.Error("maxlen=5, 'caféés' (6 runes): expected FAIL (rune-count > limit)")
	}

	// CJK: "日本語" is 3 runes / 9 bytes — passes maxlen=5 by runes, would have
	// failed by bytes.
	if result := Validate(schemaType, map[string]any{"name": "日本語"}); result.HasErrors() {
		t.Errorf("maxlen=5, '日本語' (3 runes, 9 bytes): expected PASS, got errors: %v", result.Errors)
	}
}

type minlenInput struct {
	Name string `json:"name" validate:"minlen=3"`
}

func TestMinLen_MultiByte(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "length-in-runes", "minlen-counts-runes-not-bytes")
	// minlen counts runes too: "日本" is 2 runes / 6 bytes — it
	// must FAIL minlen=3 even though its byte length (6) exceeds 3.
	schemaType := reflect.TypeOf(minlenInput{}) // minlen=3
	if result := Validate(schemaType, map[string]any{"name": "日本"}); !result.HasErrors() {
		t.Error("minlen=3, '日本' (2 runes, 6 bytes): expected FAIL (rune-count < limit)")
	}
	// "日本語" is 3 runes — passes minlen=3.
	if result := Validate(schemaType, map[string]any{"name": "日本語"}); result.HasErrors() {
		t.Errorf("minlen=3, '日本語' (3 runes): expected PASS, got errors: %v", result.Errors)
	}
}

// ---------------------------------------------------------------------------
// url constraint — malformed input and currently-accepted schemes
// ---------------------------------------------------------------------------

type urlInput struct {
	Link string `json:"link" validate:"url"`
}

func TestURL_Malformed_Rejected(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "url-scheme-allowlist", "a-malformed-url-is-rejected")
	malformed := []string{
		"not-a-url",
		"://missing-scheme",
		"",
		"   ",
		"http//missing-colon.com",
	}
	schemaType := reflect.TypeOf(urlInput{})
	for _, u := range malformed {
		result := Validate(schemaType, map[string]any{"link": u})
		if !result.HasErrors() {
			t.Errorf("malformed URL %q should be rejected but was accepted", u)
		}
	}
}

func TestURL_HTTP_Accepted(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "url-scheme-allowlist", "an-http-url-is-accepted")
	valid := []string{
		"http://example.com",
		"https://example.com",
		"https://example.com/path?query=1#anchor",
	}
	schemaType := reflect.TypeOf(urlInput{})
	for _, u := range valid {
		result := Validate(schemaType, map[string]any{"link": u})
		if result.HasErrors() {
			t.Errorf("valid URL %q should be accepted: %v", u, result.Errors)
		}
	}
}

func TestURL_NonHTTP_Rejected(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "url-scheme-allowlist", "a-non-http-scheme-is-rejected")
	// These schemes are rejected by the scheme allowlist (http/https only).
	// Separate from TestValidateURL_SchemeAllowlist in schema_test.go —
	// this set emphasizes the rejection path for clarity.
	rejected := []string{
		"ftp://files.example.com/file",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"gopher://host/1",
	}
	schemaType := reflect.TypeOf(urlInput{})
	for _, u := range rejected {
		result := Validate(schemaType, map[string]any{"link": u})
		if !result.HasErrors() {
			t.Errorf("URL with non-http(s) scheme %q should be rejected", u)
		}
	}
}

// ---------------------------------------------------------------------------
// coerceValue — failure path: unparseable string passes through unchanged
// ---------------------------------------------------------------------------

func TestCoerceValue_IntFailure_ReturnsOriginalString(t *testing.T) {
	// CHARACTERIZATION: coerceValue("abc", int) returns the original string "abc"
	// unchanged (no error is returned; the caller gets back what was passed in).
	// This means if WithCoerce() is enabled and the string cannot be parsed as
	// the target type, the original string value reaches the constraints unchanged.
	result, err := coerceValue("abc", reflect.TypeOf(0))
	if err != nil {
		t.Fatalf("a non-numeric value is left to the declared constraints, not refused here: %v", err)
	}
	s, ok := result.(string)
	if !ok {
		t.Fatalf("expected string passthrough, got %T (%v)", result, result)
	}
	if s != "abc" {
		t.Errorf("expected original string 'abc', got %q", s)
	}
}

func TestCoerceValue_IntSuccess(t *testing.T) {
	result, err := coerceValue("42", reflect.TypeOf(0))
	if err != nil {
		t.Fatalf("coerce 42: %v", err)
	}
	n, ok := result.(int)
	if !ok {
		t.Fatalf("expected int after coercion, got %T (%v)", result, result)
	}
	if n != 42 {
		t.Errorf("expected 42, got %d", n)
	}
}

func TestCoerceValue_Float64Success(t *testing.T) {
	result, err := coerceValue("3.14", reflect.TypeOf(0.0))
	if err != nil {
		t.Fatalf("coerce 3.14: %v", err)
	}
	f, ok := result.(float64)
	if !ok {
		t.Fatalf("expected float64 after coercion, got %T (%v)", result, result)
	}
	if f != 3.14 {
		t.Errorf("expected 3.14, got %f", f)
	}
}

func TestCoerceValue_Float64Failure_ReturnsOriginalString(t *testing.T) {
	result, err := coerceValue("not-a-float", reflect.TypeOf(0.0))
	if err != nil {
		t.Fatalf("a non-numeric value is left to the declared constraints, not refused here: %v", err)
	}
	s, ok := result.(string)
	if !ok {
		t.Fatalf("expected string passthrough, got %T (%v)", result, result)
	}
	if s != "not-a-float" {
		t.Errorf("expected original string, got %q", s)
	}
}

func TestCoerceValue_BoolSuccess(t *testing.T) {
	result, err := coerceValue("true", reflect.TypeOf(false))
	if err != nil {
		t.Fatalf("coerce true: %v", err)
	}
	b, ok := result.(bool)
	if !ok {
		t.Fatalf("expected bool after coercion, got %T (%v)", result, result)
	}
	if !b {
		t.Errorf("expected true, got false")
	}
}

func TestCoerceValue_NonStringPassthrough(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "opt-in-coercion", "coercion-passes-a-non-string-through-untouched")
	// Non-string inputs are returned as-is regardless of target type.
	result, err := coerceValue(42, reflect.TypeOf(""))
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	n, ok := result.(int)
	if !ok {
		t.Fatalf("expected int passthrough, got %T (%v)", result, result)
	}
	if n != 42 {
		t.Errorf("expected 42, got %v", n)
	}
}

// ---------------------------------------------------------------------------
// float64 numeric inputs — JSON numbers arrive as float64
// ---------------------------------------------------------------------------

type numericInput struct {
	Score float64 `json:"score" validate:"min=0,max=100"`
}

func TestFloat64_MinMax_Pass(t *testing.T) {
	schemaType := reflect.TypeOf(numericInput{})
	result := Validate(schemaType, map[string]any{"score": float64(75.5)})
	if result.HasErrors() {
		t.Fatalf("float64 75.5 with min=0,max=100: expected pass, got: %v", result.Errors)
	}
	// float64 is stored in Result.Data as-is.
	v, ok := result.Data["score"].(float64)
	if !ok {
		t.Fatalf("expected float64 in Data, got %T", result.Data["score"])
	}
	if v != 75.5 {
		t.Errorf("expected 75.5, got %v", v)
	}
}

func TestFloat64_BelowMin_Fails(t *testing.T) {
	schemaType := reflect.TypeOf(numericInput{})
	result := Validate(schemaType, map[string]any{"score": float64(-1.0)})
	if !result.HasErrors() {
		t.Fatal("float64 -1.0 with min=0: expected fail")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "score" && e.Constraint == "min" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected min error on score, got: %v", result.Errors)
	}
}

func TestFloat64_AboveMax_Fails(t *testing.T) {
	schemaType := reflect.TypeOf(numericInput{})
	result := Validate(schemaType, map[string]any{"score": float64(101.0)})
	if !result.HasErrors() {
		t.Fatal("float64 101.0 with max=100: expected fail")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "score" && e.Constraint == "max" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected max error on score, got: %v", result.Errors)
	}
}

// ---------------------------------------------------------------------------
// toFloat — all branches
// ---------------------------------------------------------------------------

func TestToFloat_Int(t *testing.T) {
	f, err := toFloat(int(5))
	if err != nil || f != 5.0 {
		t.Errorf("toFloat(int(5)) = %v, %v; want 5.0, nil", f, err)
	}
}

func TestToFloat_Int64(t *testing.T) {
	f, err := toFloat(int64(10))
	if err != nil || f != 10.0 {
		t.Errorf("toFloat(int64(10)) = %v, %v; want 10.0, nil", f, err)
	}
}

func TestToFloat_Float64(t *testing.T) {
	f, err := toFloat(float64(2.71))
	if err != nil || f != 2.71 {
		t.Errorf("toFloat(float64(2.71)) = %v, %v; want 2.71, nil", f, err)
	}
}

func TestToFloat_Float32(t *testing.T) {
	f, err := toFloat(float32(1.0))
	if err != nil {
		t.Errorf("toFloat(float32(1.0)) unexpected error: %v", err)
	}
	// float32→float64 may not be exact; check within tolerance.
	if f < 0.99 || f > 1.01 {
		t.Errorf("toFloat(float32(1.0)) = %v, expected ~1.0", f)
	}
}

func TestToFloat_String_Valid(t *testing.T) {
	f, err := toFloat("42.5")
	if err != nil || f != 42.5 {
		t.Errorf("toFloat(\"42.5\") = %v, %v; want 42.5, nil", f, err)
	}
}

func TestToFloat_String_Invalid(t *testing.T) {
	_, err := toFloat("not-a-number")
	if err == nil {
		t.Error("toFloat(\"not-a-number\") expected error, got nil")
	}
}

func TestToFloat_UnsupportedType_Error(t *testing.T) {
	// An unsupported type (e.g. uint) returns an error.
	_, err := toFloat(uint(7))
	if err == nil {
		t.Error("toFloat(uint(7)) expected error for unsupported type, got nil")
	}
}

// ---------------------------------------------------------------------------
// coerceDefault — all branches
// ---------------------------------------------------------------------------

func TestCoerceDefault_String(t *testing.T) {
	r := coerceDefault("hello", reflect.TypeOf(""))
	if r != "hello" {
		t.Errorf("expected 'hello', got %v", r)
	}
}

func TestCoerceDefault_Int_Valid(t *testing.T) {
	r := coerceDefault("42", reflect.TypeOf(0))
	if r != 42 {
		t.Errorf("expected 42, got %v (%T)", r, r)
	}
}

func TestCoerceDefault_Int_Invalid(t *testing.T) {
	// Unparseable int default falls back to 0.
	r := coerceDefault("bad", reflect.TypeOf(0))
	if r != 0 {
		t.Errorf("expected 0 for bad int default, got %v (%T)", r, r)
	}
}

func TestCoerceDefault_Float64_Valid(t *testing.T) {
	r := coerceDefault("3.14", reflect.TypeOf(0.0))
	if r != 3.14 {
		t.Errorf("expected 3.14, got %v", r)
	}
}

func TestCoerceDefault_Float64_Invalid(t *testing.T) {
	// Unparseable float default falls back to 0.0.
	r := coerceDefault("bad", reflect.TypeOf(0.0))
	if r != 0.0 {
		t.Errorf("expected 0.0 for bad float default, got %v (%T)", r, r)
	}
}

func TestCoerceDefault_Bool_Valid(t *testing.T) {
	r := coerceDefault("true", reflect.TypeOf(false))
	if r != true {
		t.Errorf("expected true, got %v", r)
	}
}

func TestCoerceDefault_Bool_Invalid(t *testing.T) {
	// Unparseable bool default falls back to false.
	r := coerceDefault("bad", reflect.TypeOf(false))
	if r != false {
		t.Errorf("expected false for bad bool default, got %v (%T)", r, r)
	}
}

func TestCoerceDefault_UnknownType_ReturnsDefaultStr(t *testing.T) {
	// Unknown target type (e.g. a slice): falls back to returning the raw string.
	type mySlice []string
	r := coerceDefault("fallback", reflect.TypeOf(mySlice{}))
	s, ok := r.(string)
	if !ok || s != "fallback" {
		t.Errorf("expected string 'fallback' for unknown type, got %v (%T)", r, r)
	}
}

// ---------------------------------------------------------------------------
// invalid constraint params — invalid min/max/minlen/maxlen param values
// ---------------------------------------------------------------------------

type invalidMinInput struct {
	V int `json:"v" validate:"min=abc"`
}

type invalidMaxInput struct {
	V int `json:"v" validate:"max=abc"`
}

type invalidMinLenInput struct {
	V string `json:"v" validate:"minlen=abc"`
}

type invalidMaxLenInput struct {
	V string `json:"v" validate:"maxlen=abc"`
}

func TestInvalidConstraintParam_Min(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "declaration-faults", "a-non-numeric-constraint-parameter-is-a-schema-error")
	result := Validate(reflect.TypeOf(invalidMinInput{}), map[string]any{"v": 5})
	if !result.HasErrors() {
		t.Fatal("min=abc: expected error for invalid min param")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "v" && e.Constraint == "min" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected min error on v, got: %v", result.Errors)
	}
}

func TestInvalidConstraintParam_Max(t *testing.T) {
	result := Validate(reflect.TypeOf(invalidMaxInput{}), map[string]any{"v": 5})
	if !result.HasErrors() {
		t.Fatal("max=abc: expected error for invalid max param")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "v" && e.Constraint == "max" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected max error on v, got: %v", result.Errors)
	}
}

func TestInvalidConstraintParam_MinLen(t *testing.T) {
	result := Validate(reflect.TypeOf(invalidMinLenInput{}), map[string]any{"v": "hello"})
	if !result.HasErrors() {
		t.Fatal("minlen=abc: expected error for invalid minlen param")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "v" && e.Constraint == "minlen" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected minlen error on v, got: %v", result.Errors)
	}
}

func TestInvalidConstraintParam_MaxLen(t *testing.T) {
	result := Validate(reflect.TypeOf(invalidMaxLenInput{}), map[string]any{"v": "hello"})
	if !result.HasErrors() {
		t.Fatal("maxlen=abc: expected error for invalid maxlen param")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "v" && e.Constraint == "maxlen" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected maxlen error on v, got: %v", result.Errors)
	}
}

// ---------------------------------------------------------------------------
// unknown constraint — schema-definition error reported against the field
// ---------------------------------------------------------------------------

type unknownConstraintInput struct {
	V string `json:"v" validate:"completely_unknown_xyz"`
}

func TestUnknownConstraint_IsSchemaError(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "declaration-faults", "an-unknown-constraint-name-is-a-schema-error")
	result := Validate(reflect.TypeOf(unknownConstraintInput{}), map[string]any{"v": "hello"})
	if !result.HasErrors() {
		t.Fatal("unknown constraint: expected schema-definition error")
	}
	found := false
	for _, e := range result.Errors {
		if e.Field == "v" && e.Constraint == "completely_unknown_xyz" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unknown-constraint error on field v, got: %v", result.Errors)
	}
}

func TestUnknownConstraint_AbsentField(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "declaration-faults", "a-declaration-fault-is-reported-even-when-the-field-is-absent")
	// Schema fault is reported even when the field is absent from the input.
	result := Validate(reflect.TypeOf(unknownConstraintInput{}), map[string]any{})
	if !result.HasErrors() {
		t.Fatal("unknown constraint on absent field: expected schema-definition error")
	}
}

// ---------------------------------------------------------------------------
// uncompilable pattern — already tested in schema_test.go; also cover via
// constraintDefinitionError directly for unit isolation
// ---------------------------------------------------------------------------

func TestConstraintDefinitionError_BadPattern(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "declaration-faults", "an-uncompilable-pattern-is-a-schema-error")
	c := constraint{Name: "pattern", Param: "[unclosed"}
	// parseConstraints would set parseErr; simulate by pre-setting it.
	// Use parseConstraints to get the properly constructed constraint.
	cs := parseConstraints("pattern=[unclosed")
	if len(cs) == 0 {
		t.Fatal("expected one constraint")
	}
	defErr := constraintDefinitionError(cs[0])
	if defErr == nil {
		t.Error("expected definition error for uncompilable pattern, got nil")
	}
	_ = c // c constructed manually above to show the field; actual test uses parsed form
}

func TestConstraintDefinitionError_EmptyPattern(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "declaration-faults", "an-empty-pattern-is-a-schema-error")
	// pattern with no param is a schema definition error.
	cs := parseConstraints("pattern")
	if len(cs) == 0 {
		t.Fatal("expected one constraint")
	}
	defErr := constraintDefinitionError(cs[0])
	if defErr == nil {
		t.Error("expected definition error for pattern with no regex param, got nil")
	}
}

func TestConstraintDefinitionError_ValidConstraint(t *testing.T) {
	cs := parseConstraints("email")
	if len(cs) == 0 {
		t.Fatal("expected one constraint")
	}
	defErr := constraintDefinitionError(cs[0])
	if defErr != nil {
		t.Errorf("expected no definition error for 'email', got: %v", defErr)
	}
}

func TestConstraintDefinitionError_Required(t *testing.T) {
	cs := parseConstraints("required")
	if len(cs) == 0 {
		t.Fatal("expected one constraint")
	}
	defErr := constraintDefinitionError(cs[0])
	if defErr != nil {
		t.Errorf("expected no definition error for 'required', got: %v", defErr)
	}
}
