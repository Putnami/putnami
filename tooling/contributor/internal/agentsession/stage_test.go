package agentsession

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
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
	h.writeWorkspace(true, true)
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	code, stdout, stderr := h.stage("code")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "agent stage: no agent session found for this checkout: ") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if len(h.provider.recordedSyncs()) != 0 {
		t.Errorf("syncs %+v", h.provider.recordedSyncs())
	}
}

func TestAgentStageSaysWhyItFindsNoSession(t *testing.T) {
	spectest.Proves(t, sessionFeature, stageRequirement, "agent-stage-says-why-no-session-is-found")
	h := newHarness(t)
	const prefix = "agent stage: no agent session found for this checkout: "
	expect := func(step, want string) {
		t.Helper()
		code, stdout, stderr := h.stage("code")
		if code != 1 || stdout != "" || stderr != prefix+want+"\n" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", step, code, stdout, stderr)
		}
	}
	// Without a host session id, the cause stays generic.
	h.writeWorkspace(true, true)
	expect("no session id", "run it from a Claude Code or Codex session that runs the Putnami session hooks")

	// The core writes no session hooks for a workspace that binds no missions
	// provider, so no session of it ever opens a cache, headless or not.
	h.environ["CLAUDE_CODE_SESSION_ID"] = claudeSession
	h.writeWorkspace(false, true)
	expect("unbound", "this workspace binds no missions provider, so putnami writes no session hooks: "+
		"bind missions under options.collaboration in putnami.workspace.json and run putnami install")
	// Nor for one that does not opt into this extension's agent content.
	h.writeWorkspace(true, false)
	expect("not opted in", "this workspace does not opt into the agent content of @putnami/contributor, so putnami writes no session hooks: "+
		"add extension:@putnami/contributor to agentArtifacts in putnami.workspace.json and run putnami install")

	// Both conditions hold, but the host file holds no hook yet.
	h.writeWorkspace(true, true)
	expect("not written", "the host names session "+claudeSession+", and .claude/settings.json holds no Putnami session hook: run putnami install to write them")
	// The hooks are written, but the host did not run them.
	h.writeFile(filepath.Join(".claude", "settings.json"), `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"./putnamiw agent hook --engine claude-code"}]}]}}`)
	hookLog := filepath.Join(h.env.Home, "contributor", "sessions", "hook.log")
	expect("not run", "the host names session "+claudeSession+", and .claude/settings.json holds the Putnami session hooks, but none recorded it: "+
		"check that the host trusts this workspace and runs its hooks, and read "+hookLog)
	// Codex reads its hooks from its own file.
	delete(h.environ, "CLAUDE_CODE_SESSION_ID")
	h.environ["CODEX_THREAD_ID"] = codexSession
	expect("codex", "the host names session "+codexSession+", and "+filepath.Join(".codex", "hooks.json")+" holds no Putnami session hook: run putnami install to write them")

	// Once a hook opened the session, the same command records the stage.
	h.hook(codex, "session-start", codexSession, nil)
	if code, _, stderr := h.stage("code"); code != 0 || stderr != "" {
		t.Errorf("recorded: exit %d, stderr %q", code, stderr)
	}
}

// writeWorkspace writes the harness workspace manifest: it binds missions
// and opts into this extension's agent content as asked.
func (h *harness) writeWorkspace(missions, optedIn bool) {
	h.t.Helper()
	document := map[string]any{}
	if missions {
		document["options"] = map[string]any{"collaboration": map[string]any{
			"missions": map[string]any{"provider": "@putnami/intelligence", "version": 1},
		}}
	}
	if optedIn {
		// The core trims the entry, so a padded one opts in too.
		document["agentArtifacts"] = []string{" extension: @putnami/contributor "}
	}
	data, err := json.Marshal(document)
	if err != nil {
		h.t.Fatal(err)
	}
	h.writeFile(workspaceManifest, string(data))
}

// writeFile writes one file under the harness workspace root.
func (h *harness) writeFile(name, content string) {
	h.t.Helper()
	path := filepath.Join(h.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
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
