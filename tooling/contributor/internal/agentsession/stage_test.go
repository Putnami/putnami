package agentsession

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// stage runs `agent stage` with args and answers the exit code and both
// streams.
func (h *harness) stage(args ...string) (int, string, string) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	code := Stage(context.Background(), h.env, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAgentStageSyncsTheStageOfTheSessionTheHostNames(t *testing.T) {
	spectest.Proves(t, sessionFeature, stageRequirement, "agent-stage-syncs-the-stage-of-the-current-session")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, nil)
	syncs := len(h.provider.recordedSyncs())
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	h.advance(time.Minute)
	code, stdout, stderr := h.stage("code")
	if code != 0 || stderr != "" || stdout != "The session is at stage code, and its mission "+sessionBranchKey+" knows it.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := len(h.provider.recordedSyncs()); got != syncs+1 {
		t.Fatalf("%d syncs, want %d", got, syncs+1)
	}
	// The host runs the command inside a turn, so the session works.
	sync := h.provider.lastSync(t)
	if sync.Stage != collab.LoopStageCode || sync.State != collab.SessionStateWorking || sync.Event != sessionEventStage || sync.Session.ID != claudeSession {
		t.Errorf("sync %+v", sync)
	}
	// Every later sync carries the stage.
	h.hook(claude, "stop", claudeSession, nil)
	if sync := h.provider.lastSync(t); sync.Stage != collab.LoopStageCode {
		t.Errorf("the stop sync carries stage %q", sync.Stage)
	}
}

func TestAgentStageFindsTheSessionOfTheCheckout(t *testing.T) {
	spectest.Proves(t, sessionFeature, stageRequirement, "agent-stage-falls-back-to-the-session-of-the-checkout")
	h := newHarness(t)
	h.hook(codex, "session-start", codexSession, nil)
	h.hook(codex, "stop", codexSession, nil)
	state := h.cache(codex, codexSession).State
	code, stdout, stderr := h.stage("prove")
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "The session is at stage prove") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// Outside a turn, the stage leaves the state as it is.
	if sync := h.provider.lastSync(t); sync.Stage != collab.LoopStageProve || sync.State != state || sync.Event != sessionEventStage {
		t.Errorf("sync %+v, want state %s", sync, state)
	}
	// A session of another branch is not this checkout's.
	h.branch = "feat/other"
	if code, _, stderr := h.stage("merge"); code != 1 || !strings.HasPrefix(stderr, "agent stage: no agent session found for this checkout") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestAgentStageRecordsTheStageOfASessionWithoutMission(t *testing.T) {
	spectest.Proves(t, sessionFeature, stageRequirement, "agent-stage-records-the-stage-for-the-first-sync")
	h := newHarness(t)
	h.origin = ""
	h.hook(claude, "session-start", claudeSession, nil)
	syncs := len(h.provider.recordedSyncs())
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	code, stdout, stderr := h.stage("show")
	if code != 0 || stderr != "" || stdout != "The session is at stage show. It reports to no mission yet; its first sync carries the stage.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := len(h.provider.recordedSyncs()); got != syncs {
		t.Errorf("%d syncs, want %d", got, syncs)
	}
	if cache := h.cache(claude, claudeSession); cache.Stage != collab.LoopStageShow {
		t.Errorf("cached stage %q", cache.Stage)
	}
}

func TestAgentStageRefusesAnUnknownStageAndAMissingSession(t *testing.T) {
	spectest.Proves(t, sessionFeature, stageRequirement, "agent-stage-refuses-an-unknown-stage-and-a-missing-session")
	h := newHarness(t)
	for _, args := range [][]string{nil, {"release"}, {"code", "check"}} {
		if code, stdout, stderr := h.stage(args...); code != 2 || stdout != "" || !strings.HasPrefix(stderr, "agent stage: ") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	// A session id the hooks never opened names no session.
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	code, stdout, stderr := h.stage("code")
	if code != 1 || stdout != "" || stderr != "agent stage: no agent session found for this checkout: run it from a Claude Code or Codex session that runs the Putnami session hooks\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if len(h.provider.recordedSyncs()) != 0 {
		t.Errorf("syncs %+v", h.provider.recordedSyncs())
	}
}

func TestAgentStagePrintsTheTokensSpentSoFar(t *testing.T) {
	spectest.Proves(t, sessionFeature, stageRequirement, "agent-stage-prints-the-tokens-so-far")
	h := newHarness(t)
	dir := t.TempDir()
	main := filepath.Join(dir, claudeSession+".jsonl")
	appendLines(t, main, assistantLine("msg_1", 10, 5, 100, 1000)+"\n")
	appendLines(t, filepath.Join(dir, claudeSession, "subagents", "agent-a1.jsonl"), assistantLine("msg_2", 20, 0, 0, 0)+"\n")
	h.hook(claude, "session-start", claudeSession, withTranscript(main))
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	// The stage reads what the transcript added since the last hook.
	appendLines(t, main, assistantLine("msg_3", 1, 2, 3, 4)+"\n")
	code, stdout, stderr := h.stage("code")
	want := "The session is at stage code, and its mission " + sessionBranchKey + " knows it.\nIt has spent 1145 tokens so far.\n"
	if code != 0 || stderr != "" || stdout != want {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got, want := usageOf(t, h.provider.lastSync(t)), (collab.SessionUsage{InputTokens: 31, OutputTokens: 7, CacheWriteTokens: 103, CacheReadTokens: 1004}); got != want {
		t.Errorf("the stage sync carries usage %+v, want %+v", got, want)
	}
}
