package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// makeMixedBatchPlan is a plan whose dispatch exercises every branch of
// takePendingGroup: two batch classes (an unbounded lint class whose members
// read either the shared or a project-local config, a bounded test class),
// nested project roots, a project without matching inputs, and ordinary work
// with and without a longer critical path.
func makeMixedBatchPlan(t *testing.T) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "biome.json"), `{"formatter":{"enabled":true}}`)

	var projects []*workspace.Project
	for i := range 12 {
		path := fmt.Sprintf("p%02d", i)
		if i == 5 {
			path = "p04/nested" // overlaps its parent, so the two never share a group
		}
		projects = append(projects, &workspace.Project{ID: "/" + path, Name: path, Path: path})
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
		if i != 7 { // p07 has no matching input: singleton dispatch
			writeTestFile(t, filepath.Join(root, path, "src.txt"), path)
		}
		if i%4 == 3 { // a project-local config: its own batch class
			writeTestFile(t, filepath.Join(root, path, "biome.json"), `{"formatter":{"enabled":true}}`)
		}
	}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "mixed-batch"
	ext := &extension.ExtensionDescription{
		Name:    "@putnami/typescript",
		Version: "1.0.0",
		Path:    filepath.Join(root, "extension"),
		Tasks: map[string]extension.TaskDefinition{
			"lint-all": {Declares: &extension.TaskDeclaration{}},
			"test-run": {Declares: &extension.TaskDeclaration{}},
			"compile":  {Declares: &extension.TaskDeclaration{}},
		},
	}
	lint := &extension.TaskBatchPolicy{
		Tool:        "biome",
		ConfigFiles: []string{"{projectRoot}/biome.json", "{workspaceRoot}/biome.json"},
		Kind:        extensionproto.CommandKindDiagnostics,
		Phase:       "lint",
	}
	test := &extension.TaskBatchPolicy{
		Tool:        "bun",
		MaxProjects: 3,
		// No candidate exists, so the key folds no path of the temporary
		// root: it is the same on every machine, and the test pins it.
		ConfigFiles: []string{"{workspaceRoot}/bunfig.toml"},
	}

	var planned []*ScheduledJob
	for i, project := range projects {
		planned = append(planned,
			&ScheduledJob{
				Project:        project,
				Extension:      ext,
				Step:           &extension.PipelineStep{ID: "check", Task: "lint-all"},
				CriticalPathMs: int64(1000 * (i % 3)),
				JobDef: &extension.JobDefinition{
					ExtensionName: ext.Name, Name: "lint~check", CommandName: "lint", StepID: "check",
					Command: "/bin/true", Cwd: "{projectRoot}", Cache: true,
					FilePatterns: []string{"src.txt"}, Batchable: lint,
				},
			},
			&ScheduledJob{
				Project:        project,
				Extension:      ext,
				Step:           &extension.PipelineStep{ID: "test", Task: "test-run"},
				CriticalPathMs: int64(500 * (i % 4)),
				JobDef: &extension.JobDefinition{
					ExtensionName: ext.Name, Name: "test~test", CommandName: "test", StepID: "test",
					Command: "/bin/true", Cwd: "{projectRoot}", Cache: true, Batchable: test,
				},
			},
			&ScheduledJob{
				Project:        project,
				Extension:      ext,
				Step:           &extension.PipelineStep{ID: "compile", Task: "compile"},
				CriticalPathMs: int64(700 * (i % 5)),
				JobDef: &extension.JobDefinition{
					ExtensionName: ext.Name, Name: "build~compile", CommandName: "build", StepID: "compile",
					Command: "/bin/true", Cwd: "{projectRoot}", Cache: true,
				},
			},
		)
	}
	return ws, planned
}

// mainDispatchGroups are the groups the scheduler dispatched for
// makeMixedBatchPlan before the batch key was memoized (a9bf099d), recomputing
// every candidate's key on every scan, in dispatch order.
var mainDispatchGroups = [][]string{
	{"/p04:build~compile"},
	{"/p09:build~compile"},
	{"/p03:build~compile"},
	{"/p08:build~compile"},
	{"/p02:lint~check", "/p04/nested:lint~check", "/p08:lint~check", "/p01:lint~check", "/p10:lint~check", "/p00:lint~check", "/p06:lint~check", "/p09:lint~check"},
	{"/p11:lint~check"},
	{"/p03:test~test", "/p08:test~test", "/p04:test~test"},
	{"/p07:test~test", "/p00:test~test", "/p09:test~test"},
	{"/p11:test~test", "/p04/nested:test~test", "/p01:test~test"},
	{"/p02:build~compile"},
	{"/p07:build~compile"},
	{"/p02:test~test", "/p10:test~test", "/p06:test~test"},
	{"/p07:lint~check"},
	{"/p04:lint~check"},
	{"/p01:build~compile"},
	{"/p06:build~compile"},
	{"/p11:build~compile"},
	{"/p00:build~compile"},
	{"/p04/nested:build~compile"},
	{"/p10:build~compile"},
	{"/p03:lint~check"},
}

// mainTestClassKey is the batch key the scheduler computed for the test class
// of makeMixedBatchPlan at a9bf099d, from json.Marshal(BuildJobContext(...).Params).
// The class reads no config file, so the key folds no path of the temporary
// root.
const mainTestClassKey = "e5104c5f906474c8dd446ffd401e98b905835b6921229e7eaf3455eed9c89f2a"

// newBatchMemoScheduler is this file's one scheduler construction: the memo
// and the dispatch scan it feeds are scheduler methods.
func newBatchMemoScheduler(ws *workspace.Workspace, planned []*ScheduledJob) *Scheduler {
	return newScheduler(ws, planned, nil, SchedulerConfig{}, &mockRenderer{}, nil)
}

// queueReady computes each job's batch key when the job becomes ready, and the
// dispatch scans that follow form the groups the scheduler formed before the
// key was memoized, from the key it computed then.
func TestPendingBatchMemoKeepsTheDispatchGroups(t *testing.T) {
	ws, planned := makeMixedBatchPlan(t)
	scheduler := newBatchMemoScheduler(ws, planned)
	scheduler.metrics = newSchedulerMetrics() // Run sets it up
	var mu sync.Mutex
	completed := 0
	state := planner.NewDAGState(planner.SchedulableJobs(planned))
	pending := scheduler.queueReady(nil, planned, state, map[string]*JobResult{}, &mu, &completed)
	if len(scheduler.pendingBatches) != len(planned) {
		t.Fatalf("queueReady computed %d batch keys, want one per ready job (%d)", len(scheduler.pendingBatches), len(planned))
	}
	for _, job := range planned {
		if job.JobDef.CommandName != "test" {
			continue
		}
		if key := scheduler.pendingBatches[job].key; key != mainTestClassKey {
			t.Fatalf("%s: batch key %s, want the key computed from the job context's params %s", job.Key(), key, mainTestClassKey)
		}
	}

	var got [][]string
	for len(pending) > 0 {
		var group jobGroup
		group, pending = scheduler.takePendingGroup(pending)
		got = append(got, keysOf(group.jobs))
	}
	if !reflect.DeepEqual(got, mainDispatchGroups) {
		t.Fatalf("dispatch groups differ from before the memo:\n got  %v\n want %v", got, mainDispatchGroups)
	}
}

// A dispatch scan reads no file: once a job is pending, its key is the one
// computed when it became ready, even after its inputs and configs are gone.
func TestPendingBatchScanReadsNoFile(t *testing.T) {
	ws, jobs := makeBatchSchedulerFixture(t, "")
	scheduler := newBatchMemoScheduler(ws, jobs)
	for _, job := range jobs {
		scheduler.pendingBatch(job)
	}
	if err := os.RemoveAll(ws.Root); err != nil {
		t.Fatal(err)
	}
	if key := scheduler.readyBatchKey(jobs[0]); key != "" {
		t.Fatalf("a fresh computation without inputs = %q, want no batch key", key)
	}
	group, rest := scheduler.takePendingGroup(jobs)
	if len(group.jobs) != 2 || len(rest) != 0 {
		t.Fatalf("scan regrouped from the tree: %d + %d, want the ready-time group 2 + 0", len(group.jobs), len(rest))
	}
}

// The run's membership snapshot yields the same document as resolving the
// workspace per job context, and each context owns its member list.
func TestRunWorkspaceMembershipMatchesPerContextResolution(t *testing.T) {
	ws, jobs := makeBatchSchedulerFixture(t, "")
	ws.Projects[0].Config = &wsproto.ProjectConfig{Name: "a", Tags: []string{"lint"}}
	versions := cachekey.RunVersions{"": {Base: "1.2.3"}}
	membership := cachekey.NewWorkspaceMembership(ws)

	for _, job := range jobs {
		want := BuildJobContext(ws, job, nil, nil, versions).WorkspaceProjects
		got := buildJobContext(ws, job, nil, nil, versions, membership).WorkspaceProjects
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: membership snapshot = %+v, want %+v", job.Key(), got, want)
		}
	}

	first := membership.Context(versions)
	first[0].Version = "changed"
	if again := membership.Context(versions); again[0].Version != "1.2.3" {
		t.Fatalf("a context's member list is shared: version %q", again[0].Version)
	}
	if empty := cachekey.NewWorkspaceMembership(nil).Context(versions); empty != nil {
		t.Fatalf("membership of no workspace = %v, want nil", empty)
	}
}
