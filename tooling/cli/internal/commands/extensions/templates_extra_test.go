package extensions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

func TestParseTemplateArg(t *testing.T) {
	tests := []struct {
		input       string
		wantName    string
		wantVersion string
	}{
		{"basic", "basic", ""},
		{"basic@1.2.3", "basic", "1.2.3"},
		{"basic@^2.0.0", "basic", "^2.0.0"},
		{"", "", ""},
		{"a@b@c", "a", "b@c"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			name, version := parseTemplateArg(tt.input)
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if version != tt.wantVersion {
				t.Errorf("version = %q, want %q", version, tt.wantVersion)
			}
		})
	}
}

func TestBuildTemplateMap(t *testing.T) {
	tests := []struct {
		name      string
		templates []string
		want      map[string]string
	}{
		{"empty", nil, map[string]string{}},
		{
			"defaults to latest",
			[]string{"alpha"},
			map[string]string{"alpha": "latest"},
		},
		{
			"explicit constraint via colon",
			[]string{"alpha:1.0.0", "beta:^2.0"},
			map[string]string{"alpha": "1.0.0", "beta": "^2.0"},
		},
		{
			"mixed",
			[]string{"alpha", "beta:2.0.0"},
			map[string]string{"alpha": "latest", "beta": "2.0.0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &wsproto.Config{Templates: tt.templates}
			got := shared.BuildTemplateMap(cfg)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("map[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestResolveTemplateManifestPath(t *testing.T) {
	t.Run("explicit file path", func(t *testing.T) {
		dir := t.TempDir()
		manifest := filepath.Join(dir, "putnami.template.json")
		if err := os.WriteFile(manifest, []byte(`{"name":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := resolveTemplateManifestPath([]string{manifest})
		if err != nil {
			t.Fatalf("resolveTemplateManifestPath: %v", err)
		}
		if got != manifest {
			t.Errorf("path = %q, want %q", got, manifest)
		}
	})

	t.Run("directory arg resolves manifest", func(t *testing.T) {
		dir := t.TempDir()
		manifest := filepath.Join(dir, "putnami.template.json")
		if err := os.WriteFile(manifest, []byte(`{"name":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := resolveTemplateManifestPath([]string{dir})
		if err != nil {
			t.Fatalf("resolveTemplateManifestPath: %v", err)
		}
		if got != manifest {
			t.Errorf("path = %q, want %q", got, manifest)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		_, err := resolveTemplateManifestPath([]string{filepath.Join(t.TempDir(), "nope")})
		if err == nil || !strings.Contains(err.Error(), "path not found") {
			t.Fatalf("err = %v, want path not found", err)
		}
	})

	t.Run("directory without manifest", func(t *testing.T) {
		dir := t.TempDir()
		_, err := resolveTemplateManifestPath([]string{dir})
		if err == nil || !strings.Contains(err.Error(), "no putnami.template.json") {
			t.Fatalf("err = %v, want missing manifest", err)
		}
	})

	t.Run("cwd lookup finds manifest", func(t *testing.T) {
		dir := t.TempDir()
		manifest := filepath.Join(dir, "putnami.template.json")
		if err := os.WriteFile(manifest, []byte(`{"name":"x"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		withWorkingDir(t, dir)
		got, err := resolveTemplateManifestPath(nil)
		if err != nil {
			t.Fatalf("resolveTemplateManifestPath: %v", err)
		}
		if filepath.Base(got) != "putnami.template.json" {
			t.Errorf("path = %q, want putnami.template.json", got)
		}
	})

	t.Run("cwd lookup with no manifest", func(t *testing.T) {
		withWorkingDir(t, t.TempDir())
		_, err := resolveTemplateManifestPath(nil)
		if err == nil || !strings.Contains(err.Error(), "no putnami.template.json in current directory") {
			t.Fatalf("err = %v, want missing manifest in cwd", err)
		}
	})
}

func TestFindTemplateFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go.template"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "go.mod.template"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := findTemplateFiles(dir)
	if err != nil {
		t.Fatalf("findTemplateFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("found %d template files, want 2 (%v)", len(files), files)
	}
}

func TestFindTemplateFiles_NoTemplates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := findTemplateFiles(dir)
	if err != nil {
		t.Fatalf("findTemplateFiles: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("found %d template files, want 0", len(files))
	}
}

func TestTemplatesValidate_Success(t *testing.T) {
	dir := t.TempDir()
	manifest := map[string]any{
		"name":        "sample-template",
		"version":     "1.0.0",
		"description": "A sample template",
	}
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "putnami.template.json"), manifest)
	if err := os.WriteFile(filepath.Join(dir, "main.go.template"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return TemplatesValidate([]string{dir}) })
	if err != nil {
		t.Fatalf("TemplatesValidate: %v", err)
	}
	if !strings.Contains(out, "Manifest parsed and validated") {
		t.Errorf("output = %q, want parsed message", out)
	}
	if !strings.Contains(out, "template file(s) found") {
		t.Errorf("output = %q, want template file count", out)
	}
	if !strings.Contains(out, "Template is valid.") {
		t.Errorf("output = %q, want valid message", out)
	}
}

func TestTemplatesValidate_NoTemplateFilesOK(t *testing.T) {
	dir := t.TempDir()
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "putnami.template.json"), map[string]any{
		"name":        "no-files",
		"version":     "1.0.0",
		"description": "no template files",
	})

	out, err := sharedtest.CaptureStdout(t, func() error { return TemplatesValidate([]string{dir}) })
	if err != nil {
		t.Fatalf("TemplatesValidate: %v", err)
	}
	if !strings.Contains(out, "No .template files found") {
		t.Errorf("output = %q, want optional no-files note", out)
	}
}

func TestTemplatesValidate_InvalidManifest(t *testing.T) {
	dir := t.TempDir()
	// Missing required "name" should fail strict validation.
	if err := os.WriteFile(filepath.Join(dir, "putnami.template.json"), []byte(`{"version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return TemplatesValidate([]string{dir}) })
	if err == nil {
		t.Fatal("expected error for invalid manifest")
	}
	if !strings.Contains(out, "Validation failed") {
		t.Errorf("output = %q, want validation failure block", out)
	}
}

func TestTemplatesValidate_DiagnosticsIncludeManifestPath(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "putnami.template.json")
	// Missing required "name" triggers a validation diagnostic.
	if err := os.WriteFile(manifestPath, []byte(`{"version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return TemplatesValidate([]string{manifestPath}) })
	if err == nil {
		t.Fatal("expected error for invalid manifest")
	}
	// Each diagnostic line must carry the originating manifest path, matching
	// the file:line rendering used by `sessions inspect`.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			if !strings.Contains(line, manifestPath) {
				t.Errorf("diagnostic line %q omits manifest path %q", line, manifestPath)
			}
		}
	}
}

func TestTemplatesValidate_MissingPath(t *testing.T) {
	err := TemplatesValidate([]string{filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), "path not found") {
		t.Fatalf("err = %v, want path not found", err)
	}
}

func TestTemplatesPackage_Success(t *testing.T) {
	dir := t.TempDir()
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "putnami.template.json"), map[string]any{
		"name":        "pkg-template",
		"version":     "0.1.0",
		"description": "packaged template",
	})
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# readme"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LICENSE.md"), []byte("MIT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go.template"), []byte("package {{.Name}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(t.TempDir(), "out")

	out, err := sharedtest.CaptureStdout(t, func() error {
		return TemplatesPackage(context.Background(), []string{dir, "--output", outDir, "--version", "1.2.3"})
	})
	if err != nil {
		t.Fatalf("TemplatesPackage: %v", err)
	}
	if !strings.Contains(out, "Version: 1.2.3") {
		t.Errorf("output = %q, want stamped version", out)
	}
	if !strings.Contains(out, "Archive:") || !strings.Contains(out, "Integrity:") {
		t.Errorf("output = %q, want archive+integrity lines", out)
	}
}

func TestTemplatesPackage_MissingManifest(t *testing.T) {
	err := TemplatesPackage(context.Background(), []string{filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), "path not found") {
		t.Fatalf("err = %v, want path not found", err)
	}
}

func TestTemplatesList_TextEmpty(t *testing.T) {
	dir := t.TempDir()
	cfg := &wsproto.Config{}

	out, err := sharedtest.CaptureStdout(t, func() error { return TemplatesList(dir, cfg, "") })
	if err != nil {
		t.Fatalf("TemplatesList: %v", err)
	}
	if !strings.Contains(out, "TEMPLATE") || !strings.Contains(out, "CONSTRAINT") {
		t.Errorf("output = %q, want table header", out)
	}
}

func TestTemplatesList_TextWithConfigAndLock(t *testing.T) {
	dir := t.TempDir()
	cfg := &wsproto.Config{Templates: []string{"alpha:1.0.0"}}
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "putnami.lock.json"), map[string]any{
		"version":   2,
		"templates": map[string]any{"alpha": map[string]any{"version": "1.0.0"}},
	})

	out, err := sharedtest.CaptureStdout(t, func() error { return TemplatesList(dir, cfg, "") })
	if err != nil {
		t.Fatalf("TemplatesList: %v", err)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "config") {
		t.Errorf("output = %q, want configured template row", out)
	}
}

func TestTemplatesList_JSONL(t *testing.T) {
	dir := t.TempDir()
	cfg := &wsproto.Config{Templates: []string{"alpha:1.0.0"}}

	out, err := sharedtest.CaptureStdout(t, func() error { return TemplatesList(dir, cfg, "jsonl") })
	if err != nil {
		t.Fatalf("TemplatesList: %v", err)
	}
	line := strings.TrimSpace(out)
	var entry struct {
		Name       string `json:"name"`
		Constraint string `json:"constraint"`
		Source     string `json:"source"`
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("parse jsonl %q: %v", line, err)
	}
	if entry.Name != "alpha" || entry.Constraint != "1.0.0" || entry.Source != "config" {
		t.Errorf("entry = %+v, want alpha/1.0.0/config", entry)
	}
}

func TestTemplatesRemove_RequiresName(t *testing.T) {
	err := TemplatesRemove(t.TempDir(), &wsproto.Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "template name required") {
		t.Fatalf("err = %v, want name-required error", err)
	}
}

func TestTemplatesRemove_RemovesLockEntry(t *testing.T) {
	dir := t.TempDir()
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "putnami.lock.json"), map[string]any{
		"version":   2,
		"templates": map[string]any{"alpha": map[string]any{"version": "1.0.0"}},
	})

	out, err := sharedtest.CaptureStdout(t, func() error {
		return TemplatesRemove(dir, &wsproto.Config{}, []string{"alpha"})
	})
	if err != nil {
		t.Fatalf("TemplatesRemove: %v", err)
	}
	if !strings.Contains(out, "Removed alpha") {
		t.Errorf("output = %q, want removed message", out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "putnami.lock.json"))
	if strings.Contains(string(data), `"alpha"`) {
		t.Errorf("lock file still references alpha: %s", data)
	}
}

func TestTemplatesTest_SkipBuild(t *testing.T) {
	dir := t.TempDir()
	// Manifest with a single .template that renders a putnami.json so the
	// rendered project passes the structure check.
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "putnami.template.json"), map[string]any{
		"name":    "tpl-test",
		"version": "1.0.0",
	})
	if err := os.WriteFile(filepath.Join(dir, "putnami.json.template"), []byte(`{"name":"{{.ProjectName}}"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error {
		return TemplatesTest(context.Background(), []string{dir, "--skip-build"})
	})
	if err != nil {
		t.Fatalf("TemplatesTest: %v", err)
	}
	if !strings.Contains(out, "Render template") {
		t.Errorf("output = %q, want render phase", out)
	}
	if !strings.Contains(out, "Skipping build+test") {
		t.Errorf("output = %q, want skip-build note", out)
	}
	if !strings.Contains(out, "is valid.") {
		t.Errorf("output = %q, want valid message", out)
	}
}

func TestTemplatesTest_RenderedProjectMissingConfig(t *testing.T) {
	dir := t.TempDir()
	// No putnami.json rendered → structure validation must fail.
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "putnami.template.json"), map[string]any{
		"name":    "broken-tpl",
		"version": "1.0.0",
	})
	if err := os.WriteFile(filepath.Join(dir, "main.go.template"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := sharedtest.CaptureStdout(t, func() error {
		return TemplatesTest(context.Background(), []string{dir, "--skip-build"})
	})
	if err == nil || !strings.Contains(err.Error(), "no putnami.json") {
		t.Fatalf("err = %v, want missing putnami.json error", err)
	}
}

func TestTemplatesTest_InvalidManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.template.json"), []byte(`{bad json`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := TemplatesTest(context.Background(), []string{dir, "--skip-build"})
	if err == nil || !strings.Contains(err.Error(), "template manifest is invalid") {
		t.Fatalf("err = %v, want invalid manifest error", err)
	}
}

func TestTemplatesInstall_NoTemplatesConfigured(t *testing.T) {
	dir := t.TempDir()
	out, err := sharedtest.CaptureStdout(t, func() error {
		return TemplatesInstall(context.Background(), dir, &wsproto.Config{}, nil, "")
	})
	if err != nil {
		t.Fatalf("TemplatesInstall: %v", err)
	}
	if !strings.Contains(out, "No templates configured.") {
		t.Errorf("output = %q, want no-templates message", out)
	}
}

func TestTemplatesInstall_FailureReported(t *testing.T) {
	dir := t.TempDir()
	// Point the registry at an unreachable address so the install fails fast
	// and the failure path (counter + non-nil error) is exercised.
	t.Setenv("PUTNAMI_REGISTRY_URL", "http://127.0.0.1:1")
	// Dead registry refuses instantly; skip the retry backoff so the test
	// doesn't pay the full (1s+2s) budget waiting on a host that won't recover.
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	cfg := &wsproto.Config{Templates: []string{"alpha:1.0.0"}}

	out, err := sharedtest.CaptureStdout(t, func() error {
		return TemplatesInstall(context.Background(), dir, cfg, nil, "")
	})
	if err == nil || !strings.Contains(err.Error(), "failed to install") {
		t.Fatalf("err = %v, want install failure", err)
	}
	if !strings.Contains(out, "0 installed, 0 cached, 1 failed") {
		t.Errorf("output = %q, want failure summary", out)
	}
}

func TestTemplatesInstall_JSONLEmitsFailureEntry(t *testing.T) {
	dir := t.TempDir()
	// Dead registry → install fails fast; assert the JSONL failure entry is
	// emitted and the human progress/summary lines are suppressed.
	t.Setenv("PUTNAMI_REGISTRY_URL", "http://127.0.0.1:1")
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	cfg := &wsproto.Config{Templates: []string{"alpha:1.0.0"}}

	out, err := sharedtest.CaptureStdout(t, func() error {
		return TemplatesInstall(context.Background(), dir, cfg, nil, "jsonl")
	})
	if err == nil || !strings.Contains(err.Error(), "failed to install") {
		t.Fatalf("err = %v, want install failure", err)
	}
	line := strings.TrimSpace(out)
	if line == "" {
		t.Fatal("expected a JSONL line, got empty output")
	}
	var entry struct {
		Kind   string `json:"kind"`
		Action string `json:"action"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if jErr := json.Unmarshal([]byte(line), &entry); jErr != nil {
		t.Fatalf("parse jsonl %q: %v", line, jErr)
	}
	if entry.Kind != "template" || entry.Action != "install" || entry.Name != "alpha" || entry.Status != "failed" {
		t.Errorf("entry = %+v, want template/install/alpha/failed", entry)
	}
	if entry.Error == "" {
		t.Errorf("entry.Error empty, want failure reason")
	}
	// Human summary must not leak into the JSONL stream.
	if strings.Contains(out, "installed,") || strings.Contains(out, "✓") {
		t.Errorf("output = %q, want no human summary lines", out)
	}
}

func TestTemplatesUpdate_NoTemplatesConfigured(t *testing.T) {
	dir := t.TempDir()
	out, err := sharedtest.CaptureStdout(t, func() error {
		return TemplatesUpdate(context.Background(), dir, &wsproto.Config{}, nil, "")
	})
	if err != nil {
		t.Fatalf("TemplatesUpdate: %v", err)
	}
	if !strings.Contains(out, "No templates configured.") {
		t.Errorf("output = %q, want no-templates message", out)
	}
}

func TestTemplatesUpdate_FailureSkipsButReturnsNil(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PUTNAMI_REGISTRY_URL", "http://127.0.0.1:1")
	// Dead registry refuses instantly; skip the retry backoff so the test
	// doesn't pay the full (1s+2s) budget waiting on a host that won't recover.
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	cfg := &wsproto.Config{Templates: []string{"alpha:1.0.0"}}

	out, err := sharedtest.CaptureStdout(t, func() error {
		return TemplatesUpdate(context.Background(), dir, cfg, nil, "")
	})
	// TemplatesUpdate logs failures to stderr but does not return an error.
	if err != nil {
		t.Fatalf("TemplatesUpdate: %v", err)
	}
	if !strings.Contains(out, "0 updated") {
		t.Errorf("output = %q, want updated summary", out)
	}
}

// withWorkingDir chdirs into dir for the duration of the test and restores the
// previous working directory afterward.
func withWorkingDir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}
