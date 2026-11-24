package agentsession

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const lastPromptRequirement = "last-prompt"

// prompt edits a UserPromptSubmit payload to carry text.
func prompt(text string) func(map[string]any) {
	return func(payload map[string]any) { payload["prompt"] = text }
}

// sentPrompts answers the lastPrompt of each sync the provider received, from
// index from: "-" for a sync that carried none.
func sentPrompts(h *harness, from int) []string {
	var prompts []string
	for _, sync := range h.provider.recordedSyncs()[from:] {
		if sync.LastPrompt == "" {
			prompts = append(prompts, "-")
			continue
		}
		prompts = append(prompts, sync.LastPrompt)
	}
	return prompts
}

func wantPrompts(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("the syncs carried lastPrompt %q; want %q", got, want)
	}
}

func TestAPromptReachesTheNextSync(t *testing.T) {
	spectest.Proves(t, sessionFeature, lastPromptRequirement, "a-prompt-reaches-the-next-sync")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	calls := len(h.provider.recordedCalls())
	if out := h.hook(claude, "user-prompt-submit", claudeSession, prompt("  Fix the build.\nThen push.  ")); out != nil {
		t.Fatalf("the prompt hook printed %+v", out)
	}
	if got := len(h.provider.recordedCalls()); got != calls {
		t.Fatalf("the prompt hook made %d calls; want none", got-calls)
	}
	h.hook(claude, "stop", claudeSession, nil)
	h.hook(claude, "stop", claudeSession, lastMessage("Pushed."))
	wantPrompts(t, sentPrompts(h, 0), "-", "Fix the build.\nThen push.", "-")
	if cache := h.cache(claude, claudeSession); cache.LastPrompt != "Fix the build.\nThen push." || !cache.PromptSent {
		t.Fatalf("the cache holds %q, sent %v", cache.LastPrompt, cache.PromptSent)
	}

	// A failed sync keeps the request for the next one.
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Now the docs."))
	h.denySyncs(true)
	h.hook(claude, "stop", claudeSession, lastMessage("Docs are done."))
	h.denySyncs(false)
	h.hook(claude, "stop", claudeSession, lastMessage("Docs are pushed."))
	wantPrompts(t, sentPrompts(h, 3), "Now the docs.", "Now the docs.")
}

func TestAPromptIsCutAt4096Characters(t *testing.T) {
	spectest.Proves(t, sessionFeature, lastPromptRequirement, "a-prompt-is-cut-at-4096-characters")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "user-prompt-submit", claudeSession, prompt(strings.Repeat("é", collab.MaxLastPromptLength)+"tail"))
	h.hook(claude, "stop", claudeSession, nil)
	sent := h.provider.lastSync(t).LastPrompt
	if !utf8.ValidString(sent) || utf8.RuneCountInString(sent) != collab.MaxLastPromptLength || strings.Contains(sent, "tail") {
		t.Fatalf("the sync carried %d characters (valid UTF-8 %v); want the first %d", utf8.RuneCountInString(sent), utf8.ValidString(sent), collab.MaxLastPromptLength)
	}
}

func TestAnEmptyPromptSendsNothing(t *testing.T) {
	spectest.Proves(t, sessionFeature, lastPromptRequirement, "an-empty-prompt-sends-nothing")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "user-prompt-submit", claudeSession, prompt(""))
	h.hook(claude, "stop", claudeSession, nil)
	h.hook(claude, "user-prompt-submit", claudeSession, prompt(" \n\t "))
	h.hook(claude, "stop", claudeSession, lastMessage("Nothing to do."))
	wantPrompts(t, sentPrompts(h, 0), "-", "-", "-")
	if got := h.cache(claude, claudeSession).LastPrompt; got != "" {
		t.Fatalf("an empty request recorded %q", got)
	}

	// A Codex session records no request, whatever its payload says.
	h = newHarness(t)
	h.hook(codex, "session-start", codexSession, nil)
	payload := h.payload(claude, "user-prompt-submit", codexSession, nil)
	h.run([]string{"--engine", codex}, payload)
	h.hook(codex, "stop", codexSession, nil)
	wantPrompts(t, sentPrompts(h, 0), "-", "-")
}

func TestANewPromptReplacesTheLastOne(t *testing.T) {
	spectest.Proves(t, sessionFeature, lastPromptRequirement, "a-new-prompt-replaces-the-last-one")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("First request."))
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Second request."))
	h.hook(claude, "stop", claudeSession, nil)
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Third request."))
	h.hook(claude, "stop", claudeSession, lastMessage("Third done."))
	wantPrompts(t, sentPrompts(h, 0), "-", "Second request.", "Third request.")
	data, err := json.Marshal(h.cache(claude, claudeSession))
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"First request.", "Second request."} {
		if strings.Contains(string(data), old) {
			t.Fatalf("the cache keeps %q: %s", old, data)
		}
	}
}

func TestADeliveredMessageIsNotThePersonsPrompt(t *testing.T) {
	spectest.Proves(t, sessionFeature, lastPromptRequirement, "a-delivered-message-is-not-the-persons-prompt")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.seed(claude, claudeSession, func(cache *sessionCache) { cache.Seen = append(cache.Seen, "msg_owner1") })
	delivered := renderMessage(inboxMessage{ID: "msg_owner1", Kind: collab.InboxKindPrompt, Text: "Rebase on main.", CreatedAt: "2026-10-21T10:00:00Z"})
	h.hook(claude, "user-prompt-submit", claudeSession, prompt(delivered+"\n\n"+sessionReplyWays))
	h.hook(claude, "stop", claudeSession, nil)
	wantPrompts(t, sentPrompts(h, 0), "-", "-")
	if got := h.cache(claude, claudeSession).LastPrompt; got != "" {
		t.Fatalf("a delivered message recorded %q", got)
	}

	// A request that names a message the session never delivered is the
	// person's own.
	own := "What did (message id msg_other) say?"
	h.hook(claude, "user-prompt-submit", claudeSession, prompt(own))
	h.hook(claude, "stop", claudeSession, lastMessage("It asked for a rebase."))
	wantPrompts(t, sentPrompts(h, 2), own)
}

// promptRefusingCLI refuses a sync that carries lastPrompt as invalid, as a
// CLI that does not know the member does, and passes every other call on.
type promptRefusingCLI struct {
	Host
	refused int
	message string
}

func (o *promptRefusingCLI) Call(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error) {
	if operation == collab.OperationSync && strings.Contains(string(input), `"lastPrompt"`) {
		o.refused++
		return collab.Envelope{Contract: contract, Version: 1, Operation: operation, Outcome: collab.OutcomeInvalid,
			Error: &collab.Error{Message: o.message, Reason: collab.ReasonRequestInvalid}}, nil
	}
	return o.Host.Call(ctx, contract, operation, input)
}

func TestASyncRefusedForThePromptGoesOnceWithoutIt(t *testing.T) {
	spectest.Proves(t, sessionFeature, lastPromptRequirement, "a-sync-refused-for-the-prompt-goes-once-without-it")
	for _, message := range []string{`unknown member "lastPrompt"`, "last_prompt: the request is refused"} {
		h := newHarness(t)
		cli := &promptRefusingCLI{Host: h.provider, message: message}
		h.env.Host = cli
		h.hook(claude, "session-start", claudeSession, nil)
		h.hook(claude, "user-prompt-submit", claudeSession, prompt("Fix the build."))
		h.hook(claude, "stop", claudeSession, nil)
		h.hook(claude, "stop", claudeSession, lastMessage("Fixed."))
		if cli.refused != 1 {
			t.Fatalf("%s: %d syncs refused; want 1", message, cli.refused)
		}
		wantPrompts(t, sentPrompts(h, 0), "-", "-", "-")
		if !h.cache(claude, claudeSession).PromptSent {
			t.Fatalf("%s: a request refused by name is not recorded as sent", message)
		}
		if !logSays(h.hookLog(), "the person's request, which goes again with a new request") {
			t.Errorf("%s: hook.log %v", message, h.hookLog())
		}
	}

	// A refusal of the plan forgets the request, so the next sync carries it
	// again.
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "post-tool-use-todo-write", claudeSession, nil)
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Fix the build."))
	h.provider.refusePlan = true
	h.hook(claude, "stop", claudeSession, nil)
	from := len(h.provider.recordedSyncs())
	wantPrompts(t, sentPrompts(h, from-2), "Fix the build.", "-")
	if h.cache(claude, claudeSession).PromptSent {
		t.Fatal("a request dropped for the plan is recorded as sent")
	}
	if !logSays(h.hookLog(), "the person's request, which goes again with the next sync") {
		t.Errorf("hook.log %v", h.hookLog())
	}
	h.hook(claude, "stop", claudeSession, lastMessage("Fixed."))
	wantPrompts(t, sentPrompts(h, from), "Fix the build.")
	if !h.cache(claude, claudeSession).PromptSent {
		t.Fatal("the resent request is not recorded as sent")
	}
}

// A request takes no event claim: the same request sent again within the
// claim window is a new request, and a second hook source that sends the
// recorded request again changes nothing.
func TestARepeatedPromptIsANewRequest(t *testing.T) {
	spectest.Proves(t, sessionFeature, lastPromptRequirement, "a-new-prompt-replaces-the-last-one")
	h := newHarness(t)
	h.env.DuplicateWindow = duplicateEventWindow
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Request A."))
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Request B."))
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Request A."))
	h.hook(claude, "stop", claudeSession, nil)
	wantPrompts(t, sentPrompts(h, 0), "-", "Request A.")

	// The workspace and the user's hook files both send the next request.
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Request C."))
	h.hook(claude, "stop", claudeSession, lastMessage("C is done."))
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Request C."))
	h.hook(claude, "stop", claudeSession, lastMessage("C is pushed."))
	wantPrompts(t, sentPrompts(h, 2), "Request C.", "-")
}

// A session that moves to another mission sends that mission the request it
// already sent to the one it left, once.
func TestAMovedSessionSendsItsPromptToItsNewMission(t *testing.T) {
	spectest.Proves(t, sessionFeature, lastPromptRequirement, "a-prompt-reaches-the-next-sync")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.hook(claude, "user-prompt-submit", claudeSession, prompt("Build the parser."))
	h.hook(claude, "stop", claudeSession, nil)
	h.attach("--issue", "4883")
	h.hook(claude, "stop", claudeSession, lastMessage("Attached."))
	h.hook(claude, "stop", claudeSession, lastMessage("Still attached."))
	syncs := h.provider.recordedSyncs()
	wantPrompts(t, sentPrompts(h, 0), "-", "Build the parser.", "Build the parser.", "-")
	if syncs[2].Mission.Key != "issue-4883" || syncs[1].Mission.Key == "issue-4883" {
		t.Fatalf("the request went to missions %q then %q", syncs[1].Mission.Key, syncs[2].Mission.Key)
	}

	// A sync answer about the mission the session left records nothing for
	// the new one.
	cache := &sessionCache{LastPrompt: "Build the parser.", Mission: sessionMissionState{Key: "issue-4883"}}
	sent := "Build the parser."
	_ = mergeSyncAnswer(cache, syncSnapshot{missionKey: "branch-old", lastPrompt: &sent}, collab.MissionsSyncResult{}, h.now)
	if cache.PromptSent {
		t.Fatal("an answer about the mission left marks the request sent to the new one")
	}
}
