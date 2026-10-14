package cachekey

import (
	"reflect"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/store"
)

func TestCacheEventConversions(t *testing.T) {
	t.Parallel()
	raw := []jobmodel.RawJobEvent{
		{Version: 1, Type: jobmodel.EventTypeMeta, Time: "2026-06-10T00:00:00Z", Level: "info", Message: "kept", Data: map[string]any{"k": "v"}},
		{Version: 1, Type: jobmodel.EventTypeLog, Message: "dropped"},
		{Version: 1, Type: jobmodel.EventTypeSummary, Message: "summary"},
	}

	events := cacheEventsFromRaw(raw)
	if len(events) != 2 {
		t.Fatalf("cacheEventsFromRaw kept %d events, want 2", len(events))
	}
	if events[0].Type != jobmodel.EventTypeMeta || events[1].Type != jobmodel.EventTypeSummary {
		t.Fatalf("unexpected converted event types: %#v", events)
	}
	events[0].Data["k"] = "changed"
	if raw[0].Data["k"] != "v" {
		t.Fatal("cacheEventsFromRaw must clone event data")
	}

	back := rawEventsFromCache([]cache.ActionEvent{
		{Version: 1, Type: jobmodel.EventTypeMetric, Message: "metric", Data: map[string]any{"n": float64(1)}},
		{Version: 1},
	})
	if len(back) != 1 || back[0].Type != jobmodel.EventTypeMetric {
		t.Fatalf("rawEventsFromCache = %#v, want one metric event", back)
	}
	back[0].Data["n"] = float64(2)
	if got := rawEventsFromCache([]cache.ActionEvent{{Type: jobmodel.EventTypeMetric, Data: map[string]any{"n": float64(1)}}})[0].Data["n"]; got != float64(1) {
		t.Fatalf("rawEventsFromCache must clone event data, got %v", got)
	}
}

// TestCacheEventsRetainResultEventWithTestSummary pins a capture fix:
// the "result" event carries the job's testSummary/coverageSummary in its Data,
// so it MUST be persisted in the cache entry (and survive the round-trip back
// into a JobResult) for a cache hit to replay those summaries. Before the fix
// EventTypeResult was filtered out at capture, so warm JSONL streams lost the
// test/coverage summaries entirely.
func TestCacheEventsRetainResultEventWithTestSummary(t *testing.T) {
	t.Parallel()
	summary := map[string]any{"total": float64(12), "passed": float64(12), "failed": float64(0)}
	coverage := map[string]any{"pct": float64(87.5)}
	raw := []jobmodel.RawJobEvent{
		{Version: 1, Type: jobmodel.EventTypeMeta, Message: "meta"},
		{Version: 1, Type: jobmodel.EventTypeLog, Message: "dropped"},
		{Version: 1, Type: jobmodel.EventTypeResult, Time: "2026-07-20T00:00:00Z", Message: "result", Data: map[string]any{
			"status": "success",
			"data":   map[string]any{"testSummary": summary, "coverageSummary": coverage},
		}},
	}

	stored := cacheEventsFromRaw(raw)

	var cached *cache.ActionEvent
	for i := range stored {
		if stored[i].Type == jobmodel.EventTypeLog {
			t.Fatalf("log events must not be cached, got %#v", stored[i])
		}
		if stored[i].Type == jobmodel.EventTypeResult {
			cached = &stored[i]
		}
	}
	if cached == nil {
		t.Fatal("result event was not retained by cacheEventsFromRaw")
	}

	// The testSummary must survive the round-trip back into a JobResult so the
	// replay path can re-emit it byte-for-byte.
	back := rawEventsFromCache(stored)
	var replayed *jobmodel.RawJobEvent
	for i := range back {
		if back[i].Type == jobmodel.EventTypeResult {
			replayed = &back[i]
		}
	}
	if replayed == nil {
		t.Fatal("result event lost on rawEventsFromCache round-trip")
	}
	data, ok := replayed.Data["data"].(map[string]any)
	if !ok {
		t.Fatalf("result data missing nested data map: %#v", replayed.Data)
	}
	ts, ok := data["testSummary"].(map[string]any)
	if !ok || ts["passed"] != float64(12) {
		t.Fatalf("testSummary not preserved through cache round-trip: %#v", data)
	}
	if cov, ok := data["coverageSummary"].(map[string]any); !ok || cov["pct"] != float64(87.5) {
		t.Fatalf("coverageSummary not preserved through cache round-trip: %#v", data)
	}
}

// TestSynthesizeResultEvent covers the upgrade-compatibility path: cache
// keys omit the CLI version, so entries written before result events were
// captured stay valid across an upgrade with result.Data but no cached result
// event. SynthesizeResultEvent rebuilds the on-wire result event from the
// restored result so those legacy warm hits still carry testSummary/coverage.
func TestSynthesizeResultEvent(t *testing.T) {
	t.Parallel()
	summary := map[string]any{"passed": float64(7), "failed": float64(0)}
	result := &jobmodel.JobResult{
		Status: "success",
		Data:   map[string]any{"testSummary": summary},
	}
	ev, ok := SynthesizeResultEvent(result)
	if !ok {
		t.Fatal("synthesizeResultEvent should synthesize when result.Data is present")
	}
	// Stamped with the version the CLI advertises and is the only one it accepts
	// back, like every other CLI-synthesized event: a replayed
	// event must be indistinguishable from a captured one, version included.
	if ev.Type != jobmodel.EventTypeResult || ev.Version != runtimeproto.MaxKnownProtocolVersion {
		t.Fatalf("unexpected synthesized event envelope: %#v", ev)
	}
	if ev.Data["status"] != "success" {
		t.Errorf("synthesized status = %v, want success", ev.Data["status"])
	}
	// The report payload nests under "data" exactly as a subprocess emits it, so
	// the replayed job:event is indistinguishable from a captured one.
	data, ok := ev.Data["data"].(map[string]any)
	if !ok {
		t.Fatalf("synthesized event missing nested data map: %#v", ev.Data)
	}
	if ts, ok := data["testSummary"].(map[string]any); !ok || ts["passed"] != float64(7) {
		t.Fatalf("synthesized event lost testSummary: %#v", data)
	}

	// A failure carries its error alongside any data.
	errEv, ok := SynthesizeResultEvent(&jobmodel.JobResult{Status: "failed", Error: &jobmodel.JobError{Message: "boom", Code: "E_BOOM"}})
	if !ok {
		t.Fatal("synthesizeResultEvent should synthesize when an error is present")
	}
	errData, ok := errEv.Data["error"].(map[string]any)
	if !ok || errData["code"] != "E_BOOM" || errData["message"] != "boom" {
		t.Fatalf("synthesized error payload = %#v", errEv.Data["error"])
	}

	// Nothing report-relevant → no synthesis (plain builds stay untouched).
	if _, ok := SynthesizeResultEvent(&jobmodel.JobResult{Status: "success"}); ok {
		t.Error("data-less, error-free result must not synthesize an event")
	}
	if _, ok := SynthesizeResultEvent(nil); ok {
		t.Error("nil result must not synthesize an event")
	}
}

func TestJobResultCacheRoundTrips(t *testing.T) {
	t.Parallel()
	entry := &store.EntryResult{
		Status: "failed",
		Data:   map[string]any{"artifact": "out"},
		Error:  &store.EntryError{Message: "boom", Code: "E_BOOM"},
		Events: []cache.ActionEvent{{Version: 1, Type: jobmodel.EventTypeDiagnostic, Message: "diag"}},
	}

	job := JobResultFromEntryResult(entry)
	if job.Status != "failed" || job.Error == nil || job.Error.Code != "E_BOOM" || len(job.Events) != 1 {
		t.Fatalf("jobResultFromEntryResult = %#v", job)
	}
	if job.CacheHit || job.Coalesced {
		t.Fatalf("stored result unexpectedly retained run-local reuse outcome: %#v", job)
	}

	roundTrip := EntryResultFromJobResult(job)
	if roundTrip.Status != entry.Status || roundTrip.Error.Code != entry.Error.Code || len(roundTrip.Events) != 1 {
		t.Fatalf("entryResultFromJobResult = %#v", roundTrip)
	}

	action := ActionResultFromEntryResult(entry, 12, 34, true)
	if action.DurationMs != 12 || action.SizeBytes != 34 || action.Error.Code != "E_BOOM" || len(action.Events) != 1 {
		t.Fatalf("actionResultFromEntryResult = %#v", action)
	}
	withoutEvents := ActionResultFromEntryResult(entry, 0, 0, false)
	if len(withoutEvents.Events) != 0 {
		t.Fatalf("actionResultFromEntryResult without events kept %#v", withoutEvents.Events)
	}

	fromAction := JobResultFromActionResult(action)
	if fromAction.Status != entry.Status || fromAction.Error.Code != entry.Error.Code || !reflect.DeepEqual(fromAction.Data, entry.Data) {
		t.Fatalf("jobResultFromActionResult = %#v", fromAction)
	}
	if fromAction.CacheHit || fromAction.Coalesced {
		t.Fatalf("remote action result unexpectedly retained run-local reuse outcome: %#v", fromAction)
	}

	if JobResultFromEntryResult(nil).Status != "" || EntryResultFromJobResult(nil).Status != "" || JobResultFromActionResult(nil).Status != "" {
		t.Fatal("nil result conversions should return empty results")
	}
}

func TestSourceMutationMarkerRoundTripsWithoutLeakingIntoResultData(t *testing.T) {
	t.Parallel()
	job := &jobmodel.JobResult{
		Status:        "success",
		Data:          map[string]any{"formatted": float64(1)},
		SourceMutated: true,
	}

	entry := EntryResultFromJobResult(job)
	if entry.Data[sourceMutationDataKey] != true {
		t.Fatalf("cached source mutation marker = %#v, want true", entry.Data)
	}

	fromEntry := JobResultFromEntryResult(entry)
	if !fromEntry.SourceMutated || !reflect.DeepEqual(fromEntry.Data, job.Data) {
		t.Fatalf("local cache round trip = %#v, want marker plus public data %#v", fromEntry, job.Data)
	}
	if _, leaked := fromEntry.Data[sourceMutationDataKey]; leaked {
		t.Fatalf("cache-private marker leaked into local result data: %#v", fromEntry.Data)
	}

	action := ActionResultFromEntryResult(entry, 0, 0, false)
	fromAction := JobResultFromActionResult(action)
	if !fromAction.SourceMutated || !reflect.DeepEqual(fromAction.Data, job.Data) {
		t.Fatalf("remote cache round trip = %#v, want marker plus public data %#v", fromAction, job.Data)
	}
	if _, leaked := fromAction.Data[sourceMutationDataKey]; leaked {
		t.Fatalf("cache-private marker leaked into remote result data: %#v", fromAction.Data)
	}
}

func TestEntryResultFromJobResultKeepsOnlyCacheSummaryEvents(t *testing.T) {
	result := &jobmodel.JobResult{
		Status: "success",
		Events: []jobmodel.RawJobEvent{
			{Version: 1, Type: jobmodel.EventTypeMeta},
			{Version: 1, Type: jobmodel.EventTypeMetric, Data: map[string]any{"name": "tests-total", "value": float64(51)}},
			{Version: 1, Type: jobmodel.EventTypeLog, Message: "streaming output"},
			{Version: 1, Type: jobmodel.EventTypeProgress, Data: map[string]any{"current": float64(1), "total": float64(2)}},
			{Version: 1, Type: jobmodel.EventTypeSummary, Data: map[string]any{"message": "51/51 passed"}},
		},
	}

	entry := EntryResultFromJobResult(result)
	if len(entry.Events) != 3 {
		t.Fatalf("cached events = %d, want meta+metric+summary only: %+v", len(entry.Events), entry.Events)
	}
	for _, ev := range entry.Events {
		if ev.Type == jobmodel.EventTypeLog || ev.Type == jobmodel.EventTypeProgress {
			t.Fatalf("non-summary event %q should not be cached: %+v", ev.Type, entry.Events)
		}
	}
}
