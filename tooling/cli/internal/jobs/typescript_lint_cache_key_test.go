package jobs

import (
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A TypeScript lint verdict describes authored source and configuration, not
// the generated .gen tree. Generation can rewrite TypeScript and JSON there
// between otherwise identical lint runs, so including it makes the stored
// lint~check-only entry unreachable on the next run.
func TestTypeScriptLintCheckOnlyCacheKeyIgnoresGeneratedFiles(t *testing.T) {
	t.Parallel()
	repoRoot := findJobsRepoRoot(t)
	manifestPath := filepath.Join(repoRoot, "typescript", "extension", "putnami.extension.json")
	manifest, err := extension.LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("load TypeScript extension manifest: %v", err)
	}
	typescript := extension.Resolve(manifest, filepath.Dir(manifestPath))
	typescript.Name = "@putnami/typescript"

	lintCommand := typescript.Jobs["lint"]
	if lintCommand == nil {
		t.Fatal("TypeScript extension has no lint command")
	}
	expanded, err := extension.ExpandPipeline(
		"lint",
		lintCommand,
		lintCommand.PipelineSteps,
		typescript,
		extension.ParamMap{"fix": false},
	)
	if err != nil {
		t.Fatalf("expand TypeScript lint pipeline: %v", err)
	}
	if len(expanded) != 1 || expanded[0].JobDef.Name != "lint~check-only" {
		t.Fatalf("read-only lint pipeline = %v, want only lint~check-only", expandedJobNames(expanded))
	}

	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	write := func(rel, contents string) {
		t.Helper()
		path := filepath.Join(projectRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/index.ts", "export const value = 1;\n")
	write("biome.json", "{}\n")
	write("package.json", "{\"name\":\"project\"}\n")
	write(".gen/src/generated.ts", "export const generated = 1;\n")
	write(".gen/schema/generated.json", "{\"generated\":1}\n")

	project := &workspace.Project{ID: "/project", Name: "project", Path: "project"}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{project})
	step := expanded[0].Step
	job := &ScheduledJob{
		Project:   project,
		Extension: typescript,
		JobDef:    expanded[0].JobDef,
		Step:      &step,
	}
	key := func() string {
		t.Helper()
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
		got, err := computeJobCacheHash(ws, job, extension.ParamMap{"fix": false}, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute lint~check-only cache key: %v", err)
		}
		return got
	}

	baseline := key()
	write(".gen/src/generated.ts", "export const generated = 2;\n")
	if got := key(); got != baseline {
		t.Errorf("generated TypeScript changed lint~check-only key (%s -> %s)", baseline, got)
	}
	write(".gen/schema/generated.json", "{\"generated\":2}\n")
	if got := key(); got != baseline {
		t.Errorf("generated JSON changed lint~check-only key (%s -> %s)", baseline, got)
	}

	write("src/index.ts", "export const value = 2;\n")
	sourceKey := key()
	if sourceKey == baseline {
		t.Error("authored TypeScript did not change lint~check-only key")
	}
	write("biome.json", "{\"formatter\":{\"enabled\":false}}\n")
	if got := key(); got == sourceKey {
		t.Error("lint configuration did not change lint~check-only key")
	}
}

func expandedJobNames(steps []extension.ExpandedStep) []string {
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.JobDef.Name)
	}
	return names
}
