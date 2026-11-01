package fmtx

import "testing"

func TestLabel(t *testing.T) {
	if got := Label("x"); got != "[x]" {
		t.Errorf("Label(x) = %q, want [x]", got)
	}
}
