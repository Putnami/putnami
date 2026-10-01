package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	agentcontext "go.putnami.dev/protocol/agentcontext"
	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
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

// packLoadedApp packs the app project via the on-disk-loaded workspace, so the
// freshly written artifact is byte-identical to what BuildAgentContextResult
// (which loads the same workspace) rebuilds. It returns the resolved revision.
func packLoadedApp(t *testing.T, root string) string {
	t.Helper()
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	app := shared.ResolveProjectSelector(ws, "/svc/app")
	if app == nil {
		t.Fatal("fixture app project did not load")
	}
	rev := resolveWorkspaceRevision(root)
	if _, err := ContextPack(ws, []*workspace.Project{app}, rev, fixtureVersion); err != nil {
		t.Fatalf("ContextPack: %v", err)
	}
	return rev
}

// TestBuildAgentContextResult_FreshAfterPack pins the freshness invariant: right
// after `context pack` writes the artifact with a given CLI version, an
// agent_context build at the SAME version reports the on-disk artifact both
// present and fresh, and the returned document is gate-passed with its
// provenance stamped with the resolved workspace revision.
func TestBuildAgentContextResult_FreshAfterPack(t *testing.T) {
	root := makeLoadableFixture(t)
	rev := packLoadedApp(t, root)

	res, err := BuildAgentContextResult(root, fixtureVersion, "/svc/app")
	if err != nil {
		t.Fatalf("BuildAgentContextResult: %v", err)
	}
	if res.Document == nil {
		t.Fatal("result document is nil")
	}
	if res.Document.Identity.ID != "/svc/app" {
		t.Errorf("document id = %q, want /svc/app", res.Document.Identity.ID)
	}
	if res.Document.Provenance.WorkspaceRevision != rev {
		t.Errorf("provenance revision = %q, want %q", res.Document.Provenance.WorkspaceRevision, rev)
	}
	if res.Document.Provenance.Generator.Version != fixtureVersion {
		t.Errorf("provenance version = %q, want %q", res.Document.Provenance.Generator.Version, fixtureVersion)
	}

	wantPath := filepath.ToSlash(filepath.Join("svc/app", agentcontext.DocumentEmitDir, agentcontext.DocumentFilename))
	if res.DiskArtifact.Path != wantPath {
		t.Errorf("artifact path = %q, want %q", res.DiskArtifact.Path, wantPath)
	}
	if !res.DiskArtifact.Present {
		t.Error("artifact must be present right after pack")
	}
	if !res.DiskArtifact.Fresh {
		t.Error("artifact must be fresh right after pack at the same version")
	}
}

// TestBuildAgentContextResult_AbsentArtifact asserts that with no `context pack`
// having run, the document is still built in-memory (the tool never depends on
// the on-disk artifact) while the artifact reports absent and not fresh. It also
// exercises selection by NAME.
func TestBuildAgentContextResult_AbsentArtifact(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-agent-guidance", "stale-or-absent-context-is-reported-for-fallback")
	root := makeLoadableFixture(t)

	res, err := BuildAgentContextResult(root, fixtureVersion, "example/app")
	if err != nil {
		t.Fatalf("BuildAgentContextResult: %v", err)
	}
	if res.Document == nil {
		t.Fatal("document must be built in-memory even with no on-disk artifact")
	}
	if res.DiskArtifact.Present {
		t.Error("no artifact was packed; Present must be false")
	}
	if res.DiskArtifact.Fresh {
		t.Error("an absent artifact can never be fresh")
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

// TestBuildAgentContextResult_StaleAfterMutation asserts that mutating a
// referenced fact after packing makes the fresh in-memory build diverge from the
// on-disk artifact: Present stays true, Fresh flips to false.
func TestBuildAgentContextResult_StaleAfterMutation(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-agent-guidance", "stale-or-absent-context-is-reported-for-fallback")
	root := makeLoadableFixture(t)
	packLoadedApp(t, root)

	// Mutate a referenced artifact so its digest — and thus the aggregated
	// document — no longer matches the packed bytes.
	capPath := filepath.Join(root, "svc", "app", "schema", "capabilities.json")
	if err := os.WriteFile(capPath, []byte(`{"protocolVersion":1,"changed":true}`+"\n"), 0o644); err != nil {
		t.Fatalf("mutate capabilities: %v", err)
	}

	res, err := BuildAgentContextResult(root, fixtureVersion, "/svc/app")
	if err != nil {
		t.Fatalf("BuildAgentContextResult: %v", err)
	}
	if !res.DiskArtifact.Present {
		t.Error("artifact is still on disk; Present must be true")
	}
	if res.DiskArtifact.Fresh {
		t.Error("a mutated reference must make the artifact stale")
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

// TestBuildAgentContextResult_VersionMismatchIsStale pins the version-parity
// invariant that the closure threading exists to protect: an artifact packed at
// one CLI version reads as stale when rebuilt at a different version, because the
// provenance generator version is part of the canonical bytes.
func TestBuildAgentContextResult_VersionMismatchIsStale(t *testing.T) {
	root := makeLoadableFixture(t)
	packLoadedApp(t, root) // packed at fixtureVersion

	res, err := BuildAgentContextResult(root, "1.2.3", "/svc/app")
	if err != nil {
		t.Fatalf("BuildAgentContextResult: %v", err)
	}
	if !res.DiskArtifact.Present {
		t.Error("artifact is on disk; Present must be true")
	}
	if res.DiskArtifact.Fresh {
		t.Error("a version mismatch must read as stale (provenance version differs)")
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
