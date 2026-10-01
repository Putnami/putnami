package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// batchCapOption is the parameter the fixtures' batch policy names, spelled as
// the Go extension names it for go-test.
const batchCapOption = "batch-max-projects"

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
		limit, batches, err := batchProjectLimit(tc.policy, tc.params)
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
	if _, _, err := batchProjectLimit(unbounded, extension.ParamMap{batchCapOption: "2"}); err == nil ||
		!strings.Contains(err.Error(), "JSON number") {
		t.Errorf("quoted number: err = %v, want an error asking for a JSON number", err)
	}
}

// makeGoTestBatchFixture plans four go-test-like jobs whose batch policy
// names the cap option and carries no static cap, as the Go extension's
// test-exec task does. Pending order is a, b, c, d: longest first.
func makeGoTestBatchFixture(t *testing.T) (*workspace.Workspace, []*ScheduledJob) {
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
	planned := make([]*ScheduledJob, 0, len(projects))
	for _, project := range projects {
		planned = append(planned, &ScheduledJob{
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

// TestGoTestBatchesHonourTheWorkspaceProjectCap drives the real grouping
// methods over one scheduler while the workspace option changes, because "a
// batch holds at most N projects" is a property of takePendingGroup and
// readyBatchKey together and of nothing smaller.
func TestGoTestBatchesHonourTheWorkspaceProjectCap(t *testing.T) {
	ws, planned := makeGoTestBatchFixture(t)
	a, b, c, d := planned[0], planned[1], planned[2], planned[3]
	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{}, &mockRenderer{}, nil)
	groupOf := func(pending []*ScheduledJob) ([]string, []*ScheduledJob) {
		t.Helper()
		group, rest := scheduler.takePendingGroup(pending)
		return keysOf(group.jobs), rest
	}
	same := func(got []string, want ...*ScheduledJob) bool {
		wanted := keysOf(want)
		if len(got) != len(wanted) {
			return false
		}
		for i := range got {
			if got[i] != wanted[i] {
				return false
			}
		}
		return true
	}

	// Today's key: the policy before it named a parameter, nothing set.
	policy := a.JobDef.Batchable
	setGoTestOptions(ws, nil)
	for _, job := range planned {
		job.JobDef.Batchable = &extension.TaskBatchPolicy{Tool: "go-test"}
	}
	today := scheduler.readyBatchKey(a)
	for _, job := range planned {
		job.JobDef.Batchable = policy
	}
	if today == "" {
		t.Fatal("fixture does not batch; every assertion below would be vacuous")
	}

	// Unset: the key is byte for byte today's and the batch is unbounded.
	if got := scheduler.readyBatchKey(a); got != today {
		t.Fatalf("unset option moved the batch key: %s, want %s", got, today)
	}
	if key, limit := scheduler.readyBatch(a); key != today || limit != 0 {
		t.Fatalf("unset readyBatch = (%s, %d), want today's key and no cap", key, limit)
	}
	if got, rest := groupOf(planned); !same(got, a, b, c, d) || len(rest) != 0 {
		t.Fatalf("unset option grouped %v + %d, want every project in one batch", got, len(rest))
	}

	// 2: the long pole pairs with the shortest peer, then the middle two pair.
	setGoTestOptions(ws, extension.ParamMap{batchCapOption: 2.0})
	capTwo := scheduler.readyBatchKey(a)
	// The cap takePendingGroup fills to is the one this key folds: both come
	// from one readyBatch call on the leader.
	if key, limit := scheduler.readyBatch(a); key != capTwo || limit != 2 {
		t.Fatalf("cap 2 readyBatch = (%s, %d), want readyBatchKey's key and the cap 2 it folds", key, limit)
	}
	got, rest := groupOf(planned)
	if !same(got, a, d) {
		t.Fatalf("cap 2 first batch = %v, want the long pole with the shortest peer", got)
	}
	if got, rest = groupOf(rest); !same(got, b, c) || len(rest) != 0 {
		t.Fatalf("cap 2 second batch = %v + %d, want the remaining pair", got, len(rest))
	}

	// 3: a different cap is a different batch key, so two workspaces with
	// different values never share a batch.
	setGoTestOptions(ws, extension.ParamMap{batchCapOption: 3.0})
	capThree := scheduler.readyBatchKey(a)
	for name, key := range map[string]string{"cap 2": capTwo, "cap 3": capThree} {
		if key == "" || key == today {
			t.Errorf("%s key = %q, want a key distinct from the unset key", name, key)
		}
	}
	if capTwo == capThree {
		t.Error("caps 2 and 3 share a batch key")
	}
	if got, rest = groupOf(planned); !same(got, a, d, c) || len(rest) != 1 {
		t.Fatalf("cap 3 first batch = %v + %d, want three projects", got, len(rest))
	}

	// A project that sets its own value batches only with peers that resolve
	// the same value.
	d.Project.Config = &wsproto.ProjectConfig{Options: map[string]extension.ParamMap{
		"test": {batchCapOption: 2.0},
	}}
	if got, rest = groupOf(planned); !same(got, a, c, b) || len(rest) != 1 || rest[0] != d {
		t.Fatalf("project override grouped %v, left %v; want the project with its own cap left out", got, keysOf(rest))
	}
	d.Project.Config = nil

	// 1: every project runs alone.
	setGoTestOptions(ws, extension.ParamMap{batchCapOption: 1.0})
	if key := scheduler.readyBatchKey(a); key != "" {
		t.Fatalf("cap 1 batch key = %q, want singleton dispatch", key)
	}
	if got, rest = groupOf(planned); !same(got, a) || len(rest) != 3 {
		t.Fatalf("cap 1 grouped %v + %d, want one project", got, len(rest))
	}

	// An invalid value never groups, and the plan refuses it before any task
	// runs.
	setGoTestOptions(ws, extension.ParamMap{batchCapOption: "six"})
	if key := scheduler.readyBatchKey(a); key != "" {
		t.Fatalf("invalid cap batch key = %q, want singleton dispatch", key)
	}
	err := validateBatchProjectLimits(planned, ws, nil)
	if err == nil || !strings.Contains(err.Error(), batchCapOption) || !strings.Contains(err.Error(), a.Key()) {
		t.Fatalf("plan validation = %v, want an error naming %s and %s", err, batchCapOption, a.Key())
	}
	setGoTestOptions(ws, extension.ParamMap{batchCapOption: 4.0})
	if err := validateBatchProjectLimits(planned, ws, nil); err != nil {
		t.Fatalf("valid cap refused: %v", err)
	}
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
		hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
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

// TestPlanRefusesAnInvalidBatchProjectCap pins the wiring: a bad value fails
// Plan itself, so every adapter (run, --plan, MCP) reports it before a task
// starts, while a valid or absent value plans normally.
func TestPlanRefusesAnInvalidBatchProjectCap(t *testing.T) {
	t.Parallel()
	plan := func(options extension.ParamMap) error {
		t.Helper()
		ws := makeTestWorkspace()
		ext := makeTestExtension()
		ext.Jobs["test"].Batchable = &extension.TaskBatchPolicy{Tool: "suite", MaxProjectsParam: batchCapOption}
		ws.Config = &wsproto.Config{}
		if options != nil {
			ws.Config.Options = map[string]extension.ParamMap{"test": options}
		}
		_, err := Plan(ws, []string{"test"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
		return err
	}

	for name, options := range map[string]extension.ParamMap{
		"unset": nil,
		"cap 2": {batchCapOption: 2.0},
		"cap 1": {batchCapOption: 1.0},
	} {
		if err := plan(options); err != nil {
			t.Errorf("%s: Plan = %v, want success", name, err)
		}
	}
	for name, options := range map[string]extension.ParamMap{
		"zero":   {batchCapOption: 0.0},
		"string": {batchCapOption: "six"},
		"camel":  {"batchMaxProjects": -1.0},
	} {
		err := plan(options)
		if err == nil || !strings.Contains(err.Error(), "invalid batch size") {
			t.Errorf("%s: Plan = %v, want an invalid batch size error", name, err)
		}
	}
}
