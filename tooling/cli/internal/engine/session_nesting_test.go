package engine

import (
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/machine"
)

// TestSessionSelectionModesMatchTheJobContract is the pin the two declarations
// rest on. protocols/cli states the selection-mode vocabulary by VALUE rather
// than importing protocols/job, so a recorded session stays readable without
// the job-context protocol; this is the test that keeps the copy honest, and it
// lives here because the engine is the one place that sees both.
func TestSessionSelectionModesMatchTheJobContract(t *testing.T) {
	t.Parallel()
	if got, want := protocolcli.SessionSelectionModes, protocoljob.SelectionModes; !reflect.DeepEqual(got, want) {
		t.Fatalf("session selection modes = %v, job-context selection modes = %v", got, want)
	}
	for _, pair := range [][2]string{
		{protocolcli.SessionSelectionModeAll, protocoljob.SelectionModeAll},
		{protocolcli.SessionSelectionModeImpacted, protocoljob.SelectionModeImpacted},
		{protocolcli.SessionSelectionModeProjects, protocoljob.SelectionModeProjects},
	} {
		if pair[0] != pair[1] {
			t.Errorf("selection mode constant drift: %q vs %q", pair[0], pair[1])
		}
	}
}

// TestRecordedSessionDocumentNamesOnlyAStatedAgent pins the agent block to the
// one caller that states it: a terminal request carries none and records none,
// and an MCP request's agent is recorded as stated.
func TestRecordedSessionDocumentNamesOnlyAStatedAgent(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "recorded-run-context",
		"an-mcp-run-names-its-agent-and-a-terminal-run-names-none")
	terminal := recordedSessionDocument(machine.Run{}, &Request{Commands: []string{"build"}}, "20261013-101500-a1b2c3", nil, nil)
	if terminal.Agent != nil {
		t.Errorf("a terminal run recorded agent %+v", terminal.Agent)
	}
	agent := &protocolcli.SessionAgent{Kind: "claude-code", SessionID: "abc"}
	mcp := recordedSessionDocument(machine.Run{}, &Request{Commands: []string{"build"}, EphemeralSession: true, Agent: agent},
		"20261013-101500-a1b2c3", nil, nil)
	if mcp.Agent == nil || *mcp.Agent != *agent {
		t.Errorf("an MCP run recorded agent %+v, want %+v", mcp.Agent, agent)
	}
}
