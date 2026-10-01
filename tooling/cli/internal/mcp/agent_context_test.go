package mcp

import (
	"context"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// TestToolAgentContextInjectedClosure asserts a tools/call for agent_context
// dispatches to the injected AgentContext closure and returns its already-shaped
// result verbatim, passing the resolved project selector through.
func TestToolAgentContextInjectedClosure(t *testing.T) {
	t.Parallel()
	dir := fixtureWorkspace(t)
	var gotSelector string
	srv := NewServer(Options{
		WorkspaceRoot: dir,
		Config:        wsproto.Load(dir),
		ServerVersion: "test",
		AgentContext: func(_ context.Context, sel string) (any, error) {
			gotSelector = sel
			return map[string]any{
				"document":     map[string]any{"id": sel},
				"diskArtifact": map[string]any{"present": true, "fresh": false},
			}, nil
		},
	})

	resps := runSession(t, srv, req(1, "tools/call", map[string]any{
		"name":      "agent_context",
		"arguments": map[string]any{"project": "app"},
	}))
	decoded, isErr := callResultJSON(t, resps[0])
	if isErr {
		t.Fatalf("agent_context returned an error result: %v", decoded)
	}
	if gotSelector != "app" {
		t.Errorf("closure received selector %q, want app", gotSelector)
	}
	doc, _ := decoded["document"].(map[string]any)
	if doc["id"] != "app" {
		t.Errorf("document = %v, want id=app", decoded["document"])
	}
	disk, _ := decoded["diskArtifact"].(map[string]any)
	if disk["present"] != true || disk["fresh"] != false {
		t.Errorf("diskArtifact = %v, want present=true fresh=false", disk)
	}
}

// TestToolAgentContextNilClosureUnavailable asserts that when no AgentContext
// closure is injected the tool fails closed with an "not available" isError
// result rather than panicking.
func TestToolAgentContextNilClosureUnavailable(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t) // no AgentContext injected
	resps := runSession(t, srv, req(1, "tools/call", map[string]any{
		"name":      "agent_context",
		"arguments": map[string]any{"project": "app"},
	}))
	decoded, isErr := callResultJSON(t, resps[0])
	if !isErr {
		t.Fatalf("nil closure should yield an isError result, got %v", decoded)
	}
	if decoded["_text"] != "agent_context is not available" {
		t.Errorf("error text = %v, want 'agent_context is not available'", decoded["_text"])
	}
}

// TestToolAgentContextMissingProject asserts an empty selector is rejected before
// the closure runs, so a missing argument never reaches the aggregator.
func TestToolAgentContextMissingProject(t *testing.T) {
	t.Parallel()
	dir := fixtureWorkspace(t)
	srv := NewServer(Options{
		WorkspaceRoot: dir,
		Config:        wsproto.Load(dir),
		ServerVersion: "test",
		AgentContext: func(_ context.Context, _ string) (any, error) {
			t.Fatal("closure must not run when project is empty")
			return nil, nil
		},
	})
	resps := runSession(t, srv, req(1, "tools/call", map[string]any{"name": "agent_context"}))
	_, isErr := callResultJSON(t, resps[0])
	if !isErr {
		t.Error("agent_context without a project must be an isError result")
	}
}
