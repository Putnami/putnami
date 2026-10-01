package engine

import (
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	protocoljob "go.putnami.dev/protocol/job"
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
