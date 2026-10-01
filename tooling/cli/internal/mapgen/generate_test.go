package mapgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func readWorkspaceFile(t *testing.T, wsRoot, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wsRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// TestGenerate_WriteThenRegenerateIsByteIdentical is the determinism acceptance
// test: generating twice over an unchanged tree produces identical bytes, and
// the second run rewrites nothing. It also proves the reduce does not read its
// own previous output — the second run reports "clean" because the render
// matched, not because it patched.
func TestGenerate_WriteThenRegenerateIsByteIdentical(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)
	selected := []*workspace.Project{api, lib}

	first, err := Generate(ws, selected)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if first.Outcome != OutcomeUpdated {
		t.Fatalf("first run outcome = %q, want %q", first.Outcome, OutcomeUpdated)
	}
	if len(first.Outputs) != 2 {
		t.Fatalf("first run wrote %v, want both documents", first.Outputs)
	}
	if first.Projects != 2 {
		t.Errorf("projects = %d, want 2", first.Projects)
	}
	if len(first.FragmentsWritten) != 2 {
		t.Errorf("fragments written = %v, want one per selected project", first.FragmentsWritten)
	}

	wantJSON := readWorkspaceFile(t, ws.Root, JSONPath)
	wantMD := readWorkspaceFile(t, ws.Root, MarkdownPath)

	second, err := Generate(ws, selected)
	if err != nil {
		t.Fatalf("Generate second run: %v", err)
	}
	if second.Outcome != OutcomeClean || len(second.Outputs) != 0 {
		t.Errorf("second run over an unchanged tree = %q %v, want clean with no writes", second.Outcome, second.Outputs)
	}
	if got := readWorkspaceFile(t, ws.Root, JSONPath); got != wantJSON {
		t.Errorf("repo-map.json is not byte-identical across runs:\n--- first:\n%s\n--- second:\n%s", wantJSON, got)
	}
	if got := readWorkspaceFile(t, ws.Root, MarkdownPath); got != wantMD {
		t.Errorf("repo-map.md is not byte-identical across runs")
	}
}

// TestGenerate_EmitsOnlyEphemeralState pins the placement decision from
// both sides: every byte this feature writes lands under the workspace's own
// gitignored .putnami state directory — never in a project's .gen, which the Go
// and TypeScript extensions declare as a
// task-owned directory output whose bytes the scheduler captures wholesale.
func TestGenerate_EmitsOnlyEphemeralState(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)
	if _, err := Generate(ws, []*workspace.Project{api, lib}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	for _, rel := range []string{JSONPath, MarkdownPath} {
		if !strings.HasPrefix(rel, FragmentDir+"/") {
			t.Errorf("document path %q is not under %q", rel, FragmentDir)
		}
		if _, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("document %s was not written: %v", rel, err)
		}
	}
	for _, p := range []*workspace.Project{api, lib} {
		rel := FragmentPath(p.Path)
		if !strings.HasPrefix(rel, FragmentDir+"/") {
			t.Errorf("fragment path %q is not under %q", rel, FragmentDir)
		}
		if _, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("fragment %s was not written: %v", rel, err)
		}
		if _, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(p.Path), ".gen")); !os.IsNotExist(err) {
			t.Errorf("a fragment leaked into %s/.gen (stat err = %v) — that directory is a declared task output", p.Path, err)
		}
	}
}

// TestGenerate_SelectionNeverNarrowsTheMap is the structural answer to the
// "wrong graph position" problem: a run that refreshes ONE project's fragment
// still renders every live project, because the reduce enumerates the live set
// and validates the fragments it did not refresh.
func TestGenerate_SelectionNeverNarrowsTheMap(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)
	if _, err := Generate(ws, []*workspace.Project{api, lib}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Edit a project OUTSIDE the selection. Its fragment's inputs digest no
	// longer matches, so the reduce rebuilds it in memory and the map still
	// updates — which is exactly the failure mode a per-project freshness test
	// cannot catch.
	writeFile(t, ws.Root, "svc/lib/README.md", "# Lib\n\nA shared library.\n")
	report, err := Generate(ws, []*workspace.Project{api})
	if err != nil {
		t.Fatalf("Generate(api only): %v", err)
	}
	if report.Projects != 2 {
		t.Errorf("projects = %d, want 2 — a narrowed selection narrowed the map", report.Projects)
	}
	if report.FragmentsReused != 0 || report.FragmentsBuilt != 2 {
		t.Errorf("reused %d / built %d, want 0 / 2 (the edited project's fragment must be invalidated)",
			report.FragmentsReused, report.FragmentsBuilt)
	}
	if len(report.FragmentsWritten) != 1 || !strings.Contains(report.FragmentsWritten[0], "svc/api") {
		t.Errorf("fragments written = %v, want only the selected project's", report.FragmentsWritten)
	}
	if got := readWorkspaceFile(t, ws.Root, JSONPath); !strings.Contains(got, "A shared library.") {
		t.Error("the map did not pick up the unselected project's README change")
	}

	// The full-selection render agrees byte-for-byte with the narrow one: a
	// regeneration from scratch always agrees, which is what makes the artifact
	// trustable.
	narrow := readWorkspaceFile(t, ws.Root, JSONPath)
	if _, err := Generate(ws, []*workspace.Project{api, lib}); err != nil {
		t.Fatalf("Generate(all): %v", err)
	}
	if got := readWorkspaceFile(t, ws.Root, JSONPath); got != narrow {
		t.Error("a narrow-selection render and a full render disagree")
	}
}

// TestGenerate_IgnoresASelectionOutsideTheLiveSet keeps a stale or foreign
// selector from putting a non-project into the map.
func TestGenerate_IgnoresASelectionOutsideTheLiveSet(t *testing.T) {
	ws, api, _ := fixtureWorkspace(t)
	ghost := &workspace.Project{ID: "/svc/ghost", Name: "example/ghost", Path: "svc/ghost"}

	report, err := Generate(ws, []*workspace.Project{api, ghost})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if report.Projects != 2 {
		t.Errorf("projects = %d, want the 2 live ones", report.Projects)
	}
	if got := readWorkspaceFile(t, ws.Root, JSONPath); strings.Contains(got, "/svc/ghost") {
		t.Error("a selection outside the live set reached the map")
	}
	if _, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(FragmentPath(ghost.Path)))); !os.IsNotExist(err) {
		t.Error("a fragment was written for a project outside the live set")
	}
}

// TestResolveMap_MatchesTheWrittenDocumentWithoutTouchingDisk pins the contract
// the MCP tool and `context map --print` both stand on: the in-memory reduce
// renders the same bytes a write renders, reusing validated fragments, and
// writes nothing at all.
func TestResolveMap_MatchesTheWrittenDocumentWithoutTouchingDisk(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)
	if _, err := Generate(ws, []*workspace.Project{api, lib}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	written := readWorkspaceFile(t, ws.Root, JSONPath)

	resolved, err := ResolveMap(ws)
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	if resolved.FragmentsReused != 2 || resolved.FragmentsBuilt != 0 {
		t.Errorf("reused %d / built %d, want 2 / 0 — persisted fragments were not validated-and-reused",
			resolved.FragmentsReused, resolved.FragmentsBuilt)
	}
	data, err := RenderJSON(resolved.Map)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if string(data) != written {
		t.Errorf("the in-memory render is not byte-identical to the written document:\n--- written:\n%s\n--- resolved:\n%s",
			written, data)
	}

	// An edit no build has picked up yet must show up in the in-memory render
	// while the on-disk document stays untouched — the whole point of serving
	// the map fresh at call time.
	writeFile(t, ws.Root, "svc/lib/README.md", "# Lib\n\nA shared library.\n")
	next, err := ResolveMap(ws)
	if err != nil {
		t.Fatalf("ResolveMap after an edit: %v", err)
	}
	fresh, err := RenderJSON(next.Map)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !strings.Contains(string(fresh), "A shared library.") {
		t.Error("the in-memory render did not pick up an uncommitted working-tree edit")
	}
	if next.FragmentsBuilt != 1 || next.FragmentsReused != 1 {
		t.Errorf("built %d / reused %d, want 1 / 1 — the reduce must be O(changed)",
			next.FragmentsBuilt, next.FragmentsReused)
	}
	if got := readWorkspaceFile(t, ws.Root, JSONPath); got != written {
		t.Error("ResolveMap rewrote the on-disk document")
	}
}

// TestResolveMap_ColdWorkspaceNeedsNoFragments proves the tool works on a clone
// where nothing has ever been built: with no persisted fragments the reduce
// rebuilds every one in memory and still writes nothing.
func TestResolveMap_ColdWorkspaceNeedsNoFragments(t *testing.T) {
	ws, _, _ := fixtureWorkspace(t)

	resolved, err := ResolveMap(ws)
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	if resolved.FragmentsBuilt != 2 || resolved.FragmentsReused != 0 {
		t.Errorf("built %d / reused %d, want 2 / 0 on a cold workspace",
			resolved.FragmentsBuilt, resolved.FragmentsReused)
	}
	if len(resolved.Map.Projects) != 2 {
		t.Errorf("projects = %d, want 2", len(resolved.Map.Projects))
	}
	if _, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(FragmentDir))); !os.IsNotExist(err) {
		t.Errorf("ResolveMap created %s (stat err = %v) — it must write nothing", FragmentDir, err)
	}
}

// TestResolveMode_PrecedenceOrder pins the mode ladder the build attachment
// reads: the explicit override wins, CI resolves to OFF (nothing is committed,
// so a runner has nothing to keep honest and no map consumer), and everything
// else writes. An unrecognized override falls through rather than failing — a
// typo in an environment variable must not brick every build in a shell.
func TestResolveMode_PrecedenceOrder(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want Mode
	}{
		{"default", nil, ModeWrite},
		{"ci is off", map[string]string{CIEnv: "true"}, ModeOff},
		{"ci blank is not ci", map[string]string{CIEnv: "  "}, ModeWrite},
		{"override beats ci", map[string]string{CIEnv: "true", ModeEnv: "write"}, ModeWrite},
		{"override off", map[string]string{ModeEnv: "off"}, ModeOff},
		{"override is case-insensitive", map[string]string{ModeEnv: "WRITE"}, ModeWrite},
		{"typo falls through to the ci default", map[string]string{CIEnv: "1", ModeEnv: "yes"}, ModeOff},
		{"typo falls through to write", map[string]string{ModeEnv: "yes"}, ModeWrite},
		{"retired check value is not a mode", map[string]string{ModeEnv: "check"}, ModeWrite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string { return tc.env[key] }
			if got := ResolveMode(getenv); got != tc.want {
				t.Errorf("ResolveMode(%v) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// TestGenerate_ReportsAnUnwritableStateDirectory pins the failure surface the
// engine turns into a warning: an unusable .putnami state directory is an error
// from Generate, not a silently skipped write.
func TestGenerate_ReportsAnUnwritableStateDirectory(t *testing.T) {
	ws, api, lib := fixtureWorkspace(t)
	// A regular file where the state directory belongs: every MkdirAll under it
	// fails, which is the portable stand-in for an unwritable .putnami.
	writeFile(t, ws.Root, FragmentDir, "not a directory\n")

	if _, err := Generate(ws, []*workspace.Project{api, lib}); err == nil {
		t.Fatal("an unwritable state directory did not fail generation")
	}
}

// TestGenerate_RendersAWorkspaceWithNoProjects keeps the empty case total: the
// reduce still emits both documents rather than skipping them.
func TestGenerate_RendersAWorkspaceWithNoProjects(t *testing.T) {
	root := t.TempDir()
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "empty"}, nil)

	report, err := Generate(ws, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if report.Projects != 0 || report.Outcome != OutcomeUpdated {
		t.Fatalf("report = %+v, want an updated render of zero projects", report)
	}
	if !strings.Contains(readWorkspaceFile(t, root, MarkdownPath), "_No projects._") {
		t.Error("the empty map does not say it is empty")
	}
}
