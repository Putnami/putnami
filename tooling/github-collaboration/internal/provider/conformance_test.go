package provider

import (
	"encoding/json"
	"testing"
	"time"

	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/protocol/features/spectest"
)

// TestTheSharedContractScenarios runs the scenarios every tasks and proposals
// provider passes, against a fresh fake GitHub each time, through the same
// handlers and redaction the runtime uses. Like GitHub, the fake lists a new
// issue only some time after its create was answered.
func TestTheSharedContractScenarios(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "task-scenarios", "the-github-provider-passes-the-task-scenarios")
	spectest.Proves(t, "collaboration/provider-contract", "proposal-scenarios", "the-github-provider-passes-the-proposal-scenarios")
	newTarget := func(contract string) func(t *testing.T) providertest.Target {
		return func(t *testing.T) providertest.Target {
			h := newHarness(t)
			h.fake.listLag = time.Second
			return providertest.Target{
				Handlers:      h.provider.Handlers(),
				WorkspaceRoot: t.TempDir(),
				Settings:      json.RawMessage(h.settings(contract)),
				Base:          "main",
				Heads:         [2]string{"topic-a", "topic-b"},
				Settle:        30 * time.Second,
			}
		}
	}
	t.Run("tasks", func(t *testing.T) {
		providertest.RunTasks(t, newTarget("tasks"))
	})
	t.Run("proposals", func(t *testing.T) {
		providertest.RunProposals(t, newTarget("proposals"))
	})
}
