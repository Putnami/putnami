package provider

import (
	"testing"
	"time"

	"go.putnami.dev/protocol/collaboration/providertest"
)

// TestTheSharedContractScenarios runs the scenarios every tasks and proposals
// provider passes against a fresh local store.
func TestTheSharedContractScenarios(t *testing.T) {
	newTarget := func(t *testing.T) providertest.Target {
		clock := time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)
		return providertest.Target{
			Handlers:      (&Provider{Now: func() time.Time { return clock }}).Handlers(),
			WorkspaceRoot: t.TempDir(),
			Base:          "main",
			Heads:         [2]string{"topic-a", "topic-b"},
		}
	}
	providertest.RunTasks(t, newTarget)
	providertest.RunProposals(t, newTarget)
}
