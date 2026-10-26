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
// one lock around maps of sessions, messages and decisions. The missions
// scenarios must pass against it.
type missionsStore struct {
	mu        sync.Mutex
	next      int
	titles    map[string]string
	sessions  map[string]*missionsSession
	messages  map[string]*missionsMessage
	decisions map[string]*missionsDecision
	// asks maps an idempotency key to the decision it recorded.
	asks map[string]string
}

type missionsSession struct {
	state    collab.SessionState
	observed time.Time
	plan     []collab.PlanStep
}

type missionsMessage struct {
	mission   string
	kind      collab.InboxKind
	text      string
	createdAt string
	delivered bool
	reply     string
}

type missionsDecision struct {
	mission  string
	question string
	content  string
	state    collab.DecisionState
}

const missionsStoreTime = "2026-10-26T09:00:00Z"

func newMissionsStore() *missionsStore {
	return &missionsStore{
		titles: map[string]string{}, sessions: map[string]*missionsSession{},
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

// missionKey names a mission inside the store.
func missionKey(mission collab.MissionName) string { return mission.Repository + "\x00" + mission.Key }

func (s *missionsStore) id(prefix string) string {
	s.next++
	return fmt.Sprintf("%s_%d", prefix, s.next)
}

func (s *missionsStore) post(_ *testing.T, mission collab.MissionName, kind collab.InboxKind, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.id("msg")
	s.messages[id] = &missionsMessage{mission: missionKey(mission), kind: kind, text: text, createdAt: missionsStoreTime}
	return id
}

// record stores a reply, or says why it cannot.
func (s *missionsStore) record(message, reply string) (replayed bool, failure *collab.Failure) {
	stored, ok := s.messages[message]
	switch {
	case !ok:
		return false, collab.Fail(collab.OutcomeNotFound, "message.unknown", "no message %s", message)
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
	if in.Mission.Title != "" {
		s.titles[mission] = in.Mission.Title
	}
	session := s.sessions[in.Session.Engine+"/"+in.Session.ID]
	if session == nil {
		session = &missionsSession{}
		s.sessions[in.Session.Engine+"/"+in.Session.ID] = session
	}
	if session.observed.IsZero() || observed.After(session.observed) {
		session.state, session.observed = in.State, observed
		if in.Plan != nil {
			session.plan = slices.Clone(in.Plan.Steps)
		}
	}
	for _, id := range in.Delivered {
		if message, ok := s.messages[id]; ok && message.mission == mission {
			message.delivered = true
		}
	}
	result := &collab.MissionsSyncResult{
		Session:       collab.SessionStatus{State: session.state},
		Mission:       collab.MissionView{Key: in.Mission.Key, Title: s.titles[mission], Plan: collab.MissionPlan{Steps: slices.Clone(session.plan)}},
		MinIntervalMs: collab.MinSyncIntervalMs, Now: missionsStoreTime,
	}
	for _, id := range in.Decisions {
		if decision, ok := s.decisions[id]; ok && decision.mission == mission {
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
		if message.mission == mission && !message.delivered {
			result.Inbox = append(result.Inbox, collab.InboxMessage{ID: id, Kind: message.kind, Text: message.text, CreatedAt: message.createdAt})
		}
	}
	slices.SortFunc(result.Inbox, func(a, b collab.InboxMessage) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(result.Mission.OpenDecisions, func(a, b collab.OpenDecision) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

func (s *missionsStore) ask(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in := *call.Input.(*collab.MissionsAskInput)
	in.Session = nil
	content, _ := json.Marshal(in)
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.asks[in.IdempotencyKey]; ok {
		decision := s.decisions[id]
		if decision.content != string(content) {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch, "key %s asked another question", in.IdempotencyKey)
		}
		return &collab.MissionsAskResult{Decision: collab.MissionDecision{ID: id, State: decision.state}, Replayed: true}, nil
	}
	id := s.id("dec")
	s.decisions[id] = &missionsDecision{mission: missionKey(in.Mission), question: in.Question, content: string(content), state: collab.DecisionStateOpen}
	s.asks[in.IdempotencyKey] = id
	return &collab.MissionsAskResult{Decision: collab.MissionDecision{ID: id, State: collab.DecisionStateOpen}}, nil
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
	return Target{Handlers: store.handlers(), WorkspaceRoot: t.TempDir(), Post: store.post}
}

func TestTheMissionsScenariosPassAgainstACorrectProvider(t *testing.T) {
	RunMissions(t, func(t *testing.T) Target { return missionsTarget(t, newMissionsStore()) })
}
