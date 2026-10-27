package template

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// fixtureNames lists the files under fixtures/<kind>, so a fixture added or
// removed without its case here fails the conformance tests.
func fixtureNames(t *testing.T, kind string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("fixtures", kind))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestConformance_ValidFixtures(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "every-fixture-gets-its-verdict", "every-valid-fixture-validates")
	want := map[string]Manifest{
		"minimal.json": {Schema: "https://putnami.dev/schemas/putnami-template.json", Name: "minimal-library",
			Description: "A minimal project template"},
		"full.json": {Schema: "https://putnami.dev/schemas/putnami-template.json", Name: "typescript-web-app",
			Version: "1.0.0", Description: "TypeScript web application with React SSR", Extension: "@putnami/typescript",
			TestVariables: map[string]string{"projectName": "my-web-app"}},
	}
	if got := fixtureNames(t, "valid"); !slices.Equal(got, []string{"full.json", "minimal.json"}) {
		t.Fatalf("fixtures/valid holds %v, want full.json and minimal.json", got)
	}
	for name, expected := range want {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures", "valid", name))
			if err != nil {
				t.Fatal(err)
			}
			m, diags := ParseAndValidateManifest(data)
			if diag.HasErrors(diags) || m == nil {
				t.Fatalf("valid fixture %s: manifest %v, diagnostics %v", name, m, diags)
			}
			if !reflect.DeepEqual(*m, expected) {
				t.Errorf("valid fixture %s decoded to %+v, want %+v", name, *m, expected)
			}
		})
	}
}

func TestConformance_InvalidFixtures(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "every-fixture-gets-its-verdict", "every-invalid-fixture-is-refused")
	want := map[string]struct{ code, field string }{
		"missing-name.json":               {"required-field", "name"},
		"missing-description.json":        {"required-field", "description"},
		"workspace-dev-dependencies.json": {"parse-error", ""},
	}
	if got := fixtureNames(t, "invalid"); !slices.Equal(got, []string{"missing-description.json", "missing-name.json", "workspace-dev-dependencies.json"}) {
		t.Fatalf("fixtures/invalid holds %v, want missing-description.json, missing-name.json and workspace-dev-dependencies.json", got)
	}
	for name, expected := range want {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("fixtures", "invalid", name))
			if err != nil {
				t.Fatal(err)
			}
			_, diags := ParseAndValidateManifest(data)
			errs := diag.Errors(diags)
			if len(errs) != 1 || errs[0].Code != expected.code || errs[0].Field != expected.field {
				t.Errorf("invalid fixture %s: diagnostics %v, want one %s at %q", name, errs, expected.code, expected.field)
			}
		})
	}
}
