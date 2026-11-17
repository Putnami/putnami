package agentsession

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

const duplicateEventRequirement = "duplicate-event"

// claimHarness is a harness whose hooks claim their events, as a launch does.
func claimHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.env.DuplicateWindow = duplicateEventWindow
	return h
}

func (h *harness) claims() []os.DirEntry {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.env.sessionsDir(), claimsDir))
	if err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	return entries
}

// The same event from a second hook source prints nothing and syncs nothing:
// the first hook handled it.
func TestASecondIdenticalEventIsSkipped(t *testing.T) {
	spectest.Proves(t, sessionFeature, duplicateEventRequirement, "a-second-identical-event-is-skipped")
	h := claimHarness(t)
	payload := h.payload(claude, "session-start", claudeSession, nil)

	if first := h.hookRaw(claude, payload); first == nil {
		t.Fatal("the first hook printed no mission sheet")
	}
	syncs := len(h.provider.syncs)
	code, stdout, stderr := h.run([]string{"--engine", claude}, payload)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("the duplicate exited %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if len(h.provider.syncs) != syncs {
		t.Fatalf("the duplicate synced: %d syncs, want %d", len(h.provider.syncs), syncs)
	}
	if got := len(h.claims()); got != 1 {
		t.Fatalf("%d claims, want 1", got)
	}
}

// Another event, the same event for another session, and the idle wake's own
// handler on the same event are each handled.
func TestADistinctEventIsHandled(t *testing.T) {
	spectest.Proves(t, sessionFeature, duplicateEventRequirement, "a-distinct-event-is-handled")
	h := claimHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	syncs := len(h.provider.syncs)
	h.hook(claude, "stop", claudeSession, lastMessage("Another turn."))
	if len(h.provider.syncs) == syncs {
		t.Fatal("a Stop with another payload was skipped")
	}
	if a, b := eventDigest(claude, false, []byte("{}")), eventDigest(claude, true, []byte("{}")); a == b {
		t.Fatal("the idle wake shares the digest of the ordinary Stop handler")
	}
	if a, b := eventDigest(claude, false, []byte("{}")), eventDigest(codex, false, []byte("{}")); a == b {
		t.Fatal("two engines share a digest")
	}
	if got := len(h.claims()); got != 3 {
		t.Fatalf("%d claims, want 3", got)
	}
}

// A claim older than the window no longer holds: the next run prunes it, and
// the same event, sent again, is handled again.
func TestAnOldMarkerIsPrunedAndReclaimed(t *testing.T) {
	spectest.Proves(t, sessionFeature, duplicateEventRequirement, "an-old-marker-is-pruned-and-reclaimed")
	h := claimHarness(t)
	payload := h.payload(claude, "stop", claudeSession, nil)
	h.hookRaw(claude, h.payload(claude, "session-start", claudeSession, nil))
	h.hookRaw(claude, payload)

	h.now = h.now.Add(duplicateEventWindow + time.Second)
	syncs := len(h.provider.syncs)
	h.hookRaw(claude, payload)
	if len(h.provider.syncs) == syncs {
		t.Fatal("the same event after the window was skipped")
	}
	// The session-start claim aged out and was pruned; the Stop claim was
	// taken again.
	if got := len(h.claims()); got != 1 {
		t.Fatalf("%d claims, want 1", got)
	}
}

// A claim that cannot be recorded is logged, and the event is handled.
func TestAnUnrecordableClaimHandlesTheEvent(t *testing.T) {
	h := claimHarness(t)
	sessions := h.env.sessionsDir()
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	// A file where the claims directory goes.
	if err := os.WriteFile(filepath.Join(sessions, claimsDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := h.hook(claude, "session-start", claudeSession, nil); out == nil {
		t.Fatal("the event was not handled")
	}
	if !logSays(h.hookLog(), "record the event claim") {
		t.Fatalf("hook.log %v", h.hookLog())
	}
}

// A zero window, the harness default, claims nothing.
func TestAZeroWindowClaimsNothing(t *testing.T) {
	h := newHarness(t)
	payload := h.payload(claude, "stop", claudeSession, nil)
	h.hookRaw(claude, payload)
	h.hookRaw(claude, payload)
	if got := len(h.claims()); got != 0 {
		t.Fatalf("%d claims, want none", got)
	}
}
