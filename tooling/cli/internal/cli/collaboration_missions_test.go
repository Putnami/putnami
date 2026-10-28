package cli

import (
	"encoding/json"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// collabMissionsWorkspace binds missions to the fixture provider, which
// declares every required missions operation.
func collabMissionsWorkspace(t *testing.T) string {
	t.Helper()
	return collabCommandWorkspace(t, `{"missions":{"provider":"@test/cli-collab","version":1}}`, func(manifest *extproto.Manifest) {
		spec, _ := collab.Lookup(collab.ContractMissions, 1)
		for _, name := range spec.RequiredOperations() {
			manifest.Tools["fixture.missions."+name] = collabCommandTool(t, collab.ContractMissions, name)
		}
	})
}

// TestCollaborationCommand_AHostOnlyOperationIsRoutedByTheCLIAndTheHostChannel:
// `putnami missions sync` and the host channel's collaboration.call route the
// host-only operation, to the same envelope.
func TestCollaborationCommand_AHostOnlyOperationIsRoutedByTheCLIAndTheHostChannel(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "host-only-operations", "the-cli-and-the-host-channel-route-a-host-only-operation")
	root := collabMissionsWorkspace(t)
	input := `{"session":{"engine":"host-b","id":"s-1"},"mission":{"repository":"putnami/putnami","key":"issue-596"},"event":"stop","state":"handed_back","observedAt":"2026-10-26T09:00:00Z"}`

	stdout, stderr, code := runCollabCommand(t, root, "", "missions", "sync", "--output=json", "--input", input)
	if code != ExitSuccess {
		t.Fatalf("putnami missions sync exited %d: %s %s", code, stdout, stderr)
	}
	cli := decodeCollabEnvelope(t, stdout)
	var result collab.MissionsSyncResult
	if err := json.Unmarshal(cli.Result, &result); err != nil || cli.Outcome != collab.OutcomeOK || result.Mission.Key != "issue-596" {
		t.Fatalf("putnami missions sync answered %s (%v)", stdout, err)
	}

	binding := hostRPCBinding{root: root, config: wsproto.Load(root), extensions: collabCommandExtensions(t, root)}
	answer, err := dialHostRPC(t, binding).CallCollaboration(t.Context(), collab.ContractMissions, collab.OperationSync, json.RawMessage(input))
	if err != nil {
		t.Fatalf("collaboration.call missions sync = %v", err)
	}
	if string(answer) != strings.TrimSpace(stdout) {
		t.Errorf("collaboration.call and the CLI disagree:\nRPC %s\nCLI %s", answer, strings.TrimSpace(stdout))
	}
}

// TestCollaborationCommand_HelpMarksAHostOnlyOperation: the help says which
// operation only the host calls.
func TestCollaborationCommand_HelpMarksAHostOnlyOperation(t *testing.T) {
	t.Parallel()
	root := collabMissionsWorkspace(t)
	stdout, _, code := runCollabCommand(t, root, "", "missions", "help")
	if code != ExitSuccess {
		t.Fatalf("putnami missions help exited %d", code)
	}
	if !strings.Contains(stdout, "calls the host-only sync") {
		t.Errorf("the help does not name the host-only operation:\n%s", stdout)
	}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if hostOnly := strings.Contains(line, "host-only"); fields[0] == "sync" && !hostOnly || fields[0] == "ask" && hostOnly {
			t.Errorf("operation line %q", line)
		}
	}
}

// TestCollaborationContracts_NoContractNameShadowsACommand: a contract word
// becomes a command group only in a workspace that binds it, so no contract
// may take the name of a built-in command, which would win the dispatch.
func TestCollaborationContracts_NoContractNameShadowsACommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/collaboration-providers", "contract-names-never-shadow-commands", "no-contract-name-is-a-built-in-command")
	root := collabMissionsWorkspace(t)
	for _, contract := range collab.ContractNames {
		if commandmeta.IsStructuredRoot(contract) {
			t.Errorf("%s is a built-in structured command", contract)
		}
		if _, found := commandmeta.Lookup(contract); found {
			t.Errorf("%s is a built-in command path", contract)
		}
		if commandmeta.ReservesVocabularyAlias(contract, nil) {
			t.Errorf("%s is reserved as a command alias", contract)
		}
		if !shouldDiscoverExtensions([]string{contract, collab.OperationCapabilities}, root) {
			t.Errorf("putnami %s skips the discovery that routes a contract word", contract)
		}
	}
}
