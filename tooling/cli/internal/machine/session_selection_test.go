package machine

import (
	"encoding/json"
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

// TestSessionSelectionReuseMatchesTheJobContract pins the reuse block the
// session records to the one the job context carries: same members, same
// order, same bytes. A consumer reading either one must find the same reused
// members, and a selection that reused nothing records no block in either.
func TestSessionSelectionReuseMatchesTheJobContract(t *testing.T) {
	t.Parallel()
	selection := &protocoljob.Selection{
		Mode: protocoljob.SelectionModeImpacted, Scoped: true,
		ProjectIDs: []string{"/apps/console"}, ReleaseSetProjects: []string{"/apps/console"},
		Reuse: &protocoljob.SelectionReuse{
			Channel: "pr-7", Head: "rs_9a03", SourceRevision: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
			Members: []protocoljob.SelectionReusedMember{
				{Ecosystem: "npm", Coordinate: "@acme/widget", Version: "1.2.0-20261014120000-abc1234"},
				{Ecosystem: "oci", Coordinate: "acme/console", Version: "1.2.0-20261014120000-abc1234"},
			},
		},
	}
	recorded := SessionSelection(selection)
	want, err := json.Marshal(selection.Reuse)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(recorded.Reuse)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("session reuse block drifted from the job context:\n session = %s\n job     = %s", got, want)
	}
	document, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, sessionFileWithSelection(t, document)); len(violations) != 0 {
		t.Fatalf("the recorded selection is refused by the session contract: %v", violations)
	}

	selection.Reuse = nil
	bare, err := json.Marshal(SessionSelection(selection))
	if err != nil {
		t.Fatal(err)
	}
	if string(bare) != `{"mode":"impacted","scoped":true,"projects":["/apps/console"],"releaseSetProjects":["/apps/console"]}` {
		t.Fatalf("a selection without reuse records = %s", bare)
	}
}

// sessionFileWithSelection is the smallest session document carrying one
// recorded selection block.
func sessionFileWithSelection(t *testing.T, selection []byte) []byte {
	t.Helper()
	document := `{"protocolVersion":2,"sessionId":"20261014-120500-3c4d5e","startTime":"2026-10-14T12:05:00.000Z",` +
		`"commands":["publish"],"selection":` + string(selection) + `,` +
		`"run":{"outcome":"success","exitCode":0,"counts":{"total":1,"succeeded":1,"failed":0,"canceled":0,"skipped":0},` +
		`"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":1}}`
	return []byte(document)
}
