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

func TestParsePort(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  int
	}{
		{8080, 8080}, {float64(8080), 8080}, {"8080", 8080}, {" 443 ", 443},
		{float64(8080.5), 0}, {0, 0}, {70000, 0}, {"${PORT}", 0}, {nil, 0}, {true, 0},
	} {
		if got := ParsePort(tc.value); got != tc.want {
			t.Errorf("ParsePort(%#v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

func TestConfiguredPort(t *testing.T) {
	write := func(files map[string]string) string {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, "conf", name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	for name, tc := range map[string]struct {
		files map[string]string
		want  int
	}{
		"no conf":                 {nil, 0},
		"base file":               {map[string]string{".env.yaml": "putnami:\n  port: 8080\n"}, 8080},
		"environment file wins":   {map[string]string{".env.yaml": "putnami:\n  port: 8080\n", ".env.production.yaml": "putnami:\n  port: 9090\n"}, 9090},
		"environment without one": {map[string]string{".env.yaml": "putnami:\n  port: 8080\n", ".env.production.yaml": "putnami:\n  name: api\n"}, 8080},
		"last duplicate key wins": {map[string]string{".env.yaml": "putnami:\n  port: 8080\n  port: 8081\nother: 1\nother: 2\n"}, 8081},
		"last putnami block wins": {map[string]string{".env.yaml": "putnami:\n  port: 8080\nputnami:\n  port: 8082\n"}, 8082},
		"unparsable file":         {map[string]string{".env.yaml": "putnami: [\n"}, 0},
		"not a mapping":           {map[string]string{".env.yaml": "- 8080\n"}, 0},
		"an aliased port":         {map[string]string{".env.yaml": "default: &p 8080\nputnami:\n  port: *p\n"}, 8080},
		"an aliased block":        {map[string]string{".env.yaml": "base: &b\n  port: 8080\nputnami: *b\n"}, 8080},
		"a merged block":          {map[string]string{".env.yaml": "base: &b\n  port: 8080\nputnami:\n  <<: *b\n  name: api\n"}, 8080},
		"own key beats the merge": {map[string]string{".env.yaml": "base: &b\n  port: 8080\nputnami:\n  <<: *b\n  port: 9000\n"}, 9000},
		"first merge source wins": {map[string]string{".env.yaml": "a: &a\n  port: 8081\nb: &b\n  port: 8082\nputnami:\n  <<: [*a, *b]\n"}, 8081},
	} {
		if got := ConfiguredPort(write(tc.files), "production"); got != tc.want {
			t.Errorf("%s: ConfiguredPort() = %d, want %d", name, got, tc.want)
		}
	}
}
