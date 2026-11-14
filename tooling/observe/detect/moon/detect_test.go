package moon

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// tasked is a moon workspace whose projects inherit tasks: every project
// from .moon/tasks.yml, the TypeScript ones from node.yml, the libraries
// from typescript-library.yml, the tagged ones from tag-ci.yml, and moon 2.x
// files by their inheritedBy condition.
var tasked = map[string]string{
	".moon/workspace.yml": "projects:\n  - 'apps/*'\n  - 'packages/*'\n",
	".moon/tasks.yml":     "tasks:\n  format:\n    command: 'prettier'\n",
	".moon/tasks/node.yml": `tasks:
  lint:
    command: 'eslint .'
  typecheck:
    command: 'tsc'
`,
	".moon/tasks/typescript-library.yml": "tasks:\n  build:\n    command: 'tsc -b'\n",
	".moon/tasks/tag-ci.yml":             "tasks:\n  smoke:\n    command: 'echo'\n    tags: ['test']\n",
	".moon/tasks/rust.yml":               "inheritedBy:\n  languages: ['rust']\ntasks:\n  clippy:\n    command: 'cargo clippy'\n",
	".moon/tasks/never.yml":              "inheritedBy:\n  languages: ['go']\n  tags: ['ci']\ntasks:\n  vet:\n    command: 'go vet'\n",
	"apps/api/moon.yml": `language: 'typescript'
layer: 'application'
tags: ['ci']
dependsOn: ['pricing']
workspace:
  inheritedTasks:
    exclude: ['typecheck']
    rename:
      lint: 'eslint'
tasks:
  test:
    command: 'vitest run'
  unit:
    command: 'vitest'
    tags: ['test', 'fast']
`,
	"apps/api/package.json":       `{"name": "@demo/api"}`,
	"apps/web/package.json":       `{"name": "@demo/web"}`,
	"apps/web/tsconfig.json":      `{}`,
	"packages/ui/moon.yml":        "type: 'library'\nlanguage: 'typescript'\nworkspace:\n  inheritedTasks:\n    include: ['build']\n",
	"packages/pricing/moon.yml":   "stack: 'backend'\n",
	"packages/pricing/Cargo.toml": "[package]\nname = \"pricing\"\n",
}

func TestDetectReadsLanguagesAndInheritedTasks(t *testing.T) {
	root := tree(t, tasked)
	got := Detect(root, Options{})
	if got.FromExport || len(got.Diagnostics) != 0 {
		t.Fatalf("FromExport %v, diagnostics %v", got.FromExport, got.Diagnostics)
	}
	want := []Project{
		{ID: "api", Dir: "apps/api", Language: "typescript", Layer: "application", Tags: []string{"ci"}, SourceFile: "apps/api/moon.yml", DependsOn: []string{"packages/pricing"},
			Tasks: []Task{{Name: "eslint", Command: "eslint ."}, {Name: "format", Command: "prettier"}, {Name: "smoke", Tags: []string{"test"}, Command: "echo"}, {Name: "test", Command: "vitest run"}, {Name: "unit", Tags: []string{"fast", "test"}, Command: "vitest"}}},
		{ID: "web", Dir: "apps/web", Language: "typescript",
			Tasks: []Task{{Name: "format", Command: "prettier"}, {Name: "lint", Command: "eslint ."}, {Name: "typecheck", Command: "tsc"}}},
		{ID: "pricing", Dir: "packages/pricing", Language: "rust", Stack: "backend", SourceFile: "packages/pricing/moon.yml",
			Tasks: []Task{{Name: "clippy", Command: "cargo clippy"}, {Name: "format", Command: "prettier"}}},
		{ID: "ui", Dir: "packages/ui", Language: "typescript", Layer: "library", SourceFile: "packages/ui/moon.yml",
			Tasks: []Task{{Name: "build", Command: "tsc -b"}}},
	}
	if !reflect.DeepEqual(got.Projects, want) {
		t.Fatalf("projects:\n got %+v\nwant %+v", got.Projects, want)
	}
}

func TestDetectPrefersTheTasksOfTheExport(t *testing.T) {
	files := map[string]string{
		"moon-graph.json": `{"graph": {"nodes": [
  {"id": "api", "source": "apps/api", "language": "typescript", "layer": "application", "config": {"tags": ["ci"]}, "tasks": {"build": {"command": "tsc", "args": ["-b"], "script": null}, "check": {"tags": ["lint"], "script": "eslint . && prettier --check ."}, "test": {"command": {"unknown": true}}}},
  {"id": "pricing", "source": "packages/pricing", "language": {"other": "zig"}}
], "edges": [[0, 1, "production"]]}}`,
		"apps/api/moon.yml":         "language: 'javascript'\ntasks:\n  test:\n    command: 'vitest'\n",
		"packages/pricing/moon.yml": "tasks:\n  test:\n    command: 'zig test'\n",
	}
	got := Detect(tree(t, files), Options{})
	if !got.FromExport {
		t.Fatal("the export was not read")
	}
	want := []Project{
		{ID: "api", Dir: "apps/api", Language: "typescript", Layer: "application", Tags: []string{"ci"}, SourceFile: "apps/api/moon.yml", DependsOn: []string{"packages/pricing"},
			Tasks: []Task{{Name: "build", Command: "tsc -b"}, {Name: "check", Tags: []string{"lint"}, Command: "eslint . && prettier --check ."}, {Name: "test"}}},
		{ID: "pricing", Dir: "packages/pricing", Language: "unknown", SourceFile: "packages/pricing/moon.yml",
			Tasks: []Task{{Name: "test", Command: "zig test"}}},
	}
	if !reflect.DeepEqual(got.Projects, want) {
		t.Fatalf("projects:\n got %+v\nwant %+v", got.Projects, want)
	}
}

// moon2Export is what moon 2.6 project-graph --json prints: graph.nodes
// holds indices and data the projects, unexpanded, so each project lists its
// tasks as taskTargets and writes no tasks key.
const moon2Export = `{
  "graph": {"nodes": [0, 1, 2], "node_holes": [], "edge_property": "directed", "edges": [[1, 2, "production"]]},
  "data": {
    "0": {"config": {"tasks": {"build-types": {"command": "tsc", "args": ["--build"]}}}, "id": "root", "language": "typescript", "layer": "unknown", "root": "/repo", "source": ".", "stack": "unknown",
          "inherited": {"configs": {".moon/tasks/all.yml": {"tasks": {"format": {"command": "prettier", "args": ["--check", "."], "tags": ["lint"]}, "smoke": {"command": "echo"}}}}, "layers": {}},
          "taskTargets": ["root:build-types", "root:format"], "toolchains": ["javascript"]},
    "1": {"config": {"tags": ["ci"], "tasks": {"typecheck": {"command": "tsc"}}}, "dependencies": [{"id": "shared-ui", "scope": "production"}], "id": "website",
          "inherited": {"configs": {
            ".moon/tasks/all.yml": {"tasks": {"format": {"command": "prettier", "args": ["--check", "."], "tags": ["lint"]}, "smoke": {"command": "echo"}}},
            ".moon/tasks/frontend.yml": {"inheritedBy": {"stack": "frontend", "order": 2}, "tasks": {"format": {"command": "prettier", "args": ["--check", "src"], "tags": ["lint"]}}}
          }, "layers": {"format": [".moon/tasks/all.yml", ".moon/tasks/frontend.yml"], "smoke": [".moon/tasks/all.yml"]}},
          "language": "typescript", "layer": "application", "root": "/repo/website", "source": "website", "stack": "frontend",
          "taskTargets": ["website:build", "website:deploy", "website:format", "website:test", "website:typecheck"], "toolchains": ["javascript", "typescript"]},
    "2": {"id": "shared-ui", "language": "typescript", "layer": "library", "root": "/repo/packages/shared-ui", "source": "packages/shared-ui", "stack": "unknown",
          "taskTargets": ["shared-ui:build", "shared-ui:lint"], "toolchains": ["javascript"]}
  }
}`

func TestDetectNamesTheTasksOfAMoon2ExportByTheirTargets(t *testing.T) {
	files := map[string]string{
		".moon/workspace.yml":             "projects:\n  - '.'\n  - 'website'\n  - 'packages/*'\n",
		".moon/toolchains.yml":            "javascript:\n  inferTasksFromScripts: true\n",
		".moon/tasks/all.yml":             "tasks:\n  format:\n    command: 'prettier --check .'\n    tags: ['lint']\n  smoke:\n    command: 'echo'\n",
		".moon/tasks/frontend.yml":        "inheritedBy:\n  stack: 'frontend'\n  order: 2\ntasks:\n  format:\n    command: 'prettier --check src'\n    tags: ['lint']\n",
		".moon/tasks/rust.yml":            "inheritedBy:\n  toolchain: 'rust'\ntasks:\n  test:\n    command: 'cargo nextest run'\n    tags: ['test']\n",
		"moon.yml":                        "tasks:\n  build-types:\n    command: 'tsc'\n    args: ['--build']\n",
		"website/moon.yml":                "tags: ['ci']\nworkspace:\n  inheritedTasks:\n    exclude: ['smoke']\ntasks:\n  typecheck:\n    command: 'tsc'\n",
		"website/package.json":            `{"name": "website", "scripts": {"build": "astro build", "deploy": "wrangler deploy", "test": "vitest"}}`,
		"packages/shared-ui/package.json": `{"name": "shared-ui", "scripts": {"build": "vite build", "lint": "eslint ."}}`,
		"moon-graph.json":                 moon2Export,
	}
	got := Detect(tree(t, files), Options{})
	if !got.FromExport || len(got.Diagnostics) != 0 {
		t.Fatalf("FromExport %v, diagnostics %v", got.FromExport, got.Diagnostics)
	}
	want := map[string][]Task{
		"root":      {{Name: "build-types", Command: "tsc --build"}, {Name: "format", Tags: []string{"lint"}, Command: "prettier --check ."}},
		"website":   {{Name: "build"}, {Name: "deploy"}, {Name: "format", Tags: []string{"lint"}, Command: "prettier --check . --check src"}, {Name: "test"}, {Name: "typecheck", Command: "tsc"}},
		"shared-ui": {{Name: "build"}, {Name: "lint"}},
	}
	if len(got.Projects) != len(want) {
		t.Fatalf("projects %+v", got.Projects)
	}
	for _, p := range got.Projects {
		if !reflect.DeepEqual(p.Tasks, want[p.ID]) {
			t.Errorf("%s tasks %+v, want %+v", p.ID, p.Tasks, want[p.ID])
		}
	}
}

func TestDetectOutsideAMoonWorkspace(t *testing.T) {
	root := tree(t, map[string]string{"README.md": "no moon\n"})
	if IsWorkspace(root) {
		t.Fatal("a repository without .moon/workspace.yml is no moon workspace")
	}
	if got := Detect(root, Options{}); len(got.Projects) != 0 {
		t.Fatalf("projects %+v", got.Projects)
	}
}

func TestInheritanceIncludeEmptyKeepsNone(t *testing.T) {
	in := readInheritance(map[string]any{"include": []any{}})
	if got := inherit(in, map[string]Task{"build": {}}); len(got) != 0 {
		t.Fatalf("kept %v", got)
	}
	if got := inherit(inheritance{}, map[string]Task{"build": {}}); len(got) != 1 {
		t.Fatalf("no setting keeps every task, kept %v", got)
	}
}

func TestStringsOf(t *testing.T) {
	if got := stringsOf("one"); !reflect.DeepEqual(got, []string{"one"}) {
		t.Fatalf("a scalar reads as one entry: %v", got)
	}
	if got := stringsOf([]any{"a", 1, "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
	if stringsOf(map[string]any{}) != nil {
		t.Fatal("a mapping reads as none")
	}
}

func TestATaskCommandLineReadsItsScriptOrItsCommandAndArgs(t *testing.T) {
	for want, settings := range map[string]map[string]any{
		"vitest run --coverage": {"command": "vitest", "args": []any{"run", "--coverage"}},
		"go vet ./...":          {"command": []any{"go", "vet"}, "args": "./..."},
		"eslint . && tsc":       {"command": "ignored", "script": "eslint . && tsc"},
		"":                      {"args": []any{"run"}},
	} {
		if got := commandLine(settings); got != want {
			t.Errorf("commandLine(%v) = %q, want %q", settings, got, want)
		}
	}
	// A project task merges over the task it inherits, as moon does: args
	// append unless mergeArgs says otherwise, and a command keeps the
	// inherited args.
	root := tree(t, map[string]string{
		".moon/workspace.yml":   "projects:\n  - 'apps/*'\n",
		".moon/tasks.yml":       "tasks:\n  lint:\n    command: 'eslint'\n    args: ['src']\n  test:\n    command: 'jest'\n    args: ['--ci']\n  build:\n    script: 'tsc && cp a b'\n",
		"apps/api/moon.yml":     "tasks:\n  lint:\n    args: ['lib']\n  test:\n    args: ['--coverage']\n    options:\n      mergeArgs: 'replace'\n  build:\n    command: 'tsc'\n",
		"apps/api/package.json": `{"name": "@demo/api"}`,
		"apps/web/moon.yml":     "tasks:\n  lint:\n    command: 'biome'\n",
		"apps/web/package.json": `{"name": "@demo/web"}`,
	})
	got := Detect(root, Options{})
	want := map[string][]Task{
		"api": {{Name: "build", Command: "tsc"}, {Name: "lint", Command: "eslint src lib"}, {Name: "test", Command: "jest --coverage"}},
		"web": {{Name: "build", Command: "tsc && cp a b"}, {Name: "lint", Command: "biome src"}, {Name: "test", Command: "jest --ci"}},
	}
	if len(got.Projects) != 2 {
		t.Fatalf("projects %+v", got.Projects)
	}
	for _, p := range got.Projects {
		if !reflect.DeepEqual(p.Tasks, want[p.ID]) {
			t.Errorf("%s tasks %+v, want %+v", p.ID, p.Tasks, want[p.ID])
		}
	}
}

func TestInfersTasksNamesTheSettingThatInfersTasksFromScripts(t *testing.T) {
	for _, c := range []struct{ file, text, want string }{
		{".moon/toolchains.yml", "javascript:\n  packageManager: 'pnpm'\n  inferTasksFromScripts: true\n", ".moon/toolchains.yml: javascript.inferTasksFromScripts"},
		{".moon/toolchain.yml", "node:\n  inferTasksFromScripts: true\n", ".moon/toolchain.yml: node.inferTasksFromScripts"},
		{".moon/toolchains.yml", "javascript:\n  inferTasksFromScripts: false\n", ""},
		{".moon/toolchains.yml", "typescript:\n  inferTasksFromScripts: true\n", ""},
		{".moon/toolchains.yml", "javascript: &anchor\n  inferTasksFromScripts: true\n", ""},
	} {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".moon"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(c.file)), []byte(c.text), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := InfersTasks(root); got != c.want {
			t.Errorf("%s %q: InfersTasks = %q, want %q", c.file, c.text, got, c.want)
		}
	}
	if got := InfersTasks(t.TempDir()); got != "" {
		t.Errorf("no toolchain file: %q", got)
	}
}

func TestInheritedByReadsSingularFormsAndIgnoresAConditionItDoesNotEvaluate(t *testing.T) {
	web := Project{Language: "typescript", Stack: "frontend", Layer: "application", Tags: []string{"ci"}}
	for _, c := range []struct {
		by   map[string]any
		want bool
	}{
		{map[string]any{"stack": "frontend", "order": "2"}, true},
		{map[string]any{"languages": []any{"typescript", "javascript"}, "tag": "ci"}, true},
		{map[string]any{"layer": "library"}, false},
		{map[string]any{"toolchain": "node"}, true},
		{map[string]any{"files": []any{"Cargo.toml"}, "stack": "backend"}, false},
		{map[string]any{"tags": map[string]any{"or": []any{"ci"}}}, false},
	} {
		if got := (taskFile{rel: ".moon/tasks/x.yml", name: "x", by: c.by}).inheritedBy(web); got != c.want {
			t.Errorf("inheritedBy %v = %v, want %v", c.by, got, c.want)
		}
	}
}

func TestMergeTaskAppendsArgsAndTagsUnlessTheUpperLayerSaysOtherwise(t *testing.T) {
	base := map[string]any{"command": "prettier", "args": []any{"--check", "."}, "tags": []any{"lint", "fmt"}}
	for _, c := range []struct {
		own        map[string]any
		args, tags []any
	}{
		{map[string]any{"args": []any{"src"}, "tags": []any{"lint", "ci"}}, []any{"--check", ".", "src"}, []any{"ci", "fmt", "lint"}},
		{map[string]any{"args": []any{"src"}, "tags": []any{"ci"}, "options": map[string]any{"mergeTags": "replace"}}, []any{"--check", ".", "src"}, []any{"ci"}},
		{map[string]any{"args": []any{"src"}, "tags": []any{"ci"}, "options": map[string]any{"merge": "replace"}}, []any{"src"}, []any{"ci"}},
		{map[string]any{"args": []any{"src"}, "options": map[string]any{"mergeArgs": "prepend"}}, []any{"src", "--check", "."}, []any{"lint", "fmt"}},
	} {
		got := mergeTask(base, c.own)
		if !reflect.DeepEqual(got["args"], c.args) || !reflect.DeepEqual(got["tags"], c.tags) {
			t.Errorf("mergeTask over %v: args %v tags %v, want %v and %v", c.own, got["args"], got["tags"], c.args, c.tags)
		}
	}
}
