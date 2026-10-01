package output

import (
	"testing"
)

func TestSetColorsEnabled(t *testing.T) {
	// Save initial state
	initial := ColorsEnabled()
	defer SetColorsEnabled(initial)

	SetColorsEnabled(false)
	if ColorsEnabled() {
		t.Error("ColorsEnabled = true after SetColorsEnabled(false)")
	}

	SetColorsEnabled(true)
	if !ColorsEnabled() {
		t.Error("ColorsEnabled = false after SetColorsEnabled(true)")
	}
}

func TestColorize_Enabled(t *testing.T) {
	initial := ColorsEnabled()
	defer SetColorsEnabled(initial)

	SetColorsEnabled(true)
	got := colorize("hello", Red)
	want := Red + "hello" + Reset
	if got != want {
		t.Errorf("colorize = %q, want %q", got, want)
	}
}

func TestColorize_Disabled(t *testing.T) {
	initial := ColorsEnabled()
	defer SetColorsEnabled(initial)

	SetColorsEnabled(false)
	got := colorize("hello", Red)
	if got != "hello" {
		t.Errorf("colorize disabled = %q, want %q", got, "hello")
	}
}

func TestColorize_EmptyColor(t *testing.T) {
	initial := ColorsEnabled()
	defer SetColorsEnabled(initial)

	SetColorsEnabled(true)
	got := colorize("hello", "")
	if got != "hello" {
		t.Errorf("colorize empty color = %q, want %q", got, "hello")
	}
}

func TestColorConstants(t *testing.T) {
	// Verify ANSI constants are non-empty
	constants := []struct {
		name  string
		value string
	}{
		{"Reset", Reset},
		{"Bold", Bold},
		{"Dim", Dim},
		{"Red", Red},
		{"Green", Green},
		{"Yellow", Yellow},
		{"Blue", Blue},
		{"Cyan", Cyan},
		{"Gray", Gray},
		{"ClearLine", ClearLine},
		{"HideCursor", HideCursor},
		{"ShowCursor", ShowCursor},
	}

	for _, c := range constants {
		if c.value == "" {
			t.Errorf("ANSI constant %s is empty", c.name)
		}
	}
}
