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
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// TestMain removes the program the install hook tests place (fixtureproc),
// which stands in for an extension's onInstall command on every platform.
func TestMain(m *testing.M) {
	code := m.Run()
	fixtureproc.Remove()
	os.Exit(code)
}

// writeInstallHookProgram places an onInstall program in extDir and returns
// the manifest command that runs it. The program records each run in marker,
// so the marker exists once the hook ran.
func writeInstallHookProgram(t *testing.T, extDir, marker string, p fixtureproc.Program) string {
	t.Helper()
	p.Record = marker
	return "{extensionRoot}/" + filepath.Base(fixtureproc.Write(t, filepath.Join(extDir, "on-install"), p))
}

// sameFile reports whether a and b name the same existing file.
func sameFile(a, b string) bool {
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(aInfo, bInfo)
}

// wantHookRun fails t unless marker recorded one onInstall run, started with
// marker as its argument, for the extension at extDir.
func wantHookRun(t *testing.T, marker, extDir string) {
	t.Helper()
	runs := fixtureproc.Runs(t, marker)
	if len(runs) != 1 {
		t.Fatalf("the onInstall hook ran %d times, want once", len(runs))
	}
	run := runs[0]
	// The workspace root reaches the hook canonicalized (/var → /private/var
	// on macOS), so compare the files the paths name.
	if len(run.Args) != 1 || !sameFile(run.Args[0], marker) {
		t.Errorf("hook args = %q, want the marker path %s alone", run.Args, marker)
	}
	for name, want := range map[string]string{"PUTNAMI_HOOK": "onInstall", "PUTNAMI_EXTENSION_ROOT": extDir} {
		if got, ok := run.LookupEnv(name); !ok || got != want {
			t.Errorf("%s = %q (set %v), want %q", name, got, ok, want)
		}
	}
}

func TestParseExtensionArg(t *testing.T) {
	tests := []struct {
		input       string
		wantName    string
		wantVersion string
	}{
		{"@putnami/go", "@putnami/go", ""},
		{"@putnami/go@1.3.0", "@putnami/go", "1.3.0"},
		{"@putnami/typescript@^2.0.0", "@putnami/typescript", "^2.0.0"},
		{"simple-ext", "simple-ext", ""},
		{"simple-ext@1.0.0", "simple-ext", "1.0.0"},
		{"/Users/me/@local/ext", "/Users/me/@local/ext", ""},
		{"./extensions/@local/ext", "./extensions/@local/ext", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			name, version := parseExtensionArg(tt.input)
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if version != tt.wantVersion {
				t.Errorf("version = %q, want %q", version, tt.wantVersion)
			}
		})
	}
}

func TestBuildExtensionMap(t *testing.T) {
	tests := []struct {
		name    string
		extList map[string]string
		wantLen int
	}{
		{
			"empty",
			nil,
			0,
		},
		{
			"simple names",
			map[string]string{"@putnami/go": "", "@putnami/typescript": ""},
			2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &wsproto.Config{
				Extensions: wsproto.ExtensionsConfig{List: tt.extList},
			}
			got := shared.BuildExtensionMap(cfg)
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestExtensionsInstallSkipsLocalWorkspaceRefsWithLatest(t *testing.T) {
	dir := t.TempDir()
	extDir := filepath.Join(dir, "extensions", "local")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(`{"commands":{}}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@local/ext"}`), 0o644); err != nil {
		t.Fatalf("write putnami.json: %v", err)
	}

	t.Setenv("PUTNAMI_REGISTRY_URL", "http://127.0.0.1:1")

	cfg := &wsproto.Config{
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{
			"/extensions/local": "",
		}},
	}

	if err := ExtensionsInstall(context.Background(), dir, cfg, []string{"--latest"}, ""); err != nil {
		t.Fatalf("ExtensionsInstall: %v", err)
	}
}

func TestExtensionsInstallJSONLSuppressesLocalHumanOutput(t *testing.T) {
	dir := t.TempDir()
	extDir := filepath.Join(dir, "extensions", "local")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(`{"name":"@local/ext","commands":{}}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@local/ext"}`), 0o644); err != nil {
		t.Fatalf("write putnami.json: %v", err)
	}

	cfg := &wsproto.Config{
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{
			"/extensions/local": "",
		}},
	}

	out, err := sharedtest.CaptureStdout(t, func() error {
		return ExtensionsInstall(context.Background(), dir, cfg, nil, "jsonl")
	})
	if err != nil {
		t.Fatalf("ExtensionsInstall: %v", err)
	}
	if strings.Contains(out, "✓") || strings.Contains(out, "(local)") {
		t.Fatalf("stdout = %q, want only JSONL entries", out)
	}
	var entry struct {
		Kind   string `json:"kind"`
		Action string `json:"action"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entry); err != nil {
		t.Fatalf("parse jsonl %q: %v", out, err)
	}
	if entry.Kind != "extension" || entry.Action != "install" || entry.Name != "/extensions/local" || entry.Status != "local" {
		t.Fatalf("entry = %+v, want extension/install/local", entry)
	}
}

func TestExtensionsInstallRunsOnInstallHookForLocalExtension(t *testing.T) {
	dir := t.TempDir()
	extDir := filepath.Join(dir, "extensions", "cloud")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	marker := filepath.Join(dir, "hook-ran.txt")
	hookCommand := writeInstallHookProgram(t, extDir, marker, fixtureproc.Program{})
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(`{
  "name": "@local/cloud",
  "hooks": {
    "onInstall": {
      "kind": "command",
      "command": "`+hookCommand+`",
      "args": ["{workspaceRoot}/hook-ran.txt"],
      "cwd": "{workspaceRoot}"
    }
  },
  "cliContract": 4,
  "commands": {
    "noop": {
      "visibility": "internal",
      "run": [{ "id": "noop", "task": "noop-exec" }]
    }
  },
  "tasks": {
    "noop-exec": {
      "kind": "command",
      "command": "true"
    }
  }
}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := &wsproto.Config{
		Name: "hook-ws",
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{
			"/extensions/cloud": "",
		}},
	}
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name":       "hook-ws",
		"extensions": []string{"/extensions/cloud"},
	})

	out, err := sharedtest.CaptureStdout(t, func() error {
		return ExtensionsInstall(context.Background(), dir, cfg, nil, "")
	})
	if err != nil {
		t.Fatalf("ExtensionsInstall: %v", err)
	}
	if !strings.Contains(out, "Running extension install hooks") {
		t.Fatalf("output = %q, want install hook phase", out)
	}

	wantHookRun(t, marker, extDir)
}

func TestExtensionsInstallJSONLRedirectsInstallHookOutput(t *testing.T) {
	dir := t.TempDir()
	extDir := filepath.Join(dir, "extensions", "cloud")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	marker := filepath.Join(dir, "hook-ran.txt")
	hookCommand := writeInstallHookProgram(t, extDir, marker, fixtureproc.Program{
		Stdout: "hook stdout should not be on stdout\n",
	})
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(`{
  "name": "@local/cloud",
  "hooks": {
    "onInstall": {
      "kind": "command",
      "command": "`+hookCommand+`",
      "args": ["{workspaceRoot}/hook-ran.txt"],
      "cwd": "{workspaceRoot}"
    }
  },
  "commands": {}
}`), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := &wsproto.Config{
		Name: "hook-ws",
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{
			"/extensions/cloud": "",
		}},
	}
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name":       "hook-ws",
		"extensions": []string{"/extensions/cloud"},
	})

	var out string
	stderr := sharedtest.CaptureStderr(t, func() {
		var err error
		out, err = sharedtest.CaptureStdout(t, func() error {
			return ExtensionsInstall(context.Background(), dir, cfg, nil, "jsonl")
		})
		if err != nil {
			t.Fatalf("ExtensionsInstall: %v", err)
		}
	})

	if strings.Contains(out, "Running extension install hooks") || strings.Contains(out, "hook stdout") {
		t.Fatalf("stdout = %q, want only JSONL entries", out)
	}
	if !strings.Contains(stderr, "Running extension install hooks") || !strings.Contains(stderr, "hook stdout should not be on stdout") {
		t.Fatalf("stderr = %q, want hook progress and hook stdout", stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", line, err)
		}
	}

	wantHookRun(t, marker, extDir)
}

func TestExtensionsInstallSpecificExtensionRunsOnlyThatHook(t *testing.T) {
	dir := t.TempDir()
	targetDir := writeLocalInstallHookExtension(t, dir, "target", "@local/target", "target-ran.txt")
	writeLocalInstallHookExtension(t, dir, "other", "@local/other", "other-ran.txt")

	cfg := &wsproto.Config{
		Name: "hook-ws",
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{
			"/extensions/target": "",
			"/extensions/other":  "",
		}},
	}
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name":       "hook-ws",
		"extensions": []string{"/extensions/target", "/extensions/other"},
	})

	out, err := sharedtest.CaptureStdout(t, func() error {
		return ExtensionsInstall(context.Background(), dir, cfg, []string{"/extensions/target"}, "")
	})
	if err != nil {
		t.Fatalf("ExtensionsInstall: %v", err)
	}
	if !strings.Contains(out, "@local/target onInstall") {
		t.Fatalf("output = %q, want target hook", out)
	}
	if strings.Contains(out, "@local/other onInstall") {
		t.Fatalf("output = %q, should not run other hook", out)
	}

	wantHookRun(t, filepath.Join(dir, "target-ran.txt"), targetDir)
	if _, err := os.Stat(filepath.Join(dir, "other-ran.txt")); !os.IsNotExist(err) {
		t.Fatalf("other hook marker exists or stat failed: %v", err)
	}
}

func TestResolveLocalExtensionRefReportsMissingWorkspacePath(t *testing.T) {
	dir := t.TempDir()

	ext, err := resolveLocalExtensionRef(dir, "/missing")
	if err == nil {
		t.Fatal("resolveLocalExtensionRef returned nil error, want failure")
	}
	if ext != nil {
		t.Fatalf("resolveLocalExtensionRef extension = %#v, want nil", ext)
	}
	if !strings.Contains(err.Error(), "local extension path") {
		t.Fatalf("resolveLocalExtensionRef error = %v, want local path failure", err)
	}
}

func writeLocalInstallHookExtension(t *testing.T, root, name, manifestName, markerName string) string {
	t.Helper()

	extDir := filepath.Join(root, "extensions", name)
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	hookCommand := writeInstallHookProgram(t, extDir, filepath.Join(root, markerName), fixtureproc.Program{})
	manifest := `{
  "name": "` + manifestName + `",
  "cliContract": 4,
  "hooks": {
    "onInstall": {
      "kind": "command",
      "command": "` + hookCommand + `",
      "args": ["{workspaceRoot}/` + markerName + `"],
      "cwd": "{workspaceRoot}"
    }
  },
  "commands": {
    "noop": {
      "visibility": "internal",
      "run": [{ "id": "noop", "task": "noop-exec" }]
    }
  },
  "tasks": {
    "noop-exec": {
      "kind": "command",
      "command": "true"
    }
  }
}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return extDir
}
