package workspace

import (
	"encoding/json"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestValidateWorkspaceConfig_Deterministic verifies that ValidateWorkspaceConfig
// produces diagnostics in a consistent order for maps with multiple entries.
func TestValidateWorkspaceConfig_Deterministic(t *testing.T) {
	c := &Config{
		Name: "ws",
		Hooks: &HooksConfig{
			Commands: map[string]*HookPhaseConfig{
				"zebra":   nil,
				"alpha":   nil,
				"middle":  nil,
				"build":   nil,
				"publish": nil,
			},
		},
	}

	canonical := ValidateWorkspaceConfig(c)
	if len(canonical) < 5 {
		t.Fatalf("expected at least 5 diagnostics, got %d", len(canonical))
	}

	for i := 0; i < 100; i++ {
		got := ValidateWorkspaceConfig(c)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: ValidateWorkspaceConfig produced non-deterministic diagnostics.\ncanonical: %v\ngot:       %v", i, canonical, got)
		}
	}
}

// TestValidateProjectConfig_Deterministic verifies that ValidateProjectConfig
// produces diagnostics in a consistent order for jobs with multiple entries.
// Go randomizes map iteration, so the sort in the validator is the only thing
// standing between a five-job config and five different diagnostic orderings.
func TestValidateProjectConfig_Deterministic(t *testing.T) {
	c := &ProjectConfig{
		Name: "p",
		Jobs: map[string]json.RawMessage{
			"zebra":  json.RawMessage(`{}`),
			"alpha":  json.RawMessage(`{}`),
			"middle": json.RawMessage(`{}`),
			"build":  json.RawMessage(`{"kind":"command"}`),
			"test":   json.RawMessage(`{"command":"go"}`),
		},
	}

	canonical := ValidateProjectConfig(c)
	if len(canonical) != 5 {
		t.Fatalf("got %d diagnostics, want one per declared job: %v", len(canonical), canonical)
	}
	if diag.HasErrors(canonical) {
		t.Fatalf("an ignored surface must warn, not reject: %v", diag.Errors(canonical))
	}

	wantFields := []string{"jobs.alpha", "jobs.build", "jobs.middle", "jobs.test", "jobs.zebra"}
	for i, want := range wantFields {
		if canonical[i].Field != want {
			t.Fatalf("diagnostic %d field = %q, want %q (sorted by job name): %v", i, canonical[i].Field, want, canonical)
		}
	}

	for i := 0; i < 100; i++ {
		got := ValidateProjectConfig(c)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: ValidateProjectConfig produced non-deterministic diagnostics.\ncanonical: %v\ngot:       %v", i, canonical, got)
		}
	}
}

// TestExtensionsConfig_Names_Deterministic verifies that Names() returns
// sorted extension names regardless of map iteration order.
func TestExtensionsConfig_Names_Deterministic(t *testing.T) {
	cfg := ExtensionsConfig{
		List: map[string]string{
			"@putnami/go":         "^1.0",
			"@putnami/typescript": "^2.0",
			"@putnami/python":     "^1.0",
			"@putnami/ci":         "",
		},
	}

	canonical := cfg.Names()
	for i := 0; i < 100; i++ {
		got := cfg.Names()
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: Names() produced non-deterministic output.\ncanonical: %v\ngot:       %v", i, canonical, got)
		}
	}

	// Verify sorted.
	for i := 1; i < len(canonical); i++ {
		if canonical[i] < canonical[i-1] {
			t.Errorf("Names not sorted: %v", canonical)
			break
		}
	}
}

// TestScopeConfig_ResolveNamePattern_Deterministic verifies deterministic
// channel iteration in ResolveNamePattern.
func TestScopeConfig_ResolveNamePattern_Deterministic(t *testing.T) {
	sc := &ScopeConfig{
		PublishConfig: map[string]map[string]any{
			"npm":    {"namePattern": "@putnami/{name}"},
			"docker": {"namePattern": "putnami/{name}"},
			"cargo":  {"namePattern": "putnami-{name}"},
		},
	}

	canonical := sc.ResolveNamePattern("app")
	for i := 0; i < 100; i++ {
		got := sc.ResolveNamePattern("app")
		if got != canonical {
			t.Fatalf("iteration %d: ResolveNamePattern produced non-deterministic output.\ncanonical: %q\ngot:       %q", i, canonical, got)
		}
	}
}
