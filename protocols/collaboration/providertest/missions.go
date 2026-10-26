package providertest

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

// RunMissions runs the missions v1 scenarios, each against a fresh target and
// in a mission of its own. A missions target can Post an inbox message.
func RunMissions(t *testing.T, newTarget func(t *testing.T) Target) {
	t.Helper()
	scenarios := []struct {
		name string
		run  func(t *testing.T, m *missionRun)
	}{
		{"a repeated ask replays the decision", missionsAskReplay},
		{"a key asked again with other content is a conflict", missionsAskMismatch},
		{"a repeated reply replays", missionsReplyReplay},
		{"another reply to a replied message is a conflict", missionsReplyClosed},
		{"a reply to an unknown message is not found", missionsReplyUnknown},
		{"a delivered message leaves the inbox", missionsDelivered},
		{"a sync ignores the ids it does not know", missionsUnknownIDs},
		{"each refused reply has its own entry", missionsRefusedReplies},
		{"an older sync never overwrites a newer one", missionsOlderSync},
		{"a sync without a plan keeps it and an empty plan clears it", missionsPlan},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			target := newTarget(t)
			if target.Post == nil {
				t.Fatal("a missions target can Post an inbox message")
			}
			run := Unique(t)
			scenario.run(t, &missionRun{
				Session: Open(t, target),
				run:     run,
				mission: collab.MissionName{Key: "pt-" + run, Title: "Scenario mission " + run},
				session: collab.SessionRef{Engine: "providertest", ID: "pt-" + run},
				start:   time.Now().UTC().Truncate(time.Second),
			})
		})
	}
}

// missionRun is one scenario: a session in a mission no other run uses.
type missionRun struct {
	*Session
	run     string
	mission collab.MissionName
	session collab.SessionRef
	// start is the scenario's clock; a sync at start.Add(d) is observed d
	// after it.
	start time.Time
}

func encode(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// unknownID is a message or decision id no provider issued.
func (m *missionRun) unknownID() string { return "pt-unknown-" + m.run }

// post writes one inbox message through the target and checks its id.
func (m *missionRun) post(kind collab.InboxKind, text string) string {
	m.t.Helper()
	id := m.target.Post(m.t, m.mission, kind, text)
	if diags := collab.ValidateInput(&collab.MissionsReplyInput{Message: id, Reply: "x"}); diags != nil {
		m.t.Fatalf("Post returned %q, which is not a message id: %s", id, collab.FormatDiagnostics(diags))
	}
	return id
}

// sync sends a sync observed after the scenario's start, changed by edit,
// and checks that the answer is about the mission it names and answers each
// reply it carries exactly once.
func (m *missionRun) sync(after time.Duration, edit func(in *collab.MissionsSyncInput)) collab.MissionsSyncResult {
	m.t.Helper()
	in := collab.MissionsSyncInput{
		Session: m.session, Mission: m.mission, Event: "post_tool_use", State: collab.SessionStateWorking,
		ObservedAt: m.start.Add(after).Format(time.RFC3339Nano),
	}
	if edit != nil {
		edit(&in)
	}
	var result collab.MissionsSyncResult
	m.OK(collab.ContractMissions, collab.OperationSync, encode(in), &result)
	if result.Mission.Key != m.mission.Key {
		m.t.Fatalf("a sync of mission %q answered mission %q", m.mission.Key, result.Mission.Key)
	}
	sent := make([]string, 0, len(in.Replies))
	answered := make([]string, 0, len(result.Replies))
	for _, reply := range in.Replies {
		sent = append(sent, reply.Message)
	}
	for _, reply := range result.Replies {
		answered = append(answered, reply.Message)
	}
	slices.Sort(sent)
	slices.Sort(answered)
	if !slices.Equal(sent, answered) {
		m.t.Fatalf("a sync carrying replies to %v answered replies to %v", sent, answered)
	}
	return result
}

func (m *missionRun) askArguments(key, question string) string {
	return encode(collab.MissionsAskInput{
		Mission: m.mission, Session: &m.session, Question: question,
		Options:        []collab.DecisionOption{{ID: "yes", Label: "Yes"}, {ID: "no", Label: "No"}},
		IdempotencyKey: key,
	})
}

// shipQuestion is the question the scenarios ask, unless they change it.
const shipQuestion = "Ship the change?"

// ask sends an ask of shipQuestion that must succeed.
func (m *missionRun) ask(key string) collab.MissionsAskResult {
	m.t.Helper()
	var result collab.MissionsAskResult
	m.OK(collab.ContractMissions, collab.OperationAsk, m.askArguments(key, shipQuestion), &result)
	return result
}

func replyArguments(message, reply string) string {
	return encode(collab.MissionsReplyInput{Message: message, Reply: reply})
}

// reply sends a reply that must succeed and checks it is about the message.
func (m *missionRun) reply(message, text string) collab.MissionsReplyResult {
	m.t.Helper()
	var result collab.MissionsReplyResult
	m.OK(collab.ContractMissions, collab.OperationReply, replyArguments(message, text), &result)
	if result.Message.ID != message {
		m.t.Fatalf("a reply to message %s answered message %s", message, result.Message.ID)
	}
	return result
}

// refused checks a non-ok answer: its reason when reason is set, and that
// repeating it as is cannot succeed.
func refused(t *testing.T, failure collab.Error, reason string) {
	t.Helper()
	if failure.Retryable || (reason != "" && failure.Reason != reason) {
		t.Fatalf("the refusal answered %+v; want reason %q, not retryable", failure, reason)
	}
}

func inboxHas(inbox []collab.InboxMessage, id string) bool {
	return slices.ContainsFunc(inbox, func(message collab.InboxMessage) bool { return message.ID == id })
}

func missionsAskReplay(t *testing.T, m *missionRun) {
	key := "providertest-ask-" + m.run
	first := m.ask(key)
	if first.Replayed || first.Decision.State != collab.DecisionStateOpen || first.Decision.Answer != "" {
		t.Fatalf("a first ask answered %+v; want an open decision, not replayed", first)
	}
	again := m.ask(key)
	if !again.Replayed || again.Decision.ID != first.Decision.ID {
		t.Fatalf("a repeated ask answered %+v; want decision %s replayed", again, first.Decision.ID)
	}
	result := m.sync(0, func(in *collab.MissionsSyncInput) { in.Decisions = []string{first.Decision.ID} })
	if want := []collab.DecisionStatus{{ID: first.Decision.ID, State: collab.DecisionStateOpen}}; !slices.Equal(result.Decisions, want) {
		t.Fatalf("a sync naming the decision answered %+v; want %+v", result.Decisions, want)
	}
	open := collab.OpenDecision{ID: first.Decision.ID, Question: shipQuestion}
	if !slices.Contains(result.Mission.OpenDecisions, open) {
		t.Fatalf("the mission's open decisions %+v lack %+v", result.Mission.OpenDecisions, open)
	}
}

func missionsAskMismatch(t *testing.T, m *missionRun) {
	key := "providertest-ask-" + m.run
	m.ask(key)
	failure := m.Fails(collab.ContractMissions, collab.OperationAsk, m.askArguments(key, "Wait for the review?"), collab.OutcomeConflict)
	refused(t, failure, collab.ReasonIdempotencyMismatch)
}

func missionsReplyReplay(t *testing.T, m *missionRun) {
	id := m.post(collab.InboxKindPrompt, "Does the gate pass?")
	first := m.reply(id, "Yes.")
	if first.Replayed || first.Message.State != collab.ReplyStateReplied {
		t.Fatalf("a first reply answered %+v; want replied, not replayed", first)
	}
	if again := m.reply(id, "Yes."); !again.Replayed {
		t.Fatalf("a repeated reply answered %+v; want it replayed", again)
	}
}

func missionsReplyClosed(t *testing.T, m *missionRun) {
	id := m.post(collab.InboxKindPrompt, "Does the gate pass?")
	m.reply(id, "Yes.")
	failure := m.Fails(collab.ContractMissions, collab.OperationReply, replyArguments(id, "No."), collab.OutcomeConflict)
	refused(t, failure, collab.ReasonMessageClosed)
}

func missionsReplyUnknown(t *testing.T, m *missionRun) {
	failure := m.Fails(collab.ContractMissions, collab.OperationReply, replyArguments(m.unknownID(), "Yes."), collab.OutcomeNotFound)
	refused(t, failure, "")
}

func missionsDelivered(t *testing.T, m *missionRun) {
	id := m.post(collab.InboxKindInfo, "The review is in.")
	before := m.sync(0, nil)
	if !inboxHas(before.Inbox, id) {
		t.Fatalf("the inbox %+v lacks the message %s posted to the mission", before.Inbox, id)
	}
	for _, message := range before.Inbox {
		if message.ID == id && (message.Kind != collab.InboxKindInfo || message.Text != "The review is in.") {
			t.Fatalf("the inbox answered %+v for the info message posted", message)
		}
	}
	delivered := m.sync(time.Second, func(in *collab.MissionsSyncInput) { in.Delivered = []string{id} })
	if inboxHas(delivered.Inbox, id) {
		t.Fatalf("the sync that delivered %s still lists it: %+v", id, delivered.Inbox)
	}
	if after := m.sync(2*time.Second, nil); inboxHas(after.Inbox, id) {
		t.Fatalf("a delivered message came back: %+v", after.Inbox)
	}
}

func missionsUnknownIDs(t *testing.T, m *missionRun) {
	decision := m.ask("providertest-ask-" + m.run).Decision
	id := m.post(collab.InboxKindInfo, "The review is in.")
	result := m.sync(0, func(in *collab.MissionsSyncInput) {
		in.Delivered = []string{m.unknownID(), id}
		in.Decisions = []string{m.unknownID(), decision.ID}
	})
	if want := []collab.DecisionStatus{{ID: decision.ID, State: collab.DecisionStateOpen}}; !slices.Equal(result.Decisions, want) {
		t.Fatalf("a sync naming an unknown decision answered %+v; want %+v", result.Decisions, want)
	}
	if inboxHas(result.Inbox, id) {
		t.Fatalf("an unknown delivered id kept the known one in the inbox: %+v", result.Inbox)
	}
}

func missionsRefusedReplies(t *testing.T, m *missionRun) {
	closed := m.post(collab.InboxKindPrompt, "First question?")
	m.reply(closed, "Yes.")
	fresh := m.post(collab.InboxKindPrompt, "Second question?")
	result := m.sync(0, func(in *collab.MissionsSyncInput) {
		in.Replies = []collab.MessageReply{{Message: closed, Reply: "No."}, {Message: fresh, Reply: "Done."}, {Message: m.unknownID(), Reply: "Hello."}}
	})
	for _, reply := range result.Replies {
		switch reply.Message {
		case closed:
			if reply.State != collab.ReplyStateRefused || reply.Reason != collab.ReasonMessageClosed {
				t.Fatalf("a second reply to %s answered %+v; want refused with %s", closed, reply, collab.ReasonMessageClosed)
			}
		case fresh:
			if reply.State != collab.ReplyStateReplied {
				t.Fatalf("a first reply to %s answered %+v; want replied", fresh, reply)
			}
		default:
			if reply.State != collab.ReplyStateRefused {
				t.Fatalf("a reply to an unknown message answered %+v; want refused", reply)
			}
		}
	}
	if again := m.reply(fresh, "Done."); !again.Replayed {
		t.Fatalf("the reply the sync recorded did not replay: %+v", again)
	}
}

func missionsOlderSync(t *testing.T, m *missionRun) {
	newer := collab.MissionPlan{Steps: []collab.PlanStep{{Key: "a", Status: collab.PlanStepDone, Text: "Newer plan"}}}
	handBack := func(in *collab.MissionsSyncInput) {
		in.Event, in.State, in.Plan = "stop", collab.SessionStateHandedBack, &newer
	}
	first := m.sync(time.Minute, handBack)
	if first.Session.State != collab.SessionStateHandedBack || !slices.Equal(first.Mission.Plan.Steps, newer.Steps) {
		t.Fatalf("a sync answered state %s and plan %+v; want %s and %+v", first.Session.State, first.Mission.Plan.Steps,
			collab.SessionStateHandedBack, newer.Steps)
	}
	older := m.sync(0, func(in *collab.MissionsSyncInput) {
		in.Plan = &collab.MissionPlan{Steps: []collab.PlanStep{{Key: "b", Status: collab.PlanStepTodo, Text: "Older plan"}}}
	})
	if older.Session.State != collab.SessionStateHandedBack || !slices.Equal(older.Mission.Plan.Steps, newer.Steps) {
		t.Fatalf("an older sync changed the session to state %s and plan %+v", older.Session.State, older.Mission.Plan.Steps)
	}
	if repeat := m.sync(time.Minute, handBack); repeat.Session.State != collab.SessionStateHandedBack || !slices.Equal(repeat.Mission.Plan.Steps, newer.Steps) {
		t.Fatalf("a repeated sync answered state %s and plan %+v", repeat.Session.State, repeat.Mission.Plan.Steps)
	}
}

func missionsPlan(t *testing.T, m *missionRun) {
	plan := collab.MissionPlan{Steps: []collab.PlanStep{
		{Key: "a", Status: collab.PlanStepDone, Text: "First step"},
		{Key: "b", Status: collab.PlanStepDoing, Text: "Second step"},
	}}
	if set := m.sync(0, func(in *collab.MissionsSyncInput) { in.Plan = &plan }); !slices.Equal(set.Mission.Plan.Steps, plan.Steps) {
		t.Fatalf("a sync with a plan answered plan %+v; want %+v", set.Mission.Plan.Steps, plan.Steps)
	}
	if kept := m.sync(time.Second, nil); !slices.Equal(kept.Mission.Plan.Steps, plan.Steps) {
		t.Fatalf("a sync without a plan answered plan %+v; want %+v kept", kept.Mission.Plan.Steps, plan.Steps)
	}
	cleared := m.sync(2*time.Second, func(in *collab.MissionsSyncInput) { in.Plan = &collab.MissionPlan{Steps: []collab.PlanStep{}} })
	if len(cleared.Mission.Plan.Steps) != 0 {
		t.Fatalf("a sync with an empty plan answered plan %+v", cleared.Mission.Plan.Steps)
	}
}
