package template

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsTemplateRootPackagingExclusion(t *testing.T) {
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "node_modules", want: true},
		{name: "putnami.json", want: true},
		{name: "putnami.features.json", want: true},
		{name: "specs", want: true},
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
	_, err := LoadManifest("/nonexistent/manifest.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadManifest_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFilename)
	os.WriteFile(path, []byte("{invalid}"), 0o644)

	_, err := LoadManifest(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestLoadManifest_WithDevDeps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFilename)

	data := `{
		"name": "test-template",
		"description": "Test",
		"workspaceDevDependencies": {
			"@putnami/typescript": "^1.0.0"
		}
	}`
	os.WriteFile(path, []byte(data), 0o644)

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.WorkspaceDevDependencies["@putnami/typescript"] != "^1.0.0" {
		t.Errorf("devDeps = %v", m.WorkspaceDevDependencies)
	}
}
