package provider

import (
	"encoding/json"
	"testing"
	"time"

	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/protocol/features/spectest"
)

// TestTheSharedContractScenarios runs the scenarios every tasks provider
// passes, against a fresh fake Jira each time, through the same handlers and
// redaction the runtime uses. Like Jira, the fake's search shows a new issue
// only some time after its create was answered.
func TestTheSharedContractScenarios(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "task-scenarios", "the-jira-provider-passes-the-task-scenarios")
	providertest.RunTasks(t, func(t *testing.T) providertest.Target {
		h := newHarness(t)
		h.fake.searchLag = time.Second
		return providertest.Target{
			Handlers:      h.provider.Handlers(),
			WorkspaceRoot: t.TempDir(),
			Settings:      json.RawMessage(h.settings),
			Settle:        30 * time.Second,
		}
	})
}
