package stacks

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

func TestDeclareExtensionsKeepsTheWorkspaceConfigOrder(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, wsproto.WorkspaceConfigFilename)
	writeFile(t, root, wsproto.WorkspaceConfigFilename, "{\n  \"name\": \"demo\",\n  \"options\": {\"@putnami/moon\": {\"mode\": \"import\"}},\n  \"extensions\": [\"@putnami/moon\"]\n}\n")
	changed, change, err := DeclareExtensions(root, []string{"@putnami/go"}, false)
	if err != nil || !changed || change != nil {
		t.Fatalf("declare: %v %+v %v", changed, change, err)
	}
	data, _ := os.ReadFile(file)
	want := "{\n  \"name\": \"demo\",\n  \"options\": {\"@putnami/moon\": {\"mode\": \"import\"}},\n  \"extensions\": [\"@putnami/moon\", \"@putnami/go\"]\n}\n"
	if string(data) != want {
		t.Errorf("config:\n%s\nwant\n%s", data, want)
	}
	if changed, _, err := DeclareExtensions(root, []string{"@putnami/go"}, false); err != nil || changed {
		t.Errorf("a declared extension changes nothing: %v %v", changed, err)
	}
	writeFile(t, root, wsproto.WorkspaceConfigFilename, "[]")
	if _, _, err := DeclareExtensions(root, []string{"@putnami/go"}, false); err == nil {
		t.Error("a config that is no object fails")
	}
	if changed, _, err := DeclareExtensions(t.TempDir(), []string{"@putnami/go"}, false); err != nil || changed {
		t.Errorf("no workspace config: %v %v", changed, err)
	}
}

func TestDeclareExtensionsReadsAnExtensionLoadedFromTheWorkspace(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"extensions": ["/moon-extension", "./tools/go", "/missing"]}`)
	writeFile(t, root, "tools/go/putnami.extension.json", `{"name": "@putnami/go"}`)
	if changed, _, err := DeclareExtensions(root, []string{"@putnami/go"}, false); err != nil || changed {
		t.Errorf("an extension the workspace loads by path is declared: %v %v", changed, err)
	}
	before, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename))
	changed, change, err := DeclareExtensions(root, []string{"@putnami/rust"}, true)
	if err != nil || !changed || change == nil {
		t.Errorf("a dry run reports an undeclared extension: %v %v", changed, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename)); string(after) != string(before) {
		t.Errorf("a dry run wrote the config:\n%s", after)
	}
}

func TestDeclareExtensionsDeclaresNothingWhenGivenNothing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"extensions": ["@putnami/nx"]}`)
	if changed, change, err := DeclareExtensions(root, nil, false); err != nil || changed || change != nil {
		t.Errorf("no extension: %v %+v %v", changed, change, err)
	}
}

func TestDeclareExtensionsKeepsTheObjectFormOfExtensions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "shop", "extensions": {"@putnami/nx": "^0.4.0"}}`)
	if changed, _, err := DeclareExtensions(root, []string{"@putnami/typescript"}, false); err != nil || !changed {
		t.Fatalf("declare: %v %v", changed, err)
	}
	data, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename))
	var doc struct {
		Extensions map[string]string `json:"extensions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("extensions lost their object form: %v\n%s", err, data)
	}
	if want := map[string]string{"@putnami/nx": "^0.4.0", "@putnami/typescript": ""}; !maps.Equal(doc.Extensions, want) {
		t.Errorf("extensions = %v, want %v", doc.Extensions, want)
	}
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"extensions": 3}`)
	if _, _, err := DeclareExtensions(root, []string{"@putnami/typescript"}, false); err == nil {
		t.Error("extensions that are neither a list nor an object fail")
	}
}
