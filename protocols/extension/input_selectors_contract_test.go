package extension

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

// selectorManifest is a manifest whose extension lists sel:build and whose
// build task names it in a project input.
func selectorManifest() *Manifest {
	return &Manifest{
		Name:     "@test/selectors",
		Commands: map[string]CommandDefinition{"build": {Run: []PipelineStep{{ID: "build", Task: "build"}}}},
		Tasks: map[string]TaskDefinition{"build": {Kind: "command", Command: "{extensionRuntime}", Inputs: map[string]TaskInputPort{
			"sources": {From: TaskInputFromProject, Files: []string{"src/**", "sel:build", "!src/gen/**"}},
		}}},
		Runtime: &RuntimeDefinition{Executable: "bin/x", InputSelectors: []string{"sel:build"}},
	}
}

func marshalManifest(t *testing.T, m *Manifest) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A manifest that lists a selector, or names one in a task, requires the
// input-selectors rung from every origin, and the highest rung wins.
func TestInputSelectorsRequireTheirContract(t *testing.T) {
	listed := &Manifest{Name: "@test/x", Runtime: &RuntimeDefinition{Executable: "bin/x", InputSelectors: []string{"sel:build"}}}
	named := &Manifest{Name: "@test/x", Tasks: map[string]TaskDefinition{"t": {Cache: &TaskCachePolicy{Key: &TaskCacheKey{Files: []string{"sel:build"}}}}}}
	misplaced := &Manifest{Name: "@test/x", Tasks: map[string]TaskDefinition{"t": {Inputs: map[string]TaskInputPort{
		"w": {From: TaskInputFromWorkspace, Files: []string{"sel:build"}},
	}}}}
	for name, m := range map[string]*Manifest{"listed": listed, "named": named, "misplaced": misplaced, "both": selectorManifest()} {
		if got := RequiredLoadContract(m); got != protocolcli.InputSelectorsContract {
			t.Errorf("%s: RequiredLoadContract = %d, want %d", name, got, protocolcli.InputSelectorsContract)
		}
		if got := RequiredCLIContract(m); got != protocolcli.InputSelectorsContract {
			t.Errorf("%s: RequiredCLIContract = %d, want %d", name, got, protocolcli.InputSelectorsContract)
		}
	}
	withContent := selectorManifest()
	withContent.AgentContent = &AgentContentContribution{}
	if got := RequiredCLIContract(withContent); got != protocolcli.InputSelectorsContract {
		t.Errorf("combined additive floor = %d, want %d", got, protocolcli.InputSelectorsContract)
	}
	plain := &Manifest{Name: "@test/x", Tasks: map[string]TaskDefinition{"t": {Inputs: map[string]TaskInputPort{
		"s": {From: TaskInputFromProject, Files: []string{"**/*.go", "git:docs/**", "docs/a:b.md"}},
	}}}}
	if got := RequiredCLIContract(plain); got != protocolcli.GitInputModeContract {
		t.Errorf("a git: pattern and a path with a colon name no selector: RequiredCLIContract = %d, want %d",
			got, protocolcli.GitInputModeContract)
	}
}

// A manifest stamped below the rung that names a selector is refused for its
// selectors, with the remedy that moves it, whatever its earlier stamp meant.
func TestManifestNamingASelectorBelowItsContractIsRefused(t *testing.T) {
	for _, stamp := range []int{6, protocolcli.InputSelectorsContract - 1} {
		m := selectorManifest()
		m.CLIContract = stamp
		_, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest that names a selector", stamp)
		}
		for _, want := range []string{"requires 14 for its input selectors", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
	m := selectorManifest()
	m.CLIContract = protocolcli.InputSelectorsContract
	loaded, err := NegotiateManifest("x.json", marshalManifest(t, m))
	if err != nil {
		t.Fatalf("contract-14 stamp refused: %v", err)
	}
	if want := []string{"sel:build"}; !reflect.DeepEqual(loaded.Runtime.InputSelectors, want) {
		t.Errorf("loaded input selectors = %v, want %v", loaded.Runtime.InputSelectors, want)
	}
}

// The full validator and the negotiated loader refuse the same selectors: one
// the manifest does not list, one outside a project input or cache.key.files,
// and an exclusion.
func TestFullValidateManifestSharesInputSelectorRulesWithLoader(t *testing.T) {
	if diags := FullValidateManifest(selectorManifest()); diag.HasErrors(diags) {
		t.Fatalf("valid project selector rejected: %v", diags)
	}
	withFiles := func(from string, files ...string) func(*Manifest) {
		return func(m *Manifest) {
			task := m.Tasks["build"]
			task.Inputs = map[string]TaskInputPort{"sources": {From: from, Files: files}}
			m.Tasks["build"] = task
		}
	}
	withKey := func(key TaskCacheKey) func(*Manifest) {
		return func(m *Manifest) {
			task := m.Tasks["build"]
			task.Inputs = nil
			task.Cache = &TaskCachePolicy{Key: &key}
			m.Tasks["build"] = task
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"unlisted selector", withFiles(TaskInputFromProject, "sel:other"), "not listed in runtime.inputSelectors"},
		{"invalid selector name", withFiles(TaskInputFromProject, "sel:Build"), "not listed in runtime.inputSelectors"},
		{"no runtime", func(m *Manifest) { m.Runtime = nil }, "not listed in runtime.inputSelectors"},
		{"workspace origin", withFiles(TaskInputFromWorkspace, "sel:build"), "only valid in a project task input"},
		{"closure origin", withFiles(TaskInputFromClosure, "sel:build"), "only valid in a project task input"},
		{"exclusion", withFiles(TaskInputFromProject, "src/**", "!sel:build"), "cannot be an exclusion"},
		{"cache key closure files", withKey(TaskCacheKey{ClosureFiles: []string{"sel:build"}}), "only valid in a project task input"},
		{"cache key workspace files", withKey(TaskCacheKey{WorkspaceFiles: []string{"sel:build"}}), "only valid in a project task input"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := selectorManifest()
			test.mutate(m)
			diags := FullValidateManifest(m)
			if !diag.HasErrors(diags) || !strings.Contains(formatDiagnostics(diags), test.want) {
				t.Fatalf("FullValidateManifest accepted %s: %v", test.name, diags)
			}
			m.CLIContract = protocolcli.InputSelectorsContract
			if _, err := NegotiateManifest("x.json", marshalManifest(t, m)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loader disagreed on %s: %v", test.name, err)
			}
		})
	}
	keyed := selectorManifest()
	withKey(TaskCacheKey{Files: []string{"src/**", "sel:build"}})(keyed)
	if diags := FullValidateManifest(keyed); diag.HasErrors(diags) {
		t.Fatalf("selector in cache.key.files rejected: %v", diags)
	}
}

// TaskSelectorInputs pairs each selector a task names with the other patterns
// of the input that names it, never with another input's.
func TestTaskSelectorInputsPairsEachSelectorWithItsInput(t *testing.T) {
	task := TaskDefinition{Inputs: map[string]TaskInputPort{
		"b-sources": {From: TaskInputFromProject, Files: []string{"**/*.go", "!**/*_test.go", "sel:build", "!**/.gen/**"}},
		"a-tests":   {From: TaskInputFromProject, Files: []string{"**/*_test.go", "sel:test"}},
		"modules":   {From: TaskInputFromProject, Files: []string{"go.mod"}},
		"workspace": {From: TaskInputFromWorkspace, Files: []string{"go.work"}},
	}, Cache: &TaskCachePolicy{Key: &TaskCacheKey{Files: []string{"sel:ignored"}}}}
	want := []SelectorInput{
		{Selector: "sel:test", Patterns: []string{"**/*_test.go"}},
		{Selector: "sel:build", Patterns: []string{"**/*.go", "!**/*_test.go", "!**/.gen/**"}},
	}
	if got := TaskSelectorInputs(task); !reflect.DeepEqual(got, want) {
		t.Errorf("TaskSelectorInputs = %#v, want %#v", got, want)
	}
	keyed := TaskDefinition{Cache: &TaskCachePolicy{Key: &TaskCacheKey{Files: []string{"sel:build", "src/**"}}}}
	if got, want := TaskSelectorInputs(keyed), []SelectorInput{{Selector: "sel:build", Patterns: []string{"src/**"}}}; !reflect.DeepEqual(got, want) {
		t.Errorf("TaskSelectorInputs of cache.key.files = %#v, want %#v", got, want)
	}
	if got := TaskSelectorInputs(TaskDefinition{Inputs: map[string]TaskInputPort{"s": {From: TaskInputFromProject, Files: []string{"git:**"}}}}); got != nil {
		t.Errorf("TaskSelectorInputs of a git: input = %#v, want nil", got)
	}
}
