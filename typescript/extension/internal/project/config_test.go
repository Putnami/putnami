package project

import (
	"os"
	"path/filepath"
	"testing"
)

// ---- readPutnamiRC ----

func TestReadPutnamiRC_NotFound(t *testing.T) {
	_, err := readPutnamiRC("/nonexistent/putnami.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestReadPutnamiRC_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "putnami.json")
	os.WriteFile(path, []byte("not json"), 0644)

	_, err := readPutnamiRC(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestReadPutnamiRC_Valid(t *testing.T) {
	content := `{
		"name": "@putnami/mylib",
		"type": "library",
		"tags": ["go", "extension"],
		"options": {}
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "putnami.json")
	os.WriteFile(path, []byte(content), 0644)

	rc, err := readPutnamiRC(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.Name != "@putnami/mylib" {
		t.Errorf("expected name '@putnami/mylib', got %q", rc.Name)
	}
	if rc.Type != "library" {
		t.Errorf("expected type 'library', got %q", rc.Type)
	}
	if len(rc.Tags) != 2 || rc.Tags[0] != "go" {
		t.Errorf("unexpected tags: %v", rc.Tags)
	}
}

// ---- GetGenerateAssets ----

func TestGetGenerateAssets_NotFound(t *testing.T) {
	assets := GetGenerateAssets("/nonexistent/putnami.json")
	if assets != nil {
		t.Errorf("expected nil for missing file, got %v", assets)
	}
}

func TestGetGenerateAssets_NoGenerateOption(t *testing.T) {
	content := `{"name": "@test/pkg", "type": "library", "tags": [], "options": {}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "putnami.json")
	os.WriteFile(path, []byte(content), 0644)

	assets := GetGenerateAssets(path)
	if assets != nil {
		t.Errorf("expected nil when no generate option, got %v", assets)
	}
}

func TestGetGenerateAssets_WithAssets(t *testing.T) {
	content := `{
		"name": "@test/pkg",
		"type": "library",
		"tags": [],
		"options": {
			"generate": {
				"assets": [
					{"from": "config/biome.json", "to": "config/biome.json"},
					{"from": "/workspace/schema.json", "to": "schema.json"}
				]
			}
		}
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "putnami.json")
	os.WriteFile(path, []byte(content), 0644)

	assets := GetGenerateAssets(path)
	if len(assets) != 2 {
		t.Fatalf("expected 2 assets, got %d", len(assets))
	}
	if assets[0].From != "config/biome.json" {
		t.Errorf("expected 'config/biome.json', got %q", assets[0].From)
	}
	if assets[0].To != "config/biome.json" {
		t.Errorf("expected 'config/biome.json', got %q", assets[0].To)
	}
	if assets[1].From != "/workspace/schema.json" {
		t.Errorf("expected '/workspace/schema.json', got %q", assets[1].From)
	}
}

func TestGetGenerateAssets_EmptyAssets(t *testing.T) {
	content := `{
		"name": "@test/pkg",
		"type": "library",
		"tags": [],
		"options": {
			"generate": {
				"assets": []
			}
		}
	}`
	dir := t.TempDir()
	path := filepath.Join(dir, "putnami.json")
	os.WriteFile(path, []byte(content), 0644)

	assets := GetGenerateAssets(path)
	if len(assets) != 0 {
		t.Errorf("expected 0 assets, got %d", len(assets))
	}
}

// ---- ResolveTsConfig ----

func TestResolveTsConfig_AppConfig(t *testing.T) {
	dir := t.TempDir()
	workspaceRoot := t.TempDir()

	// Create tsconfig.app.json in project root
	os.WriteFile(filepath.Join(dir, "tsconfig.app.json"), []byte("{}"), 0644)
	// Also create tsconfig.json — should not be picked
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte("{}"), 0644)

	result := ResolveTsConfig(dir, workspaceRoot)
	expected := filepath.Join(dir, "tsconfig.app.json")
	if result != expected {
		t.Errorf("expected tsconfig.app.json to win, got %q", result)
	}
}

func TestResolveTsConfig_LibConfig(t *testing.T) {
	dir := t.TempDir()
	workspaceRoot := t.TempDir()

	os.WriteFile(filepath.Join(dir, "tsconfig.lib.json"), []byte("{}"), 0644)

	result := ResolveTsConfig(dir, workspaceRoot)
	expected := filepath.Join(dir, "tsconfig.lib.json")
	if result != expected {
		t.Errorf("expected tsconfig.lib.json, got %q", result)
	}
}

func TestResolveTsConfig_DefaultConfig(t *testing.T) {
	dir := t.TempDir()
	workspaceRoot := t.TempDir()

	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte("{}"), 0644)

	result := ResolveTsConfig(dir, workspaceRoot)
	expected := filepath.Join(dir, "tsconfig.json")
	if result != expected {
		t.Errorf("expected project tsconfig.json, got %q", result)
	}
}

func TestResolveTsConfig_WorkspaceFallback(t *testing.T) {
	dir := t.TempDir()
	workspaceRoot := t.TempDir()

	os.WriteFile(filepath.Join(workspaceRoot, "tsconfig.json"), []byte("{}"), 0644)

	result := ResolveTsConfig(dir, workspaceRoot)
	expected := filepath.Join(workspaceRoot, "tsconfig.json")
	if result != expected {
		t.Errorf("expected workspace tsconfig.json fallback, got %q", result)
	}
}

func TestResolveTsConfig_NoneFound(t *testing.T) {
	dir := t.TempDir()
	workspaceRoot := t.TempDir()

	result := ResolveTsConfig(dir, workspaceRoot)
	if result != "" {
		t.Errorf("expected empty string when no tsconfig found, got %q", result)
	}
}
