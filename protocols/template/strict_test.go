package template

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestParseManifestStrict_Valid(t *testing.T) {
	data := []byte(`{
		"name": "my-template",
		"description": "A test template"
	}`)

	m, diags := ParseManifestStrict(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m.Name != "my-template" {
		t.Errorf("Name = %q, want my-template", m.Name)
	}
}

func TestParseManifestStrict_UnknownField(t *testing.T) {
	data := []byte(`{
		"name": "t",
		"description": "d",
		"unknownField": true
	}`)

	m, diags := ParseManifestStrict(data)
	if m != nil {
		t.Error("manifest should be nil on parse error")
	}
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for unknown field")
	}
}

func TestParseManifestStrict_InvalidJSON(t *testing.T) {
	_, diags := ParseManifestStrict([]byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateManifest_Nil(t *testing.T) {
	diags := ValidateManifest(nil)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for nil manifest")
	}
}

func TestValidateManifest_MissingRequired(t *testing.T) {
	m := &Manifest{}
	diags := ValidateManifest(m)
	errors := diag.Errors(diags)
	if len(errors) != 2 {
		t.Fatalf("expected 2 errors (name + description), got %d: %v", len(errors), errors)
	}
}

func TestValidateManifest_Valid(t *testing.T) {
	m := &Manifest{Name: "t", Description: "d"}
	diags := ValidateManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestNormalizeManifest(t *testing.T) {
	m := &Manifest{Name: "t", Description: "d"}
	NormalizeManifest(m)
	if m.WorkspaceDevDependencies == nil {
		t.Error("WorkspaceDevDependencies should be initialized")
	}
}

func TestNormalizeManifest_Nil(t *testing.T) {
	NormalizeManifest(nil) // should not panic
}

func TestParseAndValidateManifest_Valid(t *testing.T) {
	data := []byte(`{"name": "t", "description": "d"}`)
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil {
		t.Fatal("manifest should not be nil")
	}
	if m.WorkspaceDevDependencies == nil {
		t.Error("expected normalization to initialize WorkspaceDevDependencies")
	}
}

func TestParseAndValidateManifest_ParseError(t *testing.T) {
	_, diags := ParseAndValidateManifest([]byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
}

func TestParseAndValidateManifest_ValidationError(t *testing.T) {
	data := []byte(`{}`)
	_, diags := ParseAndValidateManifest(data)
	if !diag.HasErrors(diags) {
		t.Fatal("expected validation errors for empty manifest")
	}
}

func TestStrictLoadManifest_PrefixesPath(t *testing.T) {
	_, diags := StrictLoadManifest("templates/my.json", []byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error")
	}
}
