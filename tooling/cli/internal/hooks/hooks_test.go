package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestMain lets this test binary stand in for the extension commands the
// hooks run (fixtureproc), so those tests need no shell on any platform.
func TestMain(m *testing.M) {
	code := m.Run()
	fixtureproc.Remove()
	os.Exit(code)
}

// onlyRun returns the one run recorded in record.
func onlyRun(t *testing.T, record string) fixtureproc.Run {
	t.Helper()
	runs := fixtureproc.Runs(t, record)
	if len(runs) != 1 {
		t.Fatalf("the program ran %d times, want once", len(runs))
	}
	return runs[0]
}

// wantEnv fails t unless the run saw name set to want.
func wantEnv(t *testing.T, run fixtureproc.Run, name, want string) {
	t.Helper()
	if got, ok := run.LookupEnv(name); !ok || got != want {
		t.Errorf("%s = %q (set %v), want %q", name, got, ok, want)
	}
}

func TestExtractStringMap(t *testing.T) {
	data := map[string]any{
		"exports": map[string]any{
			"key1": "val1",
			"key2": "val2",
		},
	}

	result := extractStringMap(data, "exports")
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	if result["key1"] != "val1" {
		t.Errorf("key1 = %q, want %q", result["key1"], "val1")
	}
	if result["key2"] != "val2" {
		t.Errorf("key2 = %q, want %q", result["key2"], "val2")
	}
}

func TestExtractStringMap_Missing(t *testing.T) {
	data := map[string]any{}
	result := extractStringMap(data, "exports")
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestExtractStringMap_WrongType(t *testing.T) {
	data := map[string]any{
		"exports": "not-a-map",
	}
	result := extractStringMap(data, "exports")
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestExtractStringMap_NonStringValues(t *testing.T) {
	data := map[string]any{
		"exports": map[string]any{
			"key1": "val1",
			"key2": 42, // should be skipped
		},
	}

	result := extractStringMap(data, "exports")
	if len(result) != 1 {
		t.Fatalf("len = %d, want 1 (non-string skipped)", len(result))
	}
	if result["key1"] != "val1" {
		t.Errorf("key1 = %q, want %q", result["key1"], "val1")
	}
}

func TestSanitizeSegment(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"with-dashes", "with-dashes"},
		{"with_underscores", "with_underscores"},
		{"CamelCase", "CamelCase"},
		{"@putnami/go", "-putnami-go"},
		{"my.package", "my-package"},
		{"has spaces", "has-spaces"},
		{"special!@#chars", "special---chars"},
	}

	for _, tt := range tests {
		got := sanitizeSegment(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeSegment(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestHookCacheRoot(t *testing.T) {
	root := hookCacheRoot("/workspace", "@putnami/app", "@putnami/go")
	if !strings.HasPrefix(root, filepath.FromSlash("/workspace/.putnami/projects/")) {
		t.Errorf("hookCacheRoot = %q, expected to start with /workspace/.putnami/projects/", root)
	}
	if !strings.Contains(root, "-putnami-app") {
		t.Errorf("hookCacheRoot = %q, expected sanitized project name", root)
	}
	if !strings.Contains(root, "-putnami-go") {
		t.Errorf("hookCacheRoot = %q, expected sanitized extension name", root)
	}
}

func TestInstallHookCacheRoot(t *testing.T) {
	root := installHookCacheRoot("/workspace", "@putnami/cloud")
	if !strings.HasPrefix(root, filepath.FromSlash("/workspace/.putnami/cache/install-hooks/")) {
		t.Errorf("installHookCacheRoot = %q, expected install hook cache root", root)
	}
	if !strings.Contains(root, "-putnami-cloud") {
		t.Errorf("installHookCacheRoot = %q, expected sanitized extension name", root)
	}
}

func TestWriteHookContext(t *testing.T) {
	dir := t.TempDir()

	hctx := &hookContext{
		WorkspaceRoot: "/workspace",
		ProjectRoot:   "/workspace/app",
		ExtensionRoot: "/workspace/ext",
		Hook:          "preBuild",
		Extension:     "@putnami/go",
	}

	path, err := writeHookContext(hctx, dir)
	if err != nil {
		t.Fatalf("writeHookContext: %v", err)
	}
	if path == "" {
		t.Fatal("path should not be empty")
	}

	// Verify file was created
	if !fileExists(path) {
		t.Error("context file should exist")
	}
}

func TestRunOnInstallHookWorkspaceScoped(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "runs.jsonl")
	marker := filepath.Join(dir, "marker.txt")
	script := fixtureproc.Write(t, filepath.Join(dir, "on-install"), fixtureproc.Program{
		Record:  record,
		PathEnv: []string{"PUTNAMI_WORKSPACE_ROOT", "PUTNAMI_HOOK_CONTEXT"},
	})

	ws := workspace.NewWorkspace(dir, &wsproto.Config{Name: "test-ws"}, nil)
	ext := &extension.ExtensionDescription{
		Name: "@putnami/cloud",
		Path: filepath.Join(dir, "ext"),
		Hooks: &extension.ManifestHooks{
			OnInstall: &extension.HookDefinition{
				Kind:    "command",
				Command: script,
				Args:    []string{"{workspaceRoot}/marker.txt"},
				Env:     map[string]string{"HOOK_EXT": "{extensionRoot}"},
				Cwd:     "{workspaceRoot}",
			},
		},
	}

	if err := RunOnInstallHook(context.Background(), ws, ext, false); err != nil {
		t.Fatalf("RunOnInstallHook: %v", err)
	}

	run := onlyRun(t, record)
	if len(run.Args) != 1 || filepath.Clean(run.Args[0]) != marker {
		t.Errorf("args = %q, want the expanded marker path %s alone", run.Args, marker)
	}
	wantEnv(t, run, "PUTNAMI_HOOK", "onInstall")
	if run.EnvKinds["PUTNAMI_WORKSPACE_ROOT"] != "dir" || run.EnvKinds["PUTNAMI_HOOK_CONTEXT"] != "file" {
		t.Errorf("path kinds = %v, want the workspace root a directory and the hook context a file", run.EnvKinds)
	}
	wantEnv(t, run, "PUTNAMI_EXTENSION_ROOT", ext.Path)
	wantEnv(t, run, "HOOK_EXT", ext.Path)
}

func TestDefaultHookTimeoutMs(t *testing.T) {
	if DefaultHookTimeoutMs != 120_000 {
		t.Errorf("DefaultHookTimeoutMs = %d, want 120000", DefaultHookTimeoutMs)
	}
}

func TestReadHookEvents(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantNil    bool
		wantExport map[string]string
		wantAsset  map[string]string
	}{
		{
			name:    "empty reader",
			input:   "",
			wantNil: true,
		},
		{
			name:    "invalid JSON lines",
			input:   "not json\nalso {bad\n",
			wantNil: true,
		},
		{
			name:    "wrong version",
			input:   `{"v":2,"type":"summary","data":{"exports":{"k":"v"}}}` + "\n",
			wantNil: true,
		},
		{
			name:    "valid event but no summary",
			input:   `{"v":1,"type":"log","data":{"message":"hello"}}` + "\n",
			wantNil: true,
		},
		{
			name:       "summary event with exports and assets",
			input:      `{"v":1,"type":"summary","data":{"exports":{"out":"dist/main.js"},"assets":{"style":"dist/style.css"}}}` + "\n",
			wantNil:    false,
			wantExport: map[string]string{"out": "dist/main.js"},
			wantAsset:  map[string]string{"style": "dist/style.css"},
		},
		{
			name: "multiple events with summary at end",
			input: `{"v":1,"type":"log","data":{"message":"compiling"}}
{"v":1,"type":"progress","data":{"current":1,"total":3}}
{"v":1,"type":"summary","data":{"exports":{"bin":"out/app"}}}
`,
			wantNil:    false,
			wantExport: map[string]string{"bin": "out/app"},
			wantAsset:  nil,
		},
		{
			name: "mixed valid and invalid lines still finds summary",
			input: `garbage line
{"v":1,"type":"log","data":{}}
{totally broken json
{"v":1,"type":"summary","data":{"exports":{"a":"b"},"assets":{"c":"d"}}}
more garbage
`,
			wantNil:    false,
			wantExport: map[string]string{"a": "b"},
			wantAsset:  map[string]string{"c": "d"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := strings.NewReader(tt.input)
			got := readHookEvents(r, false)

			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}

			if got == nil {
				t.Fatal("expected non-nil result, got nil")
			}

			// Check exports
			if tt.wantExport == nil {
				if got.Exports != nil {
					t.Errorf("exports: expected nil, got %v", got.Exports)
				}
			} else {
				if len(got.Exports) != len(tt.wantExport) {
					t.Errorf("exports length = %d, want %d", len(got.Exports), len(tt.wantExport))
				}
				for k, v := range tt.wantExport {
					if got.Exports[k] != v {
						t.Errorf("exports[%q] = %q, want %q", k, got.Exports[k], v)
					}
				}
			}

			// Check assets
			if tt.wantAsset == nil {
				if got.Assets != nil {
					t.Errorf("assets: expected nil, got %v", got.Assets)
				}
			} else {
				if len(got.Assets) != len(tt.wantAsset) {
					t.Errorf("assets length = %d, want %d", len(got.Assets), len(tt.wantAsset))
				}
				for k, v := range tt.wantAsset {
					if got.Assets[k] != v {
						t.Errorf("assets[%q] = %q, want %q", k, got.Assets[k], v)
					}
				}
			}
		})
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// preBuildFixture builds a workspace, project, and extension whose preBuild
// hook runs the given program. The project directory is created so the hook
// subprocess has a valid working directory.
func preBuildFixture(t *testing.T, program fixtureproc.Program, timeoutMs int) (*workspace.Workspace, *extension.ExtensionDescription, *workspace.Project) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	script := fixtureproc.Write(t, filepath.Join(dir, "pre-build"), program)
	ws := workspace.NewWorkspace(dir, &wsproto.Config{Name: "test-ws"}, nil)
	proj := &workspace.Project{Name: "my-app", Path: "app"}
	ext := &extension.ExtensionDescription{
		Name: "@putnami/cloud",
		Path: filepath.Join(dir, "ext"),
		Hooks: &extension.ManifestHooks{
			PreBuild: &extension.HookDefinition{
				Kind:      "command",
				Command:   script,
				TimeoutMs: timeoutMs,
			},
		},
	}
	return ws, ext, proj
}

func TestRunPreBuildHook_NoHook(t *testing.T) {
	ws, ext, proj := preBuildFixture(t, fixtureproc.Program{}, 0)
	ext.Hooks = nil

	result, err := RunPreBuildHook(context.Background(), ws, ext, proj, false, nil)
	if err != nil {
		t.Fatalf("RunPreBuildHook: %v", err)
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil for extension without preBuild hook", result)
	}
}

// TestRunPreBuildHook_Summary exercises the concurrent stdout/stderr goroutine
// wiring and the exports/assets propagation into HookResult — the delta over
// its tested sibling RunOnInstallHook that had no direct coverage.
func TestRunPreBuildHook_Summary(t *testing.T) {
	record := filepath.Join(t.TempDir(), "runs.jsonl")
	ws, ext, proj := preBuildFixture(t, fixtureproc.Program{
		Record:  record,
		PathEnv: []string{"PUTNAMI_PROJECT_ROOT", "PUTNAMI_WORKSPACE_ROOT"},
		Stdout: `{"v":1,"type":"log","data":{"message":"building"}}` + "\n" +
			`{"v":1,"type":"summary","data":{"exports":{"out":"dist/main.js"},"assets":{"style":"dist/style.css"}}}` + "\n",
	}, 0)

	result, err := RunPreBuildHook(context.Background(), ws, ext, proj, false, nil)
	if err != nil {
		t.Fatalf("RunPreBuildHook: %v", err)
	}
	// The CLI appends "--putnami-context <file>" after any declared args.
	run := onlyRun(t, record)
	if len(run.Args) != 2 || run.Args[0] != "--putnami-context" || run.ArgKinds[1] != "file" {
		t.Errorf("args = %q (kinds %q), want --putnami-context and an existing file", run.Args, run.ArgKinds)
	}
	if run.EnvKinds["PUTNAMI_PROJECT_ROOT"] != "dir" || run.EnvKinds["PUTNAMI_WORKSPACE_ROOT"] != "dir" {
		t.Errorf("path kinds = %v, want the project and workspace roots directories", run.EnvKinds)
	}
	if result == nil {
		t.Fatal("result = nil, want HookResult carrying exports/assets")
	}
	if got := result.Exports["out"]; got != "dist/main.js" {
		t.Errorf("exports[out] = %q, want dist/main.js", got)
	}
	if got := result.Assets["style"]; got != "dist/style.css" {
		t.Errorf("assets[style] = %q, want dist/style.css", got)
	}
}

func TestRunPreBuildHook_ExpandsPreparedRuntimeAndExtensionRoot(t *testing.T) {
	ws, ext, proj := preBuildFixture(t, fixtureproc.Program{Exit: 91}, 0)
	record := filepath.Join(t.TempDir(), "runs.jsonl")
	runtimePath := fixtureproc.Write(t, filepath.Join(t.TempDir(), "prepared-runtime"), fixtureproc.Program{
		Record: record,
		Stdout: `{"v":1,"type":"summary","data":{"exports":{"runtime":"prepared"}}}` + "\n",
	})
	ext.RuntimeExecutable = runtimePath
	ext.Hooks.PreBuild.Command = "{extensionRuntime}"
	ext.Hooks.PreBuild.Args = []string{"{extensionRoot}"}

	result, err := RunPreBuildHook(context.Background(), ws, ext, proj, false, nil)
	if err != nil {
		t.Fatalf("RunPreBuildHook: %v", err)
	}
	run := onlyRun(t, record)
	if root, _ := run.LookupEnv("PUTNAMI_EXTENSION_ROOT"); len(run.Args) != 3 || run.Args[0] != root ||
		run.Args[1] != "--putnami-context" || run.ArgKinds[2] != "file" {
		t.Errorf("args = %q (kinds %q), want the extension root %q, --putnami-context and an existing file", run.Args, run.ArgKinds, root)
	}
	if got := result.Exports["runtime"]; got != "prepared" {
		t.Fatalf("exports[runtime] = %q, want prepared", got)
	}
}

func TestRunPreBuildHook_FailureTailsStderr(t *testing.T) {
	ws, ext, proj := preBuildFixture(t, fixtureproc.Program{Stderr: "boom: compile failed\n", Exit: 3}, 0)

	result, err := RunPreBuildHook(context.Background(), ws, ext, proj, false, nil)
	if err == nil {
		t.Fatal("expected error from failing hook, got nil")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on failure", result)
	}
	if !strings.Contains(err.Error(), "hook @putnami/cloud/preBuild failed") {
		t.Errorf("error = %q, want it to name the failing hook", err)
	}
	if !strings.Contains(err.Error(), "boom: compile failed") {
		t.Errorf("error = %q, want it to include the captured stderr tail", err)
	}
}

// TestRunPreBuildHook_CallerEnvWinsLast pins the env parameter: an entry the
// caller passes reaches the hook over the same key in the inherited
// environment and in the manifest's Env. The scheduler passes the held
// task-output lock ids this way.
func TestRunPreBuildHook_CallerEnvWinsLast(t *testing.T) {
	const key = "PUTNAMI_HELD_OUTPUT_LOCKS"
	t.Setenv(key, "from-the-session")
	record := filepath.Join(t.TempDir(), "runs.jsonl")
	ws, ext, proj := preBuildFixture(t, fixtureproc.Program{Record: record}, 0)
	ext.Hooks.PreBuild.Env = map[string]string{key: "from-the-manifest"}

	if _, err := RunPreBuildHook(context.Background(), ws, ext, proj, false, []string{key + "=aa,bb"}); err != nil {
		t.Fatalf("RunPreBuildHook: %v", err)
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 1 {
		t.Fatalf("the hook ran %d times, want once", len(runs))
	} else if got, _ := runs[0].LookupEnv(key); got != "aa,bb" {
		t.Fatalf("the hook saw %s=%q, want the caller's value", key, got)
	}

	if _, err := RunPreBuildHook(context.Background(), ws, ext, proj, false, nil); err != nil {
		t.Fatalf("RunPreBuildHook without caller env: %v", err)
	}
	if runs := fixtureproc.Runs(t, record); len(runs) != 2 {
		t.Fatalf("the hook ran %d times, want twice", len(runs))
	} else if got, _ := runs[1].LookupEnv(key); got != "from-the-manifest" {
		t.Fatalf("without caller env the hook saw %s=%q, want the manifest's value", key, got)
	}
}

func TestRunPreBuildHook_Timeout(t *testing.T) {
	ws, ext, proj := preBuildFixture(t, fixtureproc.Program{Sleep: 5 * time.Second}, 100)

	result, err := RunPreBuildHook(context.Background(), ws, ext, proj, false, nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on timeout", result)
	}
	if !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Errorf("error = %q, want a timeout message", err)
	}
}
