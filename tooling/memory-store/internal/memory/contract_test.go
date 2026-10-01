package memory

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// The contract scenarios. Each runs, unchanged, against the file backend, the
// Git backend on a local branch, and the Git backend behind a remote.

func TestContract_ReadCheckpointResume(t *testing.T) {
	spectest.Proves(t, feature, "one-contract-every-backend", "read-checkpoint-resume")
	spectest.Proves(t, feature, "contextual-memory", "evidence-stays-a-reference")
	forEachBackend(t, func(t *testing.T, h *harness, _ func() func()) {
		var empty collab.MemoryListResult
		h.ok(collab.OperationContext, map[string]any{}, &empty)
		if empty.Items == nil || len(empty.Items) != 0 || empty.Page.Next != "" {
			t.Fatalf("an empty store answered %+v", empty)
		}
		h.fails(collab.OperationMission, map[string]any{"mission": "42"}, collab.OutcomeNotFound)

		first := checkpoint("42", "42:plan", "Plan accepted; task 1 next.", "")
		first["title"] = "Contributor offering"
		first["identity"] = map[string]any{"scope": "/tooling"}
		first["sources"] = []any{map[string]any{"source": "github:acme/app", "id": "42"}}
		first["evidence"] = []any{map[string]any{"kind": "gate", "locator": ".putnami/sessions/20260924-111701/session.json",
			"digest": "sha256:" + strings.Repeat("ab", 32)}}
		created := h.save(first)
		record := created.Record
		if created.Replayed || record.Kind != collab.MemoryKindMission || record.Identity.Mission != "42" ||
			record.Identity.Workspace != workspaceName || record.Identity.Scope != "/tooling" || sequenceOf(t, record.Revision) != 1 ||
			record.Provenance.RecordedBy != "contract-suite" || record.Provenance.RecordedAt != "2026-09-24T08:00:00Z" {
			t.Fatalf("created %+v", created)
		}
		if !strings.Contains(record.Ref.Source, ":") || record.Ref.ID != MissionID(record.Identity) {
			t.Fatalf("reference %+v", record.Ref)
		}

		read := h.mission("42")
		if read.Revision != record.Revision || read.Content != "Plan accepted; task 1 next." || read.Title != "Contributor offering" ||
			read.Ref != record.Ref || len(read.Sources) != 1 || len(read.Evidence) != 1 || read.Evidence[0] != record.Evidence[0] {
			t.Fatalf("read %+v, wrote %+v", read, record)
		}
		// The record is contextual: it carries the references it was given and
		// no member that could state a verdict.
		raw := h.call(collab.OperationMission, map[string]any{"mission": "42"}).Result
		var members struct {
			Record map[string]json.RawMessage `json:"record"`
		}
		mustDo(t, json.Unmarshal(raw, &members))
		keys := slices.Sorted(maps.Keys(members.Record))
		if want := []string{"content", "evidence", "freshness", "identity", "kind", "provenance", "ref", "revision", "sources", "title"}; !slices.Equal(keys, want) {
			t.Fatalf("record members %v, want %v", keys, want)
		}

		h.tick(time.Hour)
		second := checkpoint("42", "42:task-1", "Task 1 merged; task 2 in review.", read.Revision)
		updated := h.save(second).Record
		if sequenceOf(t, updated.Revision) != 2 || updated.Revision == read.Revision || updated.Title != "Contributor offering" ||
			updated.Identity.Scope != "/tooling" || len(updated.Evidence) != 0 || updated.Freshness.UpdatedAt != "2026-09-24T09:00:00Z" {
			t.Fatalf("updated %+v", updated)
		}

		// Another process resumes from the store alone.
		resumed := h.fresh()
		resumed.tick(time.Minute)
		again := resumed.mission("42")
		if again.Revision != updated.Revision || again.Content != "Task 1 merged; task 2 in review." ||
			again.Freshness.UpdatedAt != "2026-09-24T09:00:00Z" || again.Freshness.RetrievedAt != "2026-09-24T09:01:00Z" {
			t.Fatalf("resumed %+v", again)
		}
		var listed collab.MemoryListResult
		resumed.ok(collab.OperationContext, map[string]any{"identity": map[string]any{"mission": "42"}}, &listed)
		if len(listed.Items) != 1 || listed.Items[0].Revision != updated.Revision {
			t.Fatalf("context %+v", listed)
		}
		resumed.save(checkpoint("42", "42:task-2", "Task 2 merged.", again.Revision))
		if final := h.mission("42"); sequenceOf(t, final.Revision) != 3 {
			t.Fatalf("final %+v", final)
		}
	})
}

func TestContract_StaleRevisionIsAConflict(t *testing.T) {
	spectest.Proves(t, feature, "compare-and-set-checkpoints", "a-stale-revision-is-a-conflict")
	forEachBackend(t, func(t *testing.T, h *harness, _ func() func()) {
		first := h.save(checkpoint("m", "k1", "one", "")).Record
		second := h.save(checkpoint("m", "k2", "two", first.Revision)).Record
		failure := h.fails(collab.OperationCheckpoint, checkpoint("m", "k3", "three", first.Revision), collab.OutcomeConflict)
		if failure.Reason != collab.ReasonRevisionConflict || failure.Current != second.Revision || failure.Retryable {
			t.Fatalf("stale %+v", failure)
		}
		failure = h.fails(collab.OperationCheckpoint, checkpoint("m", "k4", "four", ""), collab.OutcomeConflict)
		if failure.Reason != collab.ReasonAlreadyExists || failure.Current != second.Revision {
			t.Fatalf("mustNotExist on an existing mission %+v", failure)
		}
		failure = h.fails(collab.OperationCheckpoint, checkpoint("absent", "k5", "five", second.Revision), collab.OutcomeNotFound)
		if failure.Reason != "mission.missing" {
			t.Fatalf("expectedRevision on an absent mission %+v", failure)
		}
		if current := h.mission("m"); current.Revision != second.Revision || current.Content != "two" {
			t.Fatalf("a refused checkpoint wrote: %+v", current)
		}
	})
}

func TestContract_ConcurrentCheckpointsLandOnce(t *testing.T) {
	spectest.Proves(t, feature, "compare-and-set-checkpoints", "concurrent-checkpoints-at-one-revision-land-once")
	forEachBackend(t, func(t *testing.T, h *harness, _ func() func()) {
		base := h.save(checkpoint("race", "base", "base", "")).Record
		// A checkpoint of another mission moves a Git branch under the racers
		// without touching the record they compare.
		h.save(checkpoint("bystander", "bystander", "unrelated", ""))
		const writers = 6
		responses := concurrently(t, writers, func(i int) (collab.Response, error) {
			return h.do(collab.OperationCheckpoint, checkpoint("race", fmt.Sprintf("writer-%d", i), fmt.Sprintf("writer %d", i), base.Revision))
		})
		var winners []collab.MemoryRecord
		var conflicts []collab.Error
		for _, response := range responses {
			switch response.Outcome {
			case collab.OutcomeOK:
				var result collab.MemoryCheckpointResult
				mustDo(t, json.Unmarshal(response.Result, &result))
				winners = append(winners, result.Record)
			case collab.OutcomeConflict:
				conflicts = append(conflicts, *response.Error)
			default:
				t.Fatalf("a racer answered %s %+v", response.Outcome, response.Error)
			}
		}
		if len(winners) != 1 || len(conflicts) != writers-1 {
			t.Fatalf("%d writers landed and %d conflicted; exactly one lands", len(winners), len(conflicts))
		}
		for _, failure := range conflicts {
			if failure.Reason != collab.ReasonRevisionConflict || failure.Current != winners[0].Revision {
				t.Fatalf("a loser saw %+v, the winner wrote %s", failure, winners[0].Revision)
			}
		}
		if current := h.mission("race"); current.Revision != winners[0].Revision || current.Content != winners[0].Content ||
			sequenceOf(t, current.Revision) != 2 {
			t.Fatalf("the store holds %+v, the winner wrote %+v", current, winners[0])
		}
	})
}

func TestContract_ConcurrentCreationsLandOnce(t *testing.T) {
	spectest.Proves(t, feature, "compare-and-set-checkpoints", "concurrent-creations-land-once")
	forEachBackend(t, func(t *testing.T, h *harness, _ func() func()) {
		// The store itself is new: the racers also race to create it.
		const writers = 6
		responses := concurrently(t, writers, func(i int) (collab.Response, error) {
			return h.do(collab.OperationCheckpoint, checkpoint("new", fmt.Sprintf("creator-%d", i), fmt.Sprintf("creator %d", i), ""))
		})
		created := 0
		for _, response := range responses {
			switch {
			case response.Outcome == collab.OutcomeOK:
				created++
			case response.Outcome == collab.OutcomeConflict && response.Error.Reason == collab.ReasonAlreadyExists:
			default:
				t.Fatalf("a creator answered %s %+v", response.Outcome, response.Error)
			}
		}
		if created != 1 {
			t.Fatalf("%d creations landed; exactly one does", created)
		}
		if record := h.mission("new"); sequenceOf(t, record.Revision) != 1 {
			t.Fatalf("the mission holds %+v", record)
		}
	})
}

func TestContract_ARepeatedKeyReplaysTheWrite(t *testing.T) {
	spectest.Proves(t, feature, "retries-never-duplicate", "a-repeated-key-replays-the-write")
	forEachBackend(t, func(t *testing.T, h *harness, _ func() func()) {
		first := h.save(checkpoint("m", "k1", "one", "")).Record
		request := checkpoint("m", "k2", "two", first.Revision)
		landed := h.save(request)
		// The answer was lost: the caller repeats the identical request later,
		// from another process.
		retry := h.fresh()
		retry.tick(time.Hour)
		replay := retry.save(request)
		if !replay.Replayed || replay.Record.Revision != landed.Record.Revision || replay.Record.Content != "two" {
			t.Fatalf("the repeat answered %+v, the write was %+v", replay, landed)
		}
		// Having read the mission since, it repeats with the revision it read.
		replay = retry.save(checkpoint("m", "k2", "two", landed.Record.Revision))
		if !replay.Replayed || replay.Record.Revision != landed.Record.Revision {
			t.Fatalf("a repeat naming the landed revision answered %+v", replay)
		}
		if current := h.mission("m"); sequenceOf(t, current.Revision) != 2 {
			t.Fatalf("a repeat wrote again: %+v", current)
		}
		failure := h.fails(collab.OperationCheckpoint, checkpoint("m", "k2", "other content", first.Revision), collab.OutcomeConflict)
		if failure.Reason != collab.ReasonIdempotencyMismatch || failure.Current != landed.Record.Revision {
			t.Fatalf("a reused key answered %+v", failure)
		}
	})
}

func TestContract_PagesKeepOneOrder(t *testing.T) {
	spectest.Proves(t, feature, "one-contract-every-backend", "pages-keep-one-order")
	forEachBackend(t, func(t *testing.T, h *harness, _ func() func()) {
		for i := range 5 {
			h.save(checkpoint(fmt.Sprintf("mission-%d", i), fmt.Sprintf("k%d", i), fmt.Sprintf("content %d", i), ""))
		}
		all := h.all(map[string]any{}, 100)
		if len(all) != 5 {
			t.Fatalf("one page holds %d records", len(all))
		}
		paged := h.all(map[string]any{}, 2)
		if len(paged) != 5 {
			t.Fatalf("pages of 2 hold %d records", len(paged))
		}
		for i := range all {
			if paged[i].Ref != all[i].Ref {
				t.Fatalf("pages and one page disagree at %d: %v and %v", i, paged[i].Ref, all[i].Ref)
			}
		}

		// A write between two pages neither repeats nor skips a record.
		var page collab.MemoryListResult
		h.ok(collab.OperationContext, map[string]any{"page": map[string]any{"size": 2}}, &page)
		seen := map[string]bool{}
		for _, record := range page.Items {
			seen[record.Ref.ID] = true
		}
		h.save(checkpoint("mission-0", "k0-again", "changed", h.mission("mission-0").Revision))
		var tail []collab.MemoryRecord
		cursor := page.Page.Next
		for cursor != "" {
			var next collab.MemoryListResult
			h.ok(collab.OperationContext, map[string]any{"page": map[string]any{"size": 2, "cursor": cursor}}, &next)
			tail = append(tail, next.Items...)
			cursor = next.Page.Next
		}
		for _, record := range tail {
			if seen[record.Ref.ID] {
				t.Fatalf("record %s was listed twice", record.Ref.ID)
			}
			seen[record.Ref.ID] = true
		}
		if len(seen) != 5 {
			t.Fatalf("the traversal saw %d of 5 records", len(seen))
		}

		failure := h.fails(collab.OperationContext, map[string]any{"page": map[string]any{"cursor": "not-a-cursor"}}, collab.OutcomeInvalid)
		if failure.Reason != collab.ReasonRequestInvalid {
			t.Fatalf("a foreign cursor answered %+v", failure)
		}
	})
}

func TestContract_AnUnreachableStoreIsUnavailable(t *testing.T) {
	spectest.Proves(t, feature, "explicit-outage", "an-unreachable-store-is-unavailable-and-writes-nothing")
	forEachBackend(t, func(t *testing.T, h *harness, outage func() func()) {
		before := h.save(checkpoint("m", "k1", "one", "")).Record
		restore := outage()
		for _, operation := range []string{collab.OperationContext, collab.OperationMission, collab.OperationSearch} {
			arguments := map[string]any{"mission": "m"}
			switch operation {
			case collab.OperationContext:
				arguments = map[string]any{}
			case collab.OperationSearch:
				arguments = map[string]any{"query": "one"}
			}
			h.fails(operation, arguments, collab.OutcomeUnavailable)
		}
		failure := h.fails(collab.OperationCheckpoint, checkpoint("m", "k2", "two", before.Revision), collab.OutcomeUnavailable)
		if failure.Reconcile != "" {
			t.Fatalf("an unavailable store asks for reconciliation: %+v", failure)
		}
		restore()
		if current := h.mission("m"); current.Revision != before.Revision {
			t.Fatalf("the outage wrote: %+v", current)
		}
		if after := h.save(checkpoint("m", "k2", "two", before.Revision)); after.Replayed || sequenceOf(t, after.Record.Revision) != 2 {
			t.Fatalf("after the outage: %+v", after)
		}
	})
}

func TestContract_ContextSelectsByIdentityAndScope(t *testing.T) {
	spectest.Proves(t, feature, "layout-independent-context", "context-selects-by-identity-and-scope")
	forEachBackend(t, func(t *testing.T, h *harness, _ func() func()) {
		save := func(mission, scope, repository string) {
			request := checkpoint(mission, "k-"+mission, "About "+mission+" in "+scope+".", "")
			request["identity"] = map[string]any{"scope": scope, "repository": repository}
			h.save(request)
		}
		save("cli", "/tooling/cli", "acme/app")
		save("tooling", "/tooling", "acme/app")
		save("protocols", "/protocols/collaboration", "acme/app")
		save("anywhere", "", "acme/app")
		save("elsewhere", "/tooling/cli", "acme/other")

		missions := func(records []collab.MemoryRecord) []string {
			names := make([]string, 0, len(records))
			for _, record := range records {
				names = append(names, record.Identity.Mission)
			}
			slices.Sort(names)
			return names
		}
		check := func(name string, request map[string]any, selection *extension.ToolSelection, want ...string) {
			t.Helper()
			h.selection = selection
			if got := missions(h.all(request, collab.MaxPageSize)); !slices.Equal(got, want) {
				t.Errorf("%s: selected %v, want %v", name, got, want)
			}
			h.selection = nil
		}
		acme := map[string]any{"repository": "acme/app"}
		check("the whole workspace", map[string]any{}, nil, "anywhere", "cli", "elsewhere", "protocols", "tooling")
		check("one repository", map[string]any{"identity": acme}, nil, "anywhere", "cli", "protocols", "tooling")
		check("a scope", map[string]any{"identity": map[string]any{"repository": "acme/app", "scope": "/tooling"}}, nil,
			"anywhere", "cli", "tooling")
		check("a narrowed selection", map[string]any{"identity": acme},
			&extension.ToolSelection{Mode: extension.ToolSelectionModeProjects, Scoped: true, ProjectIDs: []string{"/tooling/cli"}},
			"anywhere", "cli", "tooling")
		check("an empty impact", map[string]any{"identity": acme},
			&extension.ToolSelection{Mode: extension.ToolSelectionModeImpacted, Scoped: true, EmptyImpact: true, ProjectIDs: []string{}},
			"anywhere")
		check("an unscoped selection", map[string]any{"identity": acme},
			&extension.ToolSelection{Mode: extension.ToolSelectionModeAll, ProjectIDs: []string{"/tooling/cli"}},
			"anywhere", "cli", "protocols", "tooling")
		check("another workspace", map[string]any{"identity": map[string]any{"workspace": "other"}}, nil)
		check("notes only", map[string]any{"kinds": []string{"note"}}, nil)

		var found collab.MemoryListResult
		h.ok(collab.OperationSearch, map[string]any{"query": "ABOUT TOOLING", "identity": acme}, &found)
		if got := missions(found.Items); !slices.Equal(got, []string{"tooling"}) {
			t.Errorf("search found %v", got)
		}
		h.fails(collab.OperationMission, map[string]any{"mission": "cli", "identity": map[string]any{"repository": "acme/app", "scope": "/protocols"}},
			collab.OutcomeNotFound)
		// A mission belongs to its repository: without one the request names
		// another mission.
		h.fails(collab.OperationMission, map[string]any{"mission": "anywhere"}, collab.OutcomeNotFound)
		if record := h.missionIn("anywhere", acme); record.Identity.Repository != "acme/app" {
			t.Fatalf("mission anywhere of acme/app: %+v", record)
		}
	})
}
