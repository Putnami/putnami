package workspaceclient

import (
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestFormatterInputsPreferTheProjectConfigAndFollowExtends(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "formatter-inputs-are-cache-keyed", "every-resolved-formatter-input-is-a-declared-cache-key-entry")
	root := t.TempDir()
	writeWorkspaceFile(t, root, "biome.json", `{"formatter":{"useEditorconfig":true}}`)
	writeWorkspaceFile(t, root, ".editorconfig", "root = true\n")
	writeWorkspaceFile(t, root, "services/catalog/putnami.json", `{"name":"catalog"}`)

	// No project configuration: the workspace configuration and the EditorConfig
	// beside it are the inputs.
	got, err := FormatterInputs(root, "services/catalog")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".editorconfig", "biome.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("workspace-configured inputs = %v, want %v", got, want)
	}

	// A project configuration wins, and the EditorConfig that counts is the one
	// beside the winning configuration, because Biome runs from its directory.
	writeWorkspaceFile(t, root, "services/catalog/biome.json", `{"extends":["../shared/biome.json"]}`)
	writeWorkspaceFile(t, root, "services/catalog/.editorconfig", "indent_size = 4\n")
	writeWorkspaceFile(t, root, "services/shared/biome.json", `{"formatter":{"lineWidth":100}}`)
	got, err = FormatterInputs(root, "services/catalog")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"services/catalog/.editorconfig", "services/catalog/biome.json", "services/shared/biome.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("project-configured inputs = %v, want %v", got, want)
	}
}

func TestFormatterInputsRefuseAnUndeclarableExtends(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "formatter-inputs-are-cache-keyed", "an-undeclarable-formatter-input-fails-instead-of-being-skipped")
	root := t.TempDir()
	writeWorkspaceFile(t, root, "biome.json", `{"extends":["@acme/biome-config"]}`)
	if _, err := FormatterInputs(root, "."); err == nil || !strings.Contains(err.Error(), "package specifier") {
		t.Fatalf("package-specifier extends error = %v", err)
	}

	writeWorkspaceFile(t, root, "biome.json", `{"extends":["../outside/biome.json"]}`)
	if _, err := FormatterInputs(root, "."); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("escaping extends error = %v", err)
	}

	writeWorkspaceFile(t, root, "biome.json", `{"extends":["./missing.json"]}`)
	if _, err := FormatterInputs(root, "."); err == nil || !strings.Contains(err.Error(), "not a workspace file") {
		t.Fatalf("absent extends error = %v", err)
	}
}

func TestFormatterInputsAreAbsentWithoutAnyBiomeConfiguration(t *testing.T) {
	root := t.TempDir()
	got, err := FormatterInputs(root, "services/catalog")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("inputs without a configuration = %v, want none", got)
	}
}
