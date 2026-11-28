package scheduler

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// TestALocalHitNamesTheEntryItsEventsCameFrom serves a task from its local
// task-owned entry: the hit's result names the entry, the digest of its
// result.json and how many of its events the entry supplied, and the replay
// hands the renderers those events marked as the entry's.
func TestALocalHitNamesTheEntryItsEventsCameFrom(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-reused-task-records-its-cache-entry")
	job := coverageTestJob()
	f := newCaptureFixture(t, job)
	hash := matrixKey(t, f, job)
	cold := coverageRunResult(t, `{"percentage":75,"granularity":"lines","covered":30,"total":40,"threshold":70}`)
	if !f.sched.storeDeclaredCapture(job, cold, hash) {
		t.Fatal("the producing run published no entry")
	}
	entry, err := f.cache.LookupTaskEntry(hash)
	if err != nil || entry == nil || entry.ResultDigest == "" {
		t.Fatalf("published entry = %+v (%v), want one whose result is digested", entry, err)
	}

	hit := f.runDeclaredLookup(t, job, hash)
	if hit == nil || hit.Reuse != ReuseLocalCache {
		t.Fatalf("result = %+v, want a local hit", hit)
	}
	want := EntryRecords{Key: hash, Result: entry.ResultDigest, Events: len(entry.Result.Events)}
	if hit.EntryRecords == nil || *hit.EntryRecords != want || want.Events == 0 {
		t.Fatalf("EntryRecords = %+v, want %+v", hit.EntryRecords, want)
	}
	replayed := f.sched.renderer.(*mockRenderer).jobEvents[job.Key()]
	if len(replayed) < want.Events {
		t.Fatalf("replayed %d events, want at least the entry's %d", len(replayed), want.Events)
	}
	for i, event := range replayed {
		if event.FromCacheEntry != (i < want.Events) {
			t.Fatalf("replayed event %d (%s) FromCacheEntry = %v", i, event.Type, event.FromCacheEntry)
		}
	}
	for _, event := range hit.Events {
		if event.FromCacheEntry {
			t.Fatal("the result's own events carry the replay's mark")
		}
	}
}

// TestAReplayMarksOnlyTheEventsTheEntrySupplied replays a reused result whose
// entry supplied its first event: the event the reuse added this run, and
// every event of a result that names no entry, reach the renderers unmarked.
func TestAReplayMarksOnlyTheEventsTheEntrySupplied(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-reused-task-records-its-cache-entry")
	job := coverageTestJob()
	renderer := &mockRenderer{}
	s := &Scheduler{renderer: renderer}
	events := []RawJobEvent{{Version: 2, Type: EventTypeResult}, {Version: 2, Type: EventTypeArtifact}}

	s.replayResultEvents(job, &JobResult{Events: events, EntryRecords: &EntryRecords{Key: "k", Events: 1}})
	got := renderer.jobEvents[job.Key()]
	if len(got) != 2 || !got[0].FromCacheEntry || got[1].FromCacheEntry {
		t.Fatalf("replayed %+v, want the entry's result event marked and the added artifact not", got)
	}

	renderer.jobEvents = nil
	s.replayResultEvents(job, &JobResult{Events: events})
	for _, event := range renderer.jobEvents[job.Key()] {
		if event.FromCacheEntry {
			t.Fatalf("a result that names no entry replayed %+v marked", event)
		}
	}
}
