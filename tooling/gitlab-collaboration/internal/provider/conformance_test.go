package provider

import (
	"encoding/json"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/protocol/features/spectest"
)

// TestTheSharedContractScenarios runs the scenarios every tasks and proposals
// provider passes, against a fresh fake GitLab each time, through the same
// handlers and redaction the runtime uses. The provider offers
// proposals.comments, so the comment scenario runs rather than skips.
func TestTheSharedContractScenarios(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "task-scenarios", "the-gitlab-provider-passes-the-task-scenarios")
	spectest.Proves(t, "collaboration/provider-contract", "proposal-scenarios", "the-gitlab-provider-passes-the-proposal-scenarios")
	spectest.Proves(t, "collaboration/provider-contract", "proposal-comments", "the-gitlab-provider-passes-the-comment-scenario")
	if _, offered := (&Provider{}).Handlers()[collab.OperationKey{Contract: collab.ContractProposals, Version: 1, Operation: collab.OperationComments}]; !offered {
		t.Fatal("the provider does not offer proposals.comments, so the comment scenario would skip")
	}
	t.Run("tasks", func(t *testing.T) {
		providertest.RunTasks(t, func(t *testing.T) providertest.Target {
			h := newHarness(t)
			return providertest.Target{
				Handlers:      h.provider.Handlers(),
				WorkspaceRoot: t.TempDir(),
				Settings:      json.RawMessage(h.settings),
				Settle:        5 * time.Second,
			}
		})
	})
	t.Run("proposals", func(t *testing.T) {
		providertest.RunProposals(t, func(t *testing.T) providertest.Target {
			h := newHarness(t).proposals("")
			h.branch("topic-a", fakeSHA(2))
			h.branch("topic-b", fakeSHA(3))
			return providertest.Target{
				Handlers:      h.provider.Handlers(),
				WorkspaceRoot: t.TempDir(),
				Settings:      json.RawMessage(h.settings),
				Base:          "main",
				Heads:         [2]string{"topic-a", "topic-b"},
				Settle:        5 * time.Second,
			}
		})
	})
}
