package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
)

// fakeWorkspaceMap stands in for commands.WorkspaceMapResult, which this package
// must not import (the mcp → commands edge would close a cycle through the
// engine — see internal/commands/workspace_map_mcp.go). Stating it as a TYPE
// rather than a nested map keeps the fixture's shape checked by the compiler.
type fakeWorkspaceMap struct {
	Document     fakeMapDocument `json:"document"`
	DiskArtifact fakeMapArtifact `json:"diskArtifact"`
	Provenance   fakeMapScope    `json:"provenance"`
}

type fakeMapDocument struct {
	Workspace string `json:"workspace"`
}

type fakeMapArtifact struct {
	Present bool `json:"present"`
	Fresh   bool `json:"fresh"`
}

type fakeMapScope struct {
	Section string `json:"section"`
	Project string `json:"project"`
}

// callWorkspaceMap issues one tools/call for workspace_map, omitting the
// arguments member entirely when args is nil so the "an agent calls it with no
// arguments at all" path is exercised as clients actually send it.
func callWorkspaceMap(t *testing.T, srv *Server, args map[string]any) (map[string]any, bool) {
	t.Helper()
	params := map[string]any{"name": "workspace_map"}
	if args != nil {
		params["arguments"] = args
	}
	resps := runSession(t, srv, req(1, "tools/call", params))
	return callResultJSON(t, resps[0])
}

// workspaceMapServer builds a server whose WorkspaceMap closure is the given
// function, over the shared workspace fixture.
func workspaceMapServer(t *testing.T, fn func(context.Context, string, string) (any, error)) *Server {
	t.Helper()
	dir := fixtureWorkspace(t)
	return NewServer(Options{
		WorkspaceRoot: dir,
		Config:        wsproto.Load(dir),
		ServerVersion: "test",
		WorkspaceMap:  fn,
	})
}

// TestToolWorkspaceMapInjectedClosure asserts a tools/call for workspace_map
// dispatches to the injected WorkspaceMap closure, passes both optional
// arguments through, and returns its already-shaped result verbatim.
func TestToolWorkspaceMapInjectedClosure(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "mcp-rebuilds-the-same-projection-in-memory")
	var gotSection, gotProject string
	srv := workspaceMapServer(t, func(_ context.Context, section, project string) (any, error) {
		gotSection, gotProject = section, project
		return fakeWorkspaceMap{
			Document:     fakeMapDocument{Workspace: "fixture"},
			DiskArtifact: fakeMapArtifact{Present: true, Fresh: true},
			Provenance:   fakeMapScope{Section: section, Project: project},
		}, nil
	})

	decoded, isErr := callWorkspaceMap(t, srv, map[string]any{"section": "apis", "project": "app"})
	if isErr {
		t.Fatalf("workspace_map returned an error result: %v", decoded)
	}
	if gotSection != "apis" || gotProject != "app" {
		t.Errorf("closure received (%q, %q), want (apis, app)", gotSection, gotProject)
	}
	// Re-encoded rather than walked member by member: the assertion is that the
	// closure's payload reached the client verbatim, which is a property of the
	// whole envelope, not of any one field.
	data, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-encode tool result: %v", err)
	}
	for _, want := range []string{`"workspace":"fixture"`, `"present":true`, `"fresh":true`, `"section":"apis"`, `"project":"app"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("tool result does not carry %s: %s", want, data)
		}
	}
}

// TestToolWorkspaceMapDefaultsToTheWholeMap pins the no-argument call: an agent
// that knows nothing about the workspace yet must be able to ask for everything
// without first learning the section vocabulary, so both arguments reach the
// builder empty rather than being rejected here.
func TestToolWorkspaceMapDefaultsToTheWholeMap(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "workspace-map", "mcp-rebuilds-the-same-projection-in-memory")
	called := false
	srv := workspaceMapServer(t, func(_ context.Context, section, project string) (any, error) {
		called = true
		if section != "" || project != "" {
			t.Errorf("closure received (%q, %q), want both empty", section, project)
		}
		return fakeWorkspaceMap{Document: fakeMapDocument{Workspace: "fixture"}}, nil
	})

	if _, isErr := callWorkspaceMap(t, srv, nil); isErr {
		t.Error("a no-argument workspace_map call must succeed")
	}
	if !called {
		t.Error("the closure never ran")
	}
}

// TestToolWorkspaceMapPropagatesBuilderErrors asserts a builder refusal (unknown
// section, unresolvable project, missing workspace index) reaches the agent as
// an isError result carrying the actionable message, not a protocol error it
// cannot read.
func TestToolWorkspaceMapPropagatesBuilderErrors(t *testing.T) {
	t.Parallel()
	srv := workspaceMapServer(t, func(_ context.Context, _, _ string) (any, error) {
		return nil, errors.New("unknown section: endpoints (want one of: projects, …)")
	})

	decoded, isErr := callWorkspaceMap(t, srv, map[string]any{"section": "endpoints"})
	if !isErr {
		t.Fatalf("a builder refusal must be an isError result, got %v", decoded)
	}
	if text, _ := decoded["_text"].(string); !strings.HasPrefix(text, "unknown section") {
		t.Errorf("error text = %v, want the builder's message", decoded["_text"])
	}
}

// TestToolWorkspaceMapNilClosureUnavailable asserts the tool fails closed when
// no closure is injected rather than panicking.
func TestToolWorkspaceMapNilClosureUnavailable(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t) // no WorkspaceMap injected
	decoded, isErr := callWorkspaceMap(t, srv, nil)
	if !isErr {
		t.Fatalf("nil closure should yield an isError result, got %v", decoded)
	}
	if decoded["_text"] != "workspace_map is not available" {
		t.Errorf("error text = %v, want 'workspace_map is not available'", decoded["_text"])
	}
}
