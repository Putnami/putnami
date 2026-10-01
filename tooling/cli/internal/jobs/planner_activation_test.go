package jobs

import (
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func planKeySet(planned []*ScheduledJob) map[string]bool {
	keys := make(map[string]bool, len(planned))
	for _, j := range planned {
		keys[j.Key()] = true
	}
	return keys
}

func findScheduled(planned []*ScheduledJob, key string) *ScheduledJob {
	for _, j := range planned {
		if j.Key() == key {
			return j
		}
	}
	return nil
}

// goBuildExtension mirrors the Go extension's build pipeline shape: the
// describe step carries the same plan-time activation gate as the real
// manifest (drop the node when the project doesn't import the app framework).
func goBuildExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@putnami/go",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName:   "@putnami/go",
				Name:            "build",
				Kind:            "command",
				Command:         "go",
				Cache:           true,
				ActivationFiles: []string{"**/*.go", "go.mod"},
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
					{ID: "describe", Task: "build-describe", DependsOn: []string{"^describe", "generate"},
						Activation: &extension.StepActivation{Contains: map[string]string{"go.mod": "go.putnami.dev/app"}}},
					{ID: "cross-compile", Task: "build-cross-compile", DependsOn: []string{"describe"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-generate":      {Kind: "command", Command: "go"},
			"build-describe":      {Kind: "command", Command: "go"},
			"build-cross-compile": {Kind: "command", Command: "go"},
		},
	}
}

// TestPlan_DescribePrunedForNonAppProject locks the headline behavior: the
// deterministically-skipped Go describe node is removed from the plan for
// projects that don't import go.putnami.dev/app, while remaining for app
// projects. Splicing it out must keep the rest of the DAG intact.
func TestPlan_DescribePrunedForNonAppProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// app imports the framework → describe is real work, keep it.
	writeProjectFile(t, root, "app", "go.mod", "module example.com/app\n\ngo 1.25\n\nrequire go.putnami.dev/app v0.0.1\n")
	writeProjectFile(t, root, "app", "main.go", "package main\n\nfunc main() {}\n")
	// lib does not → describe would always skip, prune it at plan time.
	writeProjectFile(t, root, "lib", "go.mod", "module example.com/lib\n\ngo 1.25\n")
	writeProjectFile(t, root, "lib", "lib.go", "package lib\n")

	projects := []*workspace.Project{
		// app depends on lib so we also exercise cross-project ^describe
		// resolution when the upstream describe node is pruned.
		{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}, Extensions: []string{"@putnami/go"}},
		{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@putnami/go"}},
	}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "ws"
	ext := goBuildExtension()

	planned, err := Plan(ws, []string{"build"}, projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	keys := planKeySet(planned)

	if !keys["/app:build~describe"] {
		t.Errorf("app describe should be planned (imports framework); keys=%v", keys)
	}
	if keys["/lib:build~describe"] {
		t.Errorf("lib describe should be pruned (no framework import); keys=%v", keys)
	}

	// lib's cross-compile must reconnect to generate now that describe is gone.
	cc := findScheduled(planned, "/lib:build~cross-compile")
	if cc == nil {
		t.Fatal("lib cross-compile missing from plan")
	}
	if !slices.Contains(cc.DependsOn, "/lib:build~generate") {
		t.Errorf("lib cross-compile should depend on generate after splice, got %v", cc.DependsOn)
	}
	if !keys["/lib:build~generate"] {
		t.Error("lib generate should survive the splice (bridged dependency)")
	}

	// app still keeps describe → its compile depends on describe, not generate.
	appCC := findScheduled(planned, "/app:build~cross-compile")
	if appCC == nil || !slices.Contains(appCC.DependsOn, "/app:build~describe") {
		t.Errorf("app cross-compile should still depend on describe, got %+v", appCC)
	}

	// app describe's ^describe pointed at lib; lib describe is pruned, so the
	// edge must resolve away cleanly with no dangling dependency. (Plan already
	// runs validatePlanDAG, which would have failed on an unresolved ref.)
	appDescribe := findScheduled(planned, "/app:build~describe")
	if appDescribe == nil {
		t.Fatal("app describe missing")
	}
	if slices.Contains(appDescribe.DependsOn, "/lib:build~describe") {
		t.Errorf("app describe should not depend on the pruned lib describe, got %v", appDescribe.DependsOn)
	}
	// app still waits for lib's generate transitively via ^generate.
	appGen := findScheduled(planned, "/app:build~generate")
	if appGen == nil || !slices.Contains(appGen.DependsOn, "/lib:build~generate") {
		t.Errorf("app generate should still depend on lib generate, got %+v", appGen)
	}
}

func TestComputePlanMetrics(t *testing.T) {
	t.Parallel()
	planned := []*ScheduledJob{
		makeJob("app", "build~generate", nil),
		makeJob("app", "build~compile", []string{"/app:build~generate"}),
		makeJob("app", "test~test", []string{"/app:build~generate"}),
		makeJob("lib", "build~generate", nil),
	}
	m := ComputePlanMetrics(planned)
	if m.Jobs != 4 {
		t.Errorf("Jobs = %d, want 4", m.Jobs)
	}
	if m.Edges != 2 {
		t.Errorf("Edges = %d, want 2", m.Edges)
	}
	if m.Projects != 2 {
		t.Errorf("Projects = %d, want 2", m.Projects)
	}
	if m.ByCommand["build"] != 3 {
		t.Errorf("ByCommand[build] = %d, want 3", m.ByCommand["build"])
	}
	if m.ByCommand["test"] != 1 {
		t.Errorf("ByCommand[test] = %d, want 1", m.ByCommand["test"])
	}
	if got := m.CommandsSorted(); len(got) != 2 || got[0] != "build" || got[1] != "test" {
		t.Errorf("CommandsSorted = %v, want [build test]", got)
	}
}

func TestComputePlanMetricsCountsSerializeAfterEdges(t *testing.T) {
	t.Parallel()
	writer := makeJob("app", "lint~format", nil)
	reader := makeJob("app", "build~compile", nil)
	reader.SerializeAfter = []string{writer.Key()}

	m := ComputePlanMetrics([]*ScheduledJob{writer, reader})
	if m.Edges != 1 {
		t.Errorf("Edges = %d, want 1 serialize edge", m.Edges)
	}
}

func TestStepIsActive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProjectFile(t, root, "app", "go.mod", "require go.putnami.dev/app v0.0.1\n")
	writeProjectFile(t, root, "lib", "go.mod", "module example.com/lib\n")
	writeProjectFile(t, root, "withgo", "main.go", "package main\n")

	cache := newPlanCache()
	scope := func(project string) activationScope {
		return activationScope{projRoot: filepath.Join(root, project), cache: cache}
	}
	gate := &extension.StepActivation{Contains: map[string]string{"go.mod": "go.putnami.dev/app"}}

	if !stepIsActive(gate, scope("app")) {
		t.Error("app should activate: go.mod requires the framework")
	}
	if stepIsActive(gate, scope("lib")) {
		t.Error("lib should not activate: go.mod lacks the framework import")
	}
	// nil gate is always active.
	if !stepIsActive(nil, scope("lib")) {
		t.Error("nil activation must always be active")
	}
	// Missing file fails closed (mirrors the runtime skip).
	if stepIsActive(gate, scope("missing")) {
		t.Error("missing go.mod should make the gate inactive")
	}

	// Files gate: active only when a matching file exists.
	filesGate := &extension.StepActivation{Files: []string{"*.go"}}
	if !stepIsActive(filesGate, scope("withgo")) {
		t.Error("withgo should activate: has a .go file")
	}
	if stepIsActive(filesGate, scope("lib")) {
		t.Error("lib should not activate via files gate: no .go file")
	}
}

// TestStepIsActive_ClosureGateFollowsTheDependencyClosure pins a repair: a
// closureFiles gate answers the SAME question the step's task answers
// at runtime (dbtestenv.ClosureDatabases walks project.dependencyClosure), so a
// project whose only database requirement is transitive keeps its provisioning
// step — while a project whose closure declares none still gets no plan node.
func TestStepIsActive_ClosureGateFollowsTheDependencyClosure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProjectFile(t, root, "lib", "infra/requirements.json", `{"protocolVersion":2}`)
	writeProjectFile(t, root, "app", "go.mod", "module example.com/app\n")
	writeProjectFile(t, root, "solo", "go.mod", "module example.com/solo\n")

	cache := newPlanCache()
	gate := &extension.StepActivation{ClosureFiles: []string{"infra/requirements.json"}}
	ownGate := &extension.StepActivation{Files: []string{"infra/requirements.json"}}

	// app declares nothing itself; its dependency lib declares the manifest.
	transitive := activationScope{
		projRoot:     filepath.Join(root, "app"),
		closureRoots: func() []string { return []string{filepath.Join(root, "app"), filepath.Join(root, "lib")} },
		cache:        cache,
	}
	if !stepIsActive(gate, transitive) {
		t.Error("a transitive-only requirement must keep the step: the runtime reads the closure")
	}
	if stepIsActive(ownGate, transitive) {
		t.Error("the project-local files gate is what this repair replaces; it must still answer no")
	}

	// A project whose whole closure declares nothing keeps its zero-cost path.
	free := activationScope{
		projRoot:     filepath.Join(root, "solo"),
		closureRoots: func() []string { return []string{filepath.Join(root, "solo")} },
		cache:        cache,
	}
	if stepIsActive(gate, free) {
		t.Error("a requirement-free closure must add no plan node")
	}

	// The seed project's own manifest still satisfies the gate: the closure
	// includes the project itself.
	seed := activationScope{
		projRoot:     filepath.Join(root, "lib"),
		closureRoots: func() []string { return []string{filepath.Join(root, "lib")} },
		cache:        cache,
	}
	if !stepIsActive(gate, seed) {
		t.Error("the closure includes the seed project, so its own manifest must activate the step")
	}
}

// TestStepActivationPredicate_ResolvesTheClosureFromTheGraphAndMemoizesIt pins
// the two properties the closure gate needs to be affordable and correct: it
// reads the workspace GRAPH (not just the project's own root), and it walks that
// graph once per project rather than once per (project, command, step).
func TestStepActivationPredicate_ResolvesTheClosureFromTheGraphAndMemoizesIt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProjectFile(t, root, "lib", "infra/requirements.json", `{"protocolVersion":2}`)
	writeProjectFile(t, root, "app", "go.mod", "module example.com/app\n")

	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app})
	ws.Graph = workspace.BuildGraph(ws.Projects)

	cache := newPlanCache()
	active := pipelineExpansionOptions(ws, app, nil, false, cache).StepActive
	gated := extension.PipelineStep{
		ID:         "test-env",
		Activation: &extension.StepActivation{ClosureFiles: []string{"infra/requirements.json"}},
	}
	if !active(gated) {
		t.Fatal("the gate did not follow the dependency graph to lib's manifest")
	}
	roots, memoized := cache.closureRoots[app.ID]
	if !memoized || len(roots) != 2 {
		t.Fatalf("closure roots memo = %v (present=%v), want the app and its dependency", roots, memoized)
	}

	// Poison the memo: a second probe must read it rather than re-walk.
	cache.closureRoots[app.ID] = []string{filepath.Join(root, "app")}
	if active(gated) {
		t.Error("the closure was recomputed instead of read from the plan cache")
	}
}
