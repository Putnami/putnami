package workspace

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const goManifest = "go/extension/putnami.extension.json"

// manifestImpactWorkspace is an in-workspace extension that imports a library,
// a project that runs under it, and a project that imports it as a package.
// The extension comes before the library, so the walk meets the extension's
// own seed first.
func manifestImpactWorkspace() *Workspace {
	return NewWorkspace("/workspace", nil, []*Project{
		{ID: "/go/extension", Name: "@putnami/go", Path: "go/extension", Dependencies: []string{"/tooling/sdk"}},
		{ID: "/tooling/sdk", Name: "sdk", Path: "tooling/sdk"},
		{ID: "/apps/web", Name: "web", Path: "apps/web", Extensions: []string{"/go/extension"}},
		{ID: "/libs/plugin", Name: "plugin", Path: "libs/plugin", Dependencies: []string{"/go/extension"}},
	})
}

// manifestImpactIndex declares the extension's three tasks, a lint task that
// reads the manifest and carries nothing, and a manifest change confined to
// the declaration of `lint~golangci-lint`.
func manifestImpactIndex() fakeTaskIndex {
	return fakeTaskIndex{
		reading: map[string][]string{
			"/go/extension|" + goManifest:              {"lint~check"},
			"/go/extension|go/extension/run.go":        {"build~describe", "test~test"},
			"/tooling/sdk|tooling/sdk/install.go":      {"build~describe", "test~test"},
			"/tooling/sdk|tooling/sdk/install_test.go": {"test~test"},
		},
		reached:      map[string][]string{"build~describe": {"build~describe"}},
		everyReached: []string{"build~describe"},
		extensionTasks: map[string][]string{"/go/extension": {
			"/go/extension#build~describe", "/go/extension#lint~golangci-lint", "/go/extension#test~test",
		}},
		manifest: map[string][]string{"/go/extension|" + goManifest: {"/go/extension#lint~golangci-lint"}},
	}
}

var everyGoTask = []string{"/go/extension#build~describe", "/go/extension#lint~golangci-lint", "/go/extension#test~test"}

// A manifest change confined to one task's declaration reaches every consumer
// for that task alone, and the edge names the manifest as a task declaration.
// The extension runs the task too, beside the tasks that read the manifest,
// and nothing it carries reaches the project that imports it.
func TestTraceChangeImpact_AConfinedManifestChangeReachesConsumersForTheTasksItMoved(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "extension-consumers-run-the-changed-extension-tasks")
	r := TraceChangeImpact(manifestImpactWorkspace(), []string{goManifest}, ChangeImpactOptions{Tasks: manifestImpactIndex()})

	if got, want := ownerProjectIDs(r.Projects), []string{"/go/extension", "/apps/web"}; !slices.Equal(got, want) {
		t.Fatalf("selected %v, want %v", got, want)
	}
	if got, want := r.Trace.ScopeOf("/apps/web"), []string{"/go/extension#lint~golangci-lint"}; !slices.Equal(got, want) {
		t.Errorf("scoped the consumer to %v, want %v", got, want)
	}
	if got, want := r.Trace.ScopeOf("/go/extension"), []string{"/go/extension#lint~golangci-lint", "lint~check"}; !slices.Equal(got, want) {
		t.Errorf("scoped the extension to %v, want %v", got, want)
	}
	want := ImpactEdge{From: "/go/extension", Kind: ImpactEdgeExtensionConsumer, Via: goManifest, Cause: ImpactCauseTaskDeclaration}
	if got := r.Trace.Edges["/apps/web"]; got != want {
		t.Errorf("edge into the consumer = %+v, want %+v", got, want)
	}
}

// A manifest change the index does not confine, or a runtime change beside a
// confined one, reaches every consumer for every task, and the edge names the
// file the runtime moved with.
func TestTraceChangeImpact_ARuntimeChangeReachesConsumersForEveryTask(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "extension-consumers-run-the-changed-extension-tasks")
	unconfined := manifestImpactIndex()
	unconfined.manifest = nil
	cases := []struct {
		name    string
		changed []string
		index   fakeTaskIndex
		via     string
	}{
		{name: "unconfined manifest", changed: []string{goManifest}, index: unconfined, via: goManifest},
		{name: "manifest then source", changed: []string{goManifest, "go/extension/run.go"}, index: manifestImpactIndex(), via: "go/extension/run.go"},
		{name: "source then manifest", changed: []string{"go/extension/run.go", goManifest}, index: manifestImpactIndex(), via: "go/extension/run.go"},
	}
	for _, tc := range cases {
		r := TraceChangeImpact(manifestImpactWorkspace(), tc.changed, ChangeImpactOptions{Tasks: tc.index})
		if got := r.Trace.ScopeOf("/apps/web"); !slices.Equal(got, everyGoTask) {
			t.Errorf("%s: scoped the consumer to %v, want %v", tc.name, got, everyGoTask)
		}
		want := ImpactEdge{From: "/go/extension", Kind: ImpactEdgeExtensionConsumer, Via: tc.via, Cause: ImpactCauseRuntimeInput}
		if got := r.Trace.Edges["/apps/web"]; got != want {
			t.Errorf("%s: edge into the consumer = %+v, want %+v", tc.name, got, want)
		}
	}
}

// A task that reads a confined manifest runs on the extension and carries to
// the project that imports it, as a task that reads any file does. It does not
// move the extension's runtime: the consumer still runs the moved task alone.
func TestTraceChangeImpact_AManifestReaderPropagatesWithoutMovingTheRuntime(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "extension-consumers-run-the-changed-extension-tasks")
	index := manifestImpactIndex()
	index.reading["/go/extension|"+goManifest] = []string{"build~describe"}

	r := TraceChangeImpact(manifestImpactWorkspace(), []string{goManifest}, ChangeImpactOptions{Tasks: index})

	if got, want := r.Trace.ScopeOf("/libs/plugin"), []string{"build~describe"}; !slices.Equal(got, want) {
		t.Errorf("scoped the importer to %v, want %v", got, want)
	}
	if got, want := r.Trace.ScopeOf("/apps/web"), []string{"/go/extension#lint~golangci-lint"}; !slices.Equal(got, want) {
		t.Errorf("scoped the consumer to %v, want %v", got, want)
	}
}

// A library change reaches the extension that imports it, and the consumer
// edge names the library file the runtime moved with, not the test file the
// same diff changed first. A test file alone moves no runtime.
func TestTraceChangeImpact_TheConsumerEdgeNamesTheRuntimeInput(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "trace-names-the-edge-that-reached-a-propagated-project")
	ws := manifestImpactWorkspace()

	r := TraceChangeImpact(ws, []string{"tooling/sdk/install_test.go", "tooling/sdk/install.go"},
		ChangeImpactOptions{Tasks: manifestImpactIndex()})

	want := ImpactEdge{From: "/go/extension", Kind: ImpactEdgeExtensionConsumer, Via: "tooling/sdk/install.go", Cause: ImpactCauseRuntimeInput}
	if got := r.Trace.Edges["/apps/web"]; got != want {
		t.Errorf("edge into the consumer = %+v, want %+v", got, want)
	}
	if got := r.Trace.ScopeOf("/apps/web"); !slices.Equal(got, everyGoTask) {
		t.Errorf("scoped the consumer to %v, want %v", got, everyGoTask)
	}

	r = TraceChangeImpact(ws, []string{"tooling/sdk/install_test.go"}, ChangeImpactOptions{Tasks: manifestImpactIndex()})
	if got, want := ownerProjectIDs(r.Projects), []string{"/tooling/sdk"}; !slices.Equal(got, want) {
		t.Errorf("a test file selected %v, want %v", got, want)
	}
}

// When a runtime change reaches an extension after its confined manifest
// change already reached the consumer, the consumer runs every task and its
// edge names the runtime input: a consumer that runs everything names the
// reason it does.
func TestTraceChangeImpact_ARuntimeEdgeReplacesTheDeclarationEdge(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "trace-names-the-edge-that-reached-a-propagated-project")
	r := TraceChangeImpact(manifestImpactWorkspace(), []string{goManifest, "tooling/sdk/install.go"},
		ChangeImpactOptions{Tasks: manifestImpactIndex()})

	want := ImpactEdge{From: "/go/extension", Kind: ImpactEdgeExtensionConsumer, Via: "tooling/sdk/install.go", Cause: ImpactCauseRuntimeInput}
	if got := r.Trace.Edges["/apps/web"]; got != want {
		t.Errorf("edge into the consumer = %+v, want %+v", got, want)
	}
	if got := r.Trace.ScopeOf("/apps/web"); !slices.Equal(got, everyGoTask) {
		t.Errorf("scoped the consumer to %v, want %v", got, everyGoTask)
	}
}

// A confined manifest change that moves no task, read by no task, selects
// nothing: a description edit runs nothing anywhere.
func TestTraceChangeImpact_AConfinedManifestChangeThatMovesNothingSelectsNothing(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	index := manifestImpactIndex()
	index.manifest["/go/extension|"+goManifest] = nil
	delete(index.reading, "/go/extension|"+goManifest)

	r := TraceChangeImpact(manifestImpactWorkspace(), []string{goManifest}, ChangeImpactOptions{Tasks: index})

	if len(r.Projects) != 0 {
		t.Errorf("selected %v, want nothing", ownerProjectIDs(r.Projects))
	}
}
