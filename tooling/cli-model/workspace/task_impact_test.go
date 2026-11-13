package workspace

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// fakeTaskIndex is a TaskImpactIndex written by hand, so the model's rules are
// tested against declarations the test states rather than against a manifest
// loader the model is not allowed to have.
type fakeTaskIndex struct {
	// reading maps "<projectID>|<path>" to the tasks of that project which
	// read the path. A pair absent from the map reads nothing.
	reading map[string][]string
	// reached maps one task to the tasks a DEPENDENT runs because it moved.
	reached map[string][]string
	// everyReached is the answer for a project that runs every task.
	everyReached []string
	// reachedIn maps a dependent project id to the tasks it may run of what
	// reached answers. A project absent from the map may run every task.
	reachedIn map[string][]string
	// extensionTasks maps an extension project id to the tasks it declares.
	extensionTasks map[string][]string
	// plannedIn maps a project id to the extension tasks the planner may run
	// in it. A project absent from the map may run every task.
	plannedIn map[string][]string
	// runtime holds the "<projectID>|<path>" pairs an extension's runtime reads.
	runtime map[string]bool
	// manifest maps "<projectID>|<path>" to the tasks a change of that
	// extension manifest moved, when the change stays inside task and step
	// declarations. A pair absent from the map is a change that does not.
	manifest map[string][]string
	// checking holds the tasks only a command of a checking kind declares.
	checking map[string]bool
}

func (f fakeTaskIndex) TasksBuildingRuntime(tasks []string) []string {
	building := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if !f.checking[task] {
			building = append(building, task)
		}
	}
	return building
}

func (f fakeTaskIndex) ReadsAsExtensionRuntime(projectID, path string) bool {
	return f.runtime[projectID+"|"+path]
}

func (f fakeTaskIndex) TasksReadingPath(projectID, path string) []string {
	return f.reading[projectID+"|"+path]
}

func (f fakeTaskIndex) TasksReachedFrom(tasks []string) []string {
	if tasks == nil {
		return f.everyReached
	}
	var out []string
	for _, task := range tasks {
		for _, next := range f.reached[task] {
			if !slices.Contains(out, next) {
				out = append(out, next)
			}
		}
	}
	slices.Sort(out)
	return out
}

// TasksReachedIn keeps what TasksReachedFrom answers, unless the fake names
// the tasks a dependent may run.
func (f fakeTaskIndex) TasksReachedIn(projectID string, tasks []string) []string {
	reached := f.TasksReachedFrom(tasks)
	may, restricted := f.reachedIn[projectID]
	if !restricted {
		return reached
	}
	kept := []string{}
	for _, task := range reached {
		if slices.Contains(may, task) {
			kept = append(kept, task)
		}
	}
	return kept
}

func (f fakeTaskIndex) TasksOfExtension(extensionProjectID string) []string {
	return f.extensionTasks[extensionProjectID]
}

// TasksPlannedIn keeps every task, unless the fake names the tasks a project
// may plan.
func (f fakeTaskIndex) TasksPlannedIn(projectID string, tasks []string) []string {
	may, restricted := f.plannedIn[projectID]
	if !restricted {
		return tasks
	}
	var out []string
	for _, task := range tasks {
		if slices.Contains(may, task) {
			out = append(out, task)
		}
	}
	return out
}

func (f fakeTaskIndex) TasksOfManifestChange(extensionProjectID, path string) ([]string, bool) {
	tasks, confined := f.manifest[extensionProjectID+"|"+path]
	return tasks, confined
}

// taskImpactWorkspace is a library, two importers and one importer of an
// importer — the shape every "does a test file reach my dependents?" question
// is asked about.
func taskImpactWorkspace() *Workspace {
	return NewWorkspace("/workspace", nil, []*Project{
		{ID: "/libs/http", Name: "http", Path: "libs/http"},
		{ID: "/libs/api", Name: "api", Path: "libs/api", Dependencies: []string{"/libs/http"}},
		{ID: "/apps/service", Name: "service", Path: "apps/service", Dependencies: []string{"/libs/api"}},
	})
}

// The standard index for that workspace: `test~test` reads test files and
// reaches nobody, `build~describe` reads sources and reaches a dependent's
// `build~describe` and what sits behind it, and a `.md` is read by no task.
func taskImpactIndex() fakeTaskIndex {
	reached := map[string][]string{
		"build~describe": {"build~compile", "build~describe", "test~test"},
	}
	return fakeTaskIndex{
		reading: map[string][]string{
			"/libs/http|libs/http/http_test.go": {"lint~lint", "test~test"},
			"/libs/http|libs/http/http.go":      {"build~compile", "build~describe", "lint~lint", "test~test"},
		},
		reached:      reached,
		everyReached: reached["build~describe"],
	}
}

// A file no task declares as an input selects nothing at all — not its own
// project, and therefore not its dependents. This is the whole point of ADR
// 0044: before it, a README selected its project and every project that
// imports it.
func TestTraceChangeImpact_AFileNoTaskReadsSelectsNothing(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	r := TraceChangeImpact(taskImpactWorkspace(), []string{"libs/http/README.md"},
		ChangeImpactOptions{Tasks: taskImpactIndex()})

	if len(r.Projects) != 0 {
		t.Fatalf("a README selected %v, want nothing", ownerProjectIDs(r.Projects))
	}
	if len(r.Trace.Seeds) != 0 {
		t.Errorf("a README seeded %v, want no seed", r.Trace.Seeds)
	}
}

// A test file seeds the tasks that read it — `test` and, honestly, `lint`,
// because the Go lint tasks declare `**/*.go` without excluding test files —
// and stops there: no manifest declares `^test`, so no dependent's task names
// one of them.
func TestTraceChangeImpact_ATestFileSeedsItsTestsAndReachesNoDependent(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	r := TraceChangeImpact(taskImpactWorkspace(), []string{"libs/http/http_test.go"},
		ChangeImpactOptions{Tasks: taskImpactIndex()})

	if got, want := ownerProjectIDs(r.Projects), []string{"/libs/http"}; !slices.Equal(got, want) {
		t.Fatalf("a test file selected %v, want %v", got, want)
	}
	if got, want := r.Trace.ScopeOf("/libs/http"), []string{"lint~lint", "test~test"}; !slices.Equal(got, want) {
		t.Errorf("scoped /libs/http to %v, want %v", got, want)
	}
}

// A non-test source seeds every task that reads it and reaches each importer
// through the `^` reference its own task declares, transitively — the closure
// the dependency edge used to carry whole.
func TestTraceChangeImpact_ASourceFileReachesEveryImporterThroughTheTaskEdge(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	r := TraceChangeImpact(taskImpactWorkspace(), []string{"libs/http/http.go"},
		ChangeImpactOptions{Tasks: taskImpactIndex()})

	want := map[string][]string{
		"/libs/http":    {"build~compile", "build~describe", "lint~lint", "test~test"},
		"/libs/api":     {"build~compile", "build~describe", "test~test"},
		"/apps/service": {"build~compile", "build~describe", "test~test"},
	}
	for id, wantScope := range want {
		if got := r.Trace.ScopeOf(id); !slices.Equal(got, wantScope) {
			t.Errorf("scoped %s to %v, want %v", id, got, wantScope)
		}
	}
	if got, want := len(r.Projects), 3; got != want {
		t.Errorf("selected %v, want %d projects", ownerProjectIDs(r.Projects), want)
	}
	if edge, ok := r.Trace.Edges["/apps/service"]; !ok || edge.From != "/libs/api" || edge.Kind != ImpactEdgeDependency {
		t.Errorf("/apps/service was reached by %+v, want a dependency edge from /libs/api", edge)
	}
}

// The walk asks the index what each dependent may run of what moved: a
// dependent keeps only those tasks, and one that may run none of them is not
// selected, nor is anything behind it through that edge.
func TestTraceChangeImpact_AnEdgeCarriesIntoEachDependentWhatItMayRun(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "a-dependent-takes-only-the-step-references-it-may-run",
		"an-edge-carries-into-each-dependent-what-it-may-run")
	spectest.Proves(t, "cli/workspace-model", "a-dependent-takes-only-the-step-references-it-may-run",
		"a-dependent-that-may-run-nothing-reached-is-not-selected")
	index := taskImpactIndex()
	index.reachedIn = map[string][]string{"/libs/api": {"build~compile", "build~describe"}}
	r := TraceChangeImpact(taskImpactWorkspace(), []string{"libs/http/http.go"},
		ChangeImpactOptions{Tasks: index})
	if got, want := r.Trace.ScopeOf("/libs/api"), []string{"build~compile", "build~describe"}; !slices.Equal(got, want) {
		t.Errorf("scoped /libs/api to %v, want %v", got, want)
	}

	index.reachedIn = map[string][]string{"/libs/api": {}}
	r = TraceChangeImpact(taskImpactWorkspace(), []string{"libs/http/http.go"},
		ChangeImpactOptions{Tasks: index})
	if got, want := ownerProjectIDs(r.Projects), []string{"/libs/http"}; !slices.Equal(got, want) {
		t.Errorf("a dependent that may run nothing reached selected %v, want %v", got, want)
	}
}

// The contract edge applies the same task rule to a provider and the generated
// client of its contract once it fires: a change to the committed contract
// reaches the client's `generate`, and what sits behind it. A provider source
// change with the contract unchanged crosses no contract edge, whatever tasks
// it carries.
func TestTraceChangeImpact_ContractEdgeCarriesTheSameTaskRule(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/apps/provider", Name: "provider", Path: "apps/provider", ContractServiceID: "catalog.items",
			ContractPath: "apps/provider/schema/openapi.json", ContractSHA256: "abc"},
		{ID: "/apps/provider/clients/ts", Name: "items-client", Path: "apps/provider/clients/ts",
			GeneratedClient: &GeneratedClientBinding{ServiceID: "catalog.items", ContractSHA256: "abc"}},
	})
	index := fakeTaskIndex{
		reading: map[string][]string{
			"/apps/provider|apps/provider/src/store.ts":        {"build~describe"},
			"/apps/provider|apps/provider/schema/openapi.json": {"build~describe"},
		},
		reached: map[string][]string{"build~describe": {"build~generate"}},
	}

	r := TraceChangeImpact(ws, []string{"apps/provider/schema/openapi.json"}, ChangeImpactOptions{Tasks: index})

	if got, want := r.Trace.ScopeOf("/apps/provider/clients/ts"), []string{"build~generate"}; !slices.Equal(got, want) {
		t.Fatalf("scoped the client to %v, want %v", got, want)
	}
	if edge := r.Trace.Edges["/apps/provider/clients/ts"]; edge.Kind != ImpactEdgeContract ||
		edge.Via != "apps/provider/schema/openapi.json" || edge.ContractSHA256 != "abc" {
		t.Errorf("the client was reached by %+v, want the contract edge naming the contract and its digest", edge)
	}

	r = TraceChangeImpact(ws, []string{"apps/provider/src/store.ts"}, ChangeImpactOptions{Tasks: index})

	if got, want := ownerProjectIDs(r.Projects), []string{"/apps/provider"}; !slices.Equal(got, want) {
		t.Errorf("a source change with the contract unchanged selected %v, want %v", got, want)
	}
}

// A project reached twice runs the UNION of what reached it, and a full reach
// dominates every task-scoped one however late it arrives.
func TestTraceChangeImpact_ScopesTakeTheUnionAndFullDominates(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/libs/a", Name: "a", Path: "libs/a"},
		{ID: "/libs/b", Name: "b", Path: "libs/b"},
		{ID: "/apps/both", Name: "both", Path: "apps/both", Dependencies: []string{"/libs/a", "/libs/b"}},
	})
	index := fakeTaskIndex{
		reading: map[string][]string{
			"/libs/a|libs/a/a.go": {"build~describe"},
			"/libs/b|libs/b/b.go": {"build~bundle"},
		},
		reached: map[string][]string{
			"build~describe": {"build~describe"},
			"build~bundle":   {"build~bundle"},
		},
		everyReached: []string{"build~bundle", "build~describe"},
	}

	r := TraceChangeImpact(ws, []string{"libs/a/a.go", "libs/b/b.go"}, ChangeImpactOptions{Tasks: index})
	if got, want := r.Trace.ScopeOf("/apps/both"), []string{"build~bundle", "build~describe"}; !slices.Equal(got, want) {
		t.Fatalf("scoped /apps/both to %v, want the union %v", got, want)
	}

	// A scope config keeps its inheritors full, and a full project's reach
	// upgrades a project a task edge had already scoped.
	full := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/libs/a", Name: "a", Path: "libs/a", Scope: ScopeContribution{ConfigPaths: []string{"libs/putnami.json"}}},
		{ID: "/apps/both", Name: "both", Path: "apps/both", Dependencies: []string{"/libs/a"}},
	})
	r = TraceChangeImpact(full, []string{"libs/putnami.json"}, ChangeImpactOptions{Tasks: index})
	if scope := r.Trace.ScopeOf("/libs/a"); scope != nil {
		t.Errorf("a scope config scoped /libs/a to %v, want it full", scope)
	}
}

// A change to an in-workspace extension's own manifest that the index does not
// confine to task and step declarations keeps the extension full and re-runs
// the extension's tasks on every consumer, exactly as an implementation change
// does. A test file inside the extension rebuilds no binary and reaches no
// consumer at all.
func TestTraceChangeImpact_ExtensionConsumerEdgeFiresOnlyWhenTheBinaryMoved(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "extension-consumers-run-the-changed-extension-tasks")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/go/extension", Name: "@putnami/go", Path: "go/extension"},
		{ID: "/apps/web", Name: "web", Path: "apps/web", Extensions: []string{"/go/extension"}},
	})
	index := fakeTaskIndex{
		reading: map[string][]string{
			"/go/extension|go/extension/run.go":      {"build~describe", "test~test"},
			"/go/extension|go/extension/run_test.go": {"test~test"},
		},
		reached:        map[string][]string{"build~describe": {"build~describe"}},
		everyReached:   []string{"build~describe"},
		extensionTasks: map[string][]string{"/go/extension": {"/go/extension#build~describe", "/go/extension#test~test"}},
	}

	cases := []struct {
		file      string
		wantWeb   []string
		wantScope []string
	}{
		{file: "go/extension/putnami.extension.json", wantWeb: []string{"/apps/web"},
			wantScope: []string{"/go/extension#build~describe", "/go/extension#test~test"}},
		{file: "go/extension/run.go", wantWeb: []string{"/apps/web"},
			wantScope: []string{"/go/extension#build~describe", "/go/extension#test~test"}},
		{file: "go/extension/run_test.go", wantWeb: nil},
	}
	for _, tc := range cases {
		r := TraceChangeImpact(ws, []string{tc.file}, ChangeImpactOptions{Tasks: index})
		selected := ownerProjectIDs(r.Projects)
		if slices.Contains(selected, "/apps/web") != (tc.wantWeb != nil) {
			t.Errorf("%s selected %v, want the consumer present=%t", tc.file, selected, tc.wantWeb != nil)
			continue
		}
		if got := r.Trace.ScopeOf("/apps/web"); !slices.Equal(got, tc.wantScope) {
			t.Errorf("%s scoped /apps/web to %v, want %v", tc.file, got, tc.wantScope)
		}
	}
}

// A file a dependency's tests read reaches the extension that imports it
// through the test pipeline's `^generate`, and the extension's tests reach its
// own dependents in turn. Those are checking tasks: they build nothing the
// extension's consumers execute, so the consumer edge does not fire. A source
// file of the same dependency reaches the extension's build, which moves the
// runtime, and every consumer runs every task of the extension. The extension's
// own test layer is the same case.
func TestTraceChangeImpact_ACheckingTaskMovesNoExtensionRuntime(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "a-checking-task-moves-no-extension-runtime", "a-file-only-checking-tasks-read-selects-no-consumer")
	spectest.Proves(t, "cli/workspace-model", "change-impact", "extension-consumers-run-the-changed-extension-tasks")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/protocols/features", Name: "features", Path: "protocols/features"},
		{ID: "/go/extension", Name: "@putnami/go", Path: "go/extension", Dependencies: []string{"/protocols/features"}},
		{ID: "/apps/web", Name: "web", Path: "apps/web", Extensions: []string{"/go/extension"}},
	})
	everyTask := []string{"/go/extension#build~describe", "/go/extension#test~generate", "/go/extension#test~test"}
	index := fakeTaskIndex{
		reading: map[string][]string{
			"/protocols/features|protocols/features/fixtures/spec.json": {"test~generate", "test~test"},
			"/protocols/features|protocols/features/feature.go":         {"build~describe", "test~generate", "test~test"},
		},
		reached: map[string][]string{
			"build~describe": {"build~describe", "test~generate", "test~test"},
			"test~generate":  {"test~generate", "test~test"},
		},
		extensionTasks: map[string][]string{"/go/extension": everyTask},
		checking:       map[string]bool{"test~generate": true, "test~test": true},
	}
	// The extension's own test layer reads every spec of the workspace.
	index.reading["/go/extension|go/extension/specs/spec.json"] = []string{"test~generate", "test~test"}

	for _, file := range []string{"protocols/features/fixtures/spec.json", "go/extension/specs/spec.json"} {
		r := TraceChangeImpact(ws, []string{file}, ChangeImpactOptions{Tasks: index})
		if got, want := r.Trace.ScopeOf("/go/extension"), []string{"test~generate", "test~test"}; !slices.Equal(got, want) {
			t.Errorf("%s scoped the extension to %v, want %v", file, got, want)
		}
		if selected := ownerProjectIDs(r.Projects); slices.Contains(selected, "/apps/web") {
			t.Errorf("%s, which only tests read, selected the consumer: %v", file, selected)
		}
	}

	r := TraceChangeImpact(ws, []string{"protocols/features/feature.go"}, ChangeImpactOptions{Tasks: index})
	if got := r.Trace.ScopeOf("/apps/web"); !slices.Equal(got, everyTask) {
		t.Errorf("a source file scoped the consumer to %v, want %v", got, everyTask)
	}

	// One diff moves the runtime and a file only tests read. The edge names
	// the runtime input, never the test-only file the extension claimed first.
	r = TraceChangeImpact(ws, []string{"go/extension/specs/spec.json", "protocols/features/feature.go"},
		ChangeImpactOptions{Tasks: index})
	want := ImpactEdge{From: "/go/extension", Kind: ImpactEdgeExtensionConsumer, Via: "protocols/features/feature.go", Cause: ImpactCauseRuntimeInput}
	if got := r.Trace.Edges["/apps/web"]; got != want {
		t.Errorf("edge into the consumer = %+v, want %+v", got, want)
	}

	// The same tasks declared by a producing command move the runtime.
	index.checking = nil
	r = TraceChangeImpact(ws, []string{"protocols/features/fixtures/spec.json"}, ChangeImpactOptions{Tasks: index})
	if got := r.Trace.ScopeOf("/apps/web"); !slices.Equal(got, everyTask) {
		t.Errorf("producing tasks scoped the consumer to %v, want %v", got, everyTask)
	}
}

// The workspace lists an extension for every project, and the extension plans
// tasks in some of them only. Its rebuilt runtime reaches a consumer for the
// tasks it may plan there, and a consumer it may plan none in is not selected:
// no action of that project executes the tool that moved.
func TestTraceChangeImpact_AnExtensionReachesOnlyTheConsumersItMayPlanIn(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "an-extension-reaches-only-the-consumers-it-may-plan-in")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/go/extension", Name: "@putnami/go", Path: "go/extension"},
		{ID: "/apps/api", Name: "api", Path: "apps/api", Extensions: []string{"/go/extension"}},
		{ID: "/apps/lib", Name: "lib", Path: "apps/lib", Extensions: []string{"/go/extension"}},
		{ID: "/apps/docs", Name: "docs", Path: "apps/docs", Extensions: []string{"/go/extension"}},
	})
	every := []string{"/go/extension#build~describe", "/go/extension#test~test"}
	index := fakeTaskIndex{
		reading:        map[string][]string{"/go/extension|go/extension/run.go": {"build~describe", "test~test"}},
		reached:        map[string][]string{"build~describe": {"build~describe"}},
		everyReached:   []string{"build~describe"},
		extensionTasks: map[string][]string{"/go/extension": every},
		plannedIn: map[string][]string{
			"/apps/lib":  {"/go/extension#build~describe"},
			"/apps/docs": {},
		},
	}

	r := TraceChangeImpact(ws, []string{"go/extension/run.go"}, ChangeImpactOptions{Tasks: index})
	selected := ownerProjectIDs(r.Projects)
	slices.Sort(selected)
	if want := []string{"/apps/api", "/apps/lib", "/go/extension"}; !slices.Equal(selected, want) {
		t.Fatalf("selected %v, want %v: a consumer the extension plans no task in is not reached", selected, want)
	}
	if got := r.Trace.ScopeOf("/apps/api"); !slices.Equal(got, every) {
		t.Errorf("/apps/api is scoped to %v, want every task %v", got, every)
	}
	if got, want := r.Trace.ScopeOf("/apps/lib"), []string{"/go/extension#build~describe"}; !slices.Equal(got, want) {
		t.Errorf("/apps/lib is scoped to %v, want the one task the extension plans there %v", got, want)
	}
	if _, reached := r.Trace.Edges["/apps/docs"]; reached {
		t.Errorf("the trace records an edge into /apps/docs: %+v", r.Trace.Edges["/apps/docs"])
	}
	if path := impactPathWithTasks(ws, "/go/extension", "/apps/docs", index); path != nil {
		t.Errorf("the impact path to /apps/docs is %+v, want none", path)
	}
}

// A root file a provider claims through watchedFiles keeps its project full
// when no task declares it: the provider answered for a reason it never had to
// state per task, and narrowing an unattributed claim is how a gate stops
// running.
func TestTraceChangeImpact_AnUnattributedRootClaimKeepsTheProjectFull(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	ws := NewWorkspace("/workspace", nil, []*Project{{ID: "/libs/http", Name: "http", Path: "libs/http"}})
	ws.AdoptProbeView(map[string]wsproto.MergedProject{
		"libs/http": {Path: "libs/http", WatchedFiles: []string{"go.work"}},
	})

	r := TraceChangeImpact(ws, []string{"go.work"}, ChangeImpactOptions{Tasks: fakeTaskIndex{}})

	if got, want := ownerProjectIDs(r.Projects), []string{"/libs/http"}; !slices.Equal(got, want) {
		t.Fatalf("a claimed root file selected %v, want %v", got, want)
	}
	if scope := r.Trace.ScopeOf("/libs/http"); scope != nil {
		t.Errorf("scoped /libs/http to %v, want it full", scope)
	}
}

// `why_impacted` answers from the same propagation and names the same task
// scopes, so the pair cannot disagree about what a change to a project runs.
func TestImpactReachWithTasks_NamesTheTaskScopeOfEveryProjectItReaches(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	ws := taskImpactWorkspace()
	index := taskImpactIndex()

	reach := ImpactReachWithTasks(ws, "/libs/http", index)

	if got, want := ownerProjectIDs(reach.Projects), []string{"/libs/http", "/libs/api", "/apps/service"}; !slices.Equal(got, want) {
		t.Fatalf("the reach of /libs/http is %v, want %v", got, want)
	}
	if scope := reach.Trace.ScopeOf("/libs/http"); scope != nil {
		t.Errorf("the seed carries scope %v, want it full", scope)
	}
	want := []string{"build~compile", "build~describe", "test~test"}
	if got := reach.Trace.ScopeOf("/apps/service"); !slices.Equal(got, want) {
		t.Errorf("the reach scoped /apps/service to %v, want %v", got, want)
	}
	steps := impactPathWithTasks(ws, "/libs/http", "/apps/service", index)
	if len(steps) != 3 || steps[2].Kind != ImpactEdgeDependency {
		t.Errorf("the path is %+v, want three hops ending on a dependency edge", steps)
	}
}

// An extension a consumer reaches as a package DEPENDENCY is named in no
// project's `extensions` list, so the consumer index does not know it. Its
// manifest is still the declaration every consumer's jobs are planned from —
// a preBuild hook runs inside their builds — so a change to it keeps the
// declaring project whole and the dependency edge carries from there.
//
// Reading "is an extension" off the consumer index instead would select the
// declaring project's own tasks alone and no project that RUNS the hook.
func TestTraceChangeImpact_APackageDependencyExtensionManifestKeepsItsProjectFull(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/ts/web", Name: "@putnami/web", Path: "ts/web"},
		{ID: "/sites/docs", Name: "docs", Path: "sites/docs", Dependencies: []string{"/ts/web"}},
	})
	index := fakeTaskIndex{
		// No task of the extension project declares its own manifest, and the
		// lint task that happens to read it carries nothing across an edge.
		reading:      map[string][]string{"/ts/web|ts/web/putnami.extension.json": {"lint~check"}},
		reached:      map[string][]string{"build~generate": {"build~generate"}},
		everyReached: []string{"build~generate"},
		// The change is confined to one task declaration, which no consumer
		// edge can carry: no `extensions` list names the project.
		manifest: map[string][]string{"/ts/web|ts/web/putnami.extension.json": {"/ts/web#lint~check"}},
	}

	r := TraceChangeImpact(ws, []string{"ts/web/putnami.extension.json"}, ChangeImpactOptions{Tasks: index})

	selected := ownerProjectIDs(r.Projects)
	if !slices.Contains(selected, "/sites/docs") {
		t.Fatalf("selected %v, want the consumer of the changed extension manifest", selected)
	}
	if scope := r.Trace.ScopeOf("/ts/web"); scope != nil {
		t.Errorf("scoped /ts/web to %v, want it full: nothing can be said to read its own manifest", scope)
	}
}

// A file of an extension's runtime — embedded into the binary its consumers
// execute — keeps the extension whole even when a foreign extension's task of
// the same name claims it. `go/extension/tools/versions.json` pins the linters
// every Go consumer runs, and TypeScript's `lint~check` over `**/*.json`
// answered for it: a task the Go extension does not run, carrying nothing.
func TestTraceChangeImpact_AnExtensionRuntimeFileReachesEveryConsumer(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "extension-consumers-run-the-changed-extension-tasks")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/go/extension", Name: "@putnami/go", Path: "go/extension"},
		{ID: "/apps/web", Name: "web", Path: "apps/web", Extensions: []string{"/go/extension"}},
	})
	file := "go/extension/tools/versions.json"
	index := fakeTaskIndex{
		reading:        map[string][]string{"/go/extension|" + file: {"lint~check"}},
		runtime:        map[string]bool{"/go/extension|" + file: true},
		extensionTasks: map[string][]string{"/go/extension": {"/go/extension#lint~golangci-lint"}},
	}

	r := TraceChangeImpact(ws, []string{file}, ChangeImpactOptions{Tasks: index})

	if !slices.Contains(ownerProjectIDs(r.Projects), "/apps/web") {
		t.Fatalf("selected %v, want the consumer of the rebuilt extension", ownerProjectIDs(r.Projects))
	}
	if scope := r.Trace.ScopeOf("/go/extension"); scope != nil {
		t.Errorf("scoped /go/extension to %v, want it full", scope)
	}
}
