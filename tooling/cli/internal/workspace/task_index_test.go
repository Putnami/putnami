package workspace

import (
	"slices"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// taskIndexExtension is a manifest in the shape the Go extension uses: a build
// pipeline whose `describe` carries the cross-project `^` edge and excludes test
// files, a test pipeline whose `test` reads them, a lint command that reads
// every source, and a finalizer that tears the test environment down.
func taskIndexExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name:    "@fixture/go",
		RelPath: "go/extension",
		Tasks: map[string]extproto.TaskDefinition{
			"build-generate": {Inputs: map[string]extproto.TaskInputPort{
				"sources":   {From: extproto.TaskInputFromProject, Files: []string{"**/*.go", "!**/*_test.go"}},
				"workspace": {From: extproto.TaskInputFromWorkspace, Files: []string{"go.work"}},
			}},
			"build-describe": {Inputs: map[string]extproto.TaskInputPort{
				"sources": {From: extproto.TaskInputFromProject, Files: []string{"**/*.go", "!**/*_test.go"}},
			}},
			"build-compile": {Inputs: map[string]extproto.TaskInputPort{
				"sources": {From: extproto.TaskInputFromProject, Files: []string{"**/*.go", "!**/*_test.go"}},
			}},
			"test-env-up":   {},
			"test-env-down": {},
			"test-exec": {Inputs: map[string]extproto.TaskInputPort{
				"tests":   {From: extproto.TaskInputFromProject, Files: []string{"**/*_test.go"}},
				"sources": {From: extproto.TaskInputFromProject, Files: []string{"**/*.go"}},
				"config":  {From: extproto.TaskInputFromClosure, Files: []string{"conf/.env.yaml"}},
			}},
			"lint-exec": {Inputs: map[string]extproto.TaskInputPort{
				"sources": {From: extproto.TaskInputFromProject, Files: []string{"**/*.go"}},
			}},
		},
		Jobs: map[string]*extension.JobDefinition{
			"build": {CommandName: "build", PipelineSteps: []extproto.PipelineStep{
				{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
				{ID: "describe", Task: "build-describe", DependsOn: []string{"^describe", "generate"}},
				{ID: "compile", Task: "build-compile", DependsOn: []string{"describe"}},
			}},
			"test": {CommandName: "test", PipelineSteps: []extproto.PipelineStep{
				{ID: "describe", Task: "build-describe", DependsOn: []string{"^describe"}},
				{ID: "test-env", Task: "test-env-up", DependsOn: []string{"describe"}},
				{ID: "test", Task: "test-exec", DependsOn: []string{"describe", "test-env"}},
				{ID: "teardown", Task: "test-env-down", RunOn: extproto.StepRunOnFinally,
					Finalizes: &extproto.FinalizesRelation{Producer: "test-env", Consumers: []string{"test"}}},
			}},
			"lint": {CommandName: "lint", PipelineSteps: []extproto.PipelineStep{
				{ID: "lint", Task: "lint-exec"},
			}},
		},
	}
}

// projectOptions builds a project config whose option layers declare the given
// file patterns, plus one cross-project generate asset when named. The untyped
// shape is the protocol's: extension options are not part of the project
// schema, so ProjectConfig.Options is where a declaration arrives untyped.
func projectOptions(layers map[string][]string, assets ...string) *wsproto.ProjectConfig {
	options := make(map[string]map[string]any, len(layers)+1)
	for name, patterns := range layers {
		values := make([]any, 0, len(patterns))
		for _, pattern := range patterns {
			values = append(values, pattern)
		}
		options[name] = map[string]any{"filePatterns": values}
	}
	entries := make([]any, 0, len(assets))
	for _, asset := range assets {
		entries = append(entries, map[string]any{"from": asset})
	}
	if len(entries) > 0 {
		options["generate"] = map[string]any{"assets": entries}
	}
	return &wsproto.ProjectConfig{Options: options}
}

func taskIndexWorkspace(t *testing.T, config *wsproto.ProjectConfig) *Workspace {
	t.Helper()
	return NewWorkspace("/workspace", nil, []*Project{
		{ID: "/go/extension", Name: "@fixture/go", Path: "go/extension"},
		{ID: "/libs/http", Name: "http", Path: "libs/http", Config: config},
	})
}

func newTaskIndexForTest(t *testing.T, config *wsproto.ProjectConfig) TaskImpactIndex {
	t.Helper()
	idx := NewTaskIndex(taskIndexWorkspace(t, config),
		[]*extension.ExtensionDescription{taskIndexExtension()})
	if idx == nil {
		t.Fatal("NewTaskIndex returned nil for a workspace with one manifest")
	}
	return idx
}

// A file is read by the tasks whose declared inputs select it, and the
// exclusions inside one input port bind: a test file is not a source of
// `build~describe`, which is what stops it from reaching a single dependent.
func TestTaskIndex_TasksReadingPathHonorsEveryDeclaredInput(t *testing.T) {
	idx := newTaskIndexForTest(t, nil)
	cases := []struct {
		path string
		want []string
	}{
		{path: "libs/http/README.md"},
		{path: "libs/http/http_test.go", want: []string{"lint~lint", "test~teardown", "test~test"}},
		{path: "libs/http/http.go", want: []string{
			"build~compile", "build~describe", "build~generate", "lint~lint",
			"test~describe", "test~teardown", "test~test", "test~test-env",
		}},
		{path: "libs/http/conf/.env.yaml", want: []string{"closure:test~test", "test~teardown", "test~test"}},
		{path: "go.work", want: []string{"build~compile", "build~describe", "build~generate"}},
		{path: "libs/other/http.go"},
	}
	for _, tc := range cases {
		if got := idx.TasksReadingPath("/libs/http", tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s is read by %v, want %v", tc.path, got, tc.want)
		}
	}
}

// A `^` reference is the only cross-project task edge. A task nothing
// references — `test~test`, `lint~lint` — reaches no dependent at all, which is
// why a test file stops at its own project.
func TestTaskIndex_TasksReachedFromFollowsUpstreamStepReferencesOnly(t *testing.T) {
	idx := newTaskIndexForTest(t, nil)
	cases := []struct {
		from []string
		want []string
	}{
		{from: []string{"test~test"}},
		{from: []string{"lint~lint"}},
		{from: []string{"build~describe"}, want: []string{"build~compile", "build~describe"}},
		{from: []string{"build~generate"}, want: []string{"build~compile", "build~describe", "build~generate"}},
		{from: []string{"build~describe", "test~test"}, want: []string{"build~compile", "build~describe"}},
	}
	for _, tc := range cases {
		if got := idx.TasksReachedFrom(tc.from); !slices.Equal(got, tc.want) {
			t.Errorf("%v reaches %v, want %v", tc.from, got, tc.want)
		}
	}
	// A nil list is "every task of the source project".
	// A full project moved every file its closure ports select, so the closure
	// scope travels too.
	want := []string{"build~compile", "build~describe", "build~generate", "closure:test~test",
		"test~describe", "test~teardown", "test~test", "test~test-env"}
	if got := idx.TasksReachedFrom(nil); !slices.Equal(got, want) {
		t.Errorf("a full project reaches %v, want %v", got, want)
	}
}

// A finalizer runs because its producer and consumers ran and it is the only
// step that tears down what they provisioned, so it belongs to the scope of
// both. Without the edge a scope naming `test` would leak the environment.
func TestTaskIndex_AFinalizerIsInTheScopeOfWhatItFinalizes(t *testing.T) {
	idx := newTaskIndexForTest(t, nil)
	for _, task := range []string{"test~test", "test~test-env"} {
		reading := idx.TasksReadingPath("/libs/http", "libs/http/http_test.go")
		if !slices.Contains(reading, "test~teardown") {
			t.Fatalf("the scope carrying %s is %v, want the finalizer test~teardown in it", task, reading)
		}
	}
}

// A project's option layers are attributed the way the cache key groups them: a
// command layer reaches that command's tasks, an extension layer reaches the
// tasks that extension declares, and a layer that resolves to neither reaches
// every task rather than none.
func TestTaskIndex_OptionLayersAreAttributedToTheTasksThatReadThem(t *testing.T) {
	idx := newTaskIndexForTest(t, projectOptions(map[string][]string{
		"test":         {"fixtures/**"},
		"@fixture/go":  {"golden/**"},
		"unattributed": {"shared/**"},
		"go/extension": {"byPath/**"},
	}, "/assets/logo.svg"))
	every := []string{
		"build~compile", "build~describe", "build~generate", "lint~lint",
		"test~describe", "test~teardown", "test~test", "test~test-env",
	}
	cases := []struct {
		path string
		want []string
	}{
		{path: "libs/http/fixtures/case.json", want: []string{"test~describe", "test~teardown", "test~test", "test~test-env"}},
		{path: "libs/http/golden/out.txt", want: every},
		{path: "libs/http/shared/thing.txt", want: every},
		// A cross-project generate asset is folded into every job's key of the
		// declaring project, so every task of it reads the asset.
		{path: "assets/logo.svg", want: every},
	}
	for _, tc := range cases {
		if got := idx.TasksReadingPath("/libs/http", tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s is read by %v, want %v", tc.path, got, tc.want)
		}
	}
}

// A project may declare an input ABOVE its own root, and two projects in this
// workspace do — `@putnami/cli` watches "../../**/putnami.json" and
// `@putnami/cli-documents` watches "git:**". The model seeds such a project from
// a file it does not own, so the index has to attribute that file to its tasks;
// answering "no task reads this" would drop the claim and leave the drift gate
// unrun over the very change it exists to watch.
//
// The reach is asymmetric on purpose. An extension manifest's `from: "project"`
// patterns are written once for every project that runs under it, so they stop
// at the project root: `libs/other/http.go` is not a source of `/libs/http`.
func TestTaskIndex_ADeclaredInputAboveTheProjectRootStillSelectsItsTasks(t *testing.T) {
	idx := newTaskIndexForTest(t, projectOptions(map[string][]string{
		"test": {"../../**/putnami.json", "git:**"},
	}))
	everyTestTask := []string{"test~describe", "test~teardown", "test~test", "test~test-env"}
	cases := []struct {
		path string
		want []string
	}{
		{path: "go/extension/putnami.json", want: everyTestTask},
		{path: "libs/other/deep/nested/putnami.json", want: everyTestTask},
		// `git:**` selects the whole repository, siblings and ancestors
		// included, and the cache hasher collects exactly that set.
		{path: "README.md", want: everyTestTask},
		// An extension's own project patterns do not escape the project root,
		// so another project's sources stay unread.
		{path: "libs/other/http.go", want: everyTestTask},
	}
	for _, tc := range cases {
		if got := idx.TasksReadingPath("/libs/http", tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s is read by %v, want %v", tc.path, got, tc.want)
		}
	}
	// Without the declaration, nothing above the root is read at all: the
	// widening comes from the project's own layer, never from the manifest.
	bare := newTaskIndexForTest(t, nil)
	for _, path := range []string{"go/extension/putnami.json", "libs/other/http.go", "README.md"} {
		if got := bare.TasksReadingPath("/libs/http", path); got != nil {
			t.Errorf("without a declared input %s is read by %v, want nil", path, got)
		}
	}
}

// An extension project's tasks are named with that project's id, so a scope
// built from the extension-consumer edge keeps the consumer's jobs of THAT
// extension and not another extension's job of the same command and step.
func TestTaskIndex_TasksOfExtensionAreQualifiedByTheExtensionProject(t *testing.T) {
	idx := newTaskIndexForTest(t, nil)
	want := []string{
		"/go/extension#build~compile", "/go/extension#build~describe", "/go/extension#build~generate",
		"/go/extension#lint~lint", "/go/extension#test~describe", "/go/extension#test~teardown",
		"/go/extension#test~test", "/go/extension#test~test-env",
	}
	if got := idx.TasksOfExtension("/go/extension"); !slices.Equal(got, want) {
		t.Errorf("the extension declares %v, want %v", got, want)
	}
	if got := idx.TasksOfExtension("/libs/http"); got != nil {
		t.Errorf("a plain project declares %v, want nil", got)
	}
}

// A published build pinned by the name of a workspace project carries no
// RelPath: its tasks are still named with the id of the project it replaces,
// so a consumer that names that project by path keeps the pinned build's jobs.
func TestTaskIndex_APinnedBuildIsQualifiedByTheProjectItReplaces(t *testing.T) {
	ext := taskIndexExtension()
	ext.RelPath, ext.PinnedOver = "", "go/extension"
	idx := NewTaskIndex(taskIndexWorkspace(t, nil), []*extension.ExtensionDescription{ext})
	if idx == nil {
		t.Fatal("NewTaskIndex returned nil for a workspace with one pinned build")
	}
	got := idx.TasksOfExtension("/go/extension")
	if !slices.Contains(got, "/go/extension#build~compile") || !slices.Contains(got, "/go/extension#test~test") {
		t.Errorf("the pinned build declares %v, want tasks qualified by /go/extension", got)
	}
}

// A run that loaded no extension gets a nil INTERFACE, never a pointer inside
// one: an index that answered "no task reads this" for every path would select
// nothing at all.
func TestNewTaskIndex_WithoutExtensionsIsANilInterface(t *testing.T) {
	if idx := NewTaskIndex(taskIndexWorkspace(t, nil), nil); idx != nil {
		t.Errorf("NewTaskIndex without extensions = %#v, want a nil interface", idx)
	}
}

// A file a `closure` port selects is read by every dependent running that
// task, transitively: the closure scope reaches the dependent's task and
// travels on, which a `^` reference never does.
func TestTaskIndex_AClosureReadTravelsToEveryDependent(t *testing.T) {
	idx := newTaskIndexForTest(t, nil)
	reading := idx.TasksReadingPath("/libs/http", "libs/http/conf/.env.yaml")
	if !slices.Contains(reading, closureScopePrefix+"test~test") {
		t.Fatalf("conf/.env.yaml is read by %v, want the closure scope of test~test", reading)
	}
	reached := idx.TasksReachedFrom(reading)
	for _, want := range []string{closureScopePrefix + "test~test", "test~test", "test~teardown"} {
		if !slices.Contains(reached, want) {
			t.Errorf("a dependent reaches %v, want %s", reached, want)
		}
	}
	// A plain source file carries no closure scope.
	if reached := idx.TasksReachedFrom(idx.TasksReadingPath("/libs/http", "libs/http/http_test.go")); len(reached) != 0 {
		t.Errorf("a test file reaches %v in a dependent, want nothing", reached)
	}
}

// An extension project's runtime files are recognized from its
// runtime.prepare.inputs, test files excluded.
func TestTaskIndex_ReadsAsExtensionRuntime(t *testing.T) {
	ext := taskIndexExtension()
	ext.Runtime = &extension.RuntimeDefinition{Prepare: &extproto.RuntimePrepare{Inputs: []string{"cmd/**", "tools/**"}}}
	idx := NewTaskIndex(taskIndexWorkspace(t, nil), []*extension.ExtensionDescription{ext})
	cases := map[string]bool{
		"go/extension/tools/versions.json": true,
		"go/extension/cmd/run.go":          true,
		"go/extension/cmd/run_test.go":     false,
		"go/extension/README.md":           false,
		"libs/http/tools/versions.json":    false,
	}
	for path, want := range cases {
		project := "/go/extension"
		if path == "libs/http/tools/versions.json" {
			project = "/libs/http"
		}
		if got := idx.ReadsAsExtensionRuntime(project, path); got != want {
			t.Errorf("ReadsAsExtensionRuntime(%s, %s) = %t, want %t", project, path, got, want)
		}
	}
}
