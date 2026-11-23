package agentsession

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	causeRequirement  = "sync-failure-cause"
	noticeRequirement = "failed-syncs-notice"
	// retiredNotice is what every surface prints for a checkout that binds
	// the retired Intelligence extension.
	retiredNotice = "This checkout binds @putnami/intelligence, which is retired; rebase on the default branch or bind @putnami/cloud."
	// lacksScope is the refusal a provider gave the syncs of checkouts that
	// still bound the retired extension.
	lacksScope = "the current Cloud session lacks intelligence.read; run `putnami cloud login` with an Intelligence-enabled account"
)

// bindWorkspace writes the workspace manifest of the harness checkout.
func (h *harness) bindWorkspace(manifest string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.root, workspaceManifest), []byte(manifest), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// A workspace that binds missions to a live provider, and one that still
// binds them to the retired Intelligence extension.
const (
	boundManifest   = `{"options":{"collaboration":{"missions":{"provider":"@acme/missions","version":1}}}}`
	retiredManifest = `{"extensions":["@putnami/intelligence"],"options":{"collaboration":{"missions":{"provider":"@putnami/intelligence","version":1}}}}`
)

// failSyncs makes every sync answer outcome with reason and message, or,
// with an empty outcome, answer again.
func (h *harness) failSyncs(outcome collab.Outcome, reason, message string) {
	h.t.Helper()
	if outcome == "" {
		h.provider.failSync = nil
		return
	}
	failed := h.provider.envelope(collab.ContractMissions, collab.OperationSync, outcome, nil, &collab.Error{Message: message, Reason: reason})
	h.provider.failSync = &failed
}

func TestAFailedSessionStartNamesTheCauseAndTheFix(t *testing.T) {
	spectest.Proves(t, sessionFeature, causeRequirement, "a-failed-session-start-names-the-cause-and-the-fix")
	h := newHarness(t)
	h.failSyncs(collab.OutcomeDenied, "auth.unavailable", "no signed-in session")
	text := contextOf(t, h.hook(claude, "session-start", claudeSession, nil))
	want := "The sync at this session start failed. The provider answered:\n> auth.unavailable: no signed-in session\nRun `putnami cloud login`, then start a new session."
	if !strings.Contains(text, want) || strings.Contains(text, "did not answer") {
		t.Errorf("context\n%s\nwant it to hold\n%s", text, want)
	}

	// A sheet an earlier start answered keeps its date beside the cause.
	h = newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.advance(time.Hour)
	h.failSyncs(collab.OutcomeUnavailable, "provider.unavailable", "the service is down")
	text = contextOf(t, h.hook(claude, "session-start", claudeSession, nil))
	want = "The sync at this session start failed; the sheet dates from 2026-10-21T10:00:00Z. The provider answered:\n> provider.unavailable: the service is down\n" +
		"Check the missions binding under options.collaboration in putnami.workspace.json; `putnami doctor` names the last hook failure."
	if !strings.Contains(text, want) {
		t.Errorf("context\n%s\nwant it to hold\n%s", text, want)
	}

	// A call that timed out is tried again by the next sync.
	reason := missedReason(fmt.Errorf("call missions.sync: %w", context.DeadlineExceeded))
	if got := syncFailureAction(reason, nil); got != "The next sync tries again." {
		t.Errorf("the action of %q is %q", reason, got)
	}
	// A provider text cannot pass for a line of the hook.
	if note := syncFailureNote("denied: first\nRun something else.", nil); !strings.Contains(note, "> denied: first\n> Run something else.") {
		t.Errorf("the cause is not quoted:\n%s", note)
	}
}

func TestASessionStartNamesARetiredExtension(t *testing.T) {
	spectest.Proves(t, sessionFeature, causeRequirement, "a-session-start-names-a-retired-extension")
	h := newHarness(t)
	h.bindWorkspace(retiredManifest)
	h.failSyncs(collab.OutcomeDenied, "auth.unavailable", lacksScope)
	out := h.hook(claude, "session-start", claudeSession, nil)
	text := contextOf(t, out)
	want := "The sync at this session start failed. The provider answered:\n> auth.unavailable: " + lacksScope + "\n" + retiredNotice
	if !strings.Contains(text, want) || strings.Count(text, retiredNotice) != 1 {
		t.Errorf("context\n%s\nwant it to hold, once,\n%s", text, want)
	}
	if strings.Contains(text, "Run `putnami cloud login`, then start a new session.") {
		t.Errorf("the context gives the sign-in advice that does not fix it:\n%s", text)
	}
	if out.SystemMessage != retiredNotice {
		t.Errorf("system message %q, want %q", out.SystemMessage, retiredNotice)
	}

	// A sync that passes still names the retired extension, first.
	h = newHarness(t)
	h.bindWorkspace(retiredManifest)
	out = h.hook(claude, "session-start", claudeSession, nil)
	if text := contextOf(t, out); !strings.HasPrefix(text, retiredNotice+"\n\n"+defaultSheet) || out.SystemMessage != retiredNotice {
		t.Errorf("context\n%s\nsystem message %q", text, out.SystemMessage)
	}

	// A workspace without a mission is told too.
	h = newHarness(t)
	h.bindWorkspace(retiredManifest)
	h.origin = ""
	if text := contextOf(t, h.hook(claude, "session-start", claudeSession, nil)); text != retiredNotice {
		t.Errorf("context\n%s", text)
	}
}

func TestThreeFailedSyncsInARowGiveOneNotice(t *testing.T) {
	spectest.Proves(t, sessionFeature, noticeRequirement, "three-failed-syncs-in-a-row-give-one-notice")
	h := newHarness(t)
	h.bindWorkspace(boundManifest)
	h.hook(claude, "session-start", claudeSession, nil)
	h.failSyncs(collab.OutcomeDenied, "auth.denied", "the session expired")
	stop := func(edit func(map[string]any)) string {
		t.Helper()
		h.advance(time.Minute)
		out := h.hook(claude, "stop", claudeSession, edit)
		if out == nil || out.HookSpecificOutput == nil {
			return ""
		}
		return out.HookSpecificOutput.AdditionalContext
	}
	for i := 1; i < syncNoticeAfter; i++ {
		if text := stop(nil); text != "" {
			t.Fatalf("failed sync %d gave %q", i, text)
		}
	}
	// The host already continues a Stop: the notice waits for the next one.
	if text := stop(func(payload map[string]any) { payload["stop_hook_active"] = true }); text != "" {
		t.Fatalf("a Stop the host continues gave %q", text)
	}
	text := stop(nil)
	want := "Putnami mission reporting: the last 4 syncs of this session failed, so its mission does not show its progress. Tell the person. " +
		"The provider answered:\n> auth.denied: the session expired\nRun `putnami cloud login`, then start a new session."
	if text != want {
		t.Fatalf("the notice is\n%s\nwant\n%s", text, want)
	}
	if !h.cache(claude, claudeSession).MissedNoticed {
		t.Fatal("the cache does not record the notice")
	}
	for range 3 {
		if text := stop(nil); text != "" {
			t.Fatalf("a later failed sync gave %q", text)
		}
	}
	if text := contextOf(t, h.hook(claude, "session-start", claudeSession, nil)); strings.Contains(text, "Putnami mission reporting") {
		t.Fatalf("the session start repeats the notice:\n%s", text)
	}

	// A sync that succeeds starts the count over.
	h.failSyncs("", "", "")
	if text := stop(nil); text != "" {
		t.Fatalf("an ok sync gave %q", text)
	}
	if cache := h.cache(claude, claudeSession); cache.Missed != nil || cache.MissedNoticed {
		t.Fatalf("an ok sync left %+v, noticed %v", cache.Missed, cache.MissedNoticed)
	}
	h.failSyncs(collab.OutcomeDenied, "auth.denied", "the session expired")
	h.hook(claude, "session-start", claudeSession, nil)
	stop(nil)
	text = contextOf(t, h.hook(claude, "session-start", claudeSession, nil))
	if strings.Count(text, "Putnami mission reporting: the last 3 syncs of this session failed") != 1 {
		t.Fatalf("the third failure in a row at session start gave\n%s", text)
	}
}

func TestAWorkspaceWithoutMissionsGetsNoNotice(t *testing.T) {
	spectest.Proves(t, sessionFeature, noticeRequirement, "three-failed-syncs-in-a-row-give-one-notice")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.provider.broken = errors.New("connection refused")
	for range syncNoticeAfter + 1 {
		h.advance(time.Minute)
		if out := h.hook(claude, "stop", claudeSession, nil); out != nil {
			t.Fatalf("a workspace that binds no missions gave %+v", out)
		}
	}
}
