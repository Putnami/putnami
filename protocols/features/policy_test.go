package features

import (
	"strings"
	"testing"
)

func sddOptions(verification any) map[string]map[string]any {
	return map[string]map[string]any{"sdd": {"verification": verification}}
}

// TestResolveVerificationModeCoversEveryParentChildCombination is the nine-way
// matrix the epic's test plan names: workspace ∈ {enforce, report, off} ×
// project ∈ {absent, strengthen, weaken}. A project override may move the
// policy in either direction, because it is a committed, reviewable exception.
func TestResolveVerificationModeCoversEveryParentChildCombination(t *testing.T) {
	modes := []VerificationMode{VerificationModeEnforce, VerificationModeReport, VerificationModeOff}
	for _, workspace := range modes {
		for _, project := range modes {
			got, source, err := ResolveVerificationMode(VerificationDomainSpecs,
				"workspace", sddOptions(map[string]any{"specs": string(workspace)}),
				"/p", sddOptions(map[string]any{"specs": string(project)}))
			if err != nil {
				t.Fatalf("workspace %s project %s: %v", workspace, project, err)
			}
			if got != project || source != VerificationModeSourceProject {
				t.Errorf("workspace %s project %s resolved (%s, %s), want the project override", workspace, project, got, source)
			}
		}
		got, source, err := ResolveVerificationMode(VerificationDomainSpecs,
			"workspace", sddOptions(map[string]any{"specs": string(workspace)}), "/p", nil)
		if err != nil {
			t.Fatalf("workspace %s inherited: %v", workspace, err)
		}
		if got != workspace || source != VerificationModeSourceWorkspace {
			t.Errorf("workspace %s inherited (%s, %s), want the workspace policy", workspace, got, source)
		}
	}
}

func TestResolveVerificationModeDefaultsToReportWithProvenance(t *testing.T) {
	got, source, err := ResolveVerificationMode(VerificationDomainSpecs, "workspace", nil, "/p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != VerificationModeReport || source != VerificationModeSourceDefault {
		t.Errorf("resolved (%s, %s), want the built-in report default", got, source)
	}
	// An unrelated sdd option block or an empty verification object inherits too.
	for name, options := range map[string]map[string]map[string]any{
		"no verification member": {"sdd": {"other": true}},
		"empty object":           sddOptions(map[string]any{}),
		"other domain only":      sddOptions(map[string]any{"features": "off"}),
	} {
		got, source, err := ResolveVerificationMode(VerificationDomainSpecs, "workspace", options, "/p", nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != VerificationModeReport || source != VerificationModeSourceDefault {
			t.Errorf("%s: resolved (%s, %s), want the default", name, got, source)
		}
	}
}

// TestDecodeVerificationPolicyValidatesEveryDomain pins that the features and
// architecture keys use the identical value set — the schema must never fork
// between verification subjects — while unknown domains and
// unknown or mistyped values are invalid config, never a fallback.
func TestDecodeVerificationPolicyValidatesEveryDomain(t *testing.T) {
	policy, err := DecodeVerificationPolicy("workspace", sddOptions(map[string]any{
		"specs": "enforce", "features": "report", "architecture": "off",
	}))
	if err != nil {
		t.Fatalf("the verification domains were rejected: %v", err)
	}
	if policy[VerificationDomainFeatures] != VerificationModeReport ||
		policy[VerificationDomainArchitecture] != VerificationModeOff {
		t.Errorf("verification domains decoded as %v", policy)
	}

	invalid := map[string]any{
		"unknown domain":   map[string]any{"spec": "enforce"},
		"unknown value":    map[string]any{"specs": "on"},
		"aliased value":    map[string]any{"specs": "info"},
		"empty value":      map[string]any{"specs": ""},
		"non-string value": map[string]any{"specs": true},
		"non-object":       "enforce",
		"numeric member":   map[string]any{"specs": 1},
	}
	for name, verification := range invalid {
		if _, err := DecodeVerificationPolicy("workspace", sddOptions(verification)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestResolveWorkspaceVerificationModeUsesOnlyWorkspaceAndDefault(t *testing.T) {
	for _, mode := range []VerificationMode{
		VerificationModeEnforce, VerificationModeReport, VerificationModeOff,
	} {
		got, source, err := ResolveWorkspaceVerificationMode(VerificationDomainArchitecture,
			"workspace", sddOptions(map[string]any{"architecture": string(mode)}))
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if got != mode || source != VerificationModeSourceWorkspace {
			t.Errorf("%s resolved (%s, %s), want workspace provenance", mode, got, source)
		}
	}

	got, source, err := ResolveWorkspaceVerificationMode(VerificationDomainArchitecture, "workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != VerificationModeReport || source != VerificationModeSourceDefault {
		t.Errorf("absent workspace policy resolved (%s, %s), want report/default", got, source)
	}

	if _, _, err := ResolveWorkspaceVerificationMode(VerificationDomainArchitecture,
		"workspace", sddOptions(map[string]any{"architecture": "audit"})); err == nil {
		t.Error("an unreadable workspace architecture policy resolved instead of failing")
	}
	if _, _, err := ResolveWorkspaceVerificationMode("verify", "workspace", nil); err == nil {
		t.Error("an unknown workspace verification domain resolved instead of failing")
	}
}

func TestDecodeVerificationPolicyErrorsNameTheScopeAndKey(t *testing.T) {
	_, err := DecodeVerificationPolicy("/go/framework/logger", sddOptions(map[string]any{"specs": "yes"}))
	if err == nil {
		t.Fatal("invalid value was accepted")
	}
	for _, want := range []string{"/go/framework/logger", "options.sdd.verification.specs", `"yes"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestResolveVerificationModeNeverFallsBackOverAnUnreadablePolicy(t *testing.T) {
	if _, _, err := ResolveVerificationMode(VerificationDomainSpecs,
		"workspace", sddOptions("enforce"), "/p", nil); err == nil {
		t.Error("an unreadable workspace policy resolved instead of failing")
	}
	if _, _, err := ResolveVerificationMode(VerificationDomainSpecs,
		"workspace", nil, "/p", sddOptions(map[string]any{"specs": "on"})); err == nil {
		t.Error("an unreadable project policy resolved instead of failing")
	}
	if _, _, err := ResolveVerificationMode("verify", "workspace", nil, "/p", nil); err == nil {
		t.Error("an unknown domain resolved instead of failing")
	}
	// A broken project policy must fail even when the workspace could answer:
	// precedence must not be used to route around invalid committed config.
	if _, _, err := ResolveVerificationMode(VerificationDomainSpecs,
		"workspace", sddOptions(map[string]any{"specs": "report"}),
		"/p", sddOptions(map[string]any{"nope": "report"})); err == nil {
		t.Error("an invalid project policy was ignored because the workspace had one")
	}
}
