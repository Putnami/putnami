package cachekey

import (
	"os"
	"path/filepath"
	"strings"
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

// TestBatchProjectLimitResolvesTheNamedOption pins the resolution rule. A
// policy that names no parameter, or whose parameter is unset, keeps its static
// cap. A positive integer replaces it, through either spelling the CLI
// delivers. 1 means one project per invocation, which is singleton dispatch.
// Anything else is an error, never a silent grouping.
func TestBatchProjectLimitResolvesTheNamedOption(t *testing.T) {
	t.Parallel()
	static := &extension.TaskBatchPolicy{Tool: "suite", MaxProjects: 4}
	named := &extension.TaskBatchPolicy{Tool: "suite", MaxProjects: 4, MaxProjectsParam: batchCapOption}
	unbounded := &extension.TaskBatchPolicy{Tool: "suite", MaxProjectsParam: batchCapOption}

	for _, tc := range []struct {
		name    string
		policy  *extension.TaskBatchPolicy
		params  extension.ParamMap
		limit   int
		batches bool
		fails   bool
	}{
		{name: "no policy", policy: nil},
		{name: "a policy naming no param ignores the option", policy: static,
			params: extension.ParamMap{batchCapOption: 2.0}, limit: 4, batches: true},
		{name: "unset keeps the static cap", policy: named, limit: 4, batches: true},
		{name: "unset keeps an unbounded policy unbounded", policy: unbounded, limit: 0, batches: true},
		{name: "null keeps the static cap", policy: named,
			params: extension.ParamMap{batchCapOption: nil}, limit: 4, batches: true},
		{name: "kebab spelling", policy: named,
			params: extension.ParamMap{batchCapOption: 6.0}, limit: 6, batches: true},
		{name: "camel alias", policy: named,
			params: extension.ParamMap{"batchMaxProjects": 6.0}, limit: 6, batches: true},
		{name: "go int", policy: unbounded,
			params: extension.ParamMap{batchCapOption: 3}, limit: 3, batches: true},
		{name: "1 runs every project alone", policy: unbounded,
			params: extension.ParamMap{batchCapOption: 1.0}, limit: 1, batches: false},
		{name: "zero", policy: unbounded, params: extension.ParamMap{batchCapOption: 0.0}, fails: true},
		{name: "negative", policy: unbounded, params: extension.ParamMap{batchCapOption: -2.0}, fails: true},
		{name: "fraction", policy: unbounded, params: extension.ParamMap{batchCapOption: 2.5}, fails: true},
		{name: "too large", policy: unbounded, params: extension.ParamMap{batchCapOption: 1e12}, fails: true},
		{name: "string", policy: unbounded, params: extension.ParamMap{batchCapOption: "6"}, fails: true},
		{name: "boolean", policy: unbounded, params: extension.ParamMap{batchCapOption: true}, fails: true},
	} {
		limit, batches, err := BatchProjectLimit(tc.policy, tc.params)
		if tc.fails {
			if err == nil || !strings.Contains(err.Error(), batchCapOption) {
				t.Errorf("%s: err = %v, want an error naming %s", tc.name, err, batchCapOption)
			}
			if batches {
				t.Errorf("%s: an invalid cap still batches", tc.name)
			}
			continue
		}
		if err != nil || limit != tc.limit || batches != tc.batches {
			t.Errorf("%s: got (%d, %t, %v), want (%d, %t, nil)", tc.name, limit, batches, err, tc.limit, tc.batches)
		}
	}

	// A quoted number is the likeliest mistake, and an undeclared command-line
	// flag arrives the same way; the error says why "2" is refused.
	if _, _, err := BatchProjectLimit(unbounded, extension.ParamMap{batchCapOption: "2"}); err == nil ||
		!strings.Contains(err.Error(), "JSON number") {
		t.Errorf("quoted number: err = %v, want an error asking for a JSON number", err)
	}
}
