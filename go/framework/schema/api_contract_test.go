// Package schema — additional exported-API contract tests, covering the surface
// contract_test.go does not reach.
// contract_test.go already covers FieldError.Error(), registerConstraint basics,
// and pointer-type parity. This file adds the remaining gap cases.
package schema

import (
	"fmt"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// ---------------------------------------------------------------------------
// FieldError.Error() — additional format assertions
// ---------------------------------------------------------------------------

func TestFieldError_Error_PrefixedField(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "error-labels", "a-prefixed-field-error-renders-its-full-name")
	// When WithLabel is used the Field already contains the prefix; the Error()
	// method just joins Field and Message without further transformation.
	e := FieldError{Field: "body.name", Message: "is required", Constraint: "required"}
	got := e.Error()
	want := "body.name: is required"
	if got != want {
		t.Errorf("FieldError.Error() = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// registerConstraint — replacement is used in end-to-end Validate dispatch
// ---------------------------------------------------------------------------

func TestRegisterConstraint_ReplacementEndToEnd(t *testing.T) {
	// Re-registering a constraint must affect subsequent Validate calls.
	// Use a unique type so the schema cache does not hide the replacement.
	type replacedEndToEndInput struct {
		Val string `json:"val" validate:"always_fail_replaced_v2"`
	}

	registerConstraint("always_fail_replaced_v2", func(value any, _ string) error {
		return fmt.Errorf("old failure")
	})
	registerConstraint("always_fail_replaced_v2", func(value any, _ string) error {
		return fmt.Errorf("new failure")
	})

	result := Validate(reflect.TypeOf(replacedEndToEndInput{}), map[string]any{"val": "x"})
	if !result.HasErrors() {
		t.Fatal("expected error from replaced constraint")
	}
	for _, e := range result.Errors {
		if e.Constraint == "always_fail_replaced_v2" && e.Message != "new failure" {
			t.Errorf("expected message 'new failure' from replaced constraint, got %q", e.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// registerConstraint replaces — verify getConstraintFunc returns new version
// ---------------------------------------------------------------------------

func TestRegisterConstraint_GetReturnsReplacement(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "custom-constraints", "the-replacement-is-what-lookup-returns")
	registerConstraint("replace_get_test", func(value any, _ string) error {
		return fmt.Errorf("first version")
	})
	registerConstraint("replace_get_test", func(value any, _ string) error {
		return fmt.Errorf("second version")
	})

	fn, ok := getConstraintFunc("replace_get_test")
	if !ok {
		t.Fatal("constraint should be registered after replacement")
	}
	err := fn("anything", "")
	if err == nil {
		t.Fatal("expected error from replacement constraint")
	}
	if err.Error() != "second version" {
		t.Errorf("expected 'second version', got %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Pointer-type schema input — additional pass-with-data assertion
// ---------------------------------------------------------------------------

type ptrAdditionalInput struct {
	Score int `json:"score" validate:"min=0,max=100"`
}

func TestValidate_PointerType_DataParity(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "no-panic-on-bad-type", "a-pointer-to-struct-produces-the-same-data-as-the-struct")
	values := map[string]any{"score": 50}

	nonPtr := Validate(reflect.TypeOf(ptrAdditionalInput{}), values)
	ptr := Validate(reflect.TypeOf(&ptrAdditionalInput{}), values)

	if nonPtr.HasErrors() {
		t.Fatalf("non-ptr: unexpected errors: %v", nonPtr.Errors)
	}
	if ptr.HasErrors() {
		t.Fatalf("ptr: unexpected errors: %v", ptr.Errors)
	}
	if nonPtr.Data["score"] != ptr.Data["score"] {
		t.Errorf("Data[score] mismatch: nonPtr=%v ptr=%v", nonPtr.Data["score"], ptr.Data["score"])
	}
}
