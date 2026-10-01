package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
)

// The Python workspace-sync task's contract, as tests.
//
// Core deleted its three-way name writer in this slice, so this task is the ONLY
// thing that aligns a pyproject.toml identity. Three properties are load-bearing:
// it writes only inside the `[project]` table, it converges (a second run writes
// nothing), and it refuses to run on an unknown membership.

func syncWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "pyproject.toml"),
		"[project]\nname = \"putnami-workspace\"\n\n[tool.uv.workspace]\nmembers = [\"app\"]\n")
	writeProbeFile(t, filepath.Join(root, "app", "pyproject.toml"),
		"[project]\nname = \"stale_name\"\nversion = \"0.1.0\"\n\n[tool.ruff]\nname = \"do-not-touch\"\n")
	writeProbeFile(t, filepath.Join(root, "web", "package.json"), `{"name":"web"}`)
	return root
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// The resolved name is core's answer — explicit putnami.json identity, scope
// namePattern, directory fallback already applied. This task writes it, and only
// into the `[project]` table.
func TestSyncPythonWorkspace_AlignsTheProjectTableOnly(t *testing.T) {
	root := syncWorkspace(t)

	// SourceName is the identity each project DECLARED, before a scope
	// namePattern override. Nothing declared app's, so core fell back to the
	// directory basename and the scope resolved it to py_example_application —
	// the one legitimate rename.
	changes, err := syncPythonWorkspace(root, []pctx.ProjectRef{
		{Name: "py_example_application", SourceName: "app", Path: "app"},
		{Name: "web", SourceName: "web", Path: "web"},
		{Name: "workspace", SourceName: "workspace", Path: "."},
	}, false)
	if err != nil {
		t.Fatalf("syncPythonWorkspace: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "app/pyproject.toml" ||
		changes[0].Before != "stale_name" || changes[0].After != "py_example_application" {
		t.Fatalf("changes = %+v, want one app rename", changes)
	}

	content := readFile(t, filepath.Join(root, "app", "pyproject.toml"))
	if !slices.Contains(splitLines(content), `name = "py_example_application"`) {
		t.Errorf("app pyproject.toml = %q, want the [project] name rewritten", content)
	}
	if !slices.Contains(splitLines(content), `name = "do-not-touch"`) {
		t.Errorf("app pyproject.toml = %q, want the [tool.ruff] name untouched", content)
	}
	// The workspace root's own manifest is never a member.
	if got := readFile(t, filepath.Join(root, "pyproject.toml")); !slices.Contains(splitLines(got), `name = "putnami-workspace"`) {
		t.Errorf("root pyproject.toml = %q, want it untouched", got)
	}
}

// REGRESSION: a member whose `[project] name` IS the identity core
// resolved from must be left byte-identical, even when the resolved project
// NAME spells something else.
//
// This is the TypeScript task's reported failure on the same wire: every
// project carries a putnami.json declaring a path-shaped name while its native
// manifest carries the published distribution name. Core reports no divergence;
// the task compared the manifest against the RESOLVED name and rewrote every
// one. Nothing rewrites the dependents that require the distribution by its
// published name, so the rename lands half-applied.
func TestSyncPythonWorkspace_LeavesADeclaredNameAlone(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "libs", "console-core", "pyproject.toml")
	writeProbeFile(t, manifest,
		"[project]\nname = \"putnami-console-core\"\nversion = \"1.0.0\"\n")
	before := readFile(t, manifest)

	changes, err := syncPythonWorkspace(root, []pctx.ProjectRef{
		{Name: "libs/console-core", SourceName: "libs/console-core", Path: "libs/console-core"},
	}, false)
	if err != nil {
		t.Fatalf("syncPythonWorkspace: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none: the declared identity does not diverge from the resolved name", changes)
	}
	if after := readFile(t, manifest); after != before {
		t.Fatalf("the manifest was rewritten:\n%s", after)
	}
}

// The absent-SourceName arm: an orchestrator older than the protocol member
// sends no declared identity, and the task must DECLINE the rename. Not
// renaming leaves a manifest a later sync can fix; renaming cannot be undone
// from here.
func TestSyncPythonWorkspace_DeclinesTheRenameWithoutASourceName(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "app", "pyproject.toml")
	writeProbeFile(t, manifest, "[project]\nname = \"putnami-app\"\n")
	before := readFile(t, manifest)

	changes, err := syncPythonWorkspace(root, []pctx.ProjectRef{{Name: "app", Path: "app"}}, false)
	if err != nil {
		t.Fatalf("syncPythonWorkspace: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none: an unknown declared identity must not authorize a rename", changes)
	}
	if after := readFile(t, manifest); after != before {
		t.Fatalf("a manifest was renamed on an absent sourceName:\n%s", after)
	}
}

// Idempotence: `projects sync` runs on every membership change, so a second run
// must write nothing — a rewritten file moves its mtime and every downstream
// cache key that observes it.
func TestSyncPythonWorkspace_IsIdempotent(t *testing.T) {
	root := syncWorkspace(t)
	selection := []pctx.ProjectRef{{Name: "py_example_application", SourceName: "app", Path: "app"}}

	if _, err := syncPythonWorkspace(root, selection, false); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	manifest := filepath.Join(root, "app", "pyproject.toml")
	info, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	changes, err := syncPythonWorkspace(root, selection, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("second sync reported %+v, want no changes", changes)
	}
	after, err := os.Stat(manifest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Error("a converged sync rewrote the manifest; a no-op must leave mtimes alone")
	}
}

// `--dry-run` reports what it WOULD change and writes nothing.
func TestSyncPythonWorkspace_DryRunWritesNothing(t *testing.T) {
	root := syncWorkspace(t)
	before := readFile(t, filepath.Join(root, "app", "pyproject.toml"))

	changes, err := syncPythonWorkspace(root, []pctx.ProjectRef{
		{Name: "renamed", SourceName: "app", Path: "app"}}, true)
	if err != nil {
		t.Fatalf("syncPythonWorkspace: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want the rename reported", changes)
	}
	if got := readFile(t, filepath.Join(root, "app", "pyproject.toml")); got != before {
		t.Error("dry-run rewrote the manifest")
	}
}

// A manifest with no `[project] name` is the one the probe already refused to
// guess an identity for. Creating the key here would invent the identity the
// probe declined to invent.
func TestSyncPythonWorkspace_LeavesAManifestWithoutAProjectNameAlone(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "app", "pyproject.toml"), "[build-system]\nrequires = []\n")
	before := readFile(t, filepath.Join(root, "app", "pyproject.toml"))

	// SourceName deliberately DIVERGES from the resolved name, so the rename
	// guard passes and this case still exercises the writer itself rather than
	// stopping short at the guard.
	changes, err := syncPythonWorkspace(root, []pctx.ProjectRef{
		{Name: "app", SourceName: "stale", Path: "app"}}, false)
	if err != nil {
		t.Fatalf("syncPythonWorkspace: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none", changes)
	}
	if got := readFile(t, filepath.Join(root, "app", "pyproject.toml")); got != before {
		t.Error("a manifest with no [project] name was rewritten")
	}
}

// FAIL CLOSED on an empty selection: "nobody named any projects" and "this
// workspace has no Python members" are indistinguishable from here.
func TestRunWorkspaceSync_RefusesAnEmptySelection(t *testing.T) {
	status, _, err := runWorkspaceSync(&pctx.Context{WorkspaceRoot: t.TempDir()}, nil, nil)
	if status != "FAILED" || err == nil {
		t.Fatalf("status=%q err=%v, want a hard failure on an empty selection", status, err)
	}
}

// The adapter declaration and the code must agree: the marker the probe reads is
// the marker the manifest declares, and the syncTask it names must exist.
func TestManifest_WorkspaceAdapterMatchesTheProbeAndSyncTask(t *testing.T) {
	m := loadExtensionManifest(t)
	if m.Workspace == nil {
		t.Fatal("the manifest declares no workspace adapter")
	}
	if !slices.Equal(m.Workspace.Markers, []string{pythonWorkspaceMarker}) {
		t.Errorf("markers = %v, want [%s]", m.Workspace.Markers, pythonWorkspaceMarker)
	}
	if !slices.Contains(m.Workspace.Inputs, pythonWorkspaceMarker) {
		t.Errorf("inputs = %v, want it to contain the marker: editing it must invalidate the snapshot",
			m.Workspace.Inputs)
	}
	// uv.lock is an input because it is the state `putnami install` records; the
	// install-state fingerprint reads the adapters' declared root inputs since
	// slice C4b, so dropping it would stop a re-locked workspace from being
	// noticed.
	if !slices.Contains(m.Workspace.Inputs, "uv.lock") {
		t.Errorf("inputs = %v, want it to contain uv.lock", m.Workspace.Inputs)
	}
	if m.Workspace.SyncTask == "" {
		t.Fatal("the workspace adapter names no syncTask; core would have no writer to hand pyproject.toml to")
	}
	if _, ok := m.Tasks[m.Workspace.SyncTask]; !ok {
		t.Fatalf("workspace.syncTask %q is not defined in tasks", m.Workspace.SyncTask)
	}
	command, ok := m.Commands["workspace-sync"]
	if !ok {
		t.Fatal(`manifest lost the "workspace-sync" command`)
	}
	// Without workspace-once activation the command is planned per project and
	// the planner attaches no resolved selection, so the task would fail closed
	// on every run.
	if command.Activation != "workspace-once" {
		t.Fatalf(`workspace-sync activation = %q, want "workspace-once"`, command.Activation)
	}
	if command.Visibility != "internal" {
		t.Errorf("workspace-sync visibility = %q, want internal", command.Visibility)
	}
}

func splitLines(content string) []string {
	var out []string
	current := ""
	for _, r := range content {
		if r == '\n' {
			out = append(out, current)
			current = ""
			continue
		}
		current += string(r)
	}
	return append(out, current)
}
