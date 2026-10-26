package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The executing side of a frozen request plans from the submitter's changed
// paths, so a change-gated step it would otherwise drop is in its plan, bound
// to the same files: the plan it proves equal to the expected one is the
// submitter's.
func TestFrozenSelectionPlansTheSubmittersChangedFiles(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-step-reads-the-changed-files",
		"a-frozen-request-plans-the-submitters-changed-files")
	root := t.TempDir()
	for _, rel := range []string{"lib/a.go", "lib/sub/b.go"} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@test/go"}}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib})
	exts := []*extension.ExtensionDescription{{
		Name: "@test/go", Version: "1.0.0",
		Jobs: map[string]*extension.JobDefinition{"test": {
			ExtensionName: "@test/go", Name: "test", Kind: "command", Command: "true", Cache: true,
			PipelineSteps: []extension.PipelineStep{
				{ID: "test", Task: "test-exec"},
				{ID: "test-race", Task: "test-race-exec",
					Activation: &extension.StepActivation{ChangedFiles: []string{"**/*.go"}},
					With: map[string]extension.InputBinding{
						"changed-files": {From: extensionproto.InputBindingFromContext, Path: extensionproto.ContextPathChangedFiles},
					}},
			},
		}},
		Tasks: map[string]extension.TaskDefinition{
			"test-exec":      {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{}},
			"test-race-exec": {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{}},
		},
	}}

	measured := []string{"lib/sub/b.go", "lib/a.go"}
	submitted, err := planner.Plan(ws, []string{"test"}, []*workspace.Project{lib}, exts, nil, nil, nil, measured)
	if err != nil {
		t.Fatal(err)
	}

	executing := &Request{Portable: &PortableExecution{Request: runner.ExecutionRequest{
		Selection: runner.SelectionBlock{Projects: []string{"/lib"}, ChangedPaths: runner.SortStrings(measured)},
	}}}
	selected, code := selectFrozenProjects(executing, ws)
	if code != ExitSuccess {
		t.Fatalf("selectFrozenProjects exit = %d", code)
	}
	replanned, err := planner.Plan(ws, []string{"test"}, selected, exts, nil, nil, nil, executing.selectionEvidence.changedPaths)
	if err != nil {
		t.Fatal(err)
	}

	expected, err := expectedPlan(submitted)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateExpectedPlan(expected, replanned); err != nil {
		t.Fatalf("the executing side planned another graph: %v", err)
	}
	race := func(planned []*scheduler.ScheduledJob) any {
		for _, job := range planned {
			if job.Key() == "/lib:test~test-race" {
				return job.JobDef.BoundParams["changed-files"]
			}
		}
		t.Fatal("/lib:test~test-race is not planned")
		return nil
	}
	if got, want := race(replanned), race(submitted); !reflect.DeepEqual(got, want) {
		t.Errorf("executing side bound %v, submitter bound %v", got, want)
	}

	// A request that measured no diff plans the step on neither side.
	executing = &Request{Portable: &PortableExecution{Request: runner.ExecutionRequest{
		Selection: runner.SelectionBlock{Projects: []string{"/lib"}, ChangedPaths: []string{}},
	}}}
	if _, code := selectFrozenProjects(executing, ws); code != ExitSuccess || executing.selectionEvidence.changedPaths != nil {
		t.Fatalf("no diff: exit %d, changed paths %v", code, executing.selectionEvidence.changedPaths)
	}
}
