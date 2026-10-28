package planner

import (
	"slices"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A command that depends on one step of another command (protocol ADR 0017)
// plans the whole prerequisite command and waits for that step alone. The
// fixtures follow the two language builds clientgen reads: a Go build whose
// describe writes the contract (generate → describe → compile, and infra after
// generate and describe), with describe removed for a library, and a
// TypeScript build without a describe step.

// stepDependencyGoBuild is a Go-like build. Its describe step runs only for a
// project that is not a library. The condition stands in for the Go
// extension's activation gate, which reads the project's go.mod.
func stepDependencyGoBuild(name string) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: name,
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: name,
				Name:          "build",
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
					{ID: "describe", Task: "build-describe", DependsOn: []string{"^describe", "generate"}, If: "project.type != 'library'"},
					{ID: "compile", Task: "build-compile", DependsOn: []string{"describe"}},
					{ID: "infra", Task: "build-infra", DependsOn: []string{"generate", "describe"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-generate": {Kind: "command", Command: "run"},
			"build-describe": {Kind: "command", Command: "run"},
			"build-compile":  {Kind: "command", Command: "run"},
			"build-infra":    {Kind: "command", Command: "run"},
		},
	}
}

// stepDependencyTypeScriptBuild is a TypeScript-like build: no describe step.
func stepDependencyTypeScriptBuild() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@putnami/typescript",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: "@putnami/typescript",
				Name:          "build",
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "ts-generate", DependsOn: []string{"^generate"}},
					{ID: "transpile", Task: "ts-transpile", DependsOn: []string{"generate"}},
					{ID: "infra", Task: "ts-infra", DependsOn: []string{"generate", "transpile"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"ts-generate":  {Kind: "command", Command: "run"},
			"ts-transpile": {Kind: "command", Command: "run"},
			"ts-infra":     {Kind: "command", Command: "run"},
		},
	}
}

// stepDependencyClientgen is a clientgen-like command with two root steps
// that depends on dependsOn.
func stepDependencyClientgen(dependsOn ...string) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@putnami/clientgen",
		Jobs: map[string]*extension.JobDefinition{
			"clientgen": {
				ExtensionName:    "@putnami/clientgen",
				Name:             "clientgen",
				CommandDependsOn: dependsOn,
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate-go", Task: "clientgen-go"},
					{ID: "generate-ts", Task: "clientgen-ts"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"clientgen-go": {Kind: "command", Command: "run"},
			"clientgen-ts": {Kind: "command", Command: "run"},
		},
	}
}

// planClientgen plans clientgen for a Go app, a Go library and a TypeScript
// app, each a clientgen provider.
func planClientgen(t *testing.T, extensions []*extension.ExtensionDescription, goExtensions ...string) []*jobmodel.ScheduledJob {
	t.Helper()
	if len(goExtensions) == 0 {
		goExtensions = []string{"@putnami/go"}
	}
	projects := []*workspace.Project{
		{ID: "/go/app", Name: "go/app", Path: "go/app", Type: "app", Extensions: append(append([]string(nil), goExtensions...), "@putnami/clientgen")},
		{ID: "/go/lib", Name: "go/lib", Path: "go/lib", Type: "library", Extensions: append(append([]string(nil), goExtensions...), "@putnami/clientgen")},
		{ID: "/ts/app", Name: "ts/app", Path: "ts/app", Type: "app", Extensions: []string{"@putnami/typescript", "@putnami/clientgen"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "step-dependency"
	planned, err := Plan(ws, []string{"clientgen"}, projects, extensions, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return planned
}

// assertClientgenWaitsFor checks that both clientgen roots of project wait for
// exactly want.
func assertClientgenWaitsFor(t *testing.T, planned []*jobmodel.ScheduledJob, project string, want []string) {
	t.Helper()
	for _, step := range []string{"generate-go", "generate-ts"} {
		key := project + ":clientgen~" + step
		job := scheduledJobByKey(planned, key)
		if job == nil {
			t.Fatalf("%s not planned; jobs=%v", key, jobKeys(planned))
		}
		if !slices.Equal(job.DependsOn, want) {
			t.Errorf("%s deps = %v, want %v", key, job.DependsOn, want)
		}
	}
}

// TestPlan_StepDependencyWaitsForTheNamedStepsOnly pins the clientgen shape:
// each root waits for describe and generate where the plan holds them, never
// for compile or infra, and the plan holds the same jobs as with a bare
// dependency on build.
func TestPlan_StepDependencyWaitsForTheNamedStepsOnly(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan", "a-step-dependency-waits-for-that-step-alone")
	steps := planClientgen(t, []*extension.ExtensionDescription{
		stepDependencyGoBuild("@putnami/go"), stepDependencyTypeScriptBuild(),
		stepDependencyClientgen("build~generate", "build~describe"),
	})
	assertClientgenWaitsFor(t, steps, "/go/app", []string{"/go/app:build~describe", "/go/app:build~generate"})
	assertClientgenWaitsFor(t, steps, "/go/lib", []string{"/go/lib:build~generate"})
	assertClientgenWaitsFor(t, steps, "/ts/app", []string{"/ts/app:build~generate"})
	for _, key := range []string{"/go/app:build~compile", "/go/app:build~infra", "/go/lib:build~compile", "/ts/app:build~infra"} {
		if scheduledJobByKey(steps, key) == nil {
			t.Errorf("%s not planned: a step dependency plans the whole command; jobs=%v", key, jobKeys(steps))
		}
	}

	whole := planClientgen(t, []*extension.ExtensionDescription{
		stepDependencyGoBuild("@putnami/go"), stepDependencyTypeScriptBuild(),
		stepDependencyClientgen("build"),
	})
	if !slices.Equal(jobKeys(steps), jobKeys(whole)) {
		t.Errorf("planned jobs with build~generate and build~describe = %v, want the jobs of a bare build %v",
			jobKeys(steps), jobKeys(whole))
	}
	// A bare command keeps waiting for every leaf step of the command.
	assertClientgenWaitsFor(t, whole, "/go/app", []string{"/go/app:build~compile", "/go/app:build~infra"})
	assertClientgenWaitsFor(t, whole, "/go/lib", []string{"/go/lib:build~compile", "/go/lib:build~infra"})
	assertClientgenWaitsFor(t, whole, "/ts/app", []string{"/ts/app:build~infra"})
}

// TestPlan_StepDependencyOnAStepThePlanDoesNotHoldAddsNoEdge pins that a
// named step the prerequisite command does not run for the project is not
// waited for, while the command is still planned.
func TestPlan_StepDependencyOnAStepThePlanDoesNotHoldAddsNoEdge(t *testing.T) {
	t.Parallel()
	planned := planClientgen(t, []*extension.ExtensionDescription{
		stepDependencyGoBuild("@putnami/go"), stepDependencyTypeScriptBuild(),
		stepDependencyClientgen("build~describe"),
	})
	assertClientgenWaitsFor(t, planned, "/go/app", []string{"/go/app:build~describe"})
	assertClientgenWaitsFor(t, planned, "/go/lib", nil)
	assertClientgenWaitsFor(t, planned, "/ts/app", nil)
	if scheduledJobByKey(planned, "/ts/app:build~generate") == nil {
		t.Errorf("/ts/app:build~generate not planned; jobs=%v", jobKeys(planned))
	}
}

// TestPlan_StepDependencyMatchesEachExtensionThatRunsTheStep pins the
// namespaced case: when two extensions contribute build to one project, each
// runs its steps under its own plan name, and the dependent waits for the
// named step of every extension that runs it.
func TestPlan_StepDependencyMatchesEachExtensionThatRunsTheStep(t *testing.T) {
	t.Parallel()
	planned := planClientgen(t, []*extension.ExtensionDescription{
		stepDependencyGoBuild("@putnami/go"), stepDependencyGoBuild("@acme/go"), stepDependencyTypeScriptBuild(),
		stepDependencyClientgen("build~describe"),
	}, "@putnami/go", "@acme/go")
	assertClientgenWaitsFor(t, planned, "/go/app", []string{
		"/go/app:build~@acme/go~describe",
		"/go/app:build~@putnami/go~describe",
	})
	assertClientgenWaitsFor(t, planned, "/go/lib", nil)
}

// TestResolveCommandDeps_StepDependency pins the edge rule on a hand-built
// plan: a root step waits for the job that is the named step of the named
// command in its own project, and for nothing of another project or another
// command.
func TestResolveCommandDeps_StepDependency(t *testing.T) {
	t.Parallel()
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	other := &workspace.Project{ID: "/other", Name: "other", Path: "other"}
	dependent := &jobmodel.ScheduledJob{Project: app, JobDef: &extension.JobDefinition{
		Name: "clientgen~generate-go", CommandName: "clientgen", StepID: "generate-go",
		CommandDependsOn: []string{"build~describe"},
	}}
	jobs := []*jobmodel.ScheduledJob{
		{Project: app, JobDef: &extension.JobDefinition{Name: "build~describe", CommandName: "build", StepID: "describe"}},
		{Project: app, JobDef: &extension.JobDefinition{Name: "build~compile", CommandName: "build", StepID: "compile", DependsOn: []string{"build~describe"}}},
		{Project: app, JobDef: &extension.JobDefinition{Name: "package~describe", CommandName: "package", StepID: "describe"}},
		{Project: other, JobDef: &extension.JobDefinition{Name: "build~describe", CommandName: "build", StepID: "describe"}},
		dependent,
	}
	resolveCommandDeps(jobs, computeCommandLeaves(jobs))
	if want := []string{"/app:build~describe"}; !slices.Equal(dependent.DependsOn, want) {
		t.Fatalf("deps = %v, want %v", dependent.DependsOn, want)
	}
}
