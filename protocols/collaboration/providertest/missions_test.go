package providertest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

// missionsStore is the smallest provider that honors the missions contract:
// one lock around maps of missions, sessions, messages and decisions. It
// serves one caller, so every id it issued is the caller's. The missions
// scenarios must pass against it.
type missionsStore struct {
	mu        sync.Mutex
	next      int
	missions  map[string]*missionsMission
	sessions  map[string]*missionsSession
	messages  map[string]*missionsMessage
	decisions map[string]*missionsDecision
	// asks maps an idempotency key to the decision it recorded.
	asks map[string]string
}

// missionsMission is what the store holds of a mission once a sync applied:
// its title and the plan the session that published last sent.
type missionsMission struct {
	title string
	plan  []collab.PlanStep
}

type missionsSession struct {
	state    collab.SessionState
	observed time.Time
}

type missionsMessage struct {
	seq       int
	mission   string
	kind      collab.InboxKind
	text      string
	createdAt string
	delivered bool
	reply     string
	// decision is the decision an answer message answers.
	decision string
}

type missionsDecision struct {
	mission  string
	question string
	content  string
	state    collab.DecisionState
	answer   string
	option   string
}

const missionsStoreTime = "2026-10-26T09:00:00Z"

func newMissionsStore() *missionsStore {
	return &missionsStore{
		missions: map[string]*missionsMission{}, sessions: map[string]*missionsSession{},
		messages: map[string]*missionsMessage{}, decisions: map[string]*missionsDecision{}, asks: map[string]string{},
	}
}

func (s *missionsStore) handlers() map[collab.OperationKey]collab.Handler {
	key := func(operation string) collab.OperationKey {
		return collab.OperationKey{Contract: collab.ContractMissions, Version: 1, Operation: operation}
	}
	return map[collab.OperationKey]collab.Handler{
		key(collab.OperationSync):  s.sync,
		key(collab.OperationAsk):   s.ask,
		key(collab.OperationReply): s.reply,
	}
}

// missionKey names a mission inside the store: its repository and key,
// compared exactly.
func missionKey(mission collab.MissionName) string { return mission.Repository + "\x00" + mission.Key }

func (s *missionsStore) id(prefix string) string {
	s.next++
	return fmt.Sprintf("%s_%d", prefix, s.next)
}

// write adds one inbox message and returns its id.
func (s *missionsStore) write(message *missionsMessage) string {
	id := s.id("msg")
	message.seq, message.createdAt = s.next, missionsStoreTime
	s.messages[id] = message
	return id
}

func (s *missionsStore) post(_ *testing.T, mission collab.MissionName, kind collab.InboxKind, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(&missionsMessage{mission: missionKey(mission), kind: kind, text: text})
}

// answer records the owner's answer and writes it to the decision's mission
// as an answer message.
func (s *missionsStore) answer(t *testing.T, id, option, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	decision, ok := s.decisions[id]
	if !ok || decision.state != collab.DecisionStateOpen {
		t.Fatalf("no open decision %s to answer", id)
	}
	decision.state, decision.answer, decision.option = collab.DecisionStateAnswered, text, option
	s.write(&missionsMessage{mission: decision.mission, kind: collab.InboxKindAnswer, text: "Answer: " + text, decision: id})
}

// record stores a reply, or says why it cannot.
func (s *missionsStore) record(message, reply string) (replayed bool, failure *collab.Failure) {
	stored, ok := s.messages[message]
	switch {
	case !ok:
		return false, collab.Fail(collab.OutcomeNotFound, collab.ReasonMessageNotFound, "no message %s", message)
	case stored.reply == reply:
		return true, nil
	case stored.reply != "":
		return false, collab.Fail(collab.OutcomeConflict, collab.ReasonMessageClosed, "message %s holds another reply", message)
	}
	stored.reply = reply
	return false, nil
}

func (s *missionsStore) sync(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.MissionsSyncInput)
	mission := missionKey(in.Mission)
	observed, err := time.Parse(time.RFC3339Nano, in.ObservedAt)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "observedAt: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[in.Session.Engine+"/"+in.Session.ID]
	if session == nil {
		session = &missionsSession{}
		s.sessions[in.Session.Engine+"/"+in.Session.ID] = session
	}
	if session.observed.IsZero() || observed.After(session.observed) {
		session.state, session.observed = in.State, observed
		held := s.missions[mission]
		if held == nil {
			held = &missionsMission{}
			s.missions[mission] = held
		}
		if in.Mission.Title != "" {
			held.title = in.Mission.Title
		}
		if in.Plan != nil {
			held.plan = slices.Clone(in.Plan.Steps)
		}
	}
	for _, id := range in.Delivered {
		if message, ok := s.messages[id]; ok {
			message.delivered = true
			if decision, ok := s.decisions[message.decision]; ok {
				decision.state = collab.DecisionStateAcknowledged
			}
		}
	}
	result := &collab.MissionsSyncResult{
		Session:       collab.SessionStatus{State: session.state},
		Mission:       collab.MissionView{Key: in.Mission.Key},
		MinIntervalMs: collab.MinSyncIntervalMs, Now: missionsStoreTime,
	}
	if held := s.missions[mission]; held != nil {
		result.Mission.Title, result.Mission.Plan = held.title, collab.MissionPlan{Steps: slices.Clone(held.plan)}
	}
	reported := map[string]bool{}
	for _, id := range in.Decisions {
		if decision, ok := s.decisions[id]; ok && !reported[id] {
			reported[id] = true
			result.Decisions = append(result.Decisions, collab.DecisionStatus{ID: id, State: decision.state})
		}
	}
	for _, reply := range in.Replies {
		status := collab.ReplyStatus{Message: reply.Message, State: collab.ReplyStateReplied}
		if _, failure := s.record(reply.Message, reply.Reply); failure != nil {
			status.State, status.Reason = collab.ReplyStateRefused, failure.Error.Reason
		}
		result.Replies = append(result.Replies, status)
	}
	for id, decision := range s.decisions {
		if decision.mission == mission && decision.state == collab.DecisionStateOpen {
			result.Mission.OpenDecisions = append(result.Mission.OpenDecisions, collab.OpenDecision{ID: id, Question: decision.question})
		}
	}
	for id, message := range s.messages {
		if message.mission != mission || message.delivered {
			continue
		}
		entry := collab.InboxMessage{ID: id, Kind: message.kind, Text: message.text, CreatedAt: message.createdAt}
		if decision, ok := s.decisions[message.decision]; ok {
			entry.Decision = &collab.InboxDecision{ID: message.decision, Question: decision.question, Answer: decision.answer, Option: decision.option}
		}
		result.Inbox = append(result.Inbox, entry)
	}
	slices.SortFunc(result.Inbox, func(a, b collab.InboxMessage) int { return s.messages[a.ID].seq - s.messages[b.ID].seq })
	slices.SortFunc(result.Mission.OpenDecisions, func(a, b collab.OpenDecision) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

// view is a decision as missions.ask answers it.
func (d *missionsDecision) view(id string) collab.MissionDecision {
	return collab.MissionDecision{ID: id, State: d.state, Answer: d.answer, Option: d.option}
}

func (s *missionsStore) ask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := *call.Input.(*collab.MissionsAskInput)
	in.Mission.Title = ""
	content, _ := json.Marshal(in)
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.asks[in.IdempotencyKey]; ok {
		decision := s.decisions[id]
		if decision.content != string(content) {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch, "key %s asked another question", in.IdempotencyKey)
		}
		return &collab.MissionsAskResult{Decision: decision.view(id), Replayed: true}, nil
	}
	id := s.id("dec")
	decision := &missionsDecision{mission: missionKey(in.Mission), question: in.Question, content: string(content), state: collab.DecisionStateOpen}
	s.decisions[id] = decision
	s.asks[in.IdempotencyKey] = id
	return &collab.MissionsAskResult{Decision: decision.view(id)}, nil
}

func (s *missionsStore) reply(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := call.Input.(*collab.MissionsReplyInput)
	s.mu.Lock()
	defer s.mu.Unlock()
	replayed, failure := s.record(in.Message, in.Reply)
	if failure != nil {
		return nil, failure
	}
	return &collab.MissionsReplyResult{Message: collab.RepliedMessage{ID: in.Message, State: collab.ReplyStateReplied}, Replayed: replayed}, nil
}

func missionsTarget(t *testing.T, store *missionsStore) Target {
	return Target{Handlers: store.handlers(), WorkspaceRoot: t.TempDir(), Post: store.post, Answer: store.answer}
}

func TestTheMissionsScenariosPassAgainstACorrectProvider(t *testing.T) {
	RunMissions(t, func(t *testing.T) Target { return missionsTarget(t, newMissionsStore()) })
}
