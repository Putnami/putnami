package cachekey

import (
	"os"
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// batchCapOption is the parameter the fixtures' batch policy names, spelled as
// the Go extension names it for go-test.
const batchCapOption = "batch-max-projects"

// makeGoTestBatchFixture plans four go-test-like jobs whose batch policy
// names the cap option and carries no static cap, as the Go extension's
// test-exec task does. Pending order is a, b, c, d: longest first.
func makeGoTestBatchFixture(t *testing.T) (*workspace.Workspace, []*jobmodel.ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "extension"), 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{"a", "b", "c", "d"}
	projects := make([]*workspace.Project, 0, len(names))
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		projects = append(projects, &workspace.Project{ID: "/" + name, Name: name, Path: name})
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, projects)
	ws.Name = "go-test-batch"
	ext := &extension.ExtensionDescription{
		Name:    "@putnami/go",
		Version: "1.0.0",
		Path:    filepath.Join(root, "extension"),
		Jobs: map[string]*extension.JobDefinition{
			"test": {Flags: map[string]extension.FlagDefinition{
				"race": {Type: "boolean", Default: false},
			}},
		},
		Tasks: map[string]extension.TaskDefinition{
			"test-exec": {Declares: &extension.TaskDeclaration{}},
		},
	}
	policy := &extension.TaskBatchPolicy{Tool: "go-test", MaxProjectsParam: batchCapOption}
	planned := make([]*jobmodel.ScheduledJob, 0, len(projects))
	for _, project := range projects {
		planned = append(planned, &jobmodel.ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: "test", Task: "test-exec"},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@putnami/go",
				Name:          "test~test",
				CommandName:   "test",
				StepID:        "test",
				Command:       "/bin/true",
				Cwd:           "{projectRoot}",
				Cache:         true,
				Batchable:     policy,
			},
		})
	}
	return ws, planned
}

// setGoTestOptions replaces the workspace's options for the Go test command.
// Nil clears them, which is a workspace that never set the option.
func setGoTestOptions(ws *workspace.Workspace, options extension.ParamMap) {
	if options == nil {
		ws.Config.Options = nil
		return
	}
	ws.Config.Options = map[string]extension.ParamMap{"@putnami/go:test": options}
}

// TestBatchProjectCapNeverKeysTheTask proves the option groups jobs without
// moving the task cache key: setting it hits the entry an unset run wrote.
// The declared race flag moving the same key shows the comparison is not
// vacuous.
func TestBatchProjectCapNeverKeysTheTask(t *testing.T) {
	t.Parallel()
	ws, planned := makeGoTestBatchFixture(t)
	job := planned[0]
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	keyWith := func(options extension.ParamMap) string {
		t.Helper()
		setGoTestOptions(ws, options)
		hash, err := ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return hash
	}

	unset := keyWith(nil)
	for _, value := range []any{2.0, 6.0, 1.0} {
		if got := keyWith(extension.ParamMap{batchCapOption: value}); got != unset {
			t.Errorf("%s=%v moved the task cache key", batchCapOption, value)
		}
	}
	if got := keyWith(extension.ParamMap{"race": true}); got == unset {
		t.Fatal("a declared flag did not move the task cache key; the comparison above proves nothing")
	}
}
