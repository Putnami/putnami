package memory

import (
	"encoding/json"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/protocol/features/spectest"
)

// TestTheSharedMemoryScenariosPassOnEveryBackend runs the protocol's memory
// scenarios, unchanged, against the file backend, the Git backend on a local
// branch, and the Git backend behind a bare remote. Each scenario gets a fresh
// store; its second provider instance shares nothing with the first but that
// store.
func TestTheSharedMemoryScenariosPassOnEveryBackend(t *testing.T) {
	spectest.Proves(t, feature, "one-contract-every-backend", "read-checkpoint-resume")
	spectest.Proves(t, feature, "compare-and-set-checkpoints", "concurrent-checkpoints-at-one-revision-land-once")
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			providertest.RunMemory(t, func(t *testing.T) providertest.Target {
				workspace := t.TempDir()
				settings, _ := b.open(t, workspace)
				return providertest.Target{
					Handlers:      New().Handlers(),
					WorkspaceRoot: workspace,
					Workspace:     workspaceName,
					Settings:      json.RawMessage(settings),
					Reopen: func(*testing.T) map[collab.OperationKey]collab.Handler {
						return New().Handlers()
					},
				}
			})
		})
	}
}
