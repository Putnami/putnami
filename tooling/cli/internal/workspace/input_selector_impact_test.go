package workspace

import (
	"slices"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// selectorIndexExtension declares one build task whose project input names
// sel:build beside a plain source pattern.
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
			"build": {CommandName: "build", PipelineSteps: []extproto.PipelineStep{{ID: "compile", Task: "compile"}}},
		},
		Runtime: &extension.RuntimeDefinition{InputSelectors: []string{"sel:build"}},
	}
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
	idx := NewTaskIndex(taskIndexWorkspace(t, nil), []*extension.ExtensionDescription{selectorIndexExtension()}, inputs)
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
	idx = NewTaskIndex(taskIndexWorkspace(t, nil), []*extension.ExtensionDescription{selectorIndexExtension()}, failed)
	if got := idx.TasksReadingPath("/libs/http", "libs/http/assets/unrelated.txt"); !slices.Equal(got, []string{"build~compile"}) {
		t.Errorf("a failed expansion selects %v, want the task", got)
	}
	idx = NewTaskIndex(taskIndexWorkspace(t, nil), []*extension.ExtensionDescription{selectorIndexExtension()}, nil)
	if got := idx.TasksReadingPath("/libs/http", "libs/http/assets/unrelated.txt"); !slices.Equal(got, []string{"build~compile"}) {
		t.Errorf("an index without a matcher selects %v, want the task", got)
	}
}

// Impact asks the extensions once for the selectors of every project a
// changed path falls in, and for no other project.
func TestInputSelectorImpactPrimesTheChangedProjects(t *testing.T) {
	inputs := &fakeMatcher{}
	idx := NewTaskIndex(taskIndexWorkspace(t, nil), []*extension.ExtensionDescription{selectorIndexExtension()}, inputs)
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

func queryEqual(a, b InputSelectorQuery) bool {
	return a.Extension == b.Extension && a.Dir == b.Dir && a.Selector == b.Selector && slices.Equal(a.Patterns, b.Patterns)
}
