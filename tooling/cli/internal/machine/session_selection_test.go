package machine

import (
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	protocoljob "go.putnami.dev/protocol/job"
)

// TestSessionSelectionProjectsTheRunSelection pins what a ledger row reads: the
// mode the run resolved, whether it was narrowed, and the project ids.
//
// The two absent answers matter as much as the present one. A run whose
// selection was never resolved records NO selection block, because inventing
// `all` for a record that stated none would claim workspace coverage the run
// never had. And the baseline is not repeated: the session's git block records
// it, and one value in two members is how the two start disagreeing.
func TestSessionSelectionProjectsTheRunSelection(t *testing.T) {
	t.Parallel()
	if got := SessionSelection(nil); got != nil {
		t.Errorf("SessionSelection(nil) = %+v, want nil (an unstated selection is absent, not `all`)", got)
	}
	if got := SessionSelection(&protocoljob.Selection{}); got != nil {
		t.Errorf("SessionSelection with no mode = %+v, want nil", got)
	}

	impacted := SessionSelection(&protocoljob.Selection{
		Mode:           protocoljob.SelectionModeImpacted,
		Scoped:         true,
		Baseline:       "origin/main",
		BaselineSource: "trunk",
		ProjectIDs:     []string{"/protocols/cli", "/tooling/cli"},
	})
	want := &protocolcli.SessionSelection{
		Mode:     protocolcli.SessionSelectionModeImpacted,
		Scoped:   true,
		Projects: []string{"/protocols/cli", "/tooling/cli"},
	}
	if !reflect.DeepEqual(impacted, want) {
		t.Errorf("SessionSelection(impacted) = %+v, want %+v", impacted, want)
	}

	all := SessionSelection(&protocoljob.Selection{Mode: protocoljob.SelectionModeAll})
	if all == nil || all.Scoped || len(all.Projects) != 0 {
		t.Errorf("SessionSelection(all) = %+v, want an unscoped whole-workspace projection", all)
	}
}
