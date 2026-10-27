package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/local-collaboration/internal/store"
)

const (
	missionArguments = `{"repository":"acme/app","key":"issue-7","title":"Ship the store"}`
	sessionArguments = `{"engine":"host-a","id":"s-1"}`
)

// missionsHarness is a harness on the default store of its workspace, with a
// way to write owner files into it.
type missionsHarness struct {
	*harness
	store *store.Store
}

func newMissionsHarness(t *testing.T) *missionsHarness {
	h := newHarness(t, "")
	return &missionsHarness{harness: h, store: store.Open(filepath.Join(h.workspace, filepath.FromSlash(DefaultRoot)))}
}

// sync sends a sync of the harness session to issue-7, observed after
// seconds past the clock, with extra members spliced in.
func (h *missionsHarness) sync(seconds int, extra string) collab.MissionsSyncResult {
	h.t.Helper()
	observed := time.Date(2026, 9, 24, 7, 0, seconds, 0, time.UTC).Format(time.RFC3339)
	arguments := `{"session":` + sessionArguments + `,"mission":` + missionArguments + `,"event":"post_tool_use","state":"working","observedAt":"` + observed + `"` + extra + `}`
	var result collab.MissionsSyncResult
	h.ok("missions", "sync", arguments, &result)
	return result
}

func (h *missionsHarness) ask(key string) collab.MissionsAskResult {
	h.t.Helper()
	var result collab.MissionsAskResult
	h.ok("missions", "ask", `{"mission":`+missionArguments+`,"question":"Ship the change?","options":[{"id":"yes","label":"Ship it"},{"id":"no","label":"Wait"}],"idempotencyKey":"`+key+`"}`, &result)
	return result
}

func (h *missionsHarness) missions() *store.Missions {
	h.t.Helper()
	var held *store.Missions
	if _, err := h.store.UpdateMissions(func(m *store.Missions) error { held = m; return nil }); err != nil {
		h.t.Fatal(err)
	}
	return held
}

func inboxIDs(inbox []collab.InboxMessage) []string {
	ids := make([]string, 0, len(inbox))
	for _, message := range inbox {
		ids = append(ids, message.ID)
	}
	return ids
}

// refusal answers the note beside an owner file, or "" when it has none.
func refusal(t *testing.T, s *store.Store, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.Root(), dir, name+store.RefusalSuffix))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestTheOwnerAnswersAndWritesMessagesAsFiles drives the file format the
// README documents: a message file reaches its mission's inbox once, an
// answer file answers its decision once, and a file the store cannot take
// stays untaken, as it is, with a note that says why, without failing any
// call.
func TestTheOwnerAnswersAndWritesMessagesAsFiles(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "missions-provider-semantics", "the-local-owner-answers-and-writes-messages-as-files")
	h := newMissionsHarness(t)

	writeRaw(t, h.store, store.MessagesDir, "gate.json", []byte(`{"mission":{"repository":"acme/app","key":"issue-7"},"kind":"prompt","text":"Does the gate pass?"}`))
	writeRaw(t, h.store, store.MessagesDir, "note.json", []byte(`{"mission":{"repository":"acme/app","key":"issue-7"},"text":"The review is in."}`))
	writeRaw(t, h.store, store.MessagesDir, "other.json", []byte(`{"mission":{"repository":"acme/app","key":"issue-8"},"text":"Not for issue-7."}`))
	for name, content := range map[string]string{
		"broken.json":    `{"mission":`,
		"unknown.json":   `{"mission":{"repository":"acme/app","key":"issue-7"},"text":"x","color":"red"}`,
		"trailing.json":  `{"mission":{"repository":"acme/app","key":"issue-7"},"text":"x"} {}`,
		"answer.json":    `{"mission":{"repository":"acme/app","key":"issue-7"},"kind":"answer","text":"x"}`,
		"upper.json":     `{"mission":{"repository":"Acme/App","key":"issue-7"},"text":"x"}`,
		"blank.json":     `{"mission":{"repository":"acme/app","key":"issue-7"},"text":"  "}`,
		".hidden.json":   `{"mission":{"repository":"acme/app","key":"issue-7"},"text":"hidden"}`,
		"draft.txt":      `{"mission":{"repository":"acme/app","key":"issue-7"},"text":"draft"}`,
		"too-long.json":  `{"mission":{"repository":"acme/app","key":"issue-7"},"text":"` + strings.Repeat("x", collab.MaxInboxTextLength+1) + `"}`,
		"oversized.json": `{"mission":{"repository":"acme/app","key":"issue-7"},"text":"oversized"}` + strings.Repeat(" ", store.MaxOwnerFileBytes),
	} {
		writeRaw(t, h.store, store.MessagesDir, name, []byte(content))
	}
	if err := os.Mkdir(filepath.Join(h.store.Root(), store.MessagesDir, "folder.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	first := h.sync(0, "")
	if got := inboxIDs(first.Inbox); !slices.Equal(got, []string{"M-1", "M-2"}) {
		t.Fatalf("the inbox lists %v; want the two valid issue-7 messages in name order", got)
	}
	if first.Inbox[0].Kind != collab.InboxKindPrompt || first.Inbox[0].Text != "Does the gate pass?" ||
		first.Inbox[1].Kind != collab.InboxKindInfo || first.Inbox[0].CreatedAt != "2026-09-24T08:00:00Z" {
		t.Fatalf("the inbox answered %+v", first.Inbox)
	}
	held := h.missions()
	if len(held.Messages) != 3 || held.Messages[0].Source == nil || held.Messages[0].Source.File != "gate.json" {
		t.Fatalf("the store took %+v; want gate, note and other", held.Messages)
	}
	for name, reason := range map[string]string{
		"broken.json":    "not one JSON object",
		"unknown.json":   `unknown field "color"`,
		"trailing.json":  "more than one JSON value",
		"answer.json":    `kind "answer" is not info or prompt`,
		"upper.json":     "mission.repository: ",
		"blank.json":     "text: ",
		"too-long.json":  "text: ",
		"oversized.json": "more than 256 KiB",
	} {
		note := refusal(t, h.store, store.MessagesDir, name)
		if !strings.HasPrefix(note, name+" is not taken: ") || !strings.Contains(note, reason) || !strings.HasSuffix(note, "\n") {
			t.Errorf("the note beside %s says %q; want why: %q", name, note, reason)
		}
	}
	for _, name := range []string{"gate.json", "note.json", "other.json", ".hidden.json", "draft.txt", "folder.json"} {
		if note := refusal(t, h.store, store.MessagesDir, name); note != "" {
			t.Errorf("%s has the note %q", name, note)
		}
	}
	if data, err := os.ReadFile(filepath.Join(h.store.Root(), store.MessagesDir, "broken.json")); err != nil || string(data) != `{"mission":` {
		t.Errorf("a refused file changed: %q, %v", data, err)
	}

	again := h.sync(1, "")
	if len(h.missions().Messages) != 3 || !slices.Equal(inboxIDs(again.Inbox), []string{"M-1", "M-2"}) {
		t.Fatalf("a file taken once was taken again: %+v", h.missions().Messages)
	}
	writeRaw(t, h.store, store.MessagesDir, "note.json", []byte(`{"mission":{"repository":"acme/app","key":"issue-7"},"text":"The second review is in."}`))
	if changed := h.sync(2, ""); !slices.Equal(inboxIDs(changed.Inbox), []string{"M-1", "M-2", "M-4"}) || changed.Inbox[2].Text != "The second review is in." {
		t.Fatalf("a taken file with new content answered %+v; want it as a new message", changed.Inbox)
	}

	asked := h.ask("ship").Decision
	other := h.ask("other").Decision
	free := h.ask("free").Decision
	writeOwnerFile(t, h.store, store.AnswersDir, asked.ID+".json", ownerAnswer{Option: "yes"})
	writeOwnerFile(t, h.store, store.AnswersDir, other.ID+".json", ownerAnswer{Option: "maybe"})
	writeOwnerFile(t, h.store, store.AnswersDir, free.ID+".json", ownerAnswer{Text: "Ask the reviewer first."})
	writeOwnerFile(t, h.store, store.AnswersDir, "D-99.json", ownerAnswer{Option: "yes"})
	answered := h.sync(3, `,"decisions":["`+asked.ID+`","`+other.ID+`","`+free.ID+`"]`)
	want := []collab.DecisionStatus{
		{ID: asked.ID, State: collab.DecisionStateAnswered},
		{ID: other.ID, State: collab.DecisionStateOpen},
		{ID: free.ID, State: collab.DecisionStateAnswered},
	}
	if !slices.Equal(answered.Decisions, want) {
		t.Fatalf("the decisions answered %+v; want %+v", answered.Decisions, want)
	}
	byDecision := map[string]collab.InboxDecision{}
	for _, message := range answered.Inbox {
		if message.Decision != nil {
			byDecision[message.Decision.ID] = *message.Decision
		}
	}
	if byDecision[asked.ID] != (collab.InboxDecision{ID: asked.ID, Question: "Ship the change?", Answer: "Ship it", Option: "yes"}) ||
		byDecision[free.ID] != (collab.InboxDecision{ID: free.ID, Question: "Ship the change?", Answer: "Ask the reviewer first."}) {
		t.Fatalf("the answer messages carry %+v", byDecision)
	}
	if !slices.Equal(answered.Mission.OpenDecisions, []collab.OpenDecision{{ID: other.ID, Question: "Ship the change?"}}) {
		t.Fatalf("the open decisions are %+v; want only %s, whose answer names no option of it", answered.Mission.OpenDecisions, other.ID)
	}

	if note := refusal(t, h.store, store.AnswersDir, other.ID+".json"); !strings.Contains(note, `has no option "maybe"; option takes one of the ids yes, no`) {
		t.Errorf("the note beside %s.json says %q", other.ID, note)
	}
	if note := refusal(t, h.store, store.AnswersDir, "D-99.json"); !strings.Contains(note, "this store holds no decision D-99") {
		t.Errorf("the note beside D-99.json says %q", note)
	}
	for _, id := range []string{asked.ID, free.ID} {
		if note := refusal(t, h.store, store.AnswersDir, id+".json"); note != "" {
			t.Errorf("a taken answer has the note %q", note)
		}
	}

	writeOwnerFile(t, h.store, store.AnswersDir, asked.ID+".json", ownerAnswer{Option: "no"})
	if replay := h.ask("ship"); replay.Decision.Answer != "Ship it" || replay.Decision.Option != "yes" {
		t.Fatalf("a second answer file changed the answer: %+v", replay.Decision)
	}
	if note := refusal(t, h.store, store.AnswersDir, asked.ID+".json"); note != "" {
		t.Errorf("an answer to a decision no longer open has the note %q", note)
	}

	// A fixed file is taken and loses its note; a removed file loses it too.
	writeRaw(t, h.store, store.MessagesDir, "broken.json", []byte(`{"mission":{"repository":"acme/app","key":"issue-7"},"text":"Fixed."}`))
	if err := os.Remove(filepath.Join(h.store.Root(), store.MessagesDir, "unknown.json")); err != nil {
		t.Fatal(err)
	}
	fixed := h.sync(4, "")
	if last := fixed.Inbox[len(fixed.Inbox)-1]; last.Text != "Fixed." {
		t.Errorf("the fixed file was not taken: %+v", fixed.Inbox)
	}
	for _, name := range []string{"broken.json", "unknown.json"} {
		if note := refusal(t, h.store, store.MessagesDir, name); note != "" {
			t.Errorf("%s keeps the note %q", name, note)
		}
	}
	if note := refusal(t, h.store, store.MessagesDir, "blank.json"); note == "" {
		t.Error("a file still refused lost its note")
	}
}

func TestAnOwnerDirectoryThatCannotBeReadIsUnavailable(t *testing.T) {
	h := newMissionsHarness(t)
	if err := os.MkdirAll(h.store.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.store.Root(), store.AnswersDir), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if failure := h.fails("missions", "ask", `{"mission":`+missionArguments+`,"question":"q","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}],"idempotencyKey":"k"}`,
		collab.OutcomeUnavailable); failure.Reason != "store.unreadable" || failure.Retryable {
		t.Fatalf("failure %+v", failure)
	}
	if err := os.Remove(filepath.Join(h.store.Root(), store.AnswersDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.store.Root(), store.MessagesDir), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if failure := h.fails("missions", "reply", `{"message":"M-1","reply":"x"}`, collab.OutcomeUnavailable); failure.Reason != "store.unreadable" {
		t.Fatalf("failure %+v", failure)
	}
}

func TestAMissionsDocumentOfAnotherVersionIsUnavailable(t *testing.T) {
	h := newMissionsHarness(t)
	if err := os.MkdirAll(h.store.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.store.Root(), "missions.json"), []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if failure := h.fails("missions", "reply", `{"message":"M-1","reply":"x"}`, collab.OutcomeUnavailable); failure.Reason != "store.unwritable" {
		t.Fatalf("failure %+v", failure)
	}
}

// TestTheInboxAndTheOpenDecisionsKeepTheirBounds: a sync lists at most 50
// messages and 64 KiB of text, oldest first, and at most 50 open decisions;
// the rest follow once the first are delivered or answered.
func TestTheInboxAndTheOpenDecisionsKeepTheirBounds(t *testing.T) {
	h := newMissionsHarness(t)
	for i := 0; i < collab.MaxInboxMessages+2; i++ {
		writeOwnerFile(t, h.store, store.MessagesDir, "m-"+strconv.Itoa(1000+i)+".json",
			ownerMessage{Mission: store.MissionKey{Repository: "acme/app", Key: "issue-7"}, Text: "message " + strconv.Itoa(i)})
	}
	full := h.sync(0, "")
	if len(full.Inbox) != collab.MaxInboxMessages || full.Inbox[0].ID != "M-1" || full.Inbox[49].ID != "M-50" {
		t.Fatalf("the inbox listed %d messages from %s", len(full.Inbox), full.Inbox[0].ID)
	}
	delivered, _ := json.Marshal(inboxIDs(full.Inbox))
	if rest := h.sync(1, `,"delivered":`+string(delivered)); !slices.Equal(inboxIDs(rest.Inbox), []string{"M-51", "M-52"}) {
		t.Fatalf("after the first 50 were delivered the inbox lists %v", inboxIDs(rest.Inbox))
	}
	h.sync(2, `,"delivered":["M-51","M-52"]`)

	large := strings.Repeat("€", collab.MaxInboxTextLength)
	for i := 0; i < 5; i++ {
		writeOwnerFile(t, h.store, store.MessagesDir, "large-"+strconv.Itoa(i)+".json",
			ownerMessage{Mission: store.MissionKey{Repository: "acme/app", Key: "issue-7"}, Text: large})
	}
	bounded := h.sync(3, "")
	if len(bounded.Inbox) != 2 || bounded.Inbox[0].ID != "M-53" {
		t.Fatalf("five messages of %d bytes listed %d from %v; want the oldest 2 within %d bytes",
			len(large), len(bounded.Inbox), inboxIDs(bounded.Inbox), collab.MaxContentBytes)
	}

	for i := 0; i <= collab.MaxOpenDecisions; i++ {
		h.ask("bound-" + strconv.Itoa(i))
	}
	open := h.sync(4, "").Mission.OpenDecisions
	if len(open) != collab.MaxOpenDecisions || open[0].ID != "D-1" {
		t.Fatalf("the mission listed %d open decisions from %+v", len(open), open[0])
	}
}

// TestAWriteThatChangesNothingLeavesTheDocument: a replayed reply and an older
// sync rewrite nothing, so a missions document is replaced only by a call that
// changed it.
func TestAWriteThatChangesNothingLeavesTheDocument(t *testing.T) {
	h := newMissionsHarness(t)
	writeOwnerFile(t, h.store, store.MessagesDir, "gate.json",
		ownerMessage{Mission: store.MissionKey{Repository: "acme/app", Key: "issue-7"}, Kind: collab.InboxKindPrompt, Text: "Does the gate pass?"})
	h.sync(10, `,"replies":[{"message":"M-1","reply":"Yes."}]`)
	path := filepath.Join(h.store.Root(), "missions.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var replay collab.MissionsReplyResult
	h.ok("missions", "reply", `{"message":"M-1","reply":"Yes."}`, &replay)
	if older := h.sync(5, ""); !replay.Replayed || older.Session.State != collab.SessionStateWorking {
		t.Fatalf("replay %+v, older sync %+v", replay, older)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("a call that changed nothing replaced the missions document")
	}
}
