package agentctx

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	gitexec "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/mapgen"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// mapCommandWorkspace writes a real two-project workspace on disk (workspace
// manifest included) so ContextMapCommand can load it the way the CLI does.
func mapCommandWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("putnami.workspace.json", `{"name":"example","version":"0.1.0","includes":["svc/api","svc/lib"]}`)
	write("svc/api/putnami.json", `{"name":"example/api","description":"The API","dependencies":["example/lib"]}`)
	write("svc/api/README.md", "# API\n\nServes tasks.\n")
	write("svc/api/schema/openapi.json",
		`{"openapi":"3.1.0","paths":{"/tasks":{"get":{"operationId":"listTasks","summary":"List tasks"}}}}`)
	write("svc/api/schema/config.jsonschema.json",
		`{"type":"object","properties":{"server":{"type":"object","properties":{"port":{"type":"integer"}}}}}`)
	write("svc/lib/putnami.json", `{"name":"example/lib"}`)
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root
}

// treeOutsideState lists every file of the workspace outside the .putnami
// state directory and .git, so a test can prove a command created, removed or
// renamed no workspace file.
func treeOutsideState(t *testing.T, root string) map[string]bool {
	t.Helper()
	files := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() && (rel == ".putnami" || rel == ".git") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			files[rel] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

// assertNoMapState fails when anything the deleted map and pack writers used to
// produce exists anywhere under root, the .putnami state directory included.
func assertNoMapState(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		switch {
		case d.IsDir() && (rel == ".putnami/context-map" || d.Name() == ".gen"):
			t.Errorf("a context command created the directory %s", rel)
		case !d.IsDir() && (d.Name() == "repo-map.json" || d.Name() == "repo-map.md" ||
			d.Name() == "map-fragment.json" || d.Name() == "agent-context.json"):
			t.Errorf("a context command wrote %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// printContextMap runs the command and returns what it printed.
func printContextMap(t *testing.T, root string, opts ContextMapOptions) string {
	t.Helper()
	out, err := iox.CaptureStdout(func() error { return ContextMapCommand(root, opts) })
	if err != nil {
		t.Fatalf("context map %+v: %v", opts, err)
	}
	return out
}

// printedMapDocument decodes the map document a structured run printed as its
// result data.
func printedMapDocument(t *testing.T, out string) (mapgen.RepoMap, json.RawMessage) {
	t.Helper()
	var envelope struct {
		Command string          `json:"command"`
		Status  string          `json:"status"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode the result envelope: %v\n%s", err, out)
	}
	if envelope.Command != "context map" || envelope.Status != "success" {
		t.Fatalf("envelope = %s / %s, want a context map success", envelope.Command, envelope.Status)
	}
	var document mapgen.RepoMap
	if err := json.Unmarshal(envelope.Data, &document); err != nil {
		t.Fatalf("decode the map document: %v", err)
	}
	return document, envelope.Data
}

func printedProjectIDs(document mapgen.RepoMap) []string {
	ids := make([]string, 0, len(document.Projects))
	for _, entry := range document.Projects {
		ids = append(ids, entry.ID)
	}
	return ids
}

// TestContextMapCommand_PrintsAndWritesNothing is the end-to-end contract: the
// command prints the map, Markdown by default and the document under
// --output=json, and not one byte reaches the disk.
func TestContextMapCommand_PrintsAndWritesNothing(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "printing-the-map-writes-nothing")
	spectest.Proves(t, "cli/context-mcp-discovery", "build-writes-no-workspace-map", "context-map-writes-no-file")
	root := mapCommandWorkspace(t)
	before := treeOutsideState(t, root)

	markdown := printContextMap(t, root, ContextMapOptions{})
	for _, want := range []string{"# Repo Map — example", "## Projects", "svc/api", "svc/lib"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("the default output is not the Markdown map (%q missing):\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, `"schemaVersion"`) {
		t.Errorf("the default output carries JSON:\n%s", markdown)
	}

	for _, format := range []string{"jsonl", "json"} {
		document, _ := printedMapDocument(t, printContextMap(t, root, ContextMapOptions{OutputFormat: format}))
		if document.SchemaVersion != mapgen.SchemaVersion || len(document.Projects) != 2 {
			t.Errorf("--output=%s data = %+v, want the whole map document", format, document)
		}
	}

	assertNoMapState(t, root)
	if after := treeOutsideState(t, root); !maps.Equal(before, after) {
		t.Errorf("context map changed the workspace tree:\nbefore: %v\nafter:  %v", before, after)
	}
}

// TestContextMapCommand_JSONDataIsTheMCPDocument keeps the two surfaces honest
// against each other: the data `--output=json` prints is exactly the document
// the MCP workspace_map tool serves, byte for byte once compacted.
func TestContextMapCommand_JSONDataIsTheMCPDocument(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "the-printed-json-is-the-mcp-document")
	root := mapCommandWorkspace(t)

	_, data := printedMapDocument(t, printContextMap(t, root, ContextMapOptions{OutputFormat: "jsonl"}))
	served, err := BuildWorkspaceMapResult(root, "", "", nil)
	if err != nil {
		t.Fatalf("workspace_map: %v", err)
	}
	want, err := json.Marshal(served.Document)
	if err != nil {
		t.Fatalf("encode the served document: %v", err)
	}
	if string(data) != string(want) {
		t.Errorf("the printed data and the MCP document disagree:\n--- printed:\n%s\n--- served:\n%s", data, want)
	}

	// The same holds for one project: the CLI selection and the MCP project
	// argument narrow through the same scope.
	_, narrowed := printedMapDocument(t, printContextMap(t, root, ContextMapOptions{Projects: "example/api", OutputFormat: "jsonl"}))
	servedOne, err := BuildWorkspaceMapResult(root, "", "example/api", nil)
	if err != nil {
		t.Fatalf("workspace_map example/api: %v", err)
	}
	if want, err = json.Marshal(servedOne.Document); err != nil {
		t.Fatalf("encode the served document: %v", err)
	}
	if string(narrowed) != string(want) {
		t.Errorf("the narrowed printed data and the MCP project answer disagree:\n--- printed:\n%s\n--- served:\n%s", narrowed, want)
	}

	markdown := printContextMap(t, root, ContextMapOptions{})
	if markdown != string(mapgen.RenderMarkdown(served.Document)) {
		t.Error("the printed Markdown is not the rendering of the MCP document")
	}
}

// TestContextMapCommand_ProjectsNarrowThePrintedMap pins --projects: the printed
// map keeps only the selected projects, each entry keeps its complete edges
// because the reduce still covers the live set, and the docs index stays.
func TestContextMapCommand_ProjectsNarrowThePrintedMap(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "a-selection-narrows-the-printed-map")
	root := mapCommandWorkspace(t)
	writeMapFixtureFile(t, root, "docs/guide.md", "# Guide\n")

	document, _ := printedMapDocument(t, printContextMap(t, root, ContextMapOptions{Projects: "example/lib", OutputFormat: "jsonl"}))
	if ids := printedProjectIDs(document); len(ids) != 1 || ids[0] != "/svc/lib" {
		t.Fatalf("printed projects = %v, want only /svc/lib", ids)
	}
	if got := document.Projects[0].Dependents; len(got) != 1 || got[0] != "/svc/api" {
		t.Errorf("lib dependents = %v, want /svc/api: an unselected project still contributes its edges", got)
	}
	if len(document.Docs) == 0 {
		t.Error("narrowing dropped the workspace docs index")
	}

	markdown := printContextMap(t, root, ContextMapOptions{Projects: "example/lib"})
	if !strings.Contains(markdown, "`svc/lib`") || strings.Contains(markdown, "| `svc/api` |") {
		t.Errorf("the narrowed Markdown does not list only svc/lib:\n%s", markdown)
	}
}

// TestContextMapCommand_ImpactedNarrowsThePrintedMap pins --impacted: an edit
// to one project prints only the projects it impacts, and an empty impacted set
// prints a map with no project rather than failing.
func TestContextMapCommand_ImpactedNarrowsThePrintedMap(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "a-selection-narrows-the-printed-map")
	root := mapCommandWorkspace(t)
	runGit := func(args ...string) {
		t.Helper()
		if output, err := exec.Command(gitexec.Executable(), append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	writeMapFixtureFile(t, root, ".gitignore", ".putnami/\n")
	runGit("init", "--quiet")
	runGit("add", "-A")
	runGit("-c", "user.name=Putnami Tests", "-c", "user.email=tests@putnami.dev", "-c", "commit.gpgsign=false",
		"commit", "--quiet", "-m", "fixture")

	empty, _ := printedMapDocument(t, printContextMap(t, root, ContextMapOptions{Impacted: true, Baseline: "HEAD", OutputFormat: "jsonl"}))
	if ids := printedProjectIDs(empty); len(ids) != 0 {
		t.Errorf("an unchanged tree printed projects %v, want none", ids)
	}
	if empty.Projects == nil {
		t.Error("an empty selection printed projects as null, want []")
	}

	writeMapFixtureFile(t, root, "svc/api/README.md", "# API\n\nServes tasks and notes.\n")
	workspace.InvalidateLoadCache(root)
	document, _ := printedMapDocument(t, printContextMap(t, root, ContextMapOptions{Impacted: true, Baseline: "HEAD", OutputFormat: "jsonl"}))
	if ids := printedProjectIDs(document); len(ids) != 1 || ids[0] != "/svc/api" {
		t.Errorf("printed projects = %v, want only the edited /svc/api", ids)
	}
	assertNoMapState(t, root)
}

func writeMapFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestContextMapCommand_RefusesADegradedIdentity pins the guard that keeps the
// command and the MCP tool from disagreeing: a workspace that declares
// extensions but has adopted no provider view resolves every project from
// authored config alone (no provider-derived dependency edges), so the map would
// render without edges an agent would then trust.
func TestContextMapCommand_RefusesADegradedIdentity(t *testing.T) {
	root := mapCommandWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"),
		[]byte(`{"name":"example","version":"0.1.0","includes":["svc/api","svc/lib"],"extensions":["@putnami/go"]}`),
		0o644); err != nil {
		t.Fatalf("rewrite workspace manifest: %v", err)
	}
	workspace.InvalidateLoadCache(root)

	for _, format := range []string{"", "jsonl"} {
		out, err := iox.CaptureStdout(func() error {
			return ContextMapCommand(root, ContextMapOptions{OutputFormat: format})
		})
		if err == nil || !errors.Is(err, protocolcli.ErrInvalidConfig) {
			t.Fatalf("context map (output %q) error = %v, want ErrInvalidConfig", format, err)
		}
		if !strings.Contains(err.Error(), "projects sync") {
			t.Errorf("error does not name the repair command: %v", err)
		}
		if out != "" {
			t.Errorf("a refused run printed a map:\n%s", out)
		}
	}
}

// TestContextMapCommand_UnknownSelectorIsNotFound keeps a typo'd selector from
// silently mapping the whole workspace.
func TestContextMapCommand_UnknownSelectorIsNotFound(t *testing.T) {
	root := mapCommandWorkspace(t)
	err := ContextMapCommand(root, ContextMapOptions{Projects: "example/nope"})
	if err == nil || !errors.Is(err, cmderr.ErrNotFound) {
		t.Fatalf("unknown selector error = %v, want ErrNotFound", err)
	}
}

// TestContextMapCommand_RefusesAStaleProviderView closes the second half of the
// identity gate: an ADOPTED view is not enough, because workspace.Load restores
// the recorded snapshot after structural validation only, never after checking
// its input digests. Without the staleness check, editing workspace metadata
// and running `putnami context map` directly renders the PREVIOUS tree's
// identities and dependency edges — a confident wrong answer.
func TestContextMapCommand_RefusesAStaleProviderView(t *testing.T) {
	root := mapCommandWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"),
		[]byte(`{"name":"example","version":"0.1.0","includes":["svc/api","svc/lib"],"extensions":["@putnami/go"]}`),
		0o644); err != nil {
		t.Fatalf("rewrite workspace manifest: %v", err)
	}
	workspace.InvalidateLoadCache(root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}

	// Record a provider view over the CURRENT tree, the way a probing run does.
	result := wsproto.ProbeResult{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: "@putnami/go",
		Projects:  []wsproto.ProbeProject{{Path: "svc/api", SourceName: "example/api"}},
	}
	wsproto.NormalizeProbeResult(&result)
	snapshot := workspace.NewSnapshotWithProviders(ws, ws.ProbeDigest(), []workspace.SnapshotProvider{{
		Extension: result.Extension,
		Digest:    wsproto.ProbeResultDigest(result),
		Result:    result,
	}})
	if err := workspace.WriteSnapshot(root, snapshot, workspace.SnapshotWritePolicy{}); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	workspace.InvalidateLoadCache(root)

	// Fresh view: the map renders.
	printContextMap(t, root, ContextMapOptions{})

	// Change a recorded metadata input WITHOUT re-probing — the exact state a
	// direct `context map` used to trust.
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"),
		[]byte(`{"name":"example","version":"0.2.0","includes":["svc/api","svc/lib"],"extensions":["@putnami/go"]}`),
		0o644); err != nil {
		t.Fatalf("edit workspace manifest: %v", err)
	}
	workspace.InvalidateLoadCache(root)

	err = ContextMapCommand(root, ContextMapOptions{})
	if err == nil || !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("context map over a stale view error = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "stale") || !strings.Contains(err.Error(), "projects sync") {
		t.Errorf("refusal does not name the staleness and its repair: %v", err)
	}
}
