package template

import (
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

func TestParseManifestStrict_Valid(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "the-strict-parser-refuses-undeclared-members", "a-declared-manifest-parses")
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
	spectest.Proves(t, "template/manifest-contract", "the-strict-parser-refuses-undeclared-members", "an-undeclared-member-is-refused-with-no-manifest")
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
	spectest.Proves(t, "template/manifest-contract", "the-strict-parser-refuses-undeclared-members", "input-that-is-not-json-is-refused")
	_, diags := ParseManifestStrict([]byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateManifest_Nil(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "name-and-description-are-required", "a-nil-manifest-is-refused")
	diags := ValidateManifest(nil)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for nil manifest")
	}
}

func TestValidateManifest_MissingRequired(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "name-and-description-are-required", "each-missing-member-is-its-own-error")
	m := &Manifest{}
	diags := ValidateManifest(m)
	errors := diag.Errors(diags)
	if len(errors) != 2 {
		t.Fatalf("expected 2 errors (name + description), got %d: %v", len(errors), errors)
	}
}

func TestValidateManifest_Valid(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "name-and-description-are-required", "name-and-description-suffice")
	m := &Manifest{Name: "t", Description: "d"}
	diags := ValidateManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestParseAndValidateManifest_Valid(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "name-and-description-are-required", "name-and-description-suffice")
	data := []byte(`{"name": "t", "description": "d"}`)
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil {
		t.Fatal("manifest should not be nil")
	}
}

func TestParseAndValidateManifest_ParseError(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "the-strict-parser-refuses-undeclared-members", "input-that-is-not-json-is-refused")
	_, diags := ParseAndValidateManifest([]byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
}

func TestParseAndValidateManifest_ValidationError(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "name-and-description-are-required", "an-empty-or-version-only-manifest-is-refused")
	for _, input := range []string{`{}`, `{"version":"1.0.0"}`} {
		_, diags := ParseAndValidateManifest([]byte(input))
		errs := diag.Errors(diags)
		fields := make([]string, 0, len(errs))
		for _, d := range errs {
			fields = append(fields, d.Field)
		}
		if !slices.Equal(fields, []string{"name", "description"}) {
			t.Errorf("%s: errors at %v, want name and description", input, fields)
		}
	}
}

func TestStrictLoadManifest_PrefixesPath(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "the-strict-parser-refuses-undeclared-members", "input-that-is-not-json-is-refused")
	_, diags := StrictLoadManifest("templates/my.json", []byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error")
	}
}
