package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// batchCapOption is the parameter the fixtures' batch policy names, spelled as
// the Go extension names it for go-test.
const batchCapOption = "batch-max-projects"

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
	err := planner.ValidateBatchProjectLimits(planned, ws, nil)
	if err == nil || !strings.Contains(err.Error(), batchCapOption) || !strings.Contains(err.Error(), a.Key()) {
		t.Fatalf("plan validation = %v, want an error naming %s and %s", err, batchCapOption, a.Key())
	}
	setGoTestOptions(ws, extension.ParamMap{batchCapOption: 4.0})
	if err := planner.ValidateBatchProjectLimits(planned, ws, nil); err != nil {
		t.Fatalf("valid cap refused: %v", err)
	}
}
