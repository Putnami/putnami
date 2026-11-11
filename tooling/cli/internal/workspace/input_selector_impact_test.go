package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// selectorIndexExtension declares one build task whose project input names
// sel:build beside a plain source pattern. Its build command runs in a project
// that lists the extension, as discovery fills a job's extension in.
func selectorIndexExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name:    "@test/selectors",
		RelPath: "go/extension",
		Tasks: map[string]extproto.TaskDefinition{
			"compile": {Inputs: map[string]extproto.TaskInputPort{
				"sources": {From: extproto.TaskInputFromProject, Files: []string{"src/**", "sel:build", "!src/gen/**"}},
			}},
		},
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				CommandName: "build", Name: "build", ExtensionName: "@test/selectors", ExtensionPath: "go/extension",
				PipelineSteps: []extproto.PipelineStep{{ID: "compile", Task: "compile"}},
			},
		},
		Runtime: &extension.RuntimeDefinition{InputSelectors: []string{"sel:build"}},
	}
}

// selectorIndexWorkspace is taskIndexWorkspace with libs/http listing the
// selector extension by its path ID.
func selectorIndexWorkspace(t *testing.T) *Workspace {
	t.Helper()
	ws := taskIndexWorkspace(t, nil)
	ws.ProjectByID("/libs/http").Extensions = []string{"/go/extension"}
	return ws
}

// fakeMatcher answers every selector with one listing and one basis set, and
// records each query and each priming call.
type fakeMatcher struct {
	files   []string
	content []string
	failed  bool
	asked   []InputSelectorQuery
	primed  [][]InputSelectorQuery
}

func (m *fakeMatcher) SelectsInput(query InputSelectorQuery, rel string) bool {
	m.asked = append(m.asked, query)
	if m.failed {
		return true
	}
	return slices.Contains(m.files, rel) || slices.Contains(m.content, rel)
}

func (m *fakeMatcher) PrimeInputs(queries []InputSelectorQuery) {
	m.primed = append(m.primed, queries)
}

// A path the selector's answer lists or its basis selects is read by the task
// that names the selector, whether the path still exists or not; any other
// path is not, and a selector the extension cannot expand selects every path.
func TestInputSelectorSelectsChangedAndDeletedTargets(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "go-embed-selector-selects-changed-and-deleted-targets")
	inputs := &fakeMatcher{files: []string{"assets/removed.sql"}, content: []string{"lib/source.txt"}}
	idx := NewTaskIndex(selectorIndexWorkspace(t), []*extension.ExtensionDescription{selectorIndexExtension()}, inputs)
	for path, want := range map[string][]string{
		"libs/http/assets/removed.sql":   {"build~compile"},
		"libs/http/lib/source.txt":       {"build~compile"},
		"libs/http/src/main.txt":         {"build~compile"},
		"libs/http/assets/unrelated.txt": nil,
	} {
		if got := idx.TasksReadingPath("/libs/http", path); !slices.Equal(got, want) {
			t.Errorf("%s is read by %v, want %v", path, got, want)
		}
	}
	want := InputSelectorQuery{Extension: "@test/selectors", Dir: "/workspace/libs/http", Selector: "sel:build", Patterns: []string{"src/**", "!src/gen/**"}}
	if len(inputs.asked) == 0 || !queryEqual(inputs.asked[0], want) {
		t.Errorf("the index asked %+v, want %+v", inputs.asked, want)
	}
	failed := &fakeMatcher{failed: true}
	idx = NewTaskIndex(selectorIndexWorkspace(t), []*extension.ExtensionDescription{selectorIndexExtension()}, failed)
	if got := idx.TasksReadingPath("/libs/http", "libs/http/assets/unrelated.txt"); !slices.Equal(got, []string{"build~compile"}) {
		t.Errorf("a failed expansion selects %v, want the task", got)
	}
	idx = NewTaskIndex(selectorIndexWorkspace(t), []*extension.ExtensionDescription{selectorIndexExtension()}, nil)
	if got := idx.TasksReadingPath("/libs/http", "libs/http/assets/unrelated.txt"); !slices.Equal(got, []string{"build~compile"}) {
		t.Errorf("an index without a matcher selects %v, want the task", got)
	}
}

// Impact asks the extensions once for the selectors of every project a
// changed path falls in, and for no other project.
func TestInputSelectorImpactPrimesTheChangedProjects(t *testing.T) {
	inputs := &fakeMatcher{}
	idx := NewTaskIndex(selectorIndexWorkspace(t), []*extension.ExtensionDescription{selectorIndexExtension()}, inputs)
	primer, ok := idx.(interface{ PrimeChangedPaths(paths []string) })
	if !ok {
		t.Fatal("the task index does not prime changed paths")
	}
	primer.PrimeChangedPaths([]string{"libs/http/a.txt", "libs/http/b.txt", "README.md"})
	if len(inputs.primed) != 1 || len(inputs.primed[0]) != 1 || inputs.primed[0][0].Dir != "/workspace/libs/http" {
		t.Fatalf("priming calls = %+v, want one query for libs/http", inputs.primed)
	}
	inputs.primed = nil
	primer.PrimeChangedPaths([]string{"README.md"})
	if len(inputs.primed) != 0 {
		t.Fatalf("a change outside every project primed %+v", inputs.primed)
	}
}

// A selector takes part only in a project the planner may run its command in:
// one that lists the extension and holds one of the command's activation
// files. Elsewhere it reads nothing, even when its extension cannot answer,
// and the index asks the extension nothing for it. Where it takes part, an
// answer that fails still reads every path.
func TestInputSelectorReadsNothingInAProjectItsExtensionCannotPlan(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs",
		"a-selector-reads-nothing-in-a-project-its-extension-cannot-plan")
	root := t.TempDir()
	for _, dir := range []string{"libs/http", "libs/bare", "libs/pub"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"libs/http/go.mod", "libs/pub/go.mod"} {
		if err := os.WriteFile(filepath.Join(root, file), []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws := NewWorkspace(root, nil, []*Project{
		{ID: "/go/extension", Name: "@fixture/go", Path: "go/extension"},
		{ID: "/libs/http", Name: "http", Path: "libs/http", Extensions: []string{"/go/extension"}},
		{ID: "/libs/bare", Name: "bare", Path: "libs/bare", Extensions: []string{"@test/selectors"}},
		{ID: "/libs/pub", Name: "pub", Path: "libs/pub"},
	})
	ext := selectorIndexExtension()
	ext.Jobs["build"].ActivationFiles = []string{"go.mod"}
	for name, inputs := range map[string]InputSelectorMatcher{"a failed answer": &fakeMatcher{failed: true}, "no matcher": nil} {
		idx := NewTaskIndex(ws, []*extension.ExtensionDescription{ext}, inputs)
		for project, want := range map[string][]string{
			"/libs/http": {"build~compile"},
			"/libs/bare": nil,
			"/libs/pub":  nil,
		} {
			path := ws.ProjectByID(project).Path + "/notes.txt"
			if got := idx.TasksReadingPath(project, path); !slices.Equal(got, want) {
				t.Errorf("%s: %s is read by %v, want %v", name, path, got, want)
			}
		}
		if got := idx.TasksReadingPath("/libs/pub", "libs/pub/src/main.go"); !slices.Equal(got, []string{"build~compile"}) {
			t.Errorf("%s: a source the task's own pattern selects is read by %v, want the task", name, got)
		}
	}
	inputs := &fakeMatcher{}
	idx := NewTaskIndex(ws, []*extension.ExtensionDescription{ext}, inputs)
	idx.(interface{ PrimeChangedPaths(paths []string) }).PrimeChangedPaths(
		[]string{"libs/http/notes.txt", "libs/bare/notes.txt", "libs/pub/notes.txt"})
	if len(inputs.primed) != 1 || len(inputs.primed[0]) != 1 || inputs.primed[0][0].Dir != filepath.Join(root, "libs/http") {
		t.Fatalf("priming calls = %+v, want one query for libs/http", inputs.primed)
	}
	for _, project := range []string{"/libs/bare", "/libs/pub"} {
		idx.TasksReadingPath(project, ws.ProjectByID(project).Path+"/notes.txt")
	}
	for _, query := range inputs.asked {
		if query.Dir != filepath.Join(root, "libs/http") {
			t.Errorf("the index asked %+v, a project the extension cannot plan", query)
		}
	}
}

func queryEqual(a, b InputSelectorQuery) bool {
	return a.Extension == b.Extension && a.Dir == b.Dir && a.Selector == b.Selector && slices.Equal(a.Patterns, b.Patterns)
}
