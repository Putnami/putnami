package jobs

import (
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A cleanup deleted every stringly dependency-resolution fallback. Each test
// below drives one deleted spelling and proves the
// reference now FAILS LOUDLY — it neither rebinds to the job the fallback used
// to pick nor quietly loses its edge — and pairs it with the positive control
// that the exact spelling still resolves.

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

func jobNamed(proj *workspace.Project, def *extension.JobDefinition) *ScheduledJob {
	return &ScheduledJob{Project: proj, JobDef: def}
}

func requireLegacyRejection(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("reference resolved silently; want a plan-time rejection")
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestResolveExternalDeps_RejectsDisplayNameFallback covers the fallback that
// made a ^ref bind to any job in the upstream project whose DISPLAY NAME
// happened to equal the referenced step id — the exact way "^generate" inside
// the build pipeline could silently bind to a standalone `generate` command.
func TestResolveExternalDeps_RejectsDisplayNameFallback(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	jobs := []*ScheduledJob{
		// A standalone `generate` command in the upstream project. It is NOT
		// the build pipeline's generate step, and nothing may bind to it as if
		// it were.
		jobNamed(utils, &extension.JobDefinition{Name: "generate"}),
		jobNamed(app, &extension.JobDefinition{Name: "build~generate", DependsOn: []string{"^generate"}}),
	}

	err := resolveExternalDeps(jobs, ws)
	requireLegacyRejection(t, err, "/app:build~generate", `"^generate"`, "/utils:generate")

	if got := jobs[1].DependsOn; len(got) != 0 {
		t.Errorf("rejected reference still produced edges %v", got)
	}
}

// TestResolveExternalDeps_ExactStepStillResolves is the positive control for
// the test above: with the upstream providing the step exactly, the same
// reference resolves — and resolves ONLY to the exact provider, never also to
// the same-named command.
func TestResolveExternalDeps_ExactStepStillResolves(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	jobs := []*ScheduledJob{
		jobNamed(utils, &extension.JobDefinition{Name: "generate"}),
		jobNamed(utils, &extension.JobDefinition{Name: "build~generate"}),
		jobNamed(app, &extension.JobDefinition{Name: "build~generate", DependsOn: []string{"^generate"}}),
	}

	if err := resolveExternalDeps(jobs, ws); err != nil {
		t.Fatalf("exact reference rejected: %v", err)
	}
	want := []string{"/utils:build~generate"}
	if got := jobs[2].DependsOn; !equalStrings(got, want) {
		t.Errorf("deps = %v, want %v", got, want)
	}
}

// TestResolveExternalDeps_RejectsPlanNameFallback covers the raw plan-name
// fallback: "^build~generate" written from a step of the `test` command used to
// bind through the upstream job's plan name, crossing command boundaries
// without saying so.
func TestResolveExternalDeps_RejectsPlanNameFallback(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	jobs := []*ScheduledJob{
		jobNamed(utils, &extension.JobDefinition{Name: "build~generate"}),
		jobNamed(app, &extension.JobDefinition{Name: "test~check", DependsOn: []string{"^build~generate"}}),
	}

	err := resolveExternalDeps(jobs, ws)
	requireLegacyRejection(t, err, "/app:test~check", `"^build~generate"`, "/utils:build~generate")
}

// TestResolveExternalDeps_RejectsSynthesizedStepKeyFallback covers the
// synthesized "<projectID>:<command>~<step>" key probe. The upstream job below
// OWNS that key while belonging to a different command and step, which is
// precisely the mismatch the synthesized form could not see.
func TestResolveExternalDeps_RejectsSynthesizedStepKeyFallback(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	upstream := &extension.JobDefinition{
		Name:         "gen",
		InternalName: "build~generate", // → key /utils:build~generate
		CommandName:  "package",
		StepID:       "gen",
	}
	jobs := []*ScheduledJob{
		jobNamed(utils, upstream),
		jobNamed(app, &extension.JobDefinition{Name: "build~generate", DependsOn: []string{"^generate"}}),
	}

	err := resolveExternalDeps(jobs, ws)
	requireLegacyRejection(t, err, "/app:build~generate", "/utils:build~generate")
}

// TestResolveExternalDeps_WorkspaceRefRejectsLegacyName pins that the "/"
// workspace reference resolves by the same exact rule as "^".
func TestResolveExternalDeps_WorkspaceRefRejectsLegacyName(t *testing.T) {
	t.Parallel()
	ws, _, app := exactRefWorkspace(t)
	wsProj := &workspace.Project{ID: ws.Name, Name: ws.Name, Path: "."}
	jobs := []*ScheduledJob{
		jobNamed(wsProj, &extension.JobDefinition{Name: "workspace-install~workspace-install"}),
		jobNamed(app, &extension.JobDefinition{Name: "lint~format", DependsOn: []string{"/workspace-install~workspace-install"}}),
	}

	err := resolveExternalDeps(jobs, ws)
	requireLegacyRejection(t, err, "/app:lint~format", ws.Name+":workspace-install~workspace-install")
}

// TestResolveExternalDeps_UnmatchedUpstreamRefIsNotAnError guards the
// rejection's scope: a ^ref that matches nothing at all is ordinary — a Go
// project depending on a TypeScript one simply has no upstream `tidy` step —
// and must stay a no-op rather than becoming an error.
func TestResolveExternalDeps_UnmatchedUpstreamRefIsNotAnError(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)
	jobs := []*ScheduledJob{
		jobNamed(utils, &extension.JobDefinition{Name: "build~transpile"}),
		jobNamed(app, &extension.JobDefinition{Name: "build~tidy", DependsOn: []string{"^tidy"}}),
	}

	if err := resolveExternalDeps(jobs, ws); err != nil {
		t.Fatalf("unmatched upstream reference rejected: %v", err)
	}
	if got := jobs[1].DependsOn; len(got) != 0 {
		t.Errorf("unmatched reference produced edges %v", got)
	}
}

// TestResolveExternalDeps_WildcardMatchesIdentityNamesOnly pins the "*" branch
// to the two identity names (the command a job belongs to, the step it is).
// A wildcard naming a derived plan name is rejected, and a bare "*" — which
// used to match every non-pipeline job through its empty step id — matches
// nothing.
func TestResolveExternalDeps_WildcardMatchesIdentityNamesOnly(t *testing.T) {
	t.Parallel()
	ws, utils, app := exactRefWorkspace(t)

	t.Run("step id resolves", func(t *testing.T) {
		jobs := []*ScheduledJob{
			jobNamed(utils, &extension.JobDefinition{Name: "build~generate"}),
			jobNamed(app, &extension.JobDefinition{Name: "deploy~apply", DependsOn: []string{"*generate"}}),
		}
		if err := resolveExternalDeps(jobs, ws); err != nil {
			t.Fatalf("wildcard on a step id rejected: %v", err)
		}
		if want := []string{"/utils:build~generate"}; !equalStrings(jobs[1].DependsOn, want) {
			t.Errorf("deps = %v, want %v", jobs[1].DependsOn, want)
		}
	})

	t.Run("plan name is rejected", func(t *testing.T) {
		jobs := []*ScheduledJob{
			jobNamed(utils, &extension.JobDefinition{Name: "build~generate"}),
			jobNamed(app, &extension.JobDefinition{Name: "deploy~apply", DependsOn: []string{"*build~generate"}}),
		}
		err := resolveExternalDeps(jobs, ws)
		requireLegacyRejection(t, err, "/app:deploy~apply", "/utils:build~generate")
	})

	t.Run("bare wildcard matches nothing", func(t *testing.T) {
		jobs := []*ScheduledJob{
			jobNamed(utils, &extension.JobDefinition{Name: "install"}),
			jobNamed(app, &extension.JobDefinition{Name: "deploy~apply", DependsOn: []string{"*"}}),
		}
		if err := resolveExternalDeps(jobs, ws); err != nil {
			t.Fatalf("bare wildcard rejected: %v", err)
		}
		if got := jobs[1].DependsOn; len(got) != 0 {
			t.Errorf("bare wildcard matched %v, want nothing", got)
		}
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
		[]*extension.ExtensionDescription{flat, pipeline}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var compile *ScheduledJob
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
		[]*extension.ExtensionDescription{ext}, nil, nil, nil)
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

// legacyMatchingKeys is the PRE-B2b resolution, kept here as the before-side of
// the comparison below: exact (command, step) first, then the raw display and
// plan names, then the two synthesized "projectID:..." key forms.
func legacyMatchingKeys(idx *plannedIndex, projectID, cmdName, stepID string) []string {
	candidates := idx.byProject[projectID]
	var keys []string
	for _, job := range candidates {
		if job.CommandName() == cmdName && job.StepID() == stepID {
			keys = append(keys, job.Key())
		}
	}
	if len(keys) > 0 {
		return dedupeSorted(keys)
	}
	for _, job := range candidates {
		if job.DisplayName() == stepID || job.PlanName() == stepID {
			keys = append(keys, job.Key())
		}
	}
	for _, key := range []string{
		projectID + ":" + stepID,
		projectID + ":" + extension.StepJobName(cmdName, stepID),
	} {
		if _, ok := idx.byKey[key]; ok {
			keys = append(keys, key)
		}
	}
	return dedupeSorted(keys)
}

// TestResolveExternalDeps_ExactManifestsResolveIdenticallyToLegacy is the
// before/after snapshot the slice owes: for a manifest whose references are all
// spelled exactly, the removed fallbacks contributed NOTHING, so the exact
// resolver and the legacy one agree on every query a real plan makes. This is
// the property that let the in-repo manifests stay untouched.
func TestResolveExternalDeps_ExactManifestsResolveIdenticallyToLegacy(t *testing.T) {
	t.Parallel()
	ws, ext := makeIntegrationWorkspace(t)
	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	idx := buildPlannedIndex(planned)
	queries := 0
	for _, job := range planned {
		cmdName := job.CommandName()
		for _, dep := range job.JobDef.DependsOn {
			if !strings.HasPrefix(dep, "^") {
				continue
			}
			stepID := dep[1:]
			for _, depName := range ws.Graph.DependenciesOf(job.Project.ID) {
				queries++
				exact := idx.matchingKeys(depName, cmdName, stepID)
				legacy := legacyMatchingKeys(idx, depName, cmdName, stepID)
				if !equalStrings(exact, legacy) {
					t.Errorf("%s ref %q against %s: exact=%v legacy=%v", job.Key(), dep, depName, exact, legacy)
				}
			}
		}
	}
	if queries == 0 {
		t.Fatal("fixture exercised no external references; the comparison is vacuous")
	}

	// Mutation control: the comparison above is only meaningful because the
	// oracle can disagree. Point one query at a legacy-only spelling and it
	// must.
	if exact, legacy := idx.matchingKeys("/packages/utils", "test", "build~generate"),
		legacyMatchingKeys(idx, "/packages/utils", "test", "build~generate"); equalStrings(exact, legacy) {
		t.Fatalf("oracle cannot disagree with the exact resolver (both %v); the comparison is vacuous", exact)
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
		planned, err := Plan(ws, []string{"publish"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
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
