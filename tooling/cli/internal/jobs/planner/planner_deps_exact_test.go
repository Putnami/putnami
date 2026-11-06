package planner

import (
	"sort"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A dependency reference binds by exact identity only. Each test below drives
// one derived spelling — a display name, a plan name or a synthesized key — and
// proves it binds nothing, paired with the positive control that the exact
// spelling resolves.

// exactRefWorkspace is the two-project fixture every test here shares:
// /app depends on /utils. extensions name the extensions each project opts
// into, so Plan-level tests can give the two projects different providers.
func exactRefWorkspace(t *testing.T, extensions ...string) (*workspace.Workspace, *workspace.Project, *workspace.Project) {
	t.Helper()
	utils := &workspace.Project{ID: "/utils", Name: "utils", Path: "utils", Extensions: extensions}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"utils"}, Extensions: extensions}
	ws := workspace.NewWorkspace("", nil, []*workspace.Project{utils, app})
	ws.Name = "exact-ws"
	return ws, utils, app
}

func jobNamed(proj *workspace.Project, def *extension.JobDefinition) *jobmodel.ScheduledJob {
	return &jobmodel.ScheduledJob{Project: proj, JobDef: def}
}

// requireNoEdges fails when the reference of job bound any job.
func requireNoEdges(t *testing.T, job *jobmodel.ScheduledJob) {
	t.Helper()
	if got := job.DependsOn; len(got) != 0 {
		t.Errorf("%s bound %v through a spelling that is not its exact identity", job.Key(), got)
	}
}

// TestResolveExternalDeps_DisplayNameBindsNothing: a ^ref never binds a job in
// the upstream project whose DISPLAY NAME equals the referenced step id, so
// "^generate" inside the build pipeline never binds a standalone `generate`
// command.
func TestResolveExternalDeps_DisplayNameBindsNothing(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	jobs := []*jobmodel.ScheduledJob{
		// A standalone `generate` command in the upstream project. It is NOT
		// the build pipeline's generate step, and nothing may bind to it as if
		// it were.
		jobNamed(utils, &extension.JobDefinition{Name: "generate"}),
		jobNamed(app, &extension.JobDefinition{Name: "build~generate", DependsOn: []string{"^generate"}}),
	}

	resolveExternalDeps(jobs, ws)
	requireNoEdges(t, jobs[1])
}

// TestResolveExternalDeps_ExactStepStillResolves is the positive control for
// the test above: with the upstream providing the step exactly, the same
// reference resolves — and resolves ONLY to the exact provider, never also to
// the same-named command.
func TestResolveExternalDeps_ExactStepStillResolves(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	jobs := []*jobmodel.ScheduledJob{
		jobNamed(utils, &extension.JobDefinition{Name: "generate"}),
		jobNamed(utils, &extension.JobDefinition{Name: "build~generate"}),
		jobNamed(app, &extension.JobDefinition{Name: "build~generate", DependsOn: []string{"^generate"}}),
	}

	resolveExternalDeps(jobs, ws)
	want := []string{"/utils:build~generate"}
	if got := jobs[2].DependsOn; !equalStrings(got, want) {
		t.Errorf("deps = %v, want %v", got, want)
	}
}

// TestResolveExternalDeps_PlanNameBindsNothing: "^build~generate" written from
// a step of the `test` command names the upstream job's plan name, which is
// not an identity, so it crosses no command boundary.
func TestResolveExternalDeps_PlanNameBindsNothing(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	jobs := []*jobmodel.ScheduledJob{
		jobNamed(utils, &extension.JobDefinition{Name: "build~generate"}),
		jobNamed(app, &extension.JobDefinition{Name: "test~check", DependsOn: []string{"^build~generate"}}),
	}

	resolveExternalDeps(jobs, ws)
	requireNoEdges(t, jobs[1])
}

// TestResolveExternalDeps_SynthesizedStepKeyBindsNothing: the upstream job
// below OWNS the key "<projectID>:<command>~<step>" the reference would
// synthesize while belonging to a different command and step, and the
// reference does not bind it.
func TestResolveExternalDeps_SynthesizedStepKeyBindsNothing(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	upstream := &extension.JobDefinition{
		Name:         "gen",
		InternalName: "build~generate", // → key /utils:build~generate
		CommandName:  "package",
		StepID:       "gen",
	}
	jobs := []*jobmodel.ScheduledJob{
		jobNamed(utils, upstream),
		jobNamed(app, &extension.JobDefinition{Name: "build~generate", DependsOn: []string{"^generate"}}),
	}

	resolveExternalDeps(jobs, ws)
	requireNoEdges(t, jobs[1])
}

// TestResolveExternalDeps_WorkspaceRefBindsByExactIdentity pins that the "/"
// workspace reference resolves by the same exact rule as "^": the workspace
// job that IS the step of the referencing command binds, and a plan-name
// spelling binds nothing.
func TestResolveExternalDeps_WorkspaceRefBindsByExactIdentity(t *testing.T) {
	t.Parallel()
	ws, _, app := exactRefWorkspace(t)
	wsProj := &workspace.Project{ID: ws.Name, Name: ws.Name, Path: "."}

	t.Run("step id resolves", func(t *testing.T) {
		jobs := []*jobmodel.ScheduledJob{
			jobNamed(wsProj, &extension.JobDefinition{Name: "lint~install"}),
			jobNamed(app, &extension.JobDefinition{Name: "lint~format", DependsOn: []string{"/install"}}),
		}
		resolveExternalDeps(jobs, ws)
		if want := []string{ws.Name + ":lint~install"}; !equalStrings(jobs[1].DependsOn, want) {
			t.Errorf("deps = %v, want %v", jobs[1].DependsOn, want)
		}
	})

	t.Run("plan name binds nothing", func(t *testing.T) {
		jobs := []*jobmodel.ScheduledJob{
			jobNamed(wsProj, &extension.JobDefinition{Name: "workspace-install~workspace-install"}),
			jobNamed(app, &extension.JobDefinition{Name: "lint~format", DependsOn: []string{"/workspace-install~workspace-install"}}),
		}
		resolveExternalDeps(jobs, ws)
		requireNoEdges(t, jobs[1])
	})
}

// TestResolveExternalDeps_UnmatchedUpstreamRefBindsNothing: a ^ref that
// matches nothing is ordinary — a Go project depending on a TypeScript one
// simply has no upstream `tidy` step — and contributes no edge.
func TestResolveExternalDeps_UnmatchedUpstreamRefBindsNothing(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	jobs := []*jobmodel.ScheduledJob{
		jobNamed(utils, &extension.JobDefinition{Name: "build~transpile"}),
		jobNamed(app, &extension.JobDefinition{Name: "build~tidy", DependsOn: []string{"^tidy"}}),
	}

	resolveExternalDeps(jobs, ws)
	requireNoEdges(t, jobs[1])
}

// TestResolveExternalDeps_WildcardMatchesIdentityNamesOnly pins the "*" branch
// to the two identity names (the command a job belongs to, the step it is).
// A wildcard naming a derived plan name binds nothing, and so does a bare "*":
// a non-pipeline job has an empty step id, and no reference binds by it.
func TestResolveExternalDeps_WildcardMatchesIdentityNamesOnly(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)

	t.Run("step id resolves", func(t *testing.T) {
		jobs := []*jobmodel.ScheduledJob{
			jobNamed(utils, &extension.JobDefinition{Name: "build~generate"}),
			jobNamed(app, &extension.JobDefinition{Name: "deploy~apply", DependsOn: []string{"*generate"}}),
		}
		resolveExternalDeps(jobs, ws)
		if want := []string{"/utils:build~generate"}; !equalStrings(jobs[1].DependsOn, want) {
			t.Errorf("deps = %v, want %v", jobs[1].DependsOn, want)
		}
	})

	t.Run("plan name binds nothing", func(t *testing.T) {
		jobs := []*jobmodel.ScheduledJob{
			jobNamed(utils, &extension.JobDefinition{Name: "build~generate"}),
			jobNamed(app, &extension.JobDefinition{Name: "deploy~apply", DependsOn: []string{"*build~generate"}}),
		}
		resolveExternalDeps(jobs, ws)
		requireNoEdges(t, jobs[1])
	})

	t.Run("bare wildcard matches nothing", func(t *testing.T) {
		jobs := []*jobmodel.ScheduledJob{
			jobNamed(utils, &extension.JobDefinition{Name: "install"}),
			jobNamed(app, &extension.JobDefinition{Name: "deploy~apply", DependsOn: []string{"*"}}),
		}
		resolveExternalDeps(jobs, ws)
		requireNoEdges(t, jobs[1])
	})
}

// TestPlan_DoesNotEmitUpstreamCommandJobByDisplayName covers the emission half
// of the removal: emitUpstreamJobs used to materialize an upstream NON-PIPELINE
// command job whenever its display name equaled the referenced step id, which
// is the only way a ^ref could ever reach a job that provides no such step.
func TestPlan_DoesNotEmitUpstreamCommandJobByDisplayName(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t, "@test/pipeline")
	// Only /utils provides the flat command; /app carries the pipeline whose
	// step references "^build".
	utils.Extensions = []string{"@test/flat"}

	flat := &extension.ExtensionDescription{
		Name: "@test/flat",
		Jobs: map[string]*extension.JobDefinition{
			"build": {ExtensionName: "@test/flat", Name: "build", Kind: "command", Command: "true"},
		},
	}
	pipeline := &extension.ExtensionDescription{
		Name: "@test/pipeline",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: "@test/pipeline",
				Name:          "build",
				Kind:          "command",
				PipelineSteps: []extension.PipelineStep{
					{ID: "compile", Task: "build-compile", DependsOn: []string{"^build"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{"build-compile": {Kind: "command", Command: "true"}},
	}

	planned, err := Plan(ws, []string{"build"}, []*workspace.Project{app},
		[]*extension.ExtensionDescription{flat, pipeline}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var compile *jobmodel.ScheduledJob
	for _, job := range planned {
		if job.Key() == "/utils:build" {
			t.Errorf("upstream command job %s was emitted by display-name equality", job.Key())
		}
		if job.Key() == "/app:build~compile" {
			compile = job
		}
	}
	if compile == nil {
		t.Fatal("/app:build~compile was not planned; the fixture proves nothing")
	}
	if len(compile.DependsOn) != 0 {
		t.Errorf("deps = %v, want none: no upstream job provides (command %q, step %q)",
			compile.DependsOn, "build", "build")
	}
}

// TestPlan_EmitsUpstreamStepForExactReference is the positive control for the
// test above: the same shape spelled exactly (an upstream pipeline step) is
// still emitted for a project outside the selection, and still bound.
func TestPlan_EmitsUpstreamStepForExactReference(t *testing.T) {
	t.Parallel()
	ws, _, app := exactRefWorkspace(t, "@test/pipeline")

	ext := &extension.ExtensionDescription{
		Name: "@test/pipeline",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: "@test/pipeline",
				Name:          "build",
				Kind:          "command",
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-generate": {Kind: "command", Command: "true"},
		},
	}
	planned, err := Plan(ws, []string{"build"}, []*workspace.Project{app},
		[]*extension.ExtensionDescription{ext}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	found := false
	for _, job := range planned {
		if job.Key() == "/app:build~generate" {
			found = true
			if want := []string{"/utils:build~generate"}; !equalStrings(job.DependsOn, want) {
				t.Errorf("deps = %v, want %v", job.DependsOn, want)
			}
		}
	}
	if !found {
		t.Fatal("/app:build~generate was not planned")
	}
}

// TestPlan_CommandDependencyEmissionIsDeterministic pins the plan node ORDER
// produced by command-level dependencies. Two prerequisite commands used to be
// emitted in Go map order, so `publish` (dependsOn build + package) planned its
// nodes in a different order run to run.
func TestPlan_CommandDependencyEmissionIsDeterministic(t *testing.T) {
	t.Parallel()
	ws, _, _ := exactRefWorkspace(t, "@test/multi")
	step := func(id string) extension.PipelineStep {
		return extension.PipelineStep{ID: id, Task: "task-" + id}
	}
	ext := &extension.ExtensionDescription{
		Name: "@test/multi",
		Jobs: map[string]*extension.JobDefinition{
			"build":   {ExtensionName: "@test/multi", Name: "build", Kind: "command", PipelineSteps: []extension.PipelineStep{step("compile")}},
			"package": {ExtensionName: "@test/multi", Name: "package", Kind: "command", PipelineSteps: []extension.PipelineStep{step("archive")}},
			"publish": {
				ExtensionName:    "@test/multi",
				Name:             "publish",
				Kind:             "command",
				CommandDependsOn: []string{"build", "package"},
				PipelineSteps:    []extension.PipelineStep{step("upload")},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"task-compile": {Kind: "command", Command: "true"},
			"task-archive": {Kind: "command", Command: "true"},
			"task-upload":  {Kind: "command", Command: "true"},
		},
	}

	snapshot := func() string {
		planned, err := Plan(ws, []string{"publish"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		keys := make([]string, 0, len(planned))
		for _, job := range planned {
			keys = append(keys, job.Key())
		}
		return strings.Join(keys, "\n")
	}
	first := snapshot()
	if !strings.Contains(first, "build~compile") || !strings.Contains(first, "package~archive") {
		t.Fatalf("fixture planned no prerequisite commands:\n%s", first)
	}
	for i := 0; i < 20; i++ {
		if next := snapshot(); next != first {
			t.Fatalf("plan node order is nondeterministic:\n--- first ---\n%s\n--- run %d ---\n%s", first, i+2, next)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
