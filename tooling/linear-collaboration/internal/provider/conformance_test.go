package provider

import (
	"encoding/json"
	"testing"

	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/protocol/features/spectest"
)

// TestTheSharedContractScenarios runs the scenarios every tasks provider
// passes, against a fresh fake Linear each time, through the same handlers and
// redaction the runtime uses.
func TestTheSharedContractScenarios(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "task-scenarios", "the-linear-provider-passes-the-task-scenarios")
	providertest.RunTasks(t, func(t *testing.T) providertest.Target {
		h := newHarness(t)
		return providertest.Target{
			Handlers:      h.provider.Handlers(),
			WorkspaceRoot: t.TempDir(),
			Settings:      json.RawMessage(h.settings),
		}
	})
}
