package workspace

import (
	"encoding/json"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestParseWorkspaceConfig_Valid(t *testing.T) {
	data := []byte(`{
		"name": "my-workspace",
		"includes": ["app"]
	}`)

	c, diags := ParseWorkspaceConfig(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if c.Name != "my-workspace" {
		t.Errorf("Name = %q, want my-workspace", c.Name)
	}
}

func TestParseWorkspaceConfig_UnknownField(t *testing.T) {
	data := []byte(`{"name": "w", "unknownField": true}`)
	c, diags := ParseWorkspaceConfig(data)
	if c != nil {
		t.Error("config should be nil on parse error")
	}
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for unknown field")
	}
}

func TestParseWorkspaceConfig_InvalidJSON(t *testing.T) {
	_, diags := ParseWorkspaceConfig([]byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateWorkspaceConfig_Nil(t *testing.T) {
	diags := ValidateWorkspaceConfig(nil)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for nil config")
	}
}

func TestValidateWorkspaceConfig_MissingName(t *testing.T) {
	c := &Config{}
	diags := ValidateWorkspaceConfig(c)
	found := false
	for _, d := range diags {
		if d.Field == "name" && d.Severity == diag.Warning {
			found = true
		}
	}
	if !found {
		t.Error("expected warning for missing name")
	}
}

func TestValidateWorkspaceConfig_Valid(t *testing.T) {
	c := &Config{Name: "ws"}
	diags := ValidateWorkspaceConfig(c)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestNormalizeWorkspaceConfig(t *testing.T) {
	c := &Config{}
	NormalizeWorkspaceConfig(c)
	if c.Options == nil {
		t.Error("Options should be initialized")
	}
	if c.Extensions.List == nil {
		t.Error("Extensions.List should be initialized")
	}
	if c.Aliases == nil {
		t.Error("Aliases should be initialized")
	}
	if c.ProjectAliases == nil {
		t.Error("ProjectAliases should be initialized")
	}
	if c.Groups == nil {
		t.Error("Groups should be initialized")
	}
}

func TestNormalizeWorkspaceConfig_Nil(t *testing.T) {
	NormalizeWorkspaceConfig(nil) // should not panic
}

func TestParseAndValidateWorkspaceConfig_Valid(t *testing.T) {
	data := []byte(`{"name": "ws"}`)
	c, diags := ParseAndValidateWorkspaceConfig(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if c == nil {
		t.Fatal("config should not be nil")
	}
	if c.Options == nil {
		t.Error("expected normalization")
	}
}

func TestParseProjectConfig_Valid(t *testing.T) {
	data := []byte(`{"name": "my-project"}`)
	c, diags := ParseProjectConfig(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if c.Name != "my-project" {
		t.Errorf("Name = %q, want my-project", c.Name)
	}
}

func TestParseProjectConfig_UnknownField(t *testing.T) {
	data := []byte(`{"name": "p", "unknownField": true}`)
	c, diags := ParseProjectConfig(data)
	if c != nil {
		t.Error("config should be nil on parse error")
	}
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for unknown field")
	}
}

func TestValidateProjectConfig_Nil(t *testing.T) {
	diags := ValidateProjectConfig(nil)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for nil config")
	}
}

func TestValidateProjectConfig_MissingName(t *testing.T) {
	c := &ProjectConfig{}
	diags := ValidateProjectConfig(c)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for missing name")
	}
}

// featureAuthority explains why a published project declares no feature. It
// must stay an answer, not an off switch: a block that says nothing, or says
// two things, would silence completeness tooling while explaining neither.
func TestValidateProjectConfig_FeatureAuthorityMustSayExactlyOneThing(t *testing.T) {
	for _, test := range []struct {
		name      string
		authority *ProjectFeatureAuthority
		wantError bool
	}{
		{name: "absent"},
		{name: "owner", authority: &ProjectFeatureAuthority{Owner: "go/api-contracts"}},
		{name: "none", authority: &ProjectFeatureAuthority{None: "a wire contract is not a user outcome"}},
		{name: "empty block", authority: &ProjectFeatureAuthority{}, wantError: true},
		{name: "blank reason", authority: &ProjectFeatureAuthority{None: "   "}, wantError: true},
		{
			name:      "both members",
			authority: &ProjectFeatureAuthority{Owner: "go/api-contracts", None: "also none"},
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			diags := ValidateProjectConfig(&ProjectConfig{Name: "p", FeatureAuthority: test.authority})
			if got := diag.HasErrors(diags); got != test.wantError {
				t.Fatalf("HasErrors = %v, want %v; diagnostics: %+v", got, test.wantError, diags)
			}
		})
	}
}

// A declared job is REPORTED, never rejected. Both halves matter: rejecting it
// would break configs that parse today, and staying silent would leave the
// author believing the block does something.
func TestValidateProjectConfig_JobsWarnAndAreNotRejected(t *testing.T) {
	c := &ProjectConfig{
		Name: "p",
		Jobs: map[string]json.RawMessage{
			"build":     json.RawMessage(`{"kind":"command","command":"go"}`),
			"typecheck": json.RawMessage(`{}`),
		},
	}
	diags := ValidateProjectConfig(c)

	if diag.HasErrors(diags) {
		t.Fatalf("`jobs` must not be a hard rejection: %v", diag.Errors(diags))
	}
	if len(diags) != 2 {
		t.Fatalf("got %d diagnostics, want one per declared job: %v", len(diags), diags)
	}

	// Sorted by job name, and each one names its own field so an editor can
	// place it on the offending line.
	if diags[0].Field != "jobs.build" || diags[1].Field != "jobs.typecheck" {
		t.Errorf("diagnostics are not in sorted job order: %v", diags)
	}
	for _, d := range diags {
		if d.Severity != diag.Warning {
			t.Errorf("severity = %v, want warning: %v", d.Severity, d)
		}
		if !strings.Contains(d.Message, "ignored") {
			t.Errorf("diagnostic does not say the job is ignored: %v", d)
		}
		// The operator needs somewhere to go, not just a refusal.
		if !strings.Contains(d.Message, "disable.jobs") || !strings.Contains(d.Message, "`tasks`") {
			t.Errorf("diagnostic does not point at the supported alternatives: %v", d)
		}
	}
}

// An EMPTY `jobs` object is still a declared ignored surface and must be
// reported. It is enough to mark a directory as a project, so validating it
// silently would let `{"jobs": {}}` pass clean — the same silent no-op the
// descope exists to end, just with nothing inside the braces. A nil map (absent
// key, or `"jobs": null`) is genuinely undeclared and stays quiet.
func TestValidateProjectConfig_EmptyJobsBlockIsReported(t *testing.T) {
	empty := ValidateProjectConfig(&ProjectConfig{Name: "p", Jobs: map[string]json.RawMessage{}})
	if diag.HasErrors(empty) {
		t.Fatalf("an empty `jobs` block must not be a hard rejection: %v", diag.Errors(empty))
	}
	if len(empty) != 1 {
		t.Fatalf("got %d diagnostics for an empty `jobs` block, want exactly one: %v", len(empty), empty)
	}
	if empty[0].Field != "jobs" {
		t.Errorf("field = %q, want the key itself (there is no entry to point at): %v", empty[0].Field, empty[0])
	}
	if empty[0].Severity != diag.Warning {
		t.Errorf("severity = %v, want warning: %v", empty[0].Severity, empty[0])
	}
	if !strings.Contains(empty[0].Message, "ignored") {
		t.Errorf("diagnostic does not say the key is ignored: %v", empty[0])
	}
	if !strings.Contains(empty[0].Message, "disable.jobs") || !strings.Contains(empty[0].Message, "`tasks`") {
		t.Errorf("diagnostic does not point at the supported alternatives: %v", empty[0])
	}

	// The nil case is the control: undeclared must stay silent, or every config
	// in the workspace would sprout a warning.
	if nilJobs := ValidateProjectConfig(&ProjectConfig{Name: "p"}); len(nilJobs) != 0 {
		t.Errorf("a config with no `jobs` key produced %d diagnostics: %v", len(nilJobs), nilJobs)
	}
}

func TestValidateProjectConfig_Valid(t *testing.T) {
	c := &ProjectConfig{Name: "p"}
	diags := ValidateProjectConfig(c)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestValidateProjectTaskTuning_TimeoutMustBePositive(t *testing.T) {
	ptr := func(v int) *int { return &v }

	for _, tc := range []struct {
		name  string
		value int
	}{
		{name: "zero is the manifest default sentinel", value: 0},
		{name: "minus one is the manifest unbounded sentinel", value: -1},
		{name: "other negative values are rejected too", value: -25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &ProjectConfig{Name: "p", Tasks: map[string]ProjectTaskTuning{
				"lint~golangci-lint": {TimeoutMs: ptr(tc.value)},
			}}
			errors := diag.Errors(ValidateProjectConfig(c))
			if len(errors) != 1 {
				t.Fatalf("errors = %v, want one invalid timeout", errors)
			}
			if errors[0].Code != "invalid-timeout" || errors[0].Field != "tasks.lint~golangci-lint.timeoutMs" {
				t.Fatalf("diagnostic = %+v, want invalid-timeout on the task timeout", errors[0])
			}
		})
	}

	for _, value := range []int{1, 900_000} {
		c := &ProjectConfig{Name: "p", Tasks: map[string]ProjectTaskTuning{
			"lint": {TimeoutMs: ptr(value)},
		}}
		if errors := diag.Errors(ValidateProjectConfig(c)); len(errors) != 0 {
			t.Fatalf("positive timeout %d rejected: %v", value, errors)
		}
	}

	// Map ordering must not make the load-time diagnostic flicker.
	c := &ProjectConfig{Name: "p", Tasks: map[string]ProjectTaskTuning{
		"test":  {TimeoutMs: ptr(-1)},
		"build": {TimeoutMs: ptr(0)},
		"lint":  {TimeoutMs: ptr(-2)},
	}}
	errors := diag.Errors(ValidateProjectTaskTuning(c))
	want := []string{"tasks.build.timeoutMs", "tasks.lint.timeoutMs", "tasks.test.timeoutMs"}
	if len(errors) != len(want) {
		t.Fatalf("errors = %v, want fields %v", errors, want)
	}
	for i, field := range want {
		if errors[i].Field != field {
			t.Errorf("errors[%d].Field = %q, want %q", i, errors[i].Field, field)
		}
	}
}

func TestNormalizeProjectConfig(t *testing.T) {
	c := &ProjectConfig{}
	NormalizeProjectConfig(c)
	if c.Options == nil {
		t.Error("Options should be initialized")
	}
	// `jobs` stays nil: an ignored surface has no canonical empty form, and a
	// non-nil empty map would read as "this directory declares jobs" to the
	// project-marker scan that keys off Jobs != nil.
	if c.Jobs != nil {
		t.Errorf("Jobs = %v, want nil — an ignored surface must not be materialized", c.Jobs)
	}
}

func TestNormalizeProjectConfig_Nil(t *testing.T) {
	NormalizeProjectConfig(nil) // should not panic
}

func TestParseAndValidateProjectConfig_Valid(t *testing.T) {
	data := []byte(`{"name": "p"}`)
	c, diags := ParseAndValidateProjectConfig(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if c == nil {
		t.Fatal("config should not be nil")
	}
}

func TestParseAndValidateProjectConfig_ParseError(t *testing.T) {
	_, diags := ParseAndValidateProjectConfig([]byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
}
