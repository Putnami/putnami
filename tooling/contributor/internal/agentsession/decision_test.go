package agentsession

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// askFixture is the missions.ask document the tests run `agent ask` with:
// no mission and no idempotency key.
var askFixture = filepath.Join("testdata", "decisions", "ask.json")

// asHost makes the host name the session of engine, as it does for the
// commands an agent runs.
func (h *harness) asHost(engine, session string) {
	for _, named := range sessionEnvironment {
		if named.engine == engine {
			h.environ[named.variable] = session
		}
	}
}

// ask runs `agent ask` with args and the given standard input, and answers
// the exit code and both streams.
func (h *harness) ask(stdin string, args ...string) (int, string, string) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	code := Ask(context.Background(), h.env, args, CommandIO{Stdin: strings.NewReader(stdin), Stdout: &stdout, Stderr: &stderr})
	return code, stdout.String(), stderr.String()
}

// askDecision runs `agent ask --input <fixture>` as the session of engine
// and fails the test unless it succeeds.
func (h *harness) askDecision(engine, session string) {
	h.t.Helper()
	h.asHost(engine, session)
	if code, _, stderr := h.ask("", "--input", askFixture); code != 0 {
		h.t.Fatalf("agent ask exited %d: %s", code, stderr)
	}
}

// reply runs `agent reply` with args and answers the exit code and both
// streams.
func (h *harness) reply(args ...string) (int, string, string) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	code := Reply(context.Background(), h.env, args, CommandIO{Stdout: &stdout, Stderr: &stderr})
	return code, stdout.String(), stderr.String()
}

func TestAnAskedDecisionBindsOnTheNextSync(t *testing.T) {
	spectest.Proves(t, sessionFeature, decisionRequirement, "an-asked-decision-binds-on-the-next-sync")
	for _, tc := range []struct{ engine, session, decision string }{
		{claude, claudeSession, decisionID},
		{codex, codexSession, "dec-codex"},
	} {
		h := newHarness(t)
		h.provider.decision = tc.decision
		h.hook(tc.engine, "session-start", tc.session, nil)
		syncs := len(h.provider.recordedSyncs())
		h.asHost(tc.engine, tc.session)
		code, stdout, stderr := h.ask("", "--input", askFixture)
		if code != 0 || stderr != "" || stdout != "DECISION_ID="+tc.decision+"\nDECISION_STATE=open\n" {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q", tc.engine, code, stdout, stderr)
		}
		// The command calls missions.ask for the session's mission and syncs
		// nothing: the id rides the next sync.
		if got := h.provider.recordedCalls(); got[len(got)-1] != "missions.ask" || len(h.provider.recordedSyncs()) != syncs {
			t.Errorf("%s: calls %v, syncs %d", tc.engine, got, len(h.provider.recordedSyncs()))
		}
		request := h.provider.asks[0]
		if request.Mission.Repository != sessionRepo || request.Mission.Key != sessionBranchKey || !strings.HasPrefix(request.IdempotencyKey, "ask-") || request.Question != "Which color should the logo be?" {
			t.Errorf("%s: ask %+v", tc.engine, request)
		}
		if cache := h.cache(tc.engine, tc.session); !slices.Equal(cache.Decisions, []string{tc.decision}) || !slices.Equal(cache.Asked, []string{tc.decision}) {
			t.Errorf("%s: decisions %v, asked %v", tc.engine, cache.Decisions, cache.Asked)
		}
		h.hook(tc.engine, "stop", tc.session, lastMessage("Asked."))
		if sync := h.provider.lastSync(t); !slices.Equal(sync.Decisions, []string{tc.decision}) {
			t.Errorf("%s: decisions %v", tc.engine, sync.Decisions)
		}
		if decisions := h.cache(tc.engine, tc.session).Decisions; decisions != nil {
			t.Errorf("%s: a bound decision stays queued: %v", tc.engine, decisions)
		}
	}

	// The same ask run again carries the same key, so the provider replays it.
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.askDecision(claude, claudeSession)
	h.askDecision(claude, claudeSession)
	if asks := h.provider.asks; len(asks) != 2 || asks[0].IdempotencyKey != asks[1].IdempotencyKey {
		t.Errorf("asks %+v", asks)
	}
	if decisions := h.cache(claude, claudeSession).Decisions; !slices.Equal(decisions, []string{decisionID}) {
		t.Errorf("a repeated ask queued %v", decisions)
	}
	// An explicit key and the mission the session reports to are kept.
	explicit := `{"mission":{"repository":"` + sessionRepo + `","key":"` + sessionBranchKey + `"},"question":"Which font?",` +
		`"options":[{"id":"a","label":"A"},{"id":"b","label":"B"}],"idempotencyKey":"font-1"}`
	if code, _, stderr := h.ask(explicit, "--input", "-"); code != 0 || h.provider.asks[2].IdempotencyKey != "font-1" {
		t.Errorf("exit %d, stderr %q, asks %+v", code, stderr, h.provider.asks)
	}

	// An ask that failed binds nothing.
	h = newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	failure := h.provider.envelope(collab.ContractMissions, collab.OperationAsk, collab.OutcomeInvalid, nil, &collab.Error{Message: "no", Reason: collab.ReasonRequestInvalid})
	h.provider.failAsk = &failure
	h.asHost(claude, claudeSession)
	code, stdout, stderr := h.ask("", "--input", askFixture)
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "agent ask: missions.ask answered invalid") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if decisions := h.cache(claude, claudeSession).Decisions; decisions != nil {
		t.Errorf("a failed ask queued %v", decisions)
	}
}

func TestAgentAskRefusesWhatItCannotBindToTheSession(t *testing.T) {
	spectest.Proves(t, sessionFeature, decisionRequirement, "an-asked-decision-binds-on-the-next-sync")
	h := newHarness(t)
	// A usage error exits 2 before the session is read.
	for _, args := range [][]string{nil, {"--input"}, {"--input", askFixture, "--input", askFixture}, {"--question", "x"}, {"--input", filepath.Join("testdata", "decisions", "absent.json")}} {
		if code, stdout, stderr := h.ask("", args...); code != 2 || stdout != "" || !strings.HasPrefix(stderr, "agent ask: ") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	for _, document := range []string{"", "{", `{"question":"x"} {}`, `{"question":"x","unknown":1}`} {
		if code, _, stderr := h.ask(document, "--input", "-"); code != 2 || !strings.HasPrefix(stderr, "agent ask: ") {
			t.Errorf("%q: exit %d, stderr %q", document, code, stderr)
		}
	}
	// A session the hooks never opened names no session.
	h.asHost(claude, claudeSession)
	code, _, stderr := h.ask("", "--input", askFixture)
	if code != 1 || !strings.HasPrefix(stderr, "agent ask: no agent session found for this checkout") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	// Another mission is not the session's.
	h.hook(claude, "session-start", claudeSession, nil)
	other := `{"mission":{"repository":"acme/other","key":"issue-7"},"question":"x","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}`
	if code, _, stderr := h.ask(other, "--input", "-"); code != 2 || !strings.Contains(stderr, "this session reports to branch:feat/session-hooks of acme/widgets") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	// A session without a mission has none to ask for.
	h = newHarness(t)
	h.origin = ""
	h.hook(claude, "session-start", claudeSession, nil)
	h.asHost(claude, claudeSession)
	if code, _, stderr := h.ask("", "--input", askFixture); code != 1 || !strings.Contains(stderr, "the session reports to no mission") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if len(h.provider.asks) != 0 {
		t.Errorf("asks %+v", h.provider.asks)
	}
}

func TestAgentAskPrintsTheResultInJSON(t *testing.T) {
	spectest.Proves(t, sessionFeature, decisionRequirement, "an-asked-decision-binds-on-the-next-sync")
	h := newHarness(t)
	h.hook(claude, "session-start", claudeSession, nil)
	h.asHost(claude, claudeSession)
	file, err := os.Open(askFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var stdout, stderr bytes.Buffer
	code := Ask(context.Background(), h.env, []string{"--input=-"}, CommandIO{Stdin: file, Stdout: &stdout, Stderr: &stderr, JSON: true})
	if want := `{"decision":{"id":"dec-logo","state":"open"},"replayed":false}` + "\n"; code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestARecordedReplyWinsOverAnAckLine(t *testing.T) {
	spectest.Proves(t, sessionFeature, decisionRequirement, "a-recorded-reply-wins-over-an-ack-line")
	closed := collab.Envelope{Contract: collab.ContractMissions, Version: 1, Operation: collab.OperationReply, Outcome: collab.OutcomeConflict,
		Error: &collab.Error{Message: "the message already holds another reply", Reason: collab.ReasonMessageClosed}}
	for _, tc := range []struct {
		engine, session string
		failure         *collab.Envelope
		code            int
	}{
		{claude, claudeSession, nil, 0},
		{codex, codexSession, &closed, 1},
	} {
		h := newHarness(t)
		h.provider.failReply = tc.failure
		h.provider.post(promptMessage())
		h.hook(tc.engine, "session-start", tc.session, nil)
		calls := len(h.provider.recordedCalls())
		code, stdout, stderr := h.reply("--message", promptID, "--reply", "Done: b.txt now holds the letter b.")
		if code != tc.code || (code == 0 && (stdout != "MESSAGE_ID=msg-prompt\nMESSAGE_STATE=replied\n" || stderr != "")) {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q", tc.engine, code, stdout, stderr)
		}
		if got := h.provider.recordedCalls()[calls:]; !slices.Equal(got, []string{"missions.reply"}) {
			t.Errorf("%s: the command called %v", tc.engine, got)
		}
		marker := repliedMarkerPath(h.env.sessionsDir(), promptID)
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("%s: marker %v", tc.engine, err)
		}
		out := h.hook(tc.engine, "stop", tc.session, lastMessage("Wrote b.txt.\n[ack msg-prompt] Done."))
		if out != nil {
			t.Errorf("%s: a replied prompt continued the turn: %+v", tc.engine, out)
		}
		sync := h.provider.lastSync(t)
		if sync.Replies != nil || !slices.Equal(sync.Delivered, []string{promptID}) {
			t.Errorf("%s: replies %+v, delivered %v", tc.engine, sync.Replies, sync.Delivered)
		}
		if cache := h.cache(tc.engine, tc.session); !slices.Equal(cache.Replied, []string{promptID}) || len(cache.Prompts) != 0 {
			t.Errorf("%s: replied %v, prompts %v", tc.engine, cache.Replied, cache.Prompts)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("%s: the marker stays: %v", tc.engine, err)
		}
	}

	// A message that holds a reply says so, and a reply the provider refused
	// for another reason leaves no marker.
	h := newHarness(t)
	h.provider.failReply = &closed
	if _, _, stderr := h.reply("--message=msg-prompt", "--reply=Done."); !strings.Contains(stderr, "message msg-prompt already holds a reply") {
		t.Errorf("stderr %q", stderr)
	}
	denied := h.provider.envelope(collab.ContractMissions, collab.OperationReply, collab.OutcomeDenied, nil, &collab.Error{Message: "no", Reason: "message.denied"})
	h = newHarness(t)
	h.provider.failReply = &denied
	code, _, stderr := h.reply("--message", promptID, "--reply", "Done.")
	if code != 1 || !strings.HasPrefix(stderr, "agent reply: missions.reply answered denied") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(repliedMarkerPath(h.env.sessionsDir(), promptID)); !os.IsNotExist(err) {
		t.Errorf("a denied reply left a marker: %v", err)
	}
}

func TestAgentReplyRefusesABadMessageOrAnEmptyReply(t *testing.T) {
	spectest.Proves(t, sessionFeature, decisionRequirement, "a-recorded-reply-wins-over-an-ack-line")
	h := newHarness(t)
	for _, args := range [][]string{nil, {"--message", promptID}, {"--reply", "x"}, {"--message", "a b", "--reply", "x"}, {"--message", promptID, "--reply", " "},
		{"--message", promptID, "--message", promptID, "--reply", "x"}, {"--message", promptID, "--reply"}, {"--message", promptID, "--reply", "x", "extra"}} {
		if code, stdout, stderr := h.reply(args...); code != 2 || stdout != "" || !strings.HasPrefix(stderr, "agent reply: ") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	if len(h.provider.replies) != 0 {
		t.Errorf("replies %+v", h.provider.replies)
	}
}
