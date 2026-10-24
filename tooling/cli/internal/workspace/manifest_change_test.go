package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

const fixtureManifestPath = "go/extension/putnami.extension.json"

// fixtureGoManifest is an extension manifest in the Go extension's shape: a
// build pipeline whose `describe` carries the `^` edge, a test task, and two
// lint steps whose tasks declare a params input, as `lint-golangci-fix` and
// `lint-golangci-readonly` do. Each task sits on one line, so a test edits it
// as text.
const fixtureGoManifest = `{
  "name": "@fixture/go",
  "version": "1.0.0",
  "runtime": {"executable": "bin/putnami-go"},
  "commands": {
    "build": {"description": "Build", "run": [
      {"id": "describe", "task": "go-describe", "dependsOn": ["^describe"]},
      {"id": "compile", "task": "go-compile", "dependsOn": ["describe"]}
    ]},
    "test": {"run": [{"id": "test", "task": "go-test"}]},
    "lint": {"run": [
      {"id": "golangci-lint", "task": "lint-golangci-fix"},
      {"id": "golangci-lint-check-only", "task": "lint-golangci-readonly"}
    ]}
  },
  "tasks": {
    "lint-golangci-fix": {"kind": "exec", "command": "golangci-lint", "inputs": {"sources": {"from": "project", "files": ["**/*.go"]}, "config-path": {"from": "params"}, "fix": {"from": "params"}}},
    "lint-golangci-readonly": {"kind": "exec", "command": "golangci-lint", "inputs": {"sources": {"from": "project", "files": ["**/*.go"]}, "config-path": {"from": "params"}, "fix": {"from": "params"}}},
    "go-describe": {"kind": "exec", "command": "go", "inputs": {"sources": {"from": "project", "files": ["**/*.go", "!**/*_test.go"]}}},
    "go-compile": {"kind": "exec", "command": "go", "inputs": {"sources": {"from": "project", "files": ["**/*.go", "!**/*_test.go"]}}},
    "go-test": {"kind": "exec", "command": "go", "inputs": {"sources": {"from": "project", "files": ["**/*.go"]}}}
  }
}
`

// configPathInput is the params input both lint tasks of the fixture declare.
const configPathInput = `, "config-path": {"from": "params"}`

// everyFixtureTask is every task the fixture manifest declares, qualified by
// its extension project.
var everyFixtureTask = []string{
	"/go/extension#build~compile", "/go/extension#build~describe", "/go/extension#lint~golangci-lint",
	"/go/extension#lint~golangci-lint-check-only", "/go/extension#test~test",
}

// editManifest replaces the first n occurrences of old in manifest, and fails
// the test unless manifest holds at least n.
func editManifest(t *testing.T, manifest, old, replacement string, n int) string {
	t.Helper()
	if got := strings.Count(manifest, old); got < n {
		t.Fatalf("the manifest holds %d of %q, want %d to edit", got, old, n)
	}
	return strings.Replace(manifest, old, replacement, n)
}

// manifestWorkspace commits a workspace in which an in-workspace extension
// imports a library and two projects run under it, with baseline as the
// extension's committed manifest; "" commits none. It returns the workspace
// root.
func manifestWorkspace(t *testing.T, baseline string) string {
	t.Helper()
	root := t.TempDir()
	gitInitWithBaseline(t, root)
	for _, file := range []string{
		"go/extension/main.go", "tooling/sdk/registrycred/install.go", "tooling/sdk/registrycred/install_test.go",
		"apps/svc/main.go", "apps/web/main.go",
	} {
		writeFile(t, filepath.Join(root, file), "package main\n")
	}
	if baseline != "" {
		writeFile(t, filepath.Join(root, fixtureManifestPath), baseline)
	}
	runGitForImpact(t, root, "add", "-A")
	runGitForImpact(t, root, "commit", "-m", "fixture")
	return root
}

// selectManifestWorkspace loads the extension from the working tree's manifest,
// as a run does, and runs the --impacted selection against main.
func selectManifestWorkspace(t *testing.T, root string) ImpactedSelection {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, fixtureManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	var manifest extension.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	description := extension.Resolve(&manifest, filepath.Join(root, "go", "extension"))
	description.RelPath = "go/extension"
	ws := NewWorkspace(root, nil, []*Project{
		{ID: "/go/extension", Name: "@fixture/go", Path: "go/extension", Dependencies: []string{"sdk"}, Extensions: []string{"/go/extension"}},
		{ID: "/tooling/sdk", Name: "sdk", Path: "tooling/sdk"},
		{ID: "/apps/svc", Name: "svc", Path: "apps/svc", Extensions: []string{"/go/extension"}},
		{ID: "/apps/web", Name: "web", Path: "apps/web", Extensions: []string{"/go/extension"}},
	})
	index := NewTaskIndex(ws, []*extension.ExtensionDescription{description}, nil)
	selection, err := ImpactedSelectionForBaselineWithTasks(ws, "main", index)
	if err != nil {
		t.Fatalf("ImpactedSelectionForBaselineWithTasks: %v", err)
	}
	return selection
}

// recordedEdge is the selection:impacted record's edge into a project.
func recordedEdge(t *testing.T, selection ImpactedSelection, project string) ImpactTraceEdge {
	t.Helper()
	edges := selection.TraceRecord().Edges
	for _, edge := range edges {
		if edge.Project == project {
			return edge
		}
	}
	t.Fatalf("the record has no edge into %s: %+v", project, edges)
	return ImpactTraceEdge{}
}

// explainedEdge is the --verbose explanation's line for the edge into a
// project.
func explainedEdge(t *testing.T, selection ImpactedSelection, project string) string {
	t.Helper()
	for _, line := range selection.TraceExplanation() {
		if strings.HasPrefix(line, "    "+project+" ← ") {
			return line
		}
	}
	t.Fatalf("the explanation has no edge into %s: %q", project, selection.TraceExplanation())
	return ""
}

// The diff shape of a manifest edit that deletes one params input from two
// lint tasks: every consumer runs those two lint steps and nothing else of the
// extension, and the trace names the manifest as a task declaration.
func TestImpactedSelection_AManifestChangeReachesTheTasksItMoves(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-manifest-change-reaches-the-tasks-it-moves")
	root := manifestWorkspace(t, fixtureGoManifest)
	writeFile(t, filepath.Join(root, fixtureManifestPath), editManifest(t, fixtureGoManifest, configPathInput, "", 2))

	selection := selectManifestWorkspace(t, root)

	want := []string{"/go/extension#lint~golangci-lint", "/go/extension#lint~golangci-lint-check-only"}
	for _, consumer := range []string{"/apps/svc", "/apps/web"} {
		if got := selection.Trace.ScopeOf(consumer); !slices.Equal(got, want) {
			t.Errorf("scoped %s to %v, want %v", consumer, got, want)
		}
	}
	if got := selection.Trace.ScopeOf("/go/extension"); !slices.Equal(got, want) {
		t.Errorf("scoped the extension to %v, want %v", got, want)
	}
	if line, wantLine := explainedEdge(t, selection, "/apps/svc"),
		"    /apps/svc ← /go/extension  [extension-consumer task declaration "+fixtureManifestPath+"; tasks "+strings.Join(want, ", ")+"]"; line != wantLine {
		t.Errorf("explanation line = %q, want %q", line, wantLine)
	}
	if edge := recordedEdge(t, selection, "/apps/svc"); edge.Via != fixtureManifestPath || edge.Cause != "task-declaration" {
		t.Errorf("recorded edge = %+v, want via %s and cause task-declaration", edge, fixtureManifestPath)
	}
}

// A task the manifest adds or removes moves with the step that names it.
func TestImpactedSelection_AnAddedOrRemovedTaskIsInTheScope(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-manifest-change-reaches-the-tasks-it-moves")
	readonlyStep := `{"id": "golangci-lint-check-only", "task": "lint-golangci-readonly"}`
	added := editManifest(t, fixtureGoManifest, readonlyStep, readonlyStep+`, {"id": "vet", "task": "lint-vet"}`, 1)
	added = editManifest(t, added, `"tasks": {`, `"tasks": {
    "lint-vet": {"kind": "exec", "command": "go", "inputs": {"sources": {"from": "project", "files": ["**/*.go"]}}},`, 1)

	removed := editManifest(t, fixtureGoManifest, `,
      `+readonlyStep, "", 1)
	removed = editManifest(t, removed, `
    "lint-golangci-readonly": {"kind": "exec", "command": "golangci-lint", "inputs": {"sources": {"from": "project", "files": ["**/*.go"]}, "config-path": {"from": "params"}, "fix": {"from": "params"}}},`, "", 1)

	cases := []struct {
		name     string
		manifest string
		want     []string
	}{
		{name: "added", manifest: added, want: []string{"/go/extension#lint~vet"}},
		{name: "removed", manifest: removed, want: []string{"/go/extension#lint~golangci-lint-check-only"}},
	}
	for _, tc := range cases {
		root := manifestWorkspace(t, fixtureGoManifest)
		writeFile(t, filepath.Join(root, fixtureManifestPath), tc.manifest)

		selection := selectManifestWorkspace(t, root)

		if got := selection.Trace.ScopeOf("/apps/svc"); !slices.Equal(got, tc.want) {
			t.Errorf("%s: scoped the consumer to %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A change outside task and step declarations reaches every consumer for every
// task, and keeps the extension whole: a hook, the runtime block, a command's
// own members, the version.
func TestImpactedSelection_AManifestChangeOutsideTaskDeclarationsReachesEveryTask(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-manifest-change-reaches-the-tasks-it-moves")
	hook := `"version": "1.0.0",
  "hooks": {"preBuild": {"kind": "exec", "command": "true"}},`
	cases := map[string]string{
		"hook":    editManifest(t, fixtureGoManifest, `"version": "1.0.0",`, hook, 1),
		"runtime": editManifest(t, fixtureGoManifest, `"bin/putnami-go"`, `"bin/putnami-go-next"`, 1),
		"command": editManifest(t, fixtureGoManifest, `"description": "Build"`, `"description": "Build the project"`, 1),
		"version": editManifest(t, fixtureGoManifest, `"version": "1.0.0"`, `"version": "1.0.1"`, 1),
		"declarations and hook": editManifest(t, editManifest(t, fixtureGoManifest, configPathInput, "", 1),
			`"version": "1.0.0",`, hook, 1),
	}
	for name, changed := range cases {
		root := manifestWorkspace(t, fixtureGoManifest)
		writeFile(t, filepath.Join(root, fixtureManifestPath), changed)

		selection := selectManifestWorkspace(t, root)

		if got := selection.Trace.ScopeOf("/apps/svc"); !slices.Equal(got, everyFixtureTask) {
			t.Errorf("%s: scoped the consumer to %v, want every task %v", name, got, everyFixtureTask)
		}
		if scope := selection.Trace.ScopeOf("/go/extension"); scope != nil {
			t.Errorf("%s: scoped the extension to %v, want it whole", name, scope)
		}
	}
}

// A layout the manifest's JSON values do not see moves nothing: a manifest
// written on one line, with its members in another order, reaches no consumer.
func TestImpactedSelection_AManifestRewrittenWithTheSameValuesMovesNothing(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-manifest-change-reaches-the-tasks-it-moves")
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fixtureGoManifest), &document); err != nil {
		t.Fatal(err)
	}
	rewritten, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	root := manifestWorkspace(t, fixtureGoManifest)
	writeFile(t, filepath.Join(root, fixtureManifestPath), string(rewritten))

	selection := selectManifestWorkspace(t, root)

	for _, consumer := range []string{"/apps/svc", "/apps/web"} {
		if slices.ContainsFunc(selection.Projects, func(p *Project) bool { return p.ID == consumer }) {
			t.Errorf("selected %s for a manifest whose values did not change", consumer)
		}
	}
}

// A manifest the extension's runtime is prepared from is a runtime input: a
// change to its task declarations alone still reaches every consumer for every
// task, and the trace names the manifest as the runtime input.
func TestImpactedSelection_AManifestTheRuntimeIsPreparedFromReachesEveryTask(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-manifest-change-reaches-the-tasks-it-moves")
	prepared := editManifest(t, fixtureGoManifest, `"runtime": {"executable": "bin/putnami-go"}`,
		`"runtime": {"executable": "bin/putnami-go", "prepare": {"command": "{extensionRoot}/bin/prepare", "inputs": ["putnami.extension.json", "cmd/**"]}}`, 1)
	root := manifestWorkspace(t, prepared)
	writeFile(t, filepath.Join(root, fixtureManifestPath), editManifest(t, prepared, configPathInput, "", 1))

	selection := selectManifestWorkspace(t, root)

	if got := selection.Trace.ScopeOf("/apps/svc"); !slices.Equal(got, everyFixtureTask) {
		t.Errorf("scoped the consumer to %v, want every task %v", got, everyFixtureTask)
	}
	if edge := recordedEdge(t, selection, "/apps/svc"); edge.Via != fixtureManifestPath || edge.Cause != "runtime-input" {
		t.Errorf("recorded edge = %+v, want via %s and cause runtime-input", edge, fixtureManifestPath)
	}
}

// A manifest the diff's base does not record, or records in a form that does
// not parse, cannot be compared: every consumer runs every task.
func TestImpactedSelection_AnUnreadableBaselineManifestReachesEveryTask(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "a-manifest-change-reaches-the-tasks-it-moves")
	cases := map[string]string{
		"not recorded":   "",
		"does not parse": "{\"name\": \"@fixture/go\",\n",
	}
	for name, baseline := range cases {
		root := manifestWorkspace(t, baseline)
		writeFile(t, filepath.Join(root, fixtureManifestPath), fixtureGoManifest)

		selection := selectManifestWorkspace(t, root)

		if got := selection.Trace.ScopeOf("/apps/svc"); !slices.Equal(got, everyFixtureTask) {
			t.Errorf("%s: scoped the consumer to %v, want every task %v", name, got, everyFixtureTask)
		}
	}
}

// A library the extension imports moves its runtime: every consumer runs every
// task, and the trace names the library file the walk traced the runtime to,
// not the test file beside it.
func TestImpactedSelection_TheTraceNamesTheRuntimeInput(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "the-trace-names-the-runtime-input")
	root := manifestWorkspace(t, fixtureGoManifest)
	for _, file := range []string{"tooling/sdk/registrycred/install.go", "tooling/sdk/registrycred/install_test.go"} {
		writeFile(t, filepath.Join(root, file), "package main\n\nvar timeout = 1\n")
	}

	selection := selectManifestWorkspace(t, root)

	if got := selection.Trace.ScopeOf("/apps/svc"); !slices.Equal(got, everyFixtureTask) {
		t.Errorf("scoped the consumer to %v, want every task %v", got, everyFixtureTask)
	}
	if line := explainedEdge(t, selection, "/apps/svc"); !strings.HasPrefix(line,
		"    /apps/svc ← /go/extension  [extension-consumer runtime input tooling/sdk/registrycred/install.go; tasks ") {
		t.Errorf("explanation line = %q, want the runtime input named", line)
	}
	if edge := recordedEdge(t, selection, "/apps/svc"); edge.Via != "tooling/sdk/registrycred/install.go" || edge.Cause != "runtime-input" {
		t.Errorf("recorded edge = %+v, want via tooling/sdk/registrycred/install.go and cause runtime-input", edge)
	}
}
