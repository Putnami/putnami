package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The portable admission reads the SAME declarations the cache key hashes.
// These pins hold the projection to the key's own rules: explicit patterns
// through the store's collector, the whole non-hidden tree for a keyed task
// with no pattern, nothing for an unkeyed task with no pattern, env names only
// for a keyed task, and declared outputs as workspace-relative paths.
func TestPortableInputsProjectTheKeyDeclarations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := testWorkspace(root, app)
	writeProjectFile(t, root, "app", "conf/local.txt", "local")
	writeProjectFile(t, root, "app", "conf/other.yaml", "other")
	writeProjectFile(t, root, "app", "src/main.go", "package main")
	writeProjectFile(t, root, "app", ".gen/generated.txt", "generated")
	writeProjectFile(t, root, "app", "node_modules/dep/index.js", "dep")
	writeProjectFile(t, root, "", "shared.lock", "lock")
	ext := &extension.ExtensionDescription{Name: "@test/ext", Tasks: map[string]extension.TaskDefinition{
		"keyed":   {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{"dist": {Kind: extension.OutputKindDirectory, Path: "dist"}}}},
		"unkeyed": {},
	}}
	job := func(task string, cache bool, def extension.JobDefinition) *ScheduledJob {
		def.Name, def.ExtensionName, def.Cache = "build~"+task, ext.Name, cache
		return &ScheduledJob{Project: app, Extension: ext, Step: &extension.PipelineStep{ID: task, Task: task}, JobDef: &def}
	}
	patterned := job("keyed", true, extension.JobDefinition{
		FilePatterns:    []string{"conf/*.txt", ".gen/*.txt"},
		TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Files: []string{"conf/*.txt", ".gen/*.txt"}, WorkspaceFiles: []string{"shared.lock"}, Env: []string{"TARGET"}}},
	})
	wholeTree := job("keyed", true, extension.JobDefinition{TaskCachePolicy: &extension.TaskCachePolicy{Outputs: []extension.TaskOutputArtifact{{ID: "bin", Kind: "file", Path: "out/bin"}}}})
	unkeyed := job("unkeyed", false, extension.JobDefinition{TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Env: []string{"IGNORED"}}}})
	unkeyedPatterned := job("unkeyed", false, extension.JobDefinition{FilePatterns: []string{"conf/*.yaml"}})

	inputs, err := PortableInputs(ws, []*ScheduledJob{patterned, wholeTree, unkeyed, unkeyedPatterned}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.Tasks) != 4 {
		t.Fatalf("tasks = %+v", inputs.Tasks)
	}
	// Declared patterns are a statement of what the task reads; the
	// whole-tree fallback of a keyed task with no declaration is not, and is
	// reported separately so nothing selected only by it can be bound.
	want := map[int][2]string{
		0: {"app/.gen/generated.txt,app/conf/local.txt,shared.lock", ""},
		1: {"", "app/conf/local.txt,app/conf/other.yaml,app/src/main.go"},
		2: {"", ""},
		3: {"app/conf/other.yaml", ""},
	}
	for index, sets := range want {
		if got := strings.Join(inputs.Tasks[index].Files, ","); got != sets[0] {
			t.Errorf("task %d declared files = %q, want %q", index, got, sets[0])
		}
		if got := strings.Join(inputs.Tasks[index].FallbackFiles, ","); got != sets[1] {
			t.Errorf("task %d fallback files = %q, want %q", index, got, sets[1])
		}
	}
	if got := strings.Join(inputs.Tasks[0].Env, ","); got != "TARGET" {
		t.Errorf("keyed env = %q, want TARGET", got)
	}
	if len(inputs.Tasks[2].Env) != 0 {
		t.Errorf("an unkeyed task contributed env names: %v", inputs.Tasks[2].Env)
	}
	if got := strings.Join(inputs.Outputs, ","); got != "app/dist,app/out/bin" {
		t.Errorf("outputs = %q", got)
	}
	// A path outside the workspace root is not capturable and is dropped.
	if got := workspaceRelativePaths(root, []string{filepath.Join(root, "..", "outside"), filepath.Join(root, "in")}); strings.Join(got, ",") != "in" {
		t.Errorf("workspaceRelativePaths = %v", got)
	}
	if err := os.RemoveAll(filepath.Join(root, "app", "conf")); err != nil {
		t.Fatal(err)
	}
	remaining, err := PortableInputs(ws, []*ScheduledJob{patterned}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := remaining.Tasks[0].Files; strings.Join(got, ",") != "app/.gen/generated.txt,shared.lock" {
		t.Errorf("a deleted input stayed in the projection: %v", got)
	}
}

// One PortableInputs call walks a (root, pattern set) once: a tree that
// changes between two collects inside the same call must answer with the
// memoized set, which is the only observable proof the second walk did not
// happen. A later call sees the later tree, because the memo is per call.
func TestPortableInputsMemoizesEachRootAndPatternSetOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProjectFile(t, root, "app", "src/main.go", "package main")
	memo := keyFileMemo{}
	first, err := memo.collect(filepath.Join(root, "app"), []string{"src/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, "app", "src/second.go", "package main")
	if again, err := memo.collect(filepath.Join(root, "app"), []string{"src/*.go"}); err != nil || len(again) != len(first) || len(again) != 1 {
		t.Fatalf("the second collect walked again: %v then %v", first, again)
	}
	if other, err := memo.collect(filepath.Join(root, "app"), []string{"src/**"}); err != nil || len(other) != 2 {
		t.Fatalf("a different pattern set shared a memo entry: %v", other)
	}
	if fresh, err := (keyFileMemo{}).collect(filepath.Join(root, "app"), []string{"src/*.go"}); err != nil || len(fresh) != 2 {
		t.Fatalf("the memo outlived its call: %v", fresh)
	}
}
