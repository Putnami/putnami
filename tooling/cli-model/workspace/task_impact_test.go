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
	// extensionTasks maps an extension project id to the tasks it declares.
	extensionTasks map[string][]string
	// runtime holds the "<projectID>|<path>" pairs an extension's runtime reads.
	runtime map[string]bool
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

func (f fakeTaskIndex) TasksOfExtension(extensionProjectID string) []string {
	return f.extensionTasks[extensionProjectID]
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

// An in-workspace extension's own manifest is not the input of any task — it
// IS the task declaration — so it keeps the extension full and re-runs the
// extension's tasks on every consumer, exactly as an implementation change
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
	steps := ImpactPathWithTasks(ws, "/libs/http", "/apps/service", index)
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
