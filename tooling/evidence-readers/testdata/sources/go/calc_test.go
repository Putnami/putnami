package calc

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2, 3) = %d, want 5", got)
	}
}

func TestSign(t *testing.T) {
	t.Run("non-negative input", func(t *testing.T) {
		if got := Sign(4); got != "[positive]" {
			t.Errorf("Sign(4) = %q, want [positive]", got)
		}
	})
}

func TestAddWrongOnPurpose(t *testing.T) {
	if got := Add(2, 2); got != 5 {
		t.Errorf("two plus two should be five: Add(2, 2) = %d, want 5", got)
	}
}

func TestSkipped(t *testing.T) {
	t.Skip("not ready")
}
