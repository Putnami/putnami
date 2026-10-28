package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/local-collaboration/internal/store"
)

// The missions of a local store have one owner: the person who works in the
// workspace. Every mission, decision and message the store holds is theirs,
// so a sync never meets an id that is not the caller's and no reply is
// refused as message.denied.
//
// The owner answers a decision and writes a message outside the contract, as
// files of the store directory (store.AnswersDir, store.MessagesDir). Every
// missions operation first takes the owner files it has not taken yet, under
// the store lock and in the same write as the operation, so two provider
// processes never take one file twice.
//
// The provider never changes or removes an owner file. Beside a file it does
// not take, it writes a note, <name>.refused, that says why, and it removes
// the note once the file is taken or gone. The note is the only report: a
// sync result has no member for the owner's mistakes, and the agent session
// that calls is not the person who wrote the file. The directory is where
// that person looks.
//
// Decision ids are D-<n> and message ids M-<n>, in the order the store
// issued them; the inbox lists a mission's messages in that order.

// takeOwnerFiles takes the answers, then the messages, the owner wrote since
// the last call. A file it cannot take stays untaken, with a note that says
// why, and is read again on every call until the owner fixes or removes it.
// A note that cannot be written never fails the call: the file stays
// untaken all the same.
func (p *Provider) takeOwnerFiles(s *store.Store, m *store.Missions) error {
	now := p.timestamp()
	for _, dir := range []string{store.AnswersDir, store.MessagesDir} {
		files, err := s.OwnerFiles(dir)
		if err != nil {
			return refuse(collab.Fail(collab.OutcomeUnavailable, "store.unreadable", "%v", err))
		}
		take := takeMessage
		if dir == store.AnswersDir {
			take = takeAnswer
		}
		refused := map[string]string{}
		for _, file := range files {
			if reason := take(m, file, now); reason != "" {
				refused[file.Name] = file.Name + " is not taken: " + reason
			}
		}
		_ = s.WriteRefusals(dir, refused)
	}
	return nil
}

// ownerAnswer is the content of an answer file: the option chosen, the
// answer's text, or both. Without a text, the answer is the option's label.
type ownerAnswer struct {
	Option string `json:"option,omitempty"`
	Text   string `json:"text,omitempty"`
}

// takeAnswer answers the open decision an answer file names and writes the
// answer message. It answers why it does not take the file, or "". The first
// answer taken stands: a file for a decision that is no longer open is not
// taken, and needs no note.
func takeAnswer(m *store.Missions, file store.OwnerFile, now string) string {
	id := strings.TrimSuffix(file.Name, ".json")
	index := decisionIndex(m, id)
	if index < 0 {
		return "this store holds no decision " + id
	}
	if m.Decisions[index].State != collab.DecisionStateOpen {
		return ""
	}
	if file.TooLarge {
		return tooLargeReason
	}
	var answer ownerAnswer
	if err := decodeStrict(file.Data, &answer); err != nil {
		return err.Error()
	}
	decision := &m.Decisions[index]
	text := answer.Text
	if answer.Option != "" {
		option := slices.IndexFunc(decision.Options, func(option collab.DecisionOption) bool { return option.ID == answer.Option })
		if option < 0 {
			ids := make([]string, 0, len(decision.Options))
			for _, option := range decision.Options {
				ids = append(ids, option.ID)
			}
			return fmt.Sprintf("decision %s has no option %q; option takes one of the ids %s", id, answer.Option, strings.Join(ids, ", "))
		}
		if text == "" {
			text = decision.Options[option].Label
		}
	}
	entry := collab.InboxMessage{
		ID: "M-1", Kind: collab.InboxKindAnswer, Text: text, CreatedAt: now,
		Decision: &collab.InboxDecision{ID: decision.ID, Question: decision.Question, Answer: text, Option: answer.Option},
	}
	if err := validInboxEntry(entry); err != nil {
		return err.Error()
	}
	decision.State, decision.Answer, decision.Option, decision.AnsweredAt = collab.DecisionStateAnswered, text, answer.Option, now
	m.Next.Message++
	m.Messages = append(m.Messages, store.Message{
		ID: "M-" + strconv.Itoa(m.Next.Message), Mission: decision.Mission, Kind: collab.InboxKindAnswer,
		Text: text, Decision: decision.ID, CreatedAt: now,
	})
	return ""
}

// tooLargeReason says why a file of more than store.MaxOwnerFileBytes is not
// taken.
const tooLargeReason = "the file holds more than 256 KiB"

// ownerMessage is the content of a message file. Kind is info or prompt, and
// info when absent.
type ownerMessage struct {
	Mission store.MissionKey `json:"mission"`
	Kind    collab.InboxKind `json:"kind,omitempty"`
	Text    string           `json:"text"`
}

// takeMessage writes the message a message file holds to its mission's
// inbox. It answers why it does not take the file, or "". A file is taken
// once for each content: the same name with the same content again is the
// message already taken.
func takeMessage(m *store.Missions, file store.OwnerFile, now string) string {
	if file.TooLarge {
		return tooLargeReason
	}
	sum := sha256.Sum256(file.Data)
	source := store.MessageSource{File: file.Name, Digest: hex.EncodeToString(sum[:])}
	if slices.ContainsFunc(m.Messages, func(message store.Message) bool {
		return message.Source != nil && *message.Source == source
	}) {
		return ""
	}
	var message ownerMessage
	if err := decodeStrict(file.Data, &message); err != nil {
		return err.Error()
	}
	if message.Kind == "" {
		message.Kind = collab.InboxKindInfo
	}
	if message.Kind != collab.InboxKindInfo && message.Kind != collab.InboxKindPrompt {
		return fmt.Sprintf("kind %q is not info or prompt", message.Kind)
	}
	if err := validMissionName(message.Mission); err != nil {
		return err.Error()
	}
	if err := validInboxEntry(collab.InboxMessage{ID: "M-1", Kind: message.Kind, Text: message.Text, CreatedAt: now}); err != nil {
		return err.Error()
	}
	m.Next.Message++
	m.Messages = append(m.Messages, store.Message{
		ID: "M-" + strconv.Itoa(m.Next.Message), Mission: message.Mission, Kind: message.Kind,
		Text: message.Text, Source: &source, CreatedAt: now,
	})
	return ""
}

// decodeStrict decodes one JSON object and refuses unknown members and
// trailing data.
func decodeStrict(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("the file is not one JSON object with only the documented members: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("the file holds more than one JSON value")
	}
	return nil
}

// validMissionName holds a mission named in an owner file to the rules a
// request's mission follows.
func validMissionName(key store.MissionKey) error {
	probe := collab.MissionsSyncInput{
		Session: collab.SessionRef{Engine: "local", ID: "owner"}, Mission: collab.MissionName{Repository: key.Repository, Key: key.Key},
		Event: "owner", State: collab.SessionStateWorking, ObservedAt: "2026-01-01T00:00:00Z",
	}
	return firstDiagnostic(collab.ValidateInput(&probe), "")
}

// validInboxEntry holds a message the owner wrote to the rules of the inbox
// it will be answered in, alone: a message the store takes always fits a
// sync's result.
func validInboxEntry(entry collab.InboxMessage) error {
	probe := collab.MissionsSyncResult{
		Session: collab.SessionStatus{State: collab.SessionStateWorking},
		Mission: collab.MissionView{Key: "probe", Plan: collab.MissionPlan{Steps: []collab.PlanStep{}}, OpenDecisions: []collab.OpenDecision{}},
		Inbox:   []collab.InboxMessage{entry}, Decisions: []collab.DecisionStatus{}, Replies: []collab.ReplyStatus{},
		MinIntervalMs: collab.MinSyncIntervalMs, Now: "2026-01-01T00:00:00Z",
	}
	return firstDiagnostic(collab.ValidateResult(&probe), "inbox[0].")
}

// firstDiagnostic states the first diagnostic as the member of the owner file
// it names, without the probe's prefix, or nil when there is none.
func firstDiagnostic(diags []diag.Diagnostic, prefix string) error {
	if len(diags) == 0 {
		return nil
	}
	first := diags[0]
	if field := strings.TrimPrefix(first.Field, prefix); field != "" {
		return fmt.Errorf("%s: %s", field, first.Message)
	}
	return errors.New(first.Message)
}

// updateMissions takes the owner files, then runs change, under the store
// lock and in one write. An unresolved write names its repeat as the
// reconciliation: every missions operation is safe to repeat.
func (p *Provider) updateMissions(call collab.Call, s *store.Store, change func(*store.Missions) error) *collab.Failure {
	_, err := s.UpdateMissions(func(m *store.Missions) error {
		if err := p.takeOwnerFiles(s, m); err != nil {
			return err
		}
		return change(m)
	})
	failure := writeFailure(err)
	if failure != nil && failure.Outcome == collab.OutcomeUnresolved {
		failure.Error.Reconcile = "repeat " + call.Contract + "." + call.Operation + " with the same document"
	}
	return failure
}

func keyOf(name collab.MissionName) store.MissionKey {
	return store.MissionKey{Repository: name.Repository, Key: name.Key}
}

func decisionIndex(m *store.Missions, id string) int {
	return slices.IndexFunc(m.Decisions, func(decision store.Decision) bool { return decision.ID == id })
}

func messageIndex(m *store.Missions, id string) int {
	return slices.IndexFunc(m.Messages, func(message store.Message) bool { return message.ID == id })
}

// distinct returns ids without repeats, in first-seen order.
func distinct(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// observedLater reports whether a sync observed at observed comes after the
// last one applied, at applied. A session never applied has none.
func observedLater(observed time.Time, applied string) bool {
	last, err := time.Parse(time.RFC3339Nano, applied)
	return err != nil || observed.After(last)
}

func (p *Provider) syncMission(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MissionsSyncInput](call)
	if failure != nil {
		return nil, failure
	}
	observed, err := time.Parse(time.RFC3339Nano, in.ObservedAt)
	if err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "observedAt: %v", err)
	}
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	mission := keyOf(in.Mission)
	var result *collab.MissionsSyncResult
	failure = p.updateMissions(call, s, func(m *store.Missions) error {
		now := p.timestamp()
		state := applySession(m, in, observed)
		for _, id := range distinct(in.Delivered) {
			deliver(m, id, now)
		}
		session := in.Session.Engine + "/" + in.Session.ID
		statuses := make([]collab.DecisionStatus, 0, len(in.Decisions))
		for _, id := range distinct(in.Decisions) {
			index := decisionIndex(m, id)
			if index < 0 {
				continue
			}
			decision := &m.Decisions[index]
			if decision.Session == "" {
				decision.Session = session
			}
			statuses = append(statuses, collab.DecisionStatus{ID: id, State: decision.State})
		}
		replies := make([]collab.ReplyStatus, 0, len(in.Replies))
		for _, reply := range in.Replies {
			status := collab.ReplyStatus{Message: reply.Message, State: collab.ReplyStateReplied}
			if _, refusal := recordReply(m, reply.Message, reply.Reply, now); refusal != nil {
				status.State, status.Reason = collab.ReplyStateRefused, refusal.Error.Reason
			}
			replies = append(replies, status)
		}
		result = &collab.MissionsSyncResult{
			Session:       collab.SessionStatus{State: state},
			Mission:       missionView(m, mission),
			Inbox:         inbox(m, mission),
			Decisions:     statuses,
			Replies:       replies,
			MinIntervalMs: collab.MinSyncIntervalMs,
			Now:           now,
		}
		return nil
	})
	if failure != nil {
		return nil, failure
	}
	return result, nil
}

// applySession applies a sync's observation of its session and its mission
// when it is later than the last one applied for the session, and returns the
// state the store holds for the session.
func applySession(m *store.Missions, in *collab.MissionsSyncInput, observed time.Time) collab.SessionState {
	index := slices.IndexFunc(m.Sessions, func(session store.Session) bool {
		return session.Engine == in.Session.Engine && session.ID == in.Session.ID
	})
	if index >= 0 && !observedLater(observed, m.Sessions[index].ObservedAt) {
		return m.Sessions[index].State
	}
	mission := keyOf(in.Mission)
	applied := store.Session{
		Engine: in.Session.Engine, ID: in.Session.ID, Mission: mission, State: in.State, ObservedAt: in.ObservedAt,
		Event: in.Event, Model: in.Model, Branch: in.Branch, LastMessage: in.LastMessage, Error: in.Error, WaitUntil: in.WaitUntil,
		Stage: in.Stage, Usage: in.Usage,
	}
	if index >= 0 {
		if applied.Stage == "" {
			applied.Stage = m.Sessions[index].Stage
		}
		if applied.Usage == nil {
			applied.Usage = m.Sessions[index].Usage
		}
	}
	if index < 0 {
		m.Sessions = append(m.Sessions, applied)
	} else {
		m.Sessions[index] = applied
	}
	held := slices.IndexFunc(m.Missions, func(held store.Mission) bool { return held.MissionKey == mission })
	if held < 0 {
		m.Missions = append(m.Missions, store.Mission{MissionKey: mission, Plan: []collab.PlanStep{}})
		held = len(m.Missions) - 1
	}
	if in.Mission.Title != "" {
		m.Missions[held].Title = in.Mission.Title
	}
	if in.Plan != nil {
		m.Missions[held].Plan = append([]collab.PlanStep{}, in.Plan.Steps...)
	}
	return in.State
}

// deliver marks a message delivered. Delivering an answer moves its decision
// from answered to acknowledged. An id the store does not hold changes
// nothing.
func deliver(m *store.Missions, id, now string) {
	index := messageIndex(m, id)
	if index < 0 {
		return
	}
	message := &m.Messages[index]
	if message.DeliveredAt == "" {
		message.DeliveredAt = now
	}
	if message.Kind != collab.InboxKindAnswer {
		return
	}
	if decision := decisionIndex(m, message.Decision); decision >= 0 && m.Decisions[decision].State == collab.DecisionStateAnswered {
		m.Decisions[decision].State = collab.DecisionStateAcknowledged
	}
}

// recordReply records the one reply a message takes. The same reply again
// replays; another reply is message.closed, and an unknown message
// message.not_found.
func recordReply(m *store.Missions, id, reply, now string) (bool, *collab.Failure) {
	index := messageIndex(m, id)
	if index < 0 {
		return false, collab.Fail(collab.OutcomeNotFound, collab.ReasonMessageNotFound, "no message %s in the local store", id)
	}
	message := &m.Messages[index]
	switch message.Reply {
	case reply:
		return true, nil
	case "":
		message.Reply, message.RepliedAt = reply, now
		return false, nil
	}
	return false, collab.Fail(collab.OutcomeConflict, collab.ReasonMessageClosed, "message %s holds another reply", id)
}

// missionView describes the named mission: its title and plan once a sync
// applied to it, and its oldest open decisions. A mission the store does not
// hold has an empty view.
func missionView(m *store.Missions, mission store.MissionKey) collab.MissionView {
	view := collab.MissionView{Key: mission.Key, Plan: collab.MissionPlan{Steps: []collab.PlanStep{}}, OpenDecisions: []collab.OpenDecision{}}
	if held := slices.IndexFunc(m.Missions, func(held store.Mission) bool { return held.MissionKey == mission }); held >= 0 {
		view.Title = m.Missions[held].Title
		view.Plan.Steps = append(view.Plan.Steps, m.Missions[held].Plan...)
	}
	for _, decision := range m.Decisions {
		if len(view.OpenDecisions) == collab.MaxOpenDecisions {
			break
		}
		if decision.Mission == mission && decision.State == collab.DecisionStateOpen {
			view.OpenDecisions = append(view.OpenDecisions, collab.OpenDecision{ID: decision.ID, Question: decision.Question})
		}
	}
	return view
}

// inbox lists the named mission's messages that no sync delivered, oldest
// first, up to the contract's bounds. It stops at the first message that
// does not fit, so a later message never passes an older one; every message
// fits an empty inbox, so the oldest is always listed.
func inbox(m *store.Missions, mission store.MissionKey) []collab.InboxMessage {
	entries := []collab.InboxMessage{}
	total := 0
	for _, message := range m.Messages {
		if message.Mission != mission || message.DeliveredAt != "" {
			continue
		}
		entry := collab.InboxMessage{ID: message.ID, Kind: message.Kind, Text: message.Text, CreatedAt: message.CreatedAt}
		size := len(entry.Text)
		if message.Kind == collab.InboxKindAnswer {
			decision := decisionIndex(m, message.Decision)
			if decision < 0 {
				continue
			}
			answered := m.Decisions[decision]
			entry.Decision = &collab.InboxDecision{ID: answered.ID, Question: answered.Question, Answer: answered.Answer, Option: answered.Option}
			size += len(answered.Question) + len(answered.Answer)
		}
		if len(entries) == collab.MaxInboxMessages || total+size > collab.MaxContentBytes {
			break
		}
		entries = append(entries, entry)
		total += size
	}
	return entries
}

// askContent is what an ask's idempotency key is compared on. The mission's
// title is not part of it.
type askContent struct {
	Repository     string                         `json:"repository"`
	Key            string                         `json:"key"`
	Question       string                         `json:"question"`
	Context        string                         `json:"context"`
	Options        []collab.DecisionOption        `json:"options"`
	Recommendation *collab.DecisionRecommendation `json:"recommendation"`
	Blocks         string                         `json:"blocks"`
}

func decisionView(decision store.Decision) collab.MissionDecision {
	return collab.MissionDecision{ID: decision.ID, State: decision.State, Answer: decision.Answer, Option: decision.Option}
}

func (p *Provider) askDecision(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MissionsAskInput](call)
	if failure != nil {
		return nil, failure
	}
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	digest := requestDigest(askContent{
		Repository: in.Mission.Repository, Key: in.Mission.Key, Question: in.Question, Context: in.Context,
		Options: in.Options, Recommendation: in.Recommendation, Blocks: in.Blocks,
	})
	var result *collab.MissionsAskResult
	failure = p.updateMissions(call, s, func(m *store.Missions) error {
		if index := slices.IndexFunc(m.Decisions, func(decision store.Decision) bool {
			return decision.IdempotencyKey == in.IdempotencyKey
		}); index >= 0 {
			if m.Decisions[index].Digest != digest {
				return refuse(collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
					"idempotency key %q was already used to ask another decision", in.IdempotencyKey))
			}
			result = &collab.MissionsAskResult{Decision: decisionView(m.Decisions[index]), Replayed: true}
			return nil
		}
		m.Next.Decision++
		decision := store.Decision{
			ID: "D-" + strconv.Itoa(m.Next.Decision), Mission: keyOf(in.Mission), IdempotencyKey: in.IdempotencyKey, Digest: digest,
			Question: in.Question, Context: in.Context, Options: in.Options, Recommendation: in.Recommendation, Blocks: in.Blocks,
			State: collab.DecisionStateOpen, AskedAt: p.timestamp(),
		}
		m.Decisions = append(m.Decisions, decision)
		result = &collab.MissionsAskResult{Decision: decisionView(decision)}
		return nil
	})
	if failure != nil {
		return nil, failure
	}
	return result, nil
}

func (p *Provider) replyToMessage(_ context.Context, call collab.Call) (any, *collab.Failure) {
	in, failure := inputOf[*collab.MissionsReplyInput](call)
	if failure != nil {
		return nil, failure
	}
	s, _, failure := storeOf(call)
	if failure != nil {
		return nil, failure
	}
	var result *collab.MissionsReplyResult
	failure = p.updateMissions(call, s, func(m *store.Missions) error {
		replayed, refusal := recordReply(m, in.Message, in.Reply, p.timestamp())
		if refusal != nil {
			return refuse(refusal)
		}
		result = &collab.MissionsReplyResult{Message: collab.RepliedMessage{ID: in.Message, State: collab.ReplyStateReplied}, Replayed: replayed}
		return nil
	})
	if failure != nil {
		return nil, failure
	}
	return result, nil
}
