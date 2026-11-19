package provider

import (
	"encoding/base64"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
)

// TestTheRunsCursorKeepsItsPlaceAndRefusesAForeignOne: two runs started at
// one clock instant page the last started first, and a cursor this provider
// did not issue, or one without the run's place in the store, is invalid.
func TestTheRunsCursorKeepsItsPlaceAndRefusesAForeignOne(t *testing.T) {
	h := newHarness(t, "")
	h.version = 2
	const mission = `{"repository":"acme/app","key":"issue-7"}`
	for _, key := range []string{"one", "two"} {
		var started collab.MissionsRunResult
		h.ok("missions", "start", `{"mission":`+mission+`,"kind":"build","input":{"build":{"intention":"Ship"}},"idempotencyKey":"`+key+`"}`, &started)
	}
	var first, second collab.MissionsRunsResult
	h.ok("missions", "runs", `{"mission":`+mission+`,"page":{"size":1}}`, &first)
	if len(first.Items) != 1 || first.Items[0].ID != "R-2" || first.Page.Next == "" {
		t.Fatalf("the first page is %+v, want R-2 and a cursor", first)
	}
	h.ok("missions", "runs", `{"mission":`+mission+`,"page":{"size":1,"cursor":"`+first.Page.Next+`"}}`, &second)
	if len(second.Items) != 1 || second.Items[0].ID != "R-1" || second.Page.Next != "" {
		t.Fatalf("the second page is %+v, want R-1 and no cursor", second)
	}
	withoutPlace := base64.RawURLEncoding.EncodeToString([]byte(`{"startedAt":"2026-09-24T08:00:00Z","id":"R-1"}`))
	for _, cursor := range []string{"not-a-cursor", withoutPlace} {
		failure := h.fails("missions", "runs", `{"mission":`+mission+`,"page":{"cursor":"`+cursor+`"}}`, collab.OutcomeInvalid)
		if failure.Reason != collab.ReasonRequestInvalid {
			t.Fatalf("cursor %q failed with %+v", cursor, failure)
		}
	}
}
