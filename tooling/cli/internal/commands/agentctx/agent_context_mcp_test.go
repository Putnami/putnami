package agentctx

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentcontext "go.putnami.dev/protocol/agentcontext"
	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// makeLoadableFixture augments the in-memory buildFixtureWorkspace layout with
// the workspace/project config files workspace.Load needs, so
// BuildAgentContextResult (which loads the workspace from disk itself) resolves
// the same two projects. It returns the workspace root.
func makeLoadableFixture(t *testing.T) string {
	t.Helper()
	ws, _ := buildFixtureWorkspace(t)
	write := func(rel, content string) {
		t.Helper()
		abs := filepath.Join(ws.Root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("putnami.workspace.json", `{"name":"fixture","includes":["svc/app","svc/lib"]}`)
	write("svc/app/putnami.json", `{"name":"example/app","type":"application","tags":["go","e2e"],"main":"main.go","dependencies":["example/lib"]}`)
	write("svc/lib/putnami.json", `{"name":"example/lib"}`)
	workspace.InvalidateLoadCache(ws.Root)
	return ws.Root
}

// packLoadedApp prints the app project's document the way `context pack` does,
// over the on-disk-loaded workspace BuildAgentContextResult also loads, at the
// given revision and CLI version.
func packLoadedApp(t *testing.T, root, revision, cliVersion string) *agentcontext.Document {
	t.Helper()
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	app := shared.ResolveProjectSelector(ws, "/svc/app")
	if app == nil {
		t.Fatal("fixture app project did not load")
	}
	report, err := ContextPack(ws, []*workspace.Project{app}, nil, revision, cliVersion)
	if err != nil {
		t.Fatalf("ContextPack: %v", err)
	}
	if len(report.Documents) != 1 {
		t.Fatalf("ContextPack returned %d documents, want 1", len(report.Documents))
	}
	return report.Documents[0]
}

// TestBuildAgentContextResult_IsThePackedDocumentAndWritesNothing pins that
// the agent_context tool and `context pack` agree byte for byte at the same
// revision and CLI version, that the provenance stamps both, that the payload
// carries no disk-artifact status, and that neither path writes a file.
func TestBuildAgentContextResult_IsThePackedDocumentAndWritesNothing(t *testing.T) {
	spectest.Proves(t, "agent-context/aggregated-agent-context", "a-producer-emits-a-valid-document-and-writes-nothing", "go-cli-agent-context-tool-serves-the-packed-document-and-writes-nothing")
	root := makeLoadableFixture(t)
	before := treeOutsideState(t, root)

	for _, version := range []string{fixtureVersion, "1.2.3"} {
		res, err := BuildAgentContextResult(root, version, "/svc/app")
		if err != nil {
			t.Fatalf("BuildAgentContextResult at %s: %v", version, err)
		}
		if res.Document == nil || res.Document.Identity.ID != "/svc/app" {
			t.Fatalf("document at %s = %+v, want /svc/app", version, res.Document)
		}
		rev := resolveWorkspaceRevision(root)
		if res.Document.Provenance.WorkspaceRevision != rev {
			t.Errorf("provenance revision = %q, want %q", res.Document.Provenance.WorkspaceRevision, rev)
		}
		if res.Document.Provenance.Generator.Version != version {
			t.Errorf("provenance version = %q, want the running CLI version %q", res.Document.Provenance.Generator.Version, version)
		}

		served, err := json.Marshal(res.Document)
		if err != nil {
			t.Fatalf("marshal the served document: %v", err)
		}
		printed, err := json.Marshal(packLoadedApp(t, root, rev, version))
		if err != nil {
			t.Fatalf("marshal the printed document: %v", err)
		}
		if string(served) != string(printed) {
			t.Errorf("at %s the agent_context document differs from the one context pack prints:\n%s\n%s", version, served, printed)
		}

		payload, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal the payload: %v", err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(payload, &keys); err != nil {
			t.Fatalf("decode the payload: %v", err)
		}
		if _, present := keys["diskArtifact"]; present {
			t.Errorf("the agent_context payload still carries diskArtifact: %s", payload)
		}
		if _, present := keys["designArtifact"]; !present {
			t.Errorf("the agent_context payload lost designArtifact: %s", payload)
		}
	}

	if after := treeOutsideState(t, root); !maps.Equal(after, before) {
		t.Errorf("agent_context or context pack changed the workspace tree:\nbefore %v\nafter  %v", before, after)
	}
	assertNoMapState(t, root)
}

// TestBuildAgentContextResult_SelectsByName asserts that a selection by NAME
// resolves the same project, and that a project without a design graph
// reports the graph absent instead of failing.
func TestBuildAgentContextResult_SelectsByName(t *testing.T) {
	root := makeLoadableFixture(t)

	res, err := BuildAgentContextResult(root, fixtureVersion, "example/app")
	if err != nil {
		t.Fatalf("BuildAgentContextResult: %v", err)
	}
	if res.Document == nil || res.Document.Identity.ID != "/svc/app" {
		t.Fatalf("document = %+v, want /svc/app selected by name", res.Document)
	}
	if res.DesignArtifact.Present || res.DesignArtifact.Compatibility != "" || len(res.Features) != 0 {
		t.Fatalf("absent design artifact = %+v, features = %+v", res.DesignArtifact, res.Features)
	}
}

func TestBuildAgentContextResultIncludesNativeFeatureSummaries(t *testing.T) {
	root := makeLoadableFixture(t)
	graph := &featureproto.DesignGraph{
		Compatibility: featureproto.DesignGraphCompatibility,
		Project:       "example/app",
		Nodes: []featureproto.DesignNode{{
			ID: "feature:auth/opaque-tokens", Kind: featureproto.DesignNodeFeature, Name: "Opaque tokens",
			Properties: map[string]string{"outcome": "Clients authenticate without exposing credentials", "owner": "identity"},
		}},
		Edges: []featureproto.DesignEdge{},
	}
	encoded, err := featureproto.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	sharedtest.WriteFeatureFixture(t, filepath.Join(root, "svc", "app", ".gen", featureproto.DesignGraphArtifact), string(encoded))

	res, err := BuildAgentContextResult(root, fixtureVersion, "/svc/app")
	if err != nil {
		t.Fatal(err)
	}
	if !res.DesignArtifact.Present || res.DesignArtifact.Compatibility != featureproto.DesignGraphCompatibility {
		t.Fatalf("design artifact = %+v", res.DesignArtifact)
	}
	if res.DesignArtifact.Unreadable != "" {
		t.Errorf("readable artifact reported a problem: %q", res.DesignArtifact.Unreadable)
	}
	if len(res.Features) != 1 || res.Features[0].ID != "auth/opaque-tokens" {
		t.Fatalf("features = %+v", res.Features)
	}
}

// TestBuildAgentContextResultServesDocumentDespiteUnreadableDesignGraph pins the
// dependency direction: the design graph is optional, gitignored build output
// that agent_context gained late. A stale one — an older compatibility marker
// survives a branch switch under .gen — must degrade to a reported status, not
// withhold the orientation document the agent actually asked for.
func TestBuildAgentContextResultServesDocumentDespiteUnreadableDesignGraph(t *testing.T) {
	root := makeLoadableFixture(t)
	sharedtest.WriteFeatureFixture(t, filepath.Join(root, "svc", "app", ".gen", featureproto.DesignGraphArtifact),
		`{"compatibility":"provisional-0","project":"example/app","nodes":[],"edges":[]}`)

	res, err := BuildAgentContextResult(root, fixtureVersion, "/svc/app")
	if err != nil {
		t.Fatalf("a stale design graph must not fail agent_context: %v", err)
	}
	if res.Document == nil {
		t.Fatal("agent_context returned no document")
	}
	if !res.DesignArtifact.Present {
		t.Errorf("design artifact = %+v, want present", res.DesignArtifact)
	}
	if !strings.Contains(res.DesignArtifact.Unreadable, "provisional-0") {
		t.Errorf("design artifact = %+v, want the reason reported", res.DesignArtifact)
	}
	// An unreadable graph has no verified compatibility to claim.
	if res.DesignArtifact.Compatibility != "" {
		t.Errorf("unreadable artifact claimed compatibility %q", res.DesignArtifact.Compatibility)
	}
	if len(res.Features) != 0 {
		t.Errorf("features = %+v, want none from an unreadable graph", res.Features)
	}
}

// TestBuildAgentContextResult_ReflectsAReferencedFileEdit asserts that each
// call rebuilds the document from the working tree: an edit to a referenced
// file changes the next document, with nothing on disk to go stale.
func TestBuildAgentContextResult_ReflectsAReferencedFileEdit(t *testing.T) {
	root := makeLoadableFixture(t)
	first, err := BuildAgentContextResult(root, fixtureVersion, "/svc/app")
	if err != nil {
		t.Fatalf("BuildAgentContextResult: %v", err)
	}
	firstBytes, err := json.Marshal(first.Document)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	capPath := filepath.Join(root, "svc", "app", "schema", "capabilities.json")
	if err := os.WriteFile(capPath, []byte(`{"protocolVersion":1,"changed":true}`+"\n"), 0o644); err != nil {
		t.Fatalf("mutate capabilities: %v", err)
	}

	second, err := BuildAgentContextResult(root, fixtureVersion, "/svc/app")
	if err != nil {
		t.Fatalf("BuildAgentContextResult after the edit: %v", err)
	}
	secondBytes, err := json.Marshal(second.Document)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(secondBytes) == string(firstBytes) {
		t.Error("the document did not change after a referenced file changed")
	}
}

// TestBuildAgentContextResult_ReflectsWorkspaceEditsInLongSession pins the
// long-lived-server freshness invariant: workspace.Load memoizes the workspace
// for the process lifetime, so in a persistent MCP session an edit to a
// project's putnami.json (main / dependencies / tags) after the first request
// would otherwise be masked by the cached graph — returning a stale document.
// BuildAgentContextResult must invalidate the memo so a later request reflects
// the current workspace, not the one warmed by an earlier call.
func TestBuildAgentContextResult_ReflectsWorkspaceEditsInLongSession(t *testing.T) {
	root := makeLoadableFixture(t)
	appPutnami := filepath.Join(root, "svc", "app", "putnami.json")

	// Warm the process-lifetime workspace cache, as an earlier request in the
	// session would. The fixture app starts with one dependency (example/lib).
	primed, err := BuildAgentContextResult(root, fixtureVersion, "/svc/app")
	if err != nil {
		t.Fatalf("prime BuildAgentContextResult: %v", err)
	}
	if len(primed.Document.Identity.Dependencies) != 1 {
		t.Fatalf("fixture app should start with one dependency, got %v", primed.Document.Identity.Dependencies)
	}

	// Edit putnami.json AFTER the cache is warm: drop the dependency — a fact that
	// flows only through the (memoized) workspace load, not through a per-request
	// committed-file read.
	edited := `{"name":"example/app","type":"application","tags":["go","e2e"],"main":"main.go","dependencies":[]}`
	if err := os.WriteFile(appPutnami, []byte(edited), 0o644); err != nil {
		t.Fatalf("edit putnami.json: %v", err)
	}

	res, err := BuildAgentContextResult(root, fixtureVersion, "/svc/app")
	if err != nil {
		t.Fatalf("BuildAgentContextResult after edit: %v", err)
	}
	if len(res.Document.Identity.Dependencies) != 0 {
		t.Errorf("identity.dependencies = %v after dropping the dependency from putnami.json; the MCP path served a stale, cached workspace",
			res.Document.Identity.Dependencies)
	}
}

// TestBuildAgentContextResult_ProjectNotFound asserts an unknown selector fails
// with a not-found (exit-2 classified) error rather than a nil-document success.
func TestBuildAgentContextResult_ProjectNotFound(t *testing.T) {
	root := makeLoadableFixture(t)

	_, err := BuildAgentContextResult(root, fixtureVersion, "/does/not/exist")
	if err == nil {
		t.Fatal("expected a not-found error for an unknown project selector")
	}
	if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
		t.Fatalf("exit code = %d, want %d (not-found → usage)", code, protocolcli.ExitUsage)
	}
}
