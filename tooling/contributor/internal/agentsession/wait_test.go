package agentsession

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

func TestTheIdleWakeExitsOnlyOnAWaitingMessage(t *testing.T) {
	spectest.Proves(t, sessionFeature, wakeRequirement, "the-idle-wake-exits-75-only-on-a-waiting-message")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	if out := h.hook(claude, "stop", claudeSession, nil); out != nil {
		t.Fatalf("the stop continued: %+v", out)
	}
	h.onSleep = func(index int) {
		if index == 2 {
			h.provider.post(infoMessage())
		}
	}
	if text := h.wake(nil); text != infoRendered {
		t.Errorf("wake text\n%s\nwant\n%s", text, infoRendered)
	}
	if !slices.Equal(h.slept, []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Second}) {
		t.Errorf("slept %v", h.slept)
	}
	syncs := h.provider.recordedSyncs()
	if first := syncs[2]; first.State != collab.SessionStateWaiting || first.WaitUntil != "2026-10-21T10:10:00Z" {
		t.Errorf("the wait published %+v", first)
	}
	// The wake publishes the working state; the message goes as delivered
	// with the next sync, once the wake wrote it.
	if last := syncs[len(syncs)-1]; last.State != collab.SessionStateWorking || last.Delivered != nil {
		t.Errorf("the wake published %+v", last)
	}
	if cache := h.cache(claude, claudeSession); !containsString(cache.ContinuedFor, infoID) || cache.Waiter != "" || !slices.Equal(cache.Acks, []string{infoID}) {
		t.Errorf("continued for %v, waiter %q, acks %v", cache.ContinuedFor, cache.Waiter, cache.Acks)
	}
}

func TestTheIdleWakeEndsWithItsWindow(t *testing.T) {
	spectest.Proves(t, sessionFeature, wakeRequirement, "the-idle-wake-ends-after-its-window")
	h := newHarness(t)
	h.provider.pace = 60_000
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	if text := h.wake(nil); text != "" {
		t.Errorf("wake text %q", text)
	}
	if total := sum(h.slept); total != sessionIdleWakeWindow || len(h.slept) != 10 {
		t.Errorf("slept %v, want %v at the provider's pace", h.slept, sessionIdleWakeWindow)
	}
	if cache := h.cache(claude, claudeSession); cache.State != collab.SessionStateHandedBack || cache.Waiter != "" {
		t.Errorf("state %s, waiter %q", cache.State, cache.Waiter)
	}
	if h.hookLog() != nil {
		t.Errorf("hook.log %v", h.hookLog())
	}
}

func TestTheIdleWakeWaitsOnlyAfterItsOwnStop(t *testing.T) {
	spectest.Proves(t, sessionFeature, wakeRequirement, "the-idle-wake-exits-75-only-on-a-waiting-message")
	// No Stop decided this event: the wake gives up after idleWakeStopWait.
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	calls := len(h.provider.recordedCalls())
	if text := h.wake(nil); text != "" {
		t.Errorf("wake text %q", text)
	}
	if total := sum(h.slept); total != idleWakeStopWait || len(h.provider.recordedCalls()) != calls {
		t.Errorf("slept %v and called %v", total, h.provider.recordedCalls()[calls:])
	}
	if !logSays(h.hookLog(), "did not decide") {
		t.Errorf("hook.log %v", h.hookLog())
	}

	// A Stop of another event does not count.
	h = newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, lastMessage("An earlier turn."))
	if text := h.wake(nil); text != "" || sum(h.slept) != idleWakeStopWait {
		t.Errorf("wake text %q after %v", text, sum(h.slept))
	}

	// A turn its Stop continued has no idle time.
	h = newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.provider.post(promptMessage())
	h.hook(claude, "stop", claudeSession, nil)
	calls = len(h.provider.recordedCalls())
	if text := h.wake(nil); text != "" || h.slept != nil || len(h.provider.recordedCalls()) != calls {
		t.Errorf("wake text %q, slept %v", text, h.slept)
	}

	// A Stop payload without a prompt names no event to pair with.
	h = newHarness(t)
	if text := h.wake(func(payload map[string]any) { delete(payload, "prompt_id") }); text != "" || h.slept != nil {
		t.Errorf("wake text %q, slept %v", text, h.slept)
	}
	if !logSays(h.hookLog(), "names no prompt_id") {
		t.Errorf("hook.log %v", h.hookLog())
	}
}

func TestTheIdleWakeEndsOnANewTurn(t *testing.T) {
	spectest.Proves(t, sessionFeature, wakeRequirement, "the-idle-wake-ends-after-its-window")
	h := newHarness(t)
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"model":"model-a"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withTranscript := func(payload map[string]any) { payload["transcript_path"] = transcript }
	h.hook(claude, "session-start", claudeSession, withTranscript)
	h.hook(claude, "stop", claudeSession, withTranscript)
	h.onSleep = func(index int) {
		if index != 0 {
			return
		}
		file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.WriteString(`{"type":"user","message":{"content":"next"}}` + "\n")
		_ = file.Close()
	}
	if text := h.wake(withTranscript); text != "" {
		t.Errorf("wake text %q", text)
	}
	if len(h.slept) != 1 {
		t.Errorf("slept %v", h.slept)
	}
	if last := h.provider.lastSync(t); last.State != collab.SessionStateWorking {
		t.Errorf("the wait ended with %+v", last)
	}
}

func TestAnSDKHostNeverWaits(t *testing.T) {
	spectest.Proves(t, sessionFeature, wakeRequirement, "the-idle-wake-exits-75-only-on-a-waiting-message")
	h := newHarness(t)
	h.environ["CLAUDE_CODE_ENTRYPOINT"] = "sdk-cli"
	h.provider.post(promptMessage())
	if text := h.wake(nil); text != "" || h.slept != nil || h.provider.recordedCalls() != nil || h.hookLog() != nil {
		t.Errorf("an SDK host waited: %q, %v, %v", text, h.slept, h.provider.recordedCalls())
	}
	// The other hooks still run for an SDK host.
	if out := h.hook(claude, "session-start", claudeSession, nil); out == nil {
		t.Error("the session start printed nothing")
	}
}

func TestTheIdleWakeRunsOnTheClaudeCodeStopOnly(t *testing.T) {
	spectest.Proves(t, sessionFeature, wakeRequirement, "the-idle-wake-exits-75-only-on-a-waiting-message")
	h := newHarness(t)
	h.provider.post(promptMessage())
	for _, run := range []struct {
		engine, fixture, session string
	}{
		{codex, "stop", codexSession},
		{claude, "session-start", claudeSession},
	} {
		code, stdout, stderr := h.run([]string{"--engine", run.engine, "--idle-wake"}, h.payload(run.engine, run.fixture, run.session, nil))
		if code != 0 || stdout != "" || stderr != "" {
			t.Errorf("%s %s: exit %d, %q, %q", run.engine, run.fixture, code, stdout, stderr)
		}
	}
	if !logSays(h.hookLog(), "runs on the Claude Code Stop event only") {
		t.Errorf("hook.log %v", h.hookLog())
	}
}
