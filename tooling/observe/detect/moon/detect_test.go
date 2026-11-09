package moon

import (
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
	"apps/api/package.json":     `{"name": "@demo/api"}`,
	"apps/web/package.json":     `{"name": "@demo/web"}`,
	"apps/web/tsconfig.json":    `{}`,
	"packages/ui/moon.yml":      "type: 'library'\nlanguage: 'typescript'\nworkspace:\n  inheritedTasks:\n    include: ['build']\n",
	"packages/pricing/moon.yml": "stack: 'backend'\n",
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
			Tasks: []Task{{Name: "eslint"}, {Name: "format"}, {Name: "smoke", Tags: []string{"test"}}, {Name: "test"}, {Name: "unit", Tags: []string{"fast", "test"}}}},
		{ID: "web", Dir: "apps/web", Language: "typescript",
			Tasks: []Task{{Name: "format"}, {Name: "lint"}, {Name: "typecheck"}}},
		{ID: "pricing", Dir: "packages/pricing", Language: "rust", Stack: "backend", SourceFile: "packages/pricing/moon.yml",
			Tasks: []Task{{Name: "clippy"}, {Name: "format"}}},
		{ID: "ui", Dir: "packages/ui", Language: "typescript", Layer: "library", SourceFile: "packages/ui/moon.yml",
			Tasks: []Task{{Name: "build"}}},
	}
	if !reflect.DeepEqual(got.Projects, want) {
		t.Fatalf("projects:\n got %+v\nwant %+v", got.Projects, want)
	}
}

func TestDetectPrefersTheTasksOfTheExport(t *testing.T) {
	files := map[string]string{
		"moon-graph.json": `{"graph": {"nodes": [
  {"id": "api", "source": "apps/api", "language": "typescript", "layer": "application", "config": {"tags": ["ci"]}, "tasks": {"build": {}, "check": {"tags": ["lint"]}}},
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
			Tasks: []Task{{Name: "build"}, {Name: "check", Tags: []string{"lint"}}}},
		{ID: "pricing", Dir: "packages/pricing", Language: "unknown", SourceFile: "packages/pricing/moon.yml",
			Tasks: []Task{{Name: "test"}}},
	}
	if !reflect.DeepEqual(got.Projects, want) {
		t.Fatalf("projects:\n got %+v\nwant %+v", got.Projects, want)
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
	if got := in.apply(map[string][]string{"build": nil}); len(got) != 0 {
		t.Fatalf("kept %v", got)
	}
	if got := (inheritance{}).apply(map[string][]string{"build": nil}); len(got) != 1 {
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
