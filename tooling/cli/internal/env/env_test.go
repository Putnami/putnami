package env

import (
	"testing"
)

func TestString(t *testing.T) {
	t.Setenv("PUTNAMI_TEST_VAR", "hello")

	got := String("test_var")
	if got != "hello" {
		t.Errorf("String = %q, want %q", got, "hello")
	}
}

func TestString_HyphenToUnderscore(t *testing.T) {
	t.Setenv("PUTNAMI_MY_FLAG", "value")

	got := String("my-flag")
	if got != "value" {
		t.Errorf("String = %q, want %q", got, "value")
	}
}

func TestString_CaseInsensitive(t *testing.T) {
	t.Setenv("PUTNAMI_LOWER", "val")

	got := String("lower")
	if got != "val" {
		t.Errorf("String = %q, want %q", got, "val")
	}
}

func TestString_Missing(t *testing.T) {
	got := String("nonexistent_xyz")
	if got != "" {
		t.Errorf("String missing = %q, want empty", got)
	}
}

func TestBool_True(t *testing.T) {
	truthy := []string{"true", "1", "yes", "TRUE", "Yes"}
	for _, v := range truthy {
		t.Setenv("PUTNAMI_BOOL_TEST", v)
		got, ok := Bool("bool_test")
		if !ok {
			t.Errorf("Bool(%q) ok = false, want true", v)
		}
		if !got {
			t.Errorf("Bool(%q) = false, want true", v)
		}
	}
}

func TestBool_False(t *testing.T) {
	falsy := []string{"false", "0", "no", "FALSE", "No"}
	for _, v := range falsy {
		t.Setenv("PUTNAMI_BOOL_TEST", v)
		got, ok := Bool("bool_test")
		if !ok {
			t.Errorf("Bool(%q) ok = false, want true", v)
		}
		if got {
			t.Errorf("Bool(%q) = true, want false", v)
		}
	}
}

func TestBool_Missing(t *testing.T) {
	if _, ok := Bool("missing_bool"); ok {
		t.Error("Bool missing should return ok=false")
	}
}

func TestBool_Invalid(t *testing.T) {
	t.Setenv("PUTNAMI_BAD_BOOL", "maybe")

	if _, ok := Bool("bad_bool"); ok {
		t.Error("Bool invalid value should return ok=false")
	}
}

// TestPrefix pins the one spelling rule the package exists to hold: every
// override is PUTNAMI_ + the flag name, uppercased with dashes as underscores.
func TestPrefix(t *testing.T) {
	if Prefix != "PUTNAMI_" {
		t.Errorf("Prefix = %q, want %q — the env vocabulary is user-facing contract", Prefix, "PUTNAMI_")
	}
}
