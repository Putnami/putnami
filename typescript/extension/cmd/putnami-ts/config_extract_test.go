package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// ---- schemaCandidatePaths ----

func TestSchemaCandidatePaths_Default(t *testing.T) {
	got := schemaCandidatePaths(nil)
	want := []string{configSchemaDefaultPath, configSchemaFallbackPath}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("schemaCandidatePaths() = %v, want %v", got, want)
	}
}

func TestSchemaCandidatePaths_ProjectOptionsSchemaFalse(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	ctx.Project.Options = map[string]json.RawMessage{
		"generate": json.RawMessage(`{"schema":false}`),
	}

	got := schemaCandidatePaths(ctx)
	if got[0] != configSchemaFallbackPath {
		t.Errorf("expected project option fallback first, got %v", got)
	}
}

// ---- jsonSchemaCompanionPath ----

func TestJsonSchemaCompanionPath_StandardJSON(t *testing.T) {
	got := jsonSchemaCompanionPath("schema/config.json")
	want := "schema/config.jsonschema.json"
	if got != want {
		t.Errorf("jsonSchemaCompanionPath = %q, want %q", got, want)
	}
}

func TestJsonSchemaCompanionPath_GenFallback(t *testing.T) {
	got := jsonSchemaCompanionPath(".gen/config-schema.json")
	want := ".gen/config-schema.jsonschema.json"
	if got != want {
		t.Errorf("jsonSchemaCompanionPath = %q, want %q", got, want)
	}
}

func TestJsonSchemaCompanionPath_NoExtension(t *testing.T) {
	got := jsonSchemaCompanionPath("schema/config")
	want := "schema/config.jsonschema"
	if got != want {
		t.Errorf("jsonSchemaCompanionPath = %q, want %q", got, want)
	}
}

// ---- loadEmittedSchema ----

func TestLoadEmittedSchema_NotFound(t *testing.T) {
	dir := t.TempDir()
	path, manifest, err := loadEmittedSchema(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if manifest != nil {
		t.Errorf("expected nil manifest when no file, got %+v", manifest)
	}
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}

func TestLoadEmittedSchema_DefaultPathWins(t *testing.T) {
	dir := t.TempDir()

	// Drop a manifest at schema/config.json
	mustWriteJSON(t, dir, configSchemaDefaultPath, map[string]any{
		"appName":    "demo",
		"version":    "1.0.0",
		"schemaHash": "sha256:abc",
		"configs":    []any{map[string]any{"path": "database", "fields": []any{}}},
	})

	path, manifest, err := loadEmittedSchema(dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if manifest == nil {
		t.Fatal("expected non-nil manifest")
	}
	if manifest.AppName != "demo" || manifest.SchemaHash != "sha256:abc" || len(manifest.Configs) != 1 {
		t.Errorf("unexpected manifest: %+v", manifest)
	}
	if filepath.Base(path) != "config.json" {
		t.Errorf("expected config.json, got %q", path)
	}
}

func TestLoadEmittedSchema_FallbackPathUsedWhenProjectOptionFalse(t *testing.T) {
	dir := t.TempDir()
	ctx, _ := makeTestCtx(t)
	ctx.Project.Options = map[string]json.RawMessage{
		"generate": json.RawMessage(`{"schema":false}`),
	}

	mustWriteJSON(t, dir, configSchemaFallbackPath, map[string]any{
		"appName":    "demo",
		"version":    "",
		"schemaHash": "sha256:fb",
		"configs":    []any{},
	})

	path, manifest, err := loadEmittedSchema(dir, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if manifest == nil {
		t.Fatal("expected non-nil manifest")
	}
	if filepath.Base(path) != "config-schema.json" {
		t.Errorf("expected fallback name, got %q", path)
	}
}

func TestLoadEmittedSchema_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, configSchemaDefaultPath), []byte("not json"))

	_, _, err := loadEmittedSchema(dir, nil)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestConfigExtractHookConfig_UsesGenerateOptions(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	ctx.Project.Options = map[string]json.RawMessage{
		"generate": json.RawMessage(`{"schema":false,"assets":[{"from":"a","to":"b"}]}`),
	}

	got := configExtractHookConfig(ctx)
	if got["schema"] != false {
		t.Fatalf("schema = %v, want false", got["schema"])
	}
	if _, ok := got["assets"]; !ok {
		t.Fatalf("assets option missing from hook config: %v", got)
	}
}

// ---- runConfigExtract ----

func TestRunConfigExtract_SkipsWhenNoHookAndNoSchema(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, _ := makeTestCtx(t)
	emit := jsonl.New()

	status, data, err := runConfigExtract(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "SKIP" {
		t.Errorf("expected SKIP when no hook ran and no schema exists, got %q", status)
	}
	if _, ok := data["reason"]; !ok {
		t.Error("expected reason field in skip data")
	}
}

func TestRunConfigExtract_SkipsWithConfigReasonWhenHookRanWithoutSchema(t *testing.T) {
	mockBunResolution(t)
	// Hook subprocesses must emit a summary event on success; an empty one
	// stands in for a configExtract hook that registered no definitions.
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	depDir := filepath.Join(projectPath, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`), 0644)
	os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	emit := jsonl.New()
	status, data, err := runConfigExtract(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "SKIP" {
		t.Errorf("expected SKIP when hook ran but no schema exists, got %q", status)
	}
	if data["reason"] != "no config definitions registered" {
		t.Errorf("reason = %v, want no config definitions registered", data["reason"])
	}
}

func TestRunConfigExtract_SuccessWithEmittedSchema(t *testing.T) {
	// Simulate the hook subprocess writing the manifest, then verify
	// the runner picks it up and returns OK with the parsed fields.
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	// Pretend @putnami/application registered a configExtract hook
	depDir := filepath.Join(projectPath, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`), 0644)
	os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	// The mocked exec stands in for the bun subprocess: it writes the
	// schema file the way the real script would, then exits OK.
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		_ = writeJSON(filepath.Join(projectPath, configSchemaDefaultPath), map[string]any{
			"appName":    "@test/pkg",
			"version":    "1.0.0",
			"schemaHash": "sha256:1234567890abcdef",
			"configs": []any{
				map[string]any{
					"path":   "database.default",
					"fields": []any{map[string]any{"name": "host", "type": "string"}},
				},
				map[string]any{
					"path":   "session",
					"fields": []any{map[string]any{"name": "cookieSecret", "type": "string", "sensitive": true}},
				},
			},
		})
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	emit := jsonl.New()
	status, data, err := runConfigExtract(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("expected OK, got %q", status)
	}
	if data["appName"] != "@test/pkg" {
		t.Errorf("appName = %v, want @test/pkg", data["appName"])
	}
	if data["schemaHash"] != "sha256:1234567890abcdef" {
		t.Errorf("schemaHash = %v", data["schemaHash"])
	}
	if data["blocks"] != 2 {
		t.Errorf("blocks = %v, want 2", data["blocks"])
	}
	wantJSON := filepath.Join(projectPath, ".gen/config-schema.json")
	// The companion path mirrors the manifest path with `.jsonschema.json`.
	if data["jsonSchema"] == wantJSON {
		t.Errorf("companion path collided with fallback manifest path: %v", data["jsonSchema"])
	}
}

func TestRunConfigExtract_ForwardsGenerateOptionsToHookContext(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Project.Options = map[string]json.RawMessage{
		"generate": json.RawMessage(`{"schema":false}`),
	}
	projectPath := filepath.Join(dir, "project")

	depDir := filepath.Join(projectPath, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`), 0644)
	os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		contextPath := ""
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--putnami-context" {
				contextPath = args[i+1]
			}
		}
		if contextPath == "" {
			t.Fatal("missing --putnami-context arg")
		}
		data, err := os.ReadFile(contextPath)
		if err != nil {
			t.Fatalf("read hook context: %v", err)
		}
		var hookCtx struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(data, &hookCtx); err != nil {
			t.Fatalf("parse hook context: %v", err)
		}
		if hookCtx.Config["schema"] != false {
			t.Fatalf("hook config schema = %v, want false", hookCtx.Config["schema"])
		}
		_ = writeJSON(filepath.Join(projectPath, configSchemaFallbackPath), map[string]any{
			"appName":    "@test/pkg",
			"version":    "1.0.0",
			"schemaHash": "sha256:fallback",
			"configs":    []any{map[string]any{"path": "app", "fields": []any{}}},
		})
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	emit := jsonl.New()
	status, data, err := runConfigExtract(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("expected OK, got %q", status)
	}
	if data["schema"] != filepath.Join(projectPath, configSchemaFallbackPath) {
		t.Fatalf("schema = %v, want fallback path", data["schema"])
	}
}

func TestRunConfigExtract_HookFailurePropagates(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	depDir := filepath.Join(projectPath, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`), 0644)
	os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "schema extraction failed"}, nil
	})

	emit := jsonl.New()
	status, _, err := runConfigExtract(ctx, emit, nil)
	if err == nil {
		t.Fatal("expected error when hook fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

// ---- stale-manifest fail-loud guards ----

// registerFakeConfigExtractHook wires a minimal @putnami/application
// dependency whose manifest declares a configExtract hook, so RunHooks
// discovers exactly one hook for the test project.
func registerFakeConfigExtractHook(t *testing.T, projectPath string) {
	t.Helper()
	depDir := filepath.Join(projectPath, "node_modules", "@putnami", "application")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWriteFile(t, filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`))
	mustWriteFile(t, filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","dependencies":{"@putnami/application":"workspace:*"}}`))
}

func staleManifestJSON() map[string]any {
	return map[string]any{
		"appName": "auth-server",
		"version": "",
		// No schemaHash: mimics a manifest written by an older extractor —
		// the exact artifact behind the "Extracted 5 config blocks ()" log.
		"configs": []any{
			map[string]any{"path": "security.auth", "fields": []any{}},
			map[string]any{"path": "security.session", "fields": []any{}},
		},
	}
}

func TestRunConfigExtract_FailsWhenManifestExistsButNoHookRegistered(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	mustWriteJSON(t, projectPath, configSchemaDefaultPath, staleManifestJSON())

	emit := jsonl.New()
	status, _, err := runConfigExtract(ctx, emit, nil)
	if err == nil {
		t.Fatal("expected error: a committed manifest without a registered hook cannot be regenerated")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunConfigExtract_FailsWhenHookReportsEmptyButManifestExists(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	registerFakeConfigExtractHook(t, projectPath)
	mustWriteJSON(t, projectPath, configSchemaDefaultPath, staleManifestJSON())

	// The hook runs but reports it registered nothing — the manifest on disk
	// is therefore a stale leftover, and re-reporting it as "Extracted N
	// config blocks" is the silent no-op this guard prevents.
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0,
			Stdout: `{"type":"summary","data":{"exports":{},"assets":{},"status":"empty"}}` + "\n"}, nil
	})

	emit := jsonl.New()
	status, _, err := runConfigExtract(ctx, emit, nil)
	if err == nil {
		t.Fatal("expected error when hook reports empty but a manifest with blocks exists")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunConfigExtract_TrustsOkStatusWithManifest(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	registerFakeConfigExtractHook(t, projectPath)

	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		_ = writeJSON(filepath.Join(projectPath, configSchemaDefaultPath), map[string]any{
			"appName":    "@test/pkg",
			"version":    "1.0.0",
			"schemaHash": "sha256:fresh",
			"configs":    []any{map[string]any{"path": "server", "fields": []any{}}},
		})
		return &exec.Result{Success: true, ExitCode: 0,
			Stdout: `{"type":"summary","data":{"exports":{},"assets":{},"status":"ok"}}` + "\n"}, nil
	})

	emit := jsonl.New()
	status, data, err := runConfigExtract(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Fatalf("expected OK, got %q", status)
	}
	if data["schemaHash"] != "sha256:fresh" {
		t.Errorf("schemaHash = %v, want sha256:fresh", data["schemaHash"])
	}
}

func TestStaleHookStatus(t *testing.T) {
	cases := []struct {
		name     string
		statuses []string
		want     string
	}{
		{"ok trusted", []string{"ok"}, ""},
		{"empty is stale", []string{"empty"}, "empty"},
		{"skipped is stale", []string{"skipped"}, "skipped"},
		{"ok wins over empty", []string{"empty", "ok"}, ""},
		{"legacy no status trusted", []string{""}, ""},
		{"unknown status trusted", []string{"weird"}, ""},
		{"no hooks", nil, ""},
	}
	for _, tc := range cases {
		if got := staleHookStatus(tc.statuses); got != tc.want {
			t.Errorf("%s: staleHookStatus(%v) = %q, want %q", tc.name, tc.statuses, got, tc.want)
		}
	}
}

// ---- helpers ----

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func mustWriteJSON(t *testing.T, dir, relPath string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mustWriteFile(t, filepath.Join(dir, relPath), data)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
