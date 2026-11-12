package planner

import (
	"slices"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// closureLint is a lint command that reads the generated clients of its
// project's dependencies: it depends on client-generate across the dependency
// closure.
func closureLint() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@putnami/go",
		Jobs: map[string]*extension.JobDefinition{
			"lint": {
				ExtensionName:    "@putnami/go",
				Name:             "lint",
				CommandDependsOn: []string{"^client-generate"},
				PipelineSteps:    []extension.PipelineStep{{ID: "check", Task: "lint-check"}},
			},
		},
		Tasks: map[string]extension.TaskDefinition{"lint-check": {Kind: "command", Command: "run"}},
	}
}

// closureClientgen is a client-generate command with two leaf steps.
func closureClientgen() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@putnami/clientgen",
		Jobs: map[string]*extension.JobDefinition{
			"client-generate": {
				ExtensionName: "@putnami/clientgen",
				Name:          "client-generate",
				PipelineSteps: []extension.PipelineStep{
					{ID: "go", Task: "client-go"},
					{ID: "ts", Task: "client-ts"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"client-go": {Kind: "command", Command: "run"},
			"client-ts": {Kind: "command", Command: "run"},
		},
	}
}

// A dependency on the dependency closure plans the prerequisite command in
// every project the dependent's project depends on, directly or transitively,
// and nowhere else. The dependent's root step waits for each one's leaf steps.
// A project of the closure that does not run the command adds nothing.
func TestPlan_ClosureDependencyPlansTheCommandAcrossTheDependencyClosure(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan", "a-closure-dependency-plans-the-command-across-the-dependency-closure")
	goOnly := []string{"@putnami/go"}
	client := []string{"@putnami/go", "@putnami/clientgen"}
	projects := []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app", Extensions: client, Dependencies: []string{"/lib", "/direct"}},
		{ID: "/lib", Name: "lib", Path: "lib", Extensions: goOnly, Dependencies: []string{"/deep"}},
		{ID: "/direct", Name: "direct", Path: "direct", Extensions: client},
		{ID: "/deep", Name: "deep", Path: "deep", Extensions: client},
		{ID: "/stranger", Name: "stranger", Path: "stranger", Extensions: client},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "closure-dependency"
	planned, err := Plan(ws, []string{"lint"}, projects[:1],
		[]*extension.ExtensionDescription{closureLint(), closureClientgen()}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	want := []string{
		"/app:lint~check",
		"/deep:client-generate~go", "/deep:client-generate~ts",
		"/direct:client-generate~go", "/direct:client-generate~ts",
	}
	got := jobKeys(planned)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("planned = %v, want %v: the closure runs client-generate, the project itself and a stranger do not", got, want)
	}
	lint := scheduledJobByKey(planned, "/app:lint~check")
	if wantDeps := want[1:]; !slices.Equal(lint.DependsOn, wantDeps) {
		t.Errorf("lint deps = %v, want %v", lint.DependsOn, wantDeps)
	}
}

// Resolution reads the closure from the workspace graph: a closure dependency
// without a graph adds no edge, and a same-project dependency on the same
// command keeps its own project's leaves alone.
func TestResolveCommandDeps_ClosureDependency(t *testing.T) {
	t.Parallel()
	dep := &workspace.Project{ID: "/dep", Name: "dep", Path: "dep"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"/dep"}}
	ws := workspace.NewWorkspace("", nil, []*workspace.Project{app, dep})
	jobs := func(dependsOn ...string) (*jobmodel.ScheduledJob, []*jobmodel.ScheduledJob) {
		dependent := &jobmodel.ScheduledJob{Project: app, JobDef: &extension.JobDefinition{
			Name: "lint~check", CommandName: "lint", StepID: "check", CommandDependsOn: dependsOn,
		}}
		return dependent, []*jobmodel.ScheduledJob{
			{Project: app, JobDef: &extension.JobDefinition{Name: "client-generate~go", CommandName: "client-generate", StepID: "go"}},
			{Project: dep, JobDef: &extension.JobDefinition{Name: "client-generate~go", CommandName: "client-generate", StepID: "go"}},
			dependent,
		}
	}

	dependent, planned := jobs("^client-generate")
	resolveCommandDeps(planned, computeCommandLeaves(planned), ws)
	if want := []string{"/dep:client-generate~go"}; !slices.Equal(dependent.DependsOn, want) {
		t.Errorf("closure deps = %v, want %v", dependent.DependsOn, want)
	}

	dependent, planned = jobs("client-generate", "^client-generate")
	resolveCommandDeps(planned, computeCommandLeaves(planned), ws)
	if want := []string{"/app:client-generate~go", "/dep:client-generate~go"}; !slices.Equal(dependent.DependsOn, want) {
		t.Errorf("same-project and closure deps = %v, want %v", dependent.DependsOn, want)
	}

	dependent, planned = jobs("^client-generate")
	resolveCommandDeps(planned, computeCommandLeaves(planned), nil)
	if len(dependent.DependsOn) != 0 {
		t.Errorf("closure deps without a workspace = %v, want none", dependent.DependsOn)
	}
}
