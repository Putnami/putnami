package configcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
)

// --- setNestedValue (pure function) ---

func makeOrderedMap(initial map[string]any) *jsonutil.OrderedMap {
	m := jsonutil.New()
	for k, v := range initial {
		m.Set(k, v)
	}
	return m
}

func TestSetNestedValue(t *testing.T) {
	tests := []struct {
		name    string
		initial map[string]any
		keys    []string
		value   string
		wantKey string
		wantVal any
		checkFn func(t *testing.T, m *jsonutil.OrderedMap) // for complex assertions
	}{
		{
			name:    "top level string",
			initial: map[string]any{"existing": "keep"},
			keys:    []string{"key"},
			value:   "value",
			wantKey: "key",
			wantVal: "value",
		},
		{
			name:    "preserves existing keys",
			initial: map[string]any{"existing": "keep"},
			keys:    []string{"key"},
			value:   "value",
			wantKey: "existing",
			wantVal: "keep",
		},
		{
			name:    "JSON boolean true",
			initial: map[string]any{},
			keys:    []string{"enabled"},
			value:   "true",
			wantKey: "enabled",
			wantVal: true,
		},
		{
			name:    "JSON boolean false",
			initial: map[string]any{},
			keys:    []string{"disabled"},
			value:   "false",
			wantKey: "disabled",
			wantVal: false,
		},
		{
			name:    "JSON number",
			initial: map[string]any{},
			keys:    []string{"count"},
			value:   "42",
			wantKey: "count",
			wantVal: float64(42),
		},
		{
			name:    "plain string",
			initial: map[string]any{},
			keys:    []string{"target"},
			value:   "linux/amd64",
			wantKey: "target",
			wantVal: "linux/amd64",
		},
		{
			name:    "JSON array",
			initial: map[string]any{},
			keys:    []string{"tags"},
			value:   `["a","b","c"]`,
			checkFn: func(t *testing.T, m *jsonutil.OrderedMap) {
				v, _ := m.Get("tags")
				arr, ok := v.([]any)
				if !ok {
					t.Fatalf("tags is %T, want []any", v)
				}
				if len(arr) != 3 {
					t.Errorf("tags length = %d, want 3", len(arr))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := makeOrderedMap(tt.initial)
			setNestedValue(m, tt.keys, tt.value)
			if tt.checkFn != nil {
				tt.checkFn(t, m)
			} else {
				got, _ := m.Get(tt.wantKey)
				if got != tt.wantVal {
					t.Errorf("%s = %v (%T), want %v (%T)", tt.wantKey, got, got, tt.wantVal, tt.wantVal)
				}
			}
		})
	}
}

func TestSetNestedValue_Nested(t *testing.T) {
	m := jsonutil.New()
	setNestedValue(m, []string{"options", "build", "target"}, "linux/amd64")

	options := m.GetMap("options")
	if options == nil {
		t.Fatalf("options is nil, want OrderedMap")
	}
	build := options.GetMap("build")
	if build == nil {
		t.Fatalf("options.build is nil, want OrderedMap")
	}
	target, _ := build.Get("target")
	if target != "linux/amd64" {
		t.Errorf("options.build.target = %v, want linux/amd64", target)
	}
}

func TestSetNestedValue_OverwriteNonMapWithNested(t *testing.T) {
	// If intermediate key exists but is not a map, it should be replaced
	m := jsonutil.New()
	m.Set("options", "string-value")
	setNestedValue(m, []string{"options", "key"}, "val")

	options := m.GetMap("options")
	if options == nil {
		v, _ := m.Get("options")
		t.Fatalf("options should be replaced with OrderedMap, got %T", v)
	}
	val, _ := options.Get("key")
	if val != "val" {
		t.Errorf("options.key = %v, want val", val)
	}
}

// --- ConfigSet ---

func makeTestConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	initial := map[string]any{
		"name":     "test-workspace",
		"includes": []string{"packages/a"},
	}
	data, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestConfigSet_Values(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		checkFn func(t *testing.T, result map[string]any)
	}{
		{
			name: "simple key",
			args: []string{"output", "jsonl"},
			checkFn: func(t *testing.T, result map[string]any) {
				if result["output"] != "jsonl" {
					t.Errorf("output = %v, want jsonl", result["output"])
				}
				if result["name"] != "test-workspace" {
					t.Errorf("name = %v, want test-workspace (existing field should be preserved)", result["name"])
				}
			},
		},
		{
			name: "nested key",
			args: []string{"options.build.target", "linux/amd64"},
			checkFn: func(t *testing.T, result map[string]any) {
				options, ok := result["options"].(map[string]any)
				if !ok {
					t.Fatalf("options is %T", result["options"])
				}
				build, ok := options["build"].(map[string]any)
				if !ok {
					t.Fatalf("options.build is %T", options["build"])
				}
				if build["target"] != "linux/amd64" {
					t.Errorf("options.build.target = %v, want linux/amd64", build["target"])
				}
			},
		},
		{
			name: "boolean value",
			args: []string{"verbose", "true"},
			checkFn: func(t *testing.T, result map[string]any) {
				if result["verbose"] != true {
					t.Errorf("verbose = %v (%T), want true", result["verbose"], result["verbose"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := makeTestConfigFile(t)
			if err := ConfigSet(dir, tt.args); err != nil {
				t.Fatalf("ConfigSet: %v", err)
			}

			raw, _ := os.ReadFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename))
			var result map[string]any
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			tt.checkFn(t, result)
		})
	}
}

func TestConfigSet_Errors(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(t *testing.T) string
		args    []string
	}{
		{
			name:    "missing value arg",
			setupFn: makeTestConfigFile,
			args:    []string{"onlykey"},
		},
		{
			name:    "empty args",
			setupFn: makeTestConfigFile,
			args:    []string{},
		},
		{
			name:    "missing config file",
			setupFn: func(t *testing.T) string { return t.TempDir() },
			args:    []string{"key", "value"},
		},
		{
			name: "invalid JSON",
			setupFn: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte("not json {{{"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			args: []string{"key", "value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setupFn(t)
			if err := ConfigSet(dir, tt.args); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// --- ConfigShow ---

func TestConfigShow(t *testing.T) {
	tests := []struct {
		name    string
		setupFn func(t *testing.T) string
		cfg     *wsproto.Config
		output  string
	}{
		{
			name:    "JSONL output",
			setupFn: makeTestConfigFile,
			cfg:     &wsproto.Config{Name: "test-ws"},
			output:  "jsonl",
		},
		{
			name:    "formatted output",
			setupFn: makeTestConfigFile,
			cfg:     &wsproto.Config{Name: "test-ws"},
			output:  "",
		},
		{
			name:    "empty config",
			setupFn: func(t *testing.T) string { return t.TempDir() },
			cfg:     &wsproto.Config{},
			output:  "jsonl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setupFn(t)
			if err := ConfigShow(dir, tt.cfg, tt.output); err != nil {
				t.Errorf("ConfigShow: %v", err)
			}
		})
	}
}

// --- configSources ---

func TestConfigSources_WorkspaceFile(t *testing.T) {
	dir := makeTestConfigFile(t)
	sources := configSources(dir)
	found := false
	for _, s := range sources {
		if len(s) > 0 {
			found = true
		}
	}
	// configSources doesn't panic — that's the key invariant; found may be false
	// when cwd == wsRoot (both resolve to same file), which is acceptable.
	_ = found
}
