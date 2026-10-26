package template

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

func TestIsTemplateRootPackagingExclusion(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "root-declarations-stay-out", "the-root-exclusions-are-pinned")
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "node_modules", want: true},
		{name: "putnami.json", want: true},
		{name: "putnami.features.json", want: true},
		{name: "specs", want: true},
		{name: "specs.baseline.json", want: true},
		{name: ".gitignore", want: true},
		{name: ".putnami", want: true},
		{name: "src", want: false},
		{name: "README.md", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := IsTemplateRootPackagingExclusion(test.name); got != test.want {
				t.Fatalf("IsTemplateRootPackagingExclusion(%q) = %v, want %v", test.name, got, test.want)
			}
		})
	}
}

func TestLoadManifest_Valid(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "the-lenient-read-answers-identity", "the-lenient-read-returns-the-identity")
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFilename)

	data := `{
		"name": "@putnami/go-library",
		"version": "1.0.0",
		"description": "Go library template",
		"extension": "@putnami/go"
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Name != "@putnami/go-library" {
		t.Errorf("Name = %q", m.Name)
	}
	if m.Description != "Go library template" {
		t.Errorf("Description = %q", m.Description)
	}
	if m.Extension != "@putnami/go" {
		t.Errorf("Extension = %q", m.Extension)
	}
}

func TestLoadManifest_FileNotFound(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "the-lenient-read-answers-identity", "the-lenient-read-fails-without-a-json-file")
	_, err := LoadManifest("/nonexistent/manifest.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadManifest_InvalidJSON(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "the-lenient-read-answers-identity", "the-lenient-read-fails-without-a-json-file")
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFilename)
	os.WriteFile(path, []byte("{invalid}"), 0o644)

	_, err := LoadManifest(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// The lenient read answers the identity question for any archive, including
// one that declares a field the strict parser refuses.
func TestLoadManifest_IgnoresAFieldTheStrictParserRefuses(t *testing.T) {
	spectest.Proves(t, "template/manifest-contract", "the-lenient-read-answers-identity", "the-lenient-read-keeps-a-manifest-the-strict-parser-refuses")
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFilename)

	data := `{
		"name": "test-template",
		"description": "Test",
		"extension": "@putnami/go",
		"workspaceDevDependencies": {
			"go.putnami.dev/go/extension": "latest"
		}
	}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "test-template" || m.Extension != "@putnami/go" {
		t.Errorf("manifest = %+v, want the name and extension", m)
	}
	if _, diags := ParseManifestStrict([]byte(data)); !diag.HasErrors(diags) {
		t.Error("the strict parser accepted workspaceDevDependencies")
	}
}
