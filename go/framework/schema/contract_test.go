// contract_test.go — exported-API contract tests for go.putnami.dev/schema.
// Covers: FieldError.Error(), registerConstraint end-to-end, and pointer-type schema input.
package schema

import (
	"fmt"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// --- FieldError.Error() ---

func TestFieldError_Error(t *testing.T) {
	e := FieldError{Field: "email", Message: "is required"}
	want := "email: is required"
	if got := e.Error(); got != want {
		t.Errorf("FieldError.Error() = %q, want %q", got, want)
	}
}

func TestFieldError_Error_WithConstraint(t *testing.T) {
	// Constraint field is optional metadata; Error() only uses Field and Message.
	e := FieldError{Field: "age", Message: "must be >= 0", Constraint: "min"}
	want := "age: must be >= 0"
	if got := e.Error(); got != want {
		t.Errorf("FieldError.Error() = %q, want %q", got, want)
	}
}

// --- registerConstraint end-to-end ---

// customConstraintInput is a test struct that uses the registered "starts_upper" constraint.
type customConstraintInput struct {
	Code string `json:"code" validate:"starts_upper"`
}

func TestRegisterConstraint_CustomPassAndFail(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "custom-constraints", "a-registered-constraint-passes-and-fails-on-its-own-terms")
	// Register a constraint that requires the first character to be uppercase.
	registerConstraint("starts_upper", func(value any, _ string) error {
		s, ok := value.(string)
		if !ok || len(s) == 0 {
			return fmt.Errorf("must be a non-empty string")
		}
		if s[0] < 'A' || s[0] > 'Z' {
			return fmt.Errorf("must start with an uppercase letter")
		}
		return nil
	})

	t.Run("pass", func(t *testing.T) {
		result := Validate(reflect.TypeOf(customConstraintInput{}), map[string]any{
			"code": "Hello",
		})
		if result.HasErrors() {
			t.Errorf("expected no errors for valid value, got: %v", result.Errors)
		}
		if result.Data["code"] != "Hello" {
			t.Errorf("expected Data[code]='Hello', got %v", result.Data["code"])
		}
	})

	t.Run("fail", func(t *testing.T) {
		result := Validate(reflect.TypeOf(customConstraintInput{}), map[string]any{
			"code": "hello",
		})
		if !result.HasErrors() {
			t.Fatal("expected error for value not starting with uppercase")
		}
		found := false
		for _, e := range result.Errors {
			if e.Field == "code" && e.Constraint == "starts_upper" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected starts_upper error on code field, got: %v", result.Errors)
		}
	})
}

func TestRegisterConstraint_ReplacesExisting(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "custom-constraints", "registration-replaces-a-built-in-of-the-same-name")
	// First version always fails.
	registerConstraint("test_replaceable", func(value any, _ string) error {
		return fmt.Errorf("always fails v1")
	})

	// Second registration replaces the first.
	registerConstraint("test_replaceable", func(value any, _ string) error {
		return nil // always passes
	})

	// Verify via getConstraintFunc that the replacement is in effect.
	fn, ok := getConstraintFunc("test_replaceable")
	if !ok {
		t.Fatal("constraint 'test_replaceable' not found after registration")
	}
	if err := fn("anything", ""); err != nil {
		t.Errorf("replaced constraint should pass, got error: %v", err)
	}
}

func TestRegisterConstraint_EndToEndDispatch(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "custom-constraints", "validation-dispatches-to-the-registered-function")
	// Register a param-aware constraint: value must have at least param characters (as bytes).
	registerConstraint("minstartlen", func(value any, param string) error {
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		min := 0
		if _, err := fmt.Sscanf(param, "%d", &min); err != nil {
			return fmt.Errorf("invalid minstartlen param: %s", param)
		}
		if len(s) < min {
			return fmt.Errorf("must be at least %d bytes", min)
		}
		return nil
	})

	type S struct {
		Label string `json:"label" validate:"minstartlen=5"`
	}

	t.Run("pass", func(t *testing.T) {
		result := Validate(reflect.TypeOf(S{}), map[string]any{"label": "Hello"})
		if result.HasErrors() {
			t.Errorf("expected pass, got: %v", result.Errors)
		}
	})

	t.Run("fail", func(t *testing.T) {
		result := Validate(reflect.TypeOf(S{}), map[string]any{"label": "Hi"})
		if !result.HasErrors() {
			t.Fatal("expected minstartlen error")
		}
		if result.Errors[0].Constraint != "minstartlen" {
			t.Errorf("expected constraint='minstartlen', got %q", result.Errors[0].Constraint)
		}
	})
}

// --- Pointer-type schema input ---

type contractUserInput struct {
	Name  string `json:"name" validate:"required"`
	Email string `json:"email" validate:"required,email"`
}

func TestValidate_PointerTypeParity(t *testing.T) {
	spectest.Proves(t, "go/struct-validation", "no-panic-on-bad-type", "a-pointer-to-struct-reports-the-same-errors-as-the-struct")
	// Validate accepts both *T and T and must produce identical results.
	values := map[string]any{} // empty — triggers required errors

	nonPtr := Validate(reflect.TypeOf(contractUserInput{}), values)
	ptr := Validate(reflect.TypeOf(&contractUserInput{}), values)

	if nonPtr.HasErrors() != ptr.HasErrors() {
		t.Errorf("HasErrors mismatch: nonPtr=%v ptr=%v", nonPtr.HasErrors(), ptr.HasErrors())
	}
	if len(nonPtr.Errors) != len(ptr.Errors) {
		t.Errorf("error count mismatch: nonPtr=%d ptr=%d", len(nonPtr.Errors), len(ptr.Errors))
	}
	// Verify the pointer form does not add or drop fields.
	for i, e := range nonPtr.Errors {
		if i >= len(ptr.Errors) {
			break
		}
		if e.Field != ptr.Errors[i].Field || e.Constraint != ptr.Errors[i].Constraint {
			t.Errorf("error[%d] mismatch: nonPtr=%v ptr=%v", i, e, ptr.Errors[i])
		}
	}
}

func TestValidate_PointerTypePassesValidData(t *testing.T) {
	values := map[string]any{
		"name":  "Alice",
		"email": "alice@example.com",
	}

	nonPtr := Validate(reflect.TypeOf(contractUserInput{}), values)
	ptr := Validate(reflect.TypeOf(&contractUserInput{}), values)

	if nonPtr.HasErrors() {
		t.Errorf("nonPtr: unexpected errors: %v", nonPtr.Errors)
	}
	if ptr.HasErrors() {
		t.Errorf("ptr: unexpected errors: %v", ptr.Errors)
	}
	// Data maps must agree.
	if nonPtr.Data["name"] != ptr.Data["name"] {
		t.Errorf("Data[name] mismatch: nonPtr=%v ptr=%v", nonPtr.Data["name"], ptr.Data["name"])
	}
	if nonPtr.Data["email"] != ptr.Data["email"] {
		t.Errorf("Data[email] mismatch: nonPtr=%v ptr=%v", nonPtr.Data["email"], ptr.Data["email"])
	}
}
