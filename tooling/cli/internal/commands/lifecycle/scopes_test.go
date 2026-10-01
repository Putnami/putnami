package lifecycle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// writeScopeWorkspace builds a workspace with one autonomous scope ("backend")
// that includes a single project, and returns the workspace root.
func writeScopeWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name":     "scoped-ws",
		"includes": []string{"backend"},
	})
	// Scope config (autonomous because it declares includes + tags).
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "backend", wsproto.ConfigFilename), map[string]any{
		"includes": []string{"api"},
		"tags":     []string{"go", "service"},
		"groups":   map[string]string{"ci": "build,test"},
	})
	// The included project.
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "backend", "api", wsproto.ConfigFilename), map[string]any{
		"name": "api-service",
	})
	return dir
}

func TestScopesList_Empty(t *testing.T) {
	dir := t.TempDir()
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name": "empty-ws",
	})

	out, err := captureStdout(t, func() error {
		return ScopesList(dir, wsproto.Load(dir), "")
	})
	if err != nil {
		t.Fatalf("ScopesList: %v", err)
	}
	if !strings.Contains(out, "No scopes configured.") {
		t.Errorf("output = %q, want no-scopes message", out)
	}
}

func TestScopesList_Text(t *testing.T) {
	dir := writeScopeWorkspace(t)
	cfg := wsproto.Load(dir)

	out, err := captureStdout(t, func() error { return ScopesList(dir, cfg, "") })
	if err != nil {
		t.Fatalf("ScopesList: %v", err)
	}
	if !strings.Contains(out, "SCOPE") || !strings.Contains(out, "PROJECTS") {
		t.Errorf("output = %q, want table header", out)
	}
	if !strings.Contains(out, "backend") {
		t.Errorf("output = %q, want scope path", out)
	}
	if !strings.Contains(out, "go, service") {
		t.Errorf("output = %q, want joined tags", out)
	}
	if !strings.Contains(out, "1 scopes") {
		t.Errorf("output = %q, want scope count", out)
	}
}

func TestScopesList_JSONL(t *testing.T) {
	dir := writeScopeWorkspace(t)
	cfg := wsproto.Load(dir)

	out, err := captureStdout(t, func() error { return ScopesList(dir, cfg, "jsonl") })
	if err != nil {
		t.Fatalf("ScopesList: %v", err)
	}
	line := strings.TrimSpace(out)
	var info ScopeInfo
	if err := json.Unmarshal([]byte(line), &info); err != nil {
		t.Fatalf("parse jsonl %q: %v", line, err)
	}
	if info.Path != "backend" {
		t.Errorf("path = %q, want backend", info.Path)
	}
	if len(info.Tags) != 2 {
		t.Errorf("tags = %v, want 2 entries", info.Tags)
	}
	if _, ok := info.Groups["ci"]; !ok {
		t.Errorf("groups = %v, want ci group", info.Groups)
	}
}

// writeConfigJSON writes an authored config verbatim. The `line` block carries
// a "{version}" placeholder, which reads far better as authored JSON than as a
// nest of Go maps.
func writeConfigJSON(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScopesList_ReportsLine pins the LINE column: it reports the tag pattern
// of the version line a scope declares, and a dash for a scope that declares
// none. A line is not inherited, so only the declaring scope reports one.
func TestScopesList_ReportsLine(t *testing.T) {
	dir := t.TempDir()
	writeConfigJSON(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename),
		`{"name":"lined-ws","includes":["typescript","backend"]}`)
	writeConfigJSON(t, filepath.Join(dir, "typescript", wsproto.ConfigFilename),
		`{"includes":["web"],"line":{"tag":"ts/v{version}"}}`)
	writeConfigJSON(t, filepath.Join(dir, "typescript", "web", wsproto.ConfigFilename), `{"name":"web"}`)
	writeConfigJSON(t, filepath.Join(dir, "backend", wsproto.ConfigFilename), `{"includes":["api"]}`)
	writeConfigJSON(t, filepath.Join(dir, "backend", "api", wsproto.ConfigFilename), `{"name":"api"}`)

	cfg := wsproto.Load(dir)
	out, err := captureStdout(t, func() error { return ScopesList(dir, cfg, "") })
	if err != nil {
		t.Fatalf("ScopesList: %v", err)
	}
	if !strings.Contains(out, "LINE") {
		t.Errorf("output = %q, want a LINE column", out)
	}
	if !strings.Contains(out, "ts/v{version}") {
		t.Errorf("output = %q, want the declared tag pattern", out)
	}

	jsonl, err := captureStdout(t, func() error { return ScopesList(dir, cfg, "jsonl") })
	if err != nil {
		t.Fatalf("ScopesList jsonl: %v", err)
	}
	lines := map[string]string{}
	for _, raw := range strings.Split(strings.TrimSpace(jsonl), "\n") {
		var info ScopeInfo
		if err := json.Unmarshal([]byte(raw), &info); err != nil {
			t.Fatalf("parse jsonl %q: %v", raw, err)
		}
		lines[info.Path] = info.Line
	}
	if lines["typescript"] != "ts/v{version}" {
		t.Errorf("typescript line = %q, want ts/v{version}", lines["typescript"])
	}
	if lines["backend"] != "" {
		t.Errorf("backend line = %q, want none", lines["backend"])
	}
}

func TestCollectScopeInfos_ResolvesProjectNames(t *testing.T) {
	dir := writeScopeWorkspace(t)
	cfg := wsproto.Load(dir)
	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatalf("workspace.Load: %v", err)
	}

	scopes := collectScopeInfos(dir, cfg, ws)
	if len(scopes) != 1 {
		t.Fatalf("got %d scopes, want 1", len(scopes))
	}
	if scopes[0].Path != "backend" {
		t.Errorf("path = %q, want backend", scopes[0].Path)
	}
	if len(scopes[0].Projects) != 1 {
		t.Fatalf("projects = %v, want 1 entry", scopes[0].Projects)
	}
	// The project name should be resolved from the project config, not the path.
	if scopes[0].Projects[0] != "api-service" {
		t.Errorf("project = %q, want api-service", scopes[0].Projects[0])
	}
}
