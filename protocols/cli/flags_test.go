package cli

import (
	"errors"
	"testing"
)

func TestReservedGlobalFlagsReturnsIndependentCopy(t *testing.T) {
	a := ReservedGlobalFlags()
	if len(a) == 0 {
		t.Fatal("ReservedGlobalFlags returned an empty set")
	}
	a[0].Name = "mutated"
	if b := ReservedGlobalFlags(); b[0].Name == "mutated" {
		t.Error("mutating the returned slice changed the canonical registry")
	}
}

func TestReservedGlobalFlagsHygiene(t *testing.T) {
	seenLong := map[string]bool{}
	seenShort := map[string]bool{}
	for _, f := range ReservedGlobalFlags() {
		if f.Name == "" {
			t.Error("reserved flag with empty Name")
		}
		if f.Usage == "" {
			t.Errorf("reserved flag --%s has empty Usage", f.Name)
		}
		if seenLong[f.Name] {
			t.Errorf("duplicate reserved long name %q", f.Name)
		}
		seenLong[f.Name] = true
		if f.Short != "" {
			if seenShort[f.Short] {
				t.Errorf("duplicate reserved short name %q (on --%s)", f.Short, f.Name)
			}
			seenShort[f.Short] = true
		}
	}
}

func TestIsReservedGlobalFlag(t *testing.T) {
	cases := []struct {
		token string
		want  bool
	}{
		{"--output", true},
		{"--output=json", true},
		{"output", true},
		{"--json", true},
		{"--help", true},
		{"-h", true},
		{"--version", true},
		{"-V", true},
		{"--verbose", true},
		{"-v", true},
		{"--debug", true},
		{"-d", true},
		{"--quiet", true},
		{"--color", true},
		{"--no-color", true},
		{"no-color", true},
		// framework-only orchestration flags must NOT be reserved globals
		{"--projects", false},
		{"--impacted", false},
		{"--no-cache", false},
		{"--max-parallel", false},
		{"--baseline", false},
		// non-flags / edge cases
		{"", false},
		{"--", false},
		{"-", false},
		{"--unknown", false},
		{"-x", false},
	}
	for _, c := range cases {
		if got := IsReservedGlobalFlag(c.token); got != c.want {
			t.Errorf("IsReservedGlobalFlag(%q) = %v, want %v", c.token, got, c.want)
		}
	}
}

func TestLookupReservedGlobalFlag(t *testing.T) {
	f, ok := LookupReservedGlobalFlag("--output=text")
	if !ok {
		t.Fatal("expected --output to be reserved")
	}
	if f.Name != "output" || !f.TakesValue {
		t.Errorf("LookupReservedGlobalFlag(--output) = %+v, want output/TakesValue", f)
	}

	f, ok = LookupReservedGlobalFlag("--no-color")
	if !ok || f.Name != "color" || !f.Negatable {
		t.Errorf("LookupReservedGlobalFlag(--no-color) = %+v/%v, want color/Negatable", f, ok)
	}

	if _, ok := LookupReservedGlobalFlag("--projects"); ok {
		t.Error("--projects must not resolve to a reserved global flag")
	}
}

func TestValidateNoReservedShadow(t *testing.T) {
	if err := ValidateNoReservedShadow("target", "race", "mytag"); err != nil {
		t.Errorf("expected no collision for non-reserved names, got %v", err)
	}

	err := ValidateNoReservedShadow("target", "output")
	if err == nil {
		t.Fatal("expected a collision error for --output")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("shadow error is not ErrUsage-classified: %v", err)
	}

	if err := ValidateNoReservedShadow("json"); !errors.Is(err, ErrUsage) {
		t.Errorf("expected ErrUsage for --json shadow, got %v", err)
	}
}
