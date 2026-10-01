package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// readTextFile reads a fixture file as text.
func readTextFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func syncFixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"),
		"{\n  \"name\": \"repo\",\n  \"private\": true,\n  \"workspaces\": [\n    \"stale/path\"\n  ]\n}\n")
	writeProbeFile(t, filepath.Join(root, "apps", "web", "package.json"), `{"name":"old-web","version":"1.0.0"}`)
	writeProbeFile(t, filepath.Join(root, "libs", "core", "package.json"), `{"name":"@acme/core"}`)
	writeProbeFile(t, filepath.Join(root, "svc", "go.mod"), "module acme/svc\n\ngo 1.25\n")
	return root
}

// syncSelection is the resolved selection as core hands it over.
//
// SourceName is the identity each project DECLARED, before a scope namePattern
// override; Name is what core resolved. apps/web is the one legitimate rename:
// nothing declared its name, so core fell back to the directory basename
// ("web") and a scope namePattern then resolved it to "@acme/web". Every other
// member declares its own name, so SourceName == Name and its manifest must be
// left alone.
func syncSelection() []pctx.ProjectRef {
	return []pctx.ProjectRef{
		{Name: "@acme/web", SourceName: "web", Path: "apps/web"},
		{Name: "@acme/core", SourceName: "@acme/core", Path: "libs/core"},
		{Name: "acme/svc", SourceName: "acme/svc", Path: "svc"},
		{Name: "repo", SourceName: "repo", Path: "."},
	}
}

// compactJSON strips the indentation writeRootManifest applies, so a test can
// compare a nested member against the spelling it was authored in.
func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.String()
}

func readJSONField(t *testing.T, path, field string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return doc[field]
}

// The two mutations that moved out of CLI core: the root workspaces array and
// each member's package.json name.
func TestSyncTypeScriptWorkspace_AlignsNamesAndWorkspaces(t *testing.T) {
	root := syncFixtureTree(t)

	changes, err := syncTypeScriptWorkspace(root, syncSelection(), false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want the one rename and the workspaces rewrite", changes)
	}

	if got := string(readJSONField(t, filepath.Join(root, "apps", "web", "package.json"), "name")); got != `"@acme/web"` {
		t.Errorf("apps/web name = %s, want the resolved project name", got)
	}
	var workspaces []string
	if err := json.Unmarshal(readJSONField(t, filepath.Join(root, "package.json"), "workspaces"), &workspaces); err != nil {
		t.Fatal(err)
	}
	// Only members that CARRY a package.json, never the workspace root itself:
	// an npm workspace root listing itself makes bun resolve the workspace to
	// itself.
	if !slices.Equal(workspaces, []string{"apps/web", "libs/core"}) {
		t.Errorf("workspaces = %v, want [apps/web libs/core]", workspaces)
	}
}

// REGRESSION: a member whose package.json name IS the identity core
// resolved from must be left byte-identical, even when the resolved project
// NAME spells something else.
//
// The reported shape: every project carries a putnami.json declaring a
// path-shaped name (`surfaces/libs/console-core`) while its package.json
// carries the npm name (`@putnami/console-core`). Core reported "0 names to
// align" — correctly, because nothing diverged — and this task rewrote 21
// manifests in the same run, because it compared the manifest against the
// RESOLVED name instead of the divergence set. Nothing rewrites the
// `workspace:*` references that still spell `@putnami/console-core`, so the
// rename lands half-applied and `bun install` fails workspace-wide.
func TestSyncTypeScriptWorkspace_LeavesADeclaredNameAlone(t *testing.T) {
	root := t.TempDir()
	// The membership already matches, so the only change this run could make is
	// the rename under test.
	writeProbeFile(t, filepath.Join(root, "package.json"),
		`{"name":"repo","private":true,"workspaces":["surfaces/apps/console","surfaces/libs/console-core"]}`)
	lib := filepath.Join(root, "surfaces", "libs", "console-core", "package.json")
	writeProbeFile(t, lib, `{"name":"@putnami/console-core","version":"1.0.0"}`)
	app := filepath.Join(root, "surfaces", "apps", "console", "package.json")
	writeProbeFile(t, app,
		`{"name":"@putnami/console","dependencies":{"@putnami/console-core":"workspace:*"}}`)

	before := readTextFile(t, lib)
	changes, err := syncTypeScriptWorkspace(root, []pctx.ProjectRef{
		{Name: "surfaces/libs/console-core", SourceName: "surfaces/libs/console-core", Path: "surfaces/libs/console-core"},
		{Name: "surfaces/apps/console", SourceName: "surfaces/apps/console", Path: "surfaces/apps/console"},
	}, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none: no project's declared identity diverges from its resolved name", changes)
	}
	if after := readTextFile(t, lib); after != before {
		t.Fatalf("the library manifest was rewritten:\n%s", after)
	}

	// The invariant the rename actually breaks: the dependent's `workspace:*`
	// key must still name the package the library publishes.
	var libName string
	if err := json.Unmarshal(readJSONField(t, lib, "name"), &libName); err != nil {
		t.Fatal(err)
	}
	var dependencies map[string]string
	if err := json.Unmarshal(readJSONField(t, app, "dependencies"), &dependencies); err != nil {
		t.Fatal(err)
	}
	if dependencies[libName] != "workspace:*" {
		t.Errorf("dependencies %v no longer resolve the library's package name %q", dependencies, libName)
	}
}

// The absent-SourceName arm: an orchestrator older than the protocol member
// sends no declared identity, and the task must DECLINE the rename. Not
// renaming leaves a manifest a later sync can fix; renaming cannot be undone
// from here, and it is the failure that breaks every `workspace:*` reference.
func TestSyncTypeScriptWorkspace_DeclinesTheRenameWithoutASourceName(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"), `{"name":"repo","workspaces":["libs/core"]}`)
	manifest := filepath.Join(root, "libs", "core", "package.json")
	writeProbeFile(t, manifest, `{"name":"@acme/core"}`)
	before := readTextFile(t, manifest)

	changes, err := syncTypeScriptWorkspace(root, []pctx.ProjectRef{{Name: "libs/core", Path: "libs/core"}}, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none: an unknown declared identity must not authorize a rename", changes)
	}
	if after := readTextFile(t, manifest); after != before {
		t.Fatalf("a manifest was renamed on an absent sourceName:\n%s", after)
	}
}

// A run that changes nothing writes nothing. `projects sync` runs on every
// membership change, so a rewrite-always sync would move file mtimes — and
// every downstream cache key that observes them — on every invocation.
func TestSyncTypeScriptWorkspace_IsIdempotentAndDoesNotTouchUnchangedFiles(t *testing.T) {
	root := syncFixtureTree(t)
	if _, err := syncTypeScriptWorkspace(root, syncSelection(), false); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	pinned := time.Unix(1_700_000_000, 0)
	touched := []string{
		filepath.Join(root, "package.json"),
		filepath.Join(root, "apps", "web", "package.json"),
		filepath.Join(root, "libs", "core", "package.json"),
	}
	for _, path := range touched {
		if err := os.Chtimes(path, pinned, pinned); err != nil {
			t.Fatal(err)
		}
	}

	changes, err := syncTypeScriptWorkspace(root, syncSelection(), false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("a converged tree reported changes: %+v", changes)
	}
	for _, path := range touched {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(pinned) {
			t.Errorf("%s was rewritten by a no-op sync", path)
		}
	}
}

// Dry-run reports exactly what a real run would change and writes nothing.
func TestSyncTypeScriptWorkspace_DryRunReportsWithoutWriting(t *testing.T) {
	root := syncFixtureTree(t)
	before, err := os.ReadFile(filepath.Join(root, "apps", "web", "package.json"))
	if err != nil {
		t.Fatal(err)
	}

	changes, err := syncTypeScriptWorkspace(root, syncSelection(), true)
	if err != nil {
		t.Fatalf("dry-run sync: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("dry-run changes = %+v, want the same two a real run makes", changes)
	}
	after, err := os.ReadFile(filepath.Join(root, "apps", "web", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("dry-run wrote a manifest:\n%s", after)
	}
}

// A workspace with no root package.json is left alone: creating one would give
// a Go-only or Python-only workspace an npm identity nobody asked for.
func TestSyncTypeScriptWorkspace_NoRootManifestIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "svc", "go.mod"), "module acme/svc\n")

	changes, err := syncTypeScriptWorkspace(root, []pctx.ProjectRef{{Name: "acme/svc", Path: "svc"}}, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %+v, want none", changes)
	}
	if _, err := os.Stat(filepath.Join(root, "package.json")); !os.IsNotExist(err) {
		t.Fatalf("sync created a root package.json (stat err = %v)", err)
	}
}

// The object form of `workspaces` is a real npm/yarn spelling; it must be read,
// not rejected, or a workspace that configured `nohoist` would fail the sync.
//
// This case seeds a membership that ALREADY matches, so it pins only the no-op
// path (nothing is written, so nothing can be lost). The rewrite path — where
// the loss was actually reachable — is pinned by
// TestSyncTypeScriptWorkspace_RewritesTheObjectWorkspacesFormAsAnObject.
func TestSyncTypeScriptWorkspace_ReadsTheObjectWorkspacesForm(t *testing.T) {
	root := syncFixtureTree(t)
	writeProbeFile(t, filepath.Join(root, "package.json"),
		`{"name":"repo","workspaces":{"packages":["apps/web","libs/core"],"nohoist":["**/react"]}}`)

	changes, err := syncTypeScriptWorkspace(root, syncSelection(), false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	// The desired set already matches, so `workspaces` must not be rewritten —
	// which is also what preserves the nohoist configuration.
	for _, change := range changes {
		if change.Field == "workspaces" {
			t.Fatalf("an already-correct object form was rewritten: %+v", change)
		}
	}
	if got := string(readJSONField(t, filepath.Join(root, "package.json"), "workspaces")); !slices.Contains(
		[]string{`{"packages":["apps/web","libs/core"],"nohoist":["**/react"]}`}, got) {
		t.Errorf("workspaces = %s, want the object form preserved verbatim", got)
	}
}

// REGRESSION: a membership CHANGE through the object form used to flatten the
// whole member to a bare array, deleting `nohoist` (and every other sibling
// member) as a side effect of adding a project. The change report said only
// "1 entries" → "2 entries", so the hoisting change left no trace anywhere.
//
// The seeded membership deliberately DISAGREES with the resolved selection, so
// the writer actually runs; the earlier object-form test returns at the
// no-change guard and never reaches it.
func TestSyncTypeScriptWorkspace_RewritesTheObjectWorkspacesFormAsAnObject(t *testing.T) {
	root := syncFixtureTree(t)
	writeProbeFile(t, filepath.Join(root, "package.json"),
		`{"name":"repo","workspaces":{"packages":["libs/core"],"nohoist":["**/react-native"],`+
			`"catalog":{"react":"^19.0.0"}},"private":true}`)

	changes, err := syncTypeScriptWorkspace(root, syncSelection(), false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !slices.ContainsFunc(changes, func(c workspaceSyncChange) bool { return c.Field == "workspaces" }) {
		t.Fatalf("changes = %+v, want the membership rewrite", changes)
	}

	raw := readJSONField(t, filepath.Join(root, "package.json"), "workspaces")
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("workspaces was not written back as an object: %s", raw)
	}

	var packages []string
	if err := json.Unmarshal(object["packages"], &packages); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(packages, []string{"apps/web", "libs/core"}) {
		t.Errorf("workspaces.packages = %v, want [apps/web libs/core]", packages)
	}
	// The sibling members are the whole point: they are bun/yarn hoisting and
	// catalog policy this task has no opinion about and must not touch.
	if got := compactJSON(t, object["nohoist"]); got != `["**/react-native"]` {
		t.Errorf("workspaces.nohoist = %s, want it preserved verbatim", got)
	}
	if got := compactJSON(t, object["catalog"]); got != `{"react":"^19.0.0"}` {
		t.Errorf("workspaces.catalog = %s, want it preserved verbatim", got)
	}

	// Authored key order survives too, or every sync produces a diff that buries
	// the one line that actually changed (applied to the nested object).
	order, err := topLevelKeyOrder(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"packages", "nohoist", "catalog"}) {
		t.Errorf("workspaces key order = %v, want the authored order preserved", order)
	}
}

// An object form with no `packages` member yet still gains one, and keeps the
// members it did author: reading it as "no membership" and writing an array
// back would delete them.
func TestSyncTypeScriptWorkspace_AddsPackagesToAnObjectFormThatLacksIt(t *testing.T) {
	root := syncFixtureTree(t)
	writeProbeFile(t, filepath.Join(root, "package.json"),
		`{"name":"repo","workspaces":{"nohoist":["**/react-native"]}}`)

	if _, err := syncTypeScriptWorkspace(root, syncSelection(), false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	raw := readJSONField(t, filepath.Join(root, "package.json"), "workspaces")
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("workspaces was not written back as an object: %s", raw)
	}
	if got := compactJSON(t, object["nohoist"]); got != `["**/react-native"]` {
		t.Errorf("workspaces.nohoist = %s, want it preserved", got)
	}
	var packages []string
	if err := json.Unmarshal(object["packages"], &packages); err != nil {
		t.Fatalf("workspaces.packages was not seeded: %s", raw)
	}
	if !slices.Equal(packages, []string{"apps/web", "libs/core"}) {
		t.Errorf("workspaces.packages = %v, want [apps/web libs/core]", packages)
	}
}

// The array form stays an array. Promoting it to the object form would be a
// gratuitous rewrite of a manifest the author chose the shape of.
func TestSyncTypeScriptWorkspace_KeepsTheArrayFormAnArray(t *testing.T) {
	root := syncFixtureTree(t)

	if _, err := syncTypeScriptWorkspace(root, syncSelection(), false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	raw := readJSONField(t, filepath.Join(root, "package.json"), "workspaces")
	var packages []string
	if err := json.Unmarshal(raw, &packages); err != nil {
		t.Fatalf("the array form was not written back as an array: %s", raw)
	}
	if !slices.Equal(packages, []string{"apps/web", "libs/core"}) {
		t.Errorf("workspaces = %v, want [apps/web libs/core]", packages)
	}
}

// REGRESSION (found end-to-end while landing the sync change): a workspace-sync that ran
// without the resolved selection rewrote the root `workspaces` array to an
// empty list, deleting the workspace's membership. The task now fails closed,
// and the manifest declares the activation that delivers the selection — both
// halves are pinned, because either one alone leaves the data-loss reachable.
func TestRunWorkspaceSync_FailsClosedWithoutASelection(t *testing.T) {
	root := syncFixtureTree(t)
	before := readTextFile(t, filepath.Join(root, "package.json"))

	ctx := &pctx.Context{WorkspaceRoot: root}
	status, _, err := runWorkspaceSync(ctx, jsonl.New(), nil)
	if err == nil || status != "FAILED" {
		t.Fatalf("runWorkspaceSync = (%q, %v), want a loud failure on an unknown membership", status, err)
	}
	if after := readTextFile(t, filepath.Join(root, "package.json")); after != before {
		t.Fatalf("the failing task still wrote the root manifest:\n%s", after)
	}
}

func TestManifest_WorkspaceSyncRunsOncePerWorkspace(t *testing.T) {
	m := loadExtensionManifest(t)

	command, ok := m.Commands["workspace-sync"]
	if !ok {
		t.Fatal(`manifest lost the "workspace-sync" command`)
	}
	// Without workspace-once activation the command is planned per project, the
	// planner attaches no resolved selection, and the task sees an empty
	// membership — the exact shape that emptied the workspaces array.
	if command.Activation != "workspace-once" {
		t.Fatalf(`workspace-sync activation = %q, want "workspace-once": the task needs the resolved `+
			`project selection, which the planner only attaches to workspace-once jobs`, command.Activation)
	}
	if m.Workspace == nil || m.Workspace.SyncTask == "" {
		t.Fatal("the workspace adapter no longer names a syncTask; core would keep writing package.json itself")
	}
	if _, ok := m.Tasks[m.Workspace.SyncTask]; !ok {
		t.Fatalf("workspace.syncTask %q is not defined in tasks", m.Workspace.SyncTask)
	}
}

// Top-level key order is preserved: a sync that reordered package.json would
// produce a diff on every run and bury the one line that actually changed.
func TestSyncTypeScriptWorkspace_PreservesManifestKeyOrder(t *testing.T) {
	root := syncFixtureTree(t)
	writeProbeFile(t, filepath.Join(root, "apps", "web", "package.json"),
		`{"private":true,"name":"old-web","version":"1.0.0","scripts":{"dev":"bun run"}}`)

	if _, err := syncTypeScriptWorkspace(root, syncSelection(), false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "apps", "web", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	order, err := topLevelKeyOrder(data)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"private", "name", "version", "scripts"}) {
		t.Fatalf("key order = %v, want the authored order preserved", order)
	}
}
