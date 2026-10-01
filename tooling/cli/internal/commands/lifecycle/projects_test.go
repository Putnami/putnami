package lifecycle

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/template"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// --- parseTags ---

func TestParseTags(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"foo", []string{"foo"}},
		{"foo,bar", []string{"foo", "bar"}},
		{"foo, bar, baz", []string{"foo", "bar", "baz"}},
		{" , , ", nil},
		{"a,,b", []string{"a", "b"}},
	}
	for _, tt := range tests {
		got := parseTags(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("parseTags(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseTags(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

// --- template.NormalizeProjectModule ---

func TestNormalizeProjectModule(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my-project", "my_project"},
		{"@scope/my-pkg", "scope_my_pkg"},
		{"MY_PROJECT", "my_project"},
		{"123start", "pkg_123start"},
		{"", "pkg"},
		{"@", "pkg"},
		{"a__b", "a_b"},
		{"_leading", "leading"},
		{"trailing_", "trailing"},
		{"hello.world", "hello_world"},
	}
	for _, tt := range tests {
		got := template.NormalizeProjectModule(tt.input)
		if got != tt.want {
			t.Errorf("template.NormalizeProjectModule(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- template.EvalTemplate ---

func TestEvalTemplate(t *testing.T) {
	vars := template.RenderVars{
		ProjectName:           "my-app",
		ProjectPath:           "packages/my-app",
		ProjectModule:         "my_app",
		PutnamiVersion:        "1.2.3",
		WorkspaceRelativePath: "../..",
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"replaces projectName",
			"name: <%= projectName %>",
			"name: my-app",
		},
		{
			"replaces projectPath",
			"path: <%= projectPath %>",
			"path: packages/my-app",
		},
		{
			"replaces projectModule",
			"module: <%= projectModule %>",
			"module: my_app",
		},
		{
			"replaces putnamiVersion",
			"version: <%= putnamiVersion %>",
			"version: 1.2.3",
		},
		{
			"replaces workspaceRelativePath",
			"ws: <%= workspaceRelativePath %>",
			"ws: ../..",
		},
		{
			"replaces multiple placeholders",
			"<%= projectName %> at <%= projectPath %>",
			"my-app at packages/my-app",
		},
		{
			"no placeholders unchanged",
			"static content",
			"static content",
		},
		{
			"unknown placeholder unchanged",
			"<%= unknown %>",
			"<%= unknown %>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := template.EvalTemplate(tt.input, vars)
			if got != tt.want {
				t.Errorf("template.EvalTemplate(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- parseProjectsCreateArgs ---

func TestParseProjectsCreateArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantName  string
		wantFlags createFlags
		wantErr   string
	}{
		{
			name:      "template only",
			args:      []string{"my-app", "--template", "typescript-web"},
			wantName:  "my-app",
			wantFlags: createFlags{template: "typescript-web"},
		},
		{
			name:      "template and path",
			args:      []string{"my-app", "--template", "typescript-web", "--path", "apps/my-app"},
			wantName:  "my-app",
			wantFlags: createFlags{template: "typescript-web", path: "apps/my-app"},
		},
		{
			name:    "missing name",
			args:    nil,
			wantErr: "usage: projects create <name> --template <template>",
		},
		{
			name:    "missing template",
			args:    []string{"my-app"},
			wantErr: "--template flag is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotFlags, err := parseProjectsCreateArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("parseProjectsCreateArgs(%v) error = %v, want %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProjectsCreateArgs(%v) returned unexpected error: %v", tt.args, err)
			}
			if gotName != tt.wantName {
				t.Errorf("name = %q, want %q", gotName, tt.wantName)
			}
			if gotFlags != tt.wantFlags {
				t.Errorf("flags = %+v, want %+v", gotFlags, tt.wantFlags)
			}
		})
	}
}

// --- resolveProjectPath ---

func TestResolveProjectPath(t *testing.T) {
	tests := []struct {
		name       string
		project    string
		customPath string
		want       string
		wantErr    string
	}{
		// The path goes to putnami.workspace.json and go.work, which every
		// host reads: it is in slash form on every platform.
		{
			name:    "default path",
			project: "@scope/my-app",
			want:    "scope/my-app",
		},
		{
			name:       "custom path",
			project:    "my-app",
			customPath: "apps/my-app",
			want:       "apps/my-app",
		},
		{
			name:       "custom path cleaned",
			project:    "my-app",
			customPath: "apps/../apps/my-app",
			want:       "apps/my-app",
		},
		{
			name:       "custom path in the host form",
			project:    "my-app",
			customPath: filepath.Join("apps", "my-app"),
			want:       "apps/my-app",
		},
		{
			name:       "absolute path rejected",
			project:    "my-app",
			customPath: filepath.Join(string(filepath.Separator), "tmp", "my-app"),
			wantErr:    "--path must be relative to the workspace",
		},
		{
			name:       "parent path rejected",
			project:    "my-app",
			customPath: filepath.Join("..", "my-app"),
			wantErr:    "--path must stay within the workspace",
		},
		{
			name:       "dot path rejected",
			project:    "my-app",
			customPath: ".",
			wantErr:    "--path must stay within the workspace",
		},
		{
			// A path relative to drive C on Windows; a plain name elsewhere.
			name:       "drive-relative path",
			project:    "my-app",
			customPath: "C:my-app",
			want:       "C:my-app",
			wantErr:    onWindows("--path must be relative to the workspace"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveProjectPath(tt.project, tt.customPath)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("resolveProjectPath(%q, %q) error = %v, want %q", tt.project, tt.customPath, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveProjectPath(%q, %q) returned unexpected error: %v", tt.project, tt.customPath, err)
			}
			if got != tt.want {
				t.Errorf("resolveProjectPath(%q, %q) = %q, want %q", tt.project, tt.customPath, got, tt.want)
			}
		})
	}
}

// onWindows returns s on Windows and "" elsewhere.
func onWindows(s string) string {
	if runtime.GOOS == "windows" {
		return s
	}
	return ""
}

// --- workspaceRelativePath ---

func TestWorkspaceRelativePath(t *testing.T) {
	tests := []struct {
		projectPath string
		want        string
	}{
		{projectPath: "packages/my-app", want: "../.."},
		{projectPath: "apps/my-app/web", want: "../../.."},
		{projectPath: "my-app", want: ".."},
	}

	for _, tt := range tests {
		got := workspaceRelativePath(tt.projectPath)
		if got != tt.want {
			t.Errorf("workspaceRelativePath(%q) = %q, want %q", tt.projectPath, got, tt.want)
		}
	}
}

// --- template.RenderDir ---

func TestCopyTemplateDir_PlainFiles(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create a plain file
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	vars := template.RenderVars{ProjectName: "app"}
	if err := template.RenderDir(src, dst, vars); err != nil {
		t.Fatalf("template.RenderDir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "README.md"))
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("copied content = %q, want %q", string(data), "hello")
	}
}

func TestCopyTemplateDir_TemplateSubstitution(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	content := "module: <%= projectModule %>\nname: <%= projectName %>"
	if err := os.WriteFile(filepath.Join(src, "go.mod.template"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	vars := template.RenderVars{
		ProjectName:   "my-lib",
		ProjectModule: "my_lib",
	}
	if err := template.RenderDir(src, dst, vars); err != nil {
		t.Fatalf("template.RenderDir: %v", err)
	}

	// Template suffix stripped
	if _, err := os.Stat(filepath.Join(dst, "go.mod.template")); err == nil {
		t.Error("template file with .template suffix should not exist in dst")
	}
	data, err := os.ReadFile(filepath.Join(dst, "go.mod"))
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	want := "module: my_lib\nname: my-lib"
	if string(data) != want {
		t.Errorf("template output = %q, want %q", string(data), want)
	}
}

func TestCopyTemplateDir_NestedDirectories(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	subdir := filepath.Join(src, "src", "pkg")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	vars := template.RenderVars{ProjectName: "myapp"}
	if err := template.RenderDir(src, dst, vars); err != nil {
		t.Fatalf("template.RenderDir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "src", "pkg", "main.go"))
	if err != nil {
		t.Fatalf("read nested file: %v", err)
	}
	if string(data) != "package main" {
		t.Errorf("nested file content = %q", string(data))
	}
}

func TestCopyTemplateDir_ModuleDirectorySubstitution(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create src/__module__/main.py (mimics python-server template)
	moduleDir := filepath.Join(src, "src", "__module__")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "__init__.py"), []byte("print('hello')"), 0o644); err != nil {
		t.Fatal(err)
	}

	vars := template.RenderVars{
		ProjectName:   "my-api",
		ProjectModule: "my_api",
	}
	if err := template.RenderDir(src, dst, vars); err != nil {
		t.Fatalf("template.RenderDir: %v", err)
	}

	// __module__ directory should be renamed to the project module
	data, err := os.ReadFile(filepath.Join(dst, "src", "my_api", "__init__.py"))
	if err != nil {
		t.Fatalf("expected src/my_api/__init__.py to exist: %v", err)
	}
	if string(data) != "print('hello')" {
		t.Errorf("file content = %q, want %q", string(data), "print('hello')")
	}

	// Original __module__ directory should NOT exist
	if _, err := os.Stat(filepath.Join(dst, "src", "__module__")); err == nil {
		t.Error("src/__module__/ should not exist in output — should be renamed to src/my_api/")
	}
}

// --- updateConfigMembership ---

func TestUpdateConfigMembership(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "putnami.workspace.json")

	initial := map[string]any{
		"name":     "my-workspace",
		"includes": []string{"packages/a"},
	}
	data, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(cfgPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	newProjects := []string{"packages/a", "packages/b", "packages/c"}
	if err := updateConfigMembership(dir, nil, newProjects); err != nil {
		t.Fatalf("updateConfigMembership: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("parse updated config: %v", err)
	}

	includes, ok := result["includes"].([]any)
	if !ok {
		t.Fatalf("includes field missing or wrong type: %T", result["includes"])
	}
	if len(includes) != 3 {
		t.Errorf("expected 3 includes, got %d", len(includes))
	}
	// Original field preserved
	if result["name"] != "my-workspace" {
		t.Errorf("name field = %v, want my-workspace", result["name"])
	}
}

func TestUpdateConfigMembership_MergesScopesAndProjects(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "putnami.workspace.json")

	initial := map[string]any{
		"name":     "my-workspace",
		"includes": []string{"go"},
	}
	data, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(cfgPath, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := updateConfigMembership(dir, []string{"go"}, []string{"tooling/cli"}); err != nil {
		t.Fatalf("updateConfigMembership: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("parse updated config: %v", err)
	}

	includes, ok := result["includes"].([]any)
	if !ok {
		t.Fatalf("includes field missing or wrong type: %T", result["includes"])
	}
	if len(includes) != 2 || includes[0] != "go" || includes[1] != "tooling/cli" {
		t.Errorf("includes = %v, want [go tooling/cli]", includes)
	}
}

func TestUpdateConfigMembership_MissingFile(t *testing.T) {
	dir := t.TempDir()
	err := updateConfigMembership(dir, nil, []string{"packages/a"})
	if err == nil {
		t.Error("expected error when config file missing, got nil")
	}
}

// --- scaffoldPackageJSONWorkspaces (the `putnami init` scaffolder) ---
//
// An earlier cleanup deleted the workspaces-array writer from `projects
// sync`; the TypeScript extension's own sync task owns it there. This copy
// survives for `putnami init` alone, which runs before any extension exists in
// the workspace it is creating.

func TestScaffoldPackageJSONWorkspaces(t *testing.T) {
	dir := t.TempDir()
	pkgPath := filepath.Join(dir, "package.json")

	// Create initial package.json
	initial := `{"name": "test-ws", "private": true}`
	if err := os.WriteFile(pkgPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create project directories with package.json
	for _, p := range []string{"packages/a", "packages/b"} {
		projDir := filepath.Join(dir, p)
		if err := os.MkdirAll(projDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projDir, "package.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := scaffoldPackageJSONWorkspaces(dir, []string{"packages/a", "packages/b"}); err != nil {
		t.Fatalf("scaffoldPackageJSONWorkspaces: %v", err)
	}

	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("parse updated package.json: %v", err)
	}

	workspaces, ok := result["workspaces"].([]any)
	if !ok {
		t.Fatalf("workspaces field missing or wrong type: %T", result["workspaces"])
	}
	if len(workspaces) != 2 {
		t.Errorf("expected 2 workspaces, got %d", len(workspaces))
	}
	// Original fields preserved
	if result["name"] != "test-ws" {
		t.Errorf("name field = %v, want test-ws", result["name"])
	}
}

func TestScaffoldPackageJSONWorkspaces_FiltersNonTS(t *testing.T) {
	dir := t.TempDir()
	pkgPath := filepath.Join(dir, "package.json")

	if err := os.WriteFile(pkgPath, []byte(`{"name": "ws"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// "packages/ts-app" has package.json, "packages/go-app" does not
	tsDir := filepath.Join(dir, "packages", "ts-app")
	goDir := filepath.Join(dir, "packages", "go-app")
	if err := os.MkdirAll(tsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(goDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tsDir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := scaffoldPackageJSONWorkspaces(dir, []string{"packages/go-app", "packages/ts-app"}); err != nil {
		t.Fatalf("scaffoldPackageJSONWorkspaces: %v", err)
	}

	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("parse: %v", err)
	}

	workspaces, ok := result["workspaces"].([]any)
	if !ok {
		t.Fatalf("workspaces missing or wrong type: %T", result["workspaces"])
	}
	if len(workspaces) != 1 {
		t.Errorf("expected 1 workspace (only TS), got %d", len(workspaces))
	}
	if len(workspaces) > 0 && workspaces[0] != "packages/ts-app" {
		t.Errorf("workspace[0] = %v, want packages/ts-app", workspaces[0])
	}
}

func TestProjectsList_SortsTextOutputByName(t *testing.T) {
	dir := makeProjectsListWorkspace(t)

	output, err := captureStdout(t, func() error {
		return ProjectsList(dir, nil, "")
	})
	if err != nil {
		t.Fatalf("ProjectsList: %v", err)
	}

	var names []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "packages/") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		names = append(names, fields[0])
	}

	want := []string{"/packages/alpha", "/packages/beta", "/packages/gamma"}
	if len(names) != len(want) {
		t.Fatalf("listed project IDs = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("listed project IDs = %v, want %v", names, want)
		}
	}
}

func TestProjectsList_SortsJSONLOutputByName(t *testing.T) {
	dir := makeProjectsListWorkspace(t)

	output, err := captureStdout(t, func() error {
		return ProjectsList(dir, nil, "jsonl")
	})
	if err != nil {
		t.Fatalf("ProjectsList: %v", err)
	}

	type projectEntry struct {
		Name string `json:"name"`
	}

	var names []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry projectEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("unmarshal JSONL line %q: %v", line, err)
		}
		names = append(names, entry.Name)
	}

	want := []string{"alpha", "beta", "gamma"}
	if len(names) != len(want) {
		t.Fatalf("JSONL project names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("JSONL project names = %v, want %v", names, want)
		}
	}
}

func TestProjectsDescribe_TransparentGroupFolderUsesLogicalID(t *testing.T) {
	dir := t.TempDir()
	writeJSONFile(t, filepath.Join(dir, "putnami.workspace.json"), map[string]any{
		"includes": []string{"identity/(workloads)/auth-server"},
	})
	projectDir := filepath.Join(dir, "identity", "(workloads)", "auth-server")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(projectDir, "putnami.json"), map[string]any{"name": "auth-server"})

	output, err := captureStdout(t, func() error {
		return ProjectsDescribe(dir, nil, []string{"/identity/auth-server"}, "")
	})
	if err != nil {
		t.Fatalf("ProjectsDescribe(logical ID): %v", err)
	}
	for _, want := range []string{"Project:      /identity/auth-server", "Path:         identity/(workloads)/auth-server"} {
		if !strings.Contains(output, want) {
			t.Errorf("ProjectsDescribe output missing %q:\n%s", want, output)
		}
	}
	if _, err := captureStdout(t, func() error {
		return ProjectsDescribe(dir, nil, []string{"/identity/workloads/auth-server"}, "")
	}); err == nil {
		t.Error("former path-derived ID must not resolve as a compatibility alias")
	}
}

func makeProjectsListWorkspace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	hometest.Temp(t)

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origDir)
	})

	writeJSONFile(t, filepath.Join(dir, "putnami.workspace.json"), map[string]any{
		"name":     "test-workspace",
		"includes": []string{"packages/beta", "packages/alpha", "packages/gamma"},
	})

	for _, project := range []struct {
		path string
		name string
		kind string
		tags []string
	}{
		{path: "packages/beta", name: "beta", kind: "library", tags: []string{"shared"}},
		{path: "packages/alpha", name: "alpha", kind: "service", tags: []string{"core"}},
		{path: "packages/gamma", name: "gamma", kind: "application", tags: []string{"ui"}},
	} {
		projectDir := filepath.Join(dir, project.path)
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", projectDir, err)
		}
		writeJSONFile(t, filepath.Join(projectDir, "putnami.json"), map[string]any{
			"name": project.name,
			"type": project.kind,
			"tags": project.tags,
		})
	}

	return dir
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeRawFile writes a fixture verbatim, creating parent directories. Used
// where the fixture's exact JSON text is the thing under test.
func writeRawFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// captureStdout redirects the real os.Stdout file descriptor for the
// duration of fn. The real implementation lives in internal/commands/sharedtest
// (go.putnami.dev/tooling/cli/internal/commands/sharedtest), so every vertical
// package split out of internal/commands can reach it too; this forwards
// rather than duplicates it, so the still-flat callers in this package need no
// change.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	return sharedtest.CaptureStdout(t, fn)
}

func TestCaptureStdout_DrainsBeyondPipeCapacity(t *testing.T) {
	want := strings.Repeat("x", 256*1024)
	got, err := captureStdout(t, func() error {
		_, writeErr := io.WriteString(os.Stdout, want)
		return writeErr
	})
	if err != nil {
		t.Fatalf("write stdout capacity probe: %v", err)
	}
	if got != want {
		t.Fatalf("captured %d bytes, want %d", len(got), len(want))
	}
}

func TestCopyTemplateDir_SkipsManifest(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// Create a template manifest and a regular file
	if err := os.WriteFile(filepath.Join(src, "putnami.template.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	vars := template.RenderVars{ProjectName: "app", ProjectModule: "app"}
	if err := template.RenderDir(src, dst, vars); err != nil {
		t.Fatalf("template.RenderDir: %v", err)
	}

	// putnami.template.json should NOT be copied
	if _, err := os.Stat(filepath.Join(dst, "putnami.template.json")); err == nil {
		t.Error("putnami.template.json should not be copied to output")
	}

	// main.go should be copied
	if _, err := os.Stat(filepath.Join(dst, "main.go")); err != nil {
		t.Error("main.go should be copied to output")
	}
}

// A project listed twice is written once, and a workspace with no project that
// holds a package.json gets an empty array, never null.
func TestScaffoldPackageJSONWorkspaces_ListsEachProjectOnceAndNeverNull(t *testing.T) {
	workspacesOf := func(t *testing.T, dir string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest map[string]json.RawMessage
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, manifest["workspaces"]); err != nil {
			t.Fatalf("workspaces = %q: %v", manifest["workspaces"], err)
		}
		return compact.String()
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"ws"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "webapp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "webapp", "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "api"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := scaffoldPackageJSONWorkspaces(dir, []string{"webapp", "api", "webapp"}); err != nil {
		t.Fatal(err)
	}
	if got := workspacesOf(t, dir); got != `["webapp"]` {
		t.Errorf("workspaces = %s, want webapp once", got)
	}

	for name, projects := range map[string][]string{"no project": nil, "no project with a package.json": {"api"}} {
		if err := scaffoldPackageJSONWorkspaces(dir, projects); err != nil {
			t.Fatal(err)
		}
		if got := workspacesOf(t, dir); got != "[]" {
			t.Errorf("%s: workspaces = %s, want []", name, got)
		}
	}
}

func TestScaffoldPackageJSONWorkspaces_NoPackageJSON(t *testing.T) {
	dir := t.TempDir()
	// No package.json at root — should be a no-op
	err := scaffoldPackageJSONWorkspaces(dir, []string{"packages/a"})
	if err != nil {
		t.Errorf("expected nil error for missing package.json, got: %v", err)
	}
}

// `projects describe` used to print each declared job's kind and command, which
// advertised an execution contract nothing honors: project-level `jobs` is
// parsed and ignored. It now lists the names and says so — an honest
// report is the difference between "my job is configured" and "my job is dead".
func TestProjectsDescribe_ListsDeclaredJobsAsIgnored(t *testing.T) {
	dir := t.TempDir()
	hometest.Temp(t)

	// Written as literal JSON: the point of this fixture is the exact on-disk
	// wire an operator authors, and the descoped `jobs` block has no Go shape
	// left to build it from.
	writeRawFile(t, filepath.Join(dir, "putnami.workspace.json"),
		`{"name":"test-workspace","includes":["packages/app"]}`)
	writeRawFile(t, filepath.Join(dir, "packages", "app", "putnami.json"),
		`{"name":"app","jobs":{"typecheck":{"kind":"command","command":"tsc"},"gen":{"kind":"command","command":"go"}}}`)

	workspace.InvalidateLoadCache(dir)
	t.Cleanup(func() { workspace.InvalidateLoadCache(dir) })

	output, err := captureStdout(t, func() error {
		return ProjectsDescribe(dir, nil, []string{"app"}, "")
	})
	if err != nil {
		t.Fatalf("ProjectsDescribe: %v", err)
	}

	if !strings.Contains(output, "declared but ignored") {
		t.Errorf("describe does not mark declared jobs as ignored:\n%s", output)
	}
	// Names, sorted — never a kind or a command, which would read as wired.
	if !strings.Contains(output, "gen, typecheck") {
		t.Errorf("describe does not list job names in sorted order:\n%s", output)
	}
	if strings.Contains(output, "tsc") {
		t.Errorf("describe still prints a job's command, advertising a contract nothing honors:\n%s", output)
	}
}

// describe reports the key on PRESENCE, like the loader warning. `{"jobs": {}}`
// is what makes this directory a project at all, so a size test would print
// nothing for a project that exists only because of the surface being hidden.
func TestProjectsDescribe_ReportsEmptyJobsBlock(t *testing.T) {
	dir := t.TempDir()
	hometest.Temp(t)

	writeRawFile(t, filepath.Join(dir, "putnami.workspace.json"),
		`{"name":"test-workspace","includes":["packages/app"]}`)
	writeRawFile(t, filepath.Join(dir, "packages", "app", "putnami.json"),
		`{"name":"app","jobs":{}}`)

	workspace.InvalidateLoadCache(dir)
	t.Cleanup(func() { workspace.InvalidateLoadCache(dir) })

	output, err := captureStdout(t, func() error {
		return ProjectsDescribe(dir, nil, []string{"app"}, "")
	})
	if err != nil {
		t.Fatalf("ProjectsDescribe: %v", err)
	}

	if !strings.Contains(output, "Jobs:") {
		t.Errorf("describe says nothing about an empty but declared `jobs` block:\n%s", output)
	}
	if !strings.Contains(output, "ignored") {
		t.Errorf("describe does not mark the empty `jobs` block as ignored:\n%s", output)
	}
}
