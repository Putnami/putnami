package agentctx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
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

// TestContextMapCommand_WritesOnlyEphemeralState is the end-to-end contract: the
// command writes both documents under the gitignored .putnami state directory,
// and a second run over an unchanged tree rewrites nothing.
func TestContextMapCommand_WritesOnlyEphemeralState(t *testing.T) {
	root := mapCommandWorkspace(t)

	if err := ContextMapCommand(root, ContextMapOptions{}); err != nil {
		t.Fatalf("context map: %v", err)
	}
	for _, rel := range []string{mapgen.JSONPath, mapgen.MarkdownPath} {
		if !strings.HasPrefix(rel, mapgen.FragmentDir+"/") {
			t.Fatalf("document %q escaped the ephemeral state directory", rel)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(data), "svc/api") {
			t.Errorf("%s does not mention the api project", rel)
		}
	}

	before, err := os.Stat(filepath.Join(root, filepath.FromSlash(mapgen.JSONPath)))
	if err != nil {
		t.Fatalf("stat map: %v", err)
	}
	if err := ContextMapCommand(root, ContextMapOptions{}); err != nil {
		t.Fatalf("second context map: %v", err)
	}
	after, err := os.Stat(filepath.Join(root, filepath.FromSlash(mapgen.JSONPath)))
	if err != nil {
		t.Fatalf("stat map: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a repeat run rewrote an unchanged map")
	}
}

// TestContextMapCommand_PrintWritesNothing pins `--print`: the document goes to
// stdout, in the requested format, and not one byte reaches the disk — including
// on a workspace where the map has never been generated.
func TestContextMapCommand_PrintWritesNothing(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "printing-the-map-writes-nothing")
	for _, tc := range []struct {
		name    string
		args    []string
		wants   string
		unwants string
	}{
		{"bare print is markdown", []string{"--print"}, "# Repo Map — example", `"schemaVersion"`},
		{"explicit md", []string{"--print=md"}, "## Projects", `"schemaVersion"`},
		{"json", []string{"--print=json"}, `"schemaVersion": 1`, "# Repo Map"},
		// What the CLI actually delivers: the global parser splits every unknown
		// `--flag=value` into two tokens before the remainder reaches a command,
		// so `--print=json` arrives here as two args.
		{"json, as the CLI splits it", []string{"--print", "json"}, `"schemaVersion": 1`, "# Repo Map"},
		{"md, as the CLI splits it", []string{"--print", "md"}, "## Projects", `"schemaVersion"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := mapCommandWorkspace(t)
			out, err := iox.CaptureStdout(func() error {
				return ContextMapCommand(root, ContextMapOptions{Args: tc.args})
			})
			if err != nil {
				t.Fatalf("context map %v: %v", tc.args, err)
			}
			if !strings.Contains(out, tc.wants) {
				t.Errorf("stdout does not carry the document (%q missing):\n%s", tc.wants, out)
			}
			if strings.Contains(out, tc.unwants) {
				t.Errorf("stdout carries the wrong rendering (%q present):\n%s", tc.unwants, out)
			}
			if !strings.Contains(out, "svc/api") {
				t.Errorf("the printed map does not mention the api project:\n%s", out)
			}
			// Nothing at all was written: no documents, no fragments.
			if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(mapgen.FragmentDir))); !os.IsNotExist(statErr) {
				t.Errorf("--print wrote into %s (stat err = %v)", mapgen.FragmentDir, statErr)
			}
		})
	}
}

// TestContextMapCommand_PrintMatchesTheWrittenDocument keeps the two surfaces
// honest against each other: what `--print=json` emits is exactly what a normal
// run writes to disk.
func TestContextMapCommand_PrintMatchesTheWrittenDocument(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "printing-the-map-writes-nothing")
	root := mapCommandWorkspace(t)
	printed, err := iox.CaptureStdout(func() error {
		return ContextMapCommand(root, ContextMapOptions{Args: []string{"--print=json"}})
	})
	if err != nil {
		t.Fatalf("context map --print=json: %v", err)
	}
	if err := ContextMapCommand(root, ContextMapOptions{}); err != nil {
		t.Fatalf("context map: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(mapgen.JSONPath)))
	if err != nil {
		t.Fatalf("read map: %v", err)
	}
	if printed != string(written) {
		t.Errorf("--print=json and the written document disagree:\n--- printed:\n%s\n--- written:\n%s", printed, written)
	}
}

// TestContextMapCommand_RejectsAnUnknownPrintFormat keeps a typo from silently
// selecting a default rendering the caller did not ask for.
func TestContextMapCommand_RejectsAnUnknownPrintFormat(t *testing.T) {
	root := mapCommandWorkspace(t)
	for _, args := range [][]string{{"--print=yaml"}, {"--print", "yaml"}} {
		err := ContextMapCommand(root, ContextMapOptions{Args: args})
		if err == nil || !errors.Is(err, protocolcli.ErrUsage) {
			t.Fatalf("context map %v error = %v, want a usage error", args, err)
		}
		if !strings.Contains(err.Error(), "json") || !strings.Contains(err.Error(), "md") {
			t.Errorf("the error does not name the accepted formats: %v", err)
		}
	}
}

// TestContextMapCommand_SelectionScopesFragmentsNotTheMap pins the map-reduce
// property at the command surface: --projects narrows which fragments are
// persisted, never which projects the map renders.
func TestContextMapCommand_SelectionScopesFragmentsNotTheMap(t *testing.T) {
	root := mapCommandWorkspace(t)

	if err := ContextMapCommand(root, ContextMapOptions{Projects: "example/api"}); err != nil {
		t.Fatalf("context map --projects: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(mapgen.JSONPath)))
	if err != nil {
		t.Fatalf("read map: %v", err)
	}
	for _, want := range []string{"svc/api", "svc/lib"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("a --projects run narrowed the map: %q missing", want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(mapgen.FragmentPath("svc/api")))); err != nil {
		t.Errorf("the selected project's fragment was not persisted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(mapgen.FragmentPath("svc/lib")))); !os.IsNotExist(err) {
		t.Errorf("an unselected project's fragment was persisted (stat err = %v)", err)
	}
}

// TestContextMapCommand_RefusesADegradedIdentity pins the guard that keeps the
// manual verb, the build attachment, and the MCP tool from disagreeing: a
// workspace that declares extensions but has adopted no provider view resolves
// every project from authored config alone (no provider-derived dependency
// edges), so the map would render without edges an agent would then trust.
func TestContextMapCommand_RefusesADegradedIdentity(t *testing.T) {
	root := mapCommandWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"),
		[]byte(`{"name":"example","version":"0.1.0","includes":["svc/api","svc/lib"],"extensions":["@putnami/go"]}`),
		0o644); err != nil {
		t.Fatalf("rewrite workspace manifest: %v", err)
	}
	workspace.InvalidateLoadCache(root)

	for _, args := range [][]string{nil, {"--print"}} {
		err := ContextMapCommand(root, ContextMapOptions{Args: args})
		if err == nil || !errors.Is(err, protocolcli.ErrInvalidConfig) {
			t.Fatalf("context map %v error = %v, want ErrInvalidConfig", args, err)
		}
		if !strings.Contains(err.Error(), "projects sync") {
			t.Errorf("error does not name the repair command: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(mapgen.JSONPath))); !os.IsNotExist(err) {
		t.Errorf("a refused run still wrote the map (stat err = %v)", err)
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

// TestContextMapCommand_StructuredOutputCarriesTheReport pins the machine
// surface: --output=jsonl emits the v2 envelope with the run report, and the
// report no longer carries a drift verdict because drift no longer exists.
func TestContextMapCommand_StructuredOutputCarriesTheReport(t *testing.T) {
	root := mapCommandWorkspace(t)
	out, err := iox.CaptureStdout(func() error {
		return ContextMapCommand(root, ContextMapOptions{OutputFormat: "jsonl"})
	})
	if err != nil {
		t.Fatalf("context map --output=jsonl: %v", err)
	}
	for _, want := range []string{`"command":"context map"`, `"outcome":"updated"`, `"projects":2`} {
		if !strings.Contains(out, want) {
			t.Errorf("structured output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "drift") {
		t.Errorf("the report still carries a drift concept:\n%s", out)
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
	if err := ContextMapCommand(root, ContextMapOptions{}); err != nil {
		t.Fatalf("context map over a fresh view: %v", err)
	}

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
