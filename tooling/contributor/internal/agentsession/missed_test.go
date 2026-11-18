package agentsession

import (
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const missedRequirement = "missed-syncs"

// denySyncs makes every sync answer denied for want of a signed-in session,
// or, with false, answer again.
func (h *harness) denySyncs(deny bool) {
	h.t.Helper()
	if !deny {
		h.provider.failSync = nil
		return
	}
	denied := h.provider.envelope(collab.ContractMissions, collab.OperationSync, collab.OutcomeDenied, nil,
		&collab.Error{Message: "authentication is unavailable", Reason: "auth.unavailable"})
	h.provider.failSync = &denied
}

func TestAFailedSyncIsCountedAndTheNextOkSyncCarriesIt(t *testing.T) {
	spectest.Proves(t, sessionFeature, missedRequirement, "a-failed-sync-is-counted-and-the-next-ok-sync-carries-it")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.denySyncs(true)
	h.hook(claude, "stop", claudeSession, nil)
	h.advance(time.Minute)
	failedAt := h.now
	h.hook(claude, "stop", claudeSession, nil)
	missed := h.cache(claude, claudeSession).Missed
	want := collab.MissedSyncs{Count: 2, Reason: "auth.unavailable: authentication is unavailable", LastAt: failedAt.UTC().Format(time.RFC3339Nano)}
	if missed == nil || *missed != want {
		t.Fatalf("the cache holds %+v; want %+v", missed, want)
	}

	h.denySyncs(false)
	h.advance(time.Minute)
	h.hook(claude, "stop", claudeSession, nil)
	if sent := h.provider.lastSync(t).MissedSyncs; sent == nil || *sent != want {
		t.Fatalf("the next sync carried %+v; want %+v", sent, want)
	}
	if missed := h.cache(claude, claudeSession).Missed; missed != nil {
		t.Fatalf("an ok sync left %+v in the cache", *missed)
	}
	h.hook(claude, "stop", claudeSession, nil)
	if sent := h.provider.lastSync(t).MissedSyncs; sent != nil {
		t.Fatalf("the sync after carried %+v again", *sent)
	}
}

func TestASyncWithNoFailureBeforeItCarriesNoMissedSyncs(t *testing.T) {
	spectest.Proves(t, sessionFeature, missedRequirement, "a-sync-with-no-failure-before-it-carries-no-missed-syncs")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	for i, sync := range h.provider.recordedSyncs() {
		if sync.MissedSyncs != nil {
			t.Fatalf("sync %d carried %+v", i, *sync.MissedSyncs)
		}
	}
}

// TestAFailureDuringAnOkSyncStaysCounted: an ok sync clears only the missed
// syncs it carried, so one that failed while it ran goes with the next.
func TestAFailureDuringAnOkSyncStaysCounted(t *testing.T) {
	spectest.Proves(t, sessionFeature, missedRequirement, "a-failed-sync-is-counted-and-the-next-ok-sync-carries-it")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.denySyncs(true)
	h.hook(claude, "stop", claudeSession, nil)
	h.denySyncs(false)
	h.provider.onSync = func(collab.MissionsSyncInput) {
		h.provider.onSync = nil
		h.seed(claude, claudeSession, func(cache *sessionCache) { cache.Missed.Count++ })
	}
	h.hook(claude, "stop", claudeSession, nil)
	if missed := h.cache(claude, claudeSession).Missed; missed == nil || missed.Count != 1 {
		t.Fatalf("the cache holds %+v; want the one failure of the concurrent sync", missed)
	}
}

func TestASyncRefusedForMissedSyncsGoesOnceWithoutThem(t *testing.T) {
	spectest.Proves(t, sessionFeature, missedRequirement, "a-sync-refused-for-missed-syncs-goes-once-without-them")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.denySyncs(true)
	h.hook(claude, "stop", claudeSession, nil)
	h.denySyncs(false)
	h.provider.refuseMissed = true
	before := len(h.provider.recordedSyncs())
	h.hook(claude, "stop", claudeSession, nil)
	syncs := h.provider.recordedSyncs()[before:]
	if len(syncs) != 2 || syncs[0].MissedSyncs == nil || syncs[1].MissedSyncs != nil {
		t.Fatalf("syncs %+v; want one with missedSyncs, then one without", syncs)
	}
	if missed := h.cache(claude, claudeSession).Missed; missed != nil {
		t.Fatalf("a refused report stayed in the cache: %+v", *missed)
	}
	if !logSays(h.hookLog(), "the report of 1 missed syncs, which is dropped") {
		t.Errorf("hook.log %v", h.hookLog())
	}
}

func TestTheHookLogNamesTheWorkspaceRoot(t *testing.T) {
	spectest.Proves(t, sessionFeature, missedRequirement, "the-hook-log-names-the-workspace-root")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.denySyncs(true)
	h.hook(claude, "stop", claudeSession, nil)
	lines := h.hookLog()
	if len(lines) == 0 {
		t.Fatal("a failed sync left no line in hook.log")
	}
	for _, line := range lines {
		if line["root"] != h.env.Root || line["root"] == "" {
			t.Fatalf("hook.log line %v does not name the workspace root %q", line, h.env.Root)
		}
	}
}
