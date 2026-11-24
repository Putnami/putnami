package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
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
	writeTestFile(t, filepath.Join(root, "bun.lock"), `{"lockfileVersion":1}`)

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
		ConfigFiles: []string{"{workspaceRoot}/bun.lock"},
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

// drainPendingGroups dispatches the whole queue and returns each group's keys,
// in dispatch order. forget drops the memo before every scan, which is how the
// scheduler computed the keys before they were memoized.
func drainPendingGroups(s *Scheduler, pending []*ScheduledJob, forget bool) [][]string {
	var groups [][]string
	for len(pending) > 0 {
		if forget {
			s.pendingBatches = nil
		}
		var group jobGroup
		group, pending = s.takePendingGroup(pending)
		groups = append(groups, keysOf(group.jobs))
	}
	return groups
}

// newBatchMemoScheduler is this file's one scheduler construction: the memo
// and the dispatch scan it feeds are scheduler methods.
func newBatchMemoScheduler(ws *workspace.Workspace, planned []*ScheduledJob) *Scheduler {
	return newScheduler(ws, planned, nil, SchedulerConfig{}, &mockRenderer{}, nil)
}

// The memo changes when a pending job's batch key is computed, never which
// groups dispatch: the groups of a fixture plan are the same whether every
// scan recomputes each key or reads the one computed when the job became ready.
func TestPendingBatchMemoKeepsTheDispatchGroups(t *testing.T) {
	ws, planned := makeMixedBatchPlan(t)
	pending := append([]*ScheduledJob(nil), planned...)
	sortPendingLongestFirst(pending)

	scheduler := newBatchMemoScheduler(ws, planned)
	recomputed := drainPendingGroups(scheduler, append([]*ScheduledJob(nil), pending...), true)

	scheduler.pendingBatches = nil
	for _, job := range pending {
		scheduler.pendingBatch(job)
	}
	got := drainPendingGroups(scheduler, append([]*ScheduledJob(nil), pending...), false)

	if !reflect.DeepEqual(got, recomputed) {
		t.Fatalf("memoized groups differ:\n got  %v\n want %v", got, recomputed)
	}
	batched := 0
	for _, group := range got {
		if len(group) > 1 {
			batched++
		}
	}
	if batched == 0 {
		t.Fatalf("fixture plan formed no batch group: %v", got)
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
