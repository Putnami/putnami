package agentsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
)

const (
	// sessionSyncInterval is the default pace of the syncs of a wait. The
	// sync answer's minIntervalMs sets another one, within the bounds.
	// SessionStart, Stop and StopFailure sync every time, which the contract
	// allows.
	sessionSyncInterval        = 30 * time.Second
	sessionSyncIntervalFloor   = time.Duration(collab.MinSyncIntervalMs) * time.Millisecond
	sessionSyncIntervalCeiling = time.Duration(collab.MaxSyncIntervalMs) * time.Millisecond
	// inboxTextMax is one more character than a hook prints of a message, so
	// the rendering still sees that a cut text was longer, and
	// sessionInboxBytesMax bounds the fetched messages a cache keeps.
	inboxTextMax         = messageTextMax + 1
	sessionInboxBytesMax = collab.MaxContentBytes
)

// syncInterval is the pace the last sync answer set, within bounds.
func syncInterval(cache *sessionCache) time.Duration {
	if cache.MinIntervalMS <= 0 {
		return sessionSyncInterval
	}
	return min(max(time.Duration(cache.MinIntervalMS)*time.Millisecond, sessionSyncIntervalFloor), sessionSyncIntervalCeiling)
}

// sessionPublishable reports whether the session names a mission: a
// repository and a mission key.
func sessionPublishable(cache *sessionCache) bool {
	return cache.Repository != "" && cache.Mission.Key != ""
}

// syncSnapshot is what one sync request sent, so the answer clears exactly
// that and keeps what hooks added during the call.
type syncSnapshot struct {
	missionKey string
	planSent   bool
	planDigest string
	acks       []string
	decisions  []string
	replies    []string
}

// buildSyncRequest is the missions.sync request of the cache. The plan goes
// only when it changed since the last successful sync, and the last message
// with a Stop only.
func buildSyncRequest(cache *sessionCache, now time.Time) (collab.MissionsSyncInput, syncSnapshot) {
	request := collab.MissionsSyncInput{
		Session:    collab.SessionRef{Engine: cache.Engine, ID: cache.Session},
		Mission:    collab.MissionName{Repository: cache.Repository, Key: cache.Mission.Key},
		Event:      firstString(cache.LastEvent, sessionEventToolUse),
		State:      cache.State,
		ObservedAt: now.UTC().Format(time.RFC3339Nano),
		Model:      boundedLine(cache.Model, collab.MaxModelLength),
	}
	if request.State == "" {
		request.State = collab.SessionStateWorking
	}
	snapshot := syncSnapshot{missionKey: cache.Mission.Key}
	// The contract takes the branch as one line: a branch with a control
	// character, which git allows above ASCII, goes without one.
	if utf8.RuneCountInString(cache.Branch) <= collab.MaxBranchLength && !strings.ContainsFunc(cache.Branch, unicode.IsControl) {
		request.Branch = cache.Branch
	}
	if !cache.Mission.Known {
		request.Mission.Title = boundedLine(missionTitle(cache.Mission.Key), collab.MaxTitleLength)
	}
	switch request.State {
	case collab.SessionStateWaiting:
		if cache.WaitUntil.IsZero() {
			request.State = collab.SessionStateHandedBack
		} else {
			request.WaitUntil = cache.WaitUntil.UTC().Format(time.RFC3339Nano)
		}
	case collab.SessionStateFailed:
		request.Error = firstString(cache.Error, "unknown")
	}
	if cache.PlanDigest != cache.PlanSynced {
		steps := clonePlan(cache.Plan)
		if steps == nil {
			steps = []planStep{}
		}
		request.Plan = &collab.MissionPlan{Steps: steps}
		snapshot.planSent, snapshot.planDigest = true, cache.PlanDigest
	}
	if request.Event == sessionEventStop {
		request.LastMessage = cache.LastMessage
	}
	request.Delivered = append([]string(nil), cache.Acks[:min(len(cache.Acks), collab.MaxListMembers)]...)
	snapshot.acks = request.Delivered
	for _, reply := range cache.Replies[:min(len(cache.Replies), collab.MaxSyncReplies)] {
		request.Replies = append(request.Replies, reply)
		snapshot.replies = append(snapshot.replies, reply.Message)
	}
	request.Decisions = append([]string(nil), cache.Decisions[:min(len(cache.Decisions), collab.MaxSyncDecisions)]...)
	snapshot.decisions = request.Decisions
	return request, snapshot
}

// mergeSyncAnswer applies a sync answer to the cache as it is now. A message
// already delivered is never queued again, and an answer about a mission the
// session left changes nothing but the pace and what the sync sent. It
// answers the replies the provider refused: a refused reply is final, so the
// cache drops it and sends no other reply to that message.
func mergeSyncAnswer(cache *sessionCache, snapshot syncSnapshot, answer collab.MissionsSyncResult, now time.Time) error {
	cache.LastSyncAt = now
	if snapshot.planSent {
		cache.PlanSynced = snapshot.planDigest
	}
	cache.Acks = without(cache.Acks, snapshot.acks)
	cache.Decisions = without(cache.Decisions, snapshot.decisions)
	statuses := make(map[string]collab.ReplyStatus, len(answer.Replies))
	for _, status := range answer.Replies {
		statuses[status.Message] = status
	}
	var refused []string
	keptReplies := cache.Replies[:0]
	for _, reply := range cache.Replies {
		status, answered := statuses[reply.Message]
		switch {
		case !containsString(snapshot.replies, reply.Message) || !answered:
			keptReplies = append(keptReplies, reply)
		case status.State == collab.ReplyStateReplied:
			cache.Replied = appendBounded(cache.Replied, reply.Message, sessionSeenMax)
		default:
			// A refused reply is final: the message takes no other one.
			cache.Replied = appendBounded(cache.Replied, reply.Message, sessionSeenMax)
			refused = append(refused, fmt.Sprintf("%s (%s)", reply.Message, boundedLine(status.Reason, 64)))
		}
	}
	cache.Replies = keptReplies
	if answer.MinIntervalMs > 0 {
		cache.MinIntervalMS = int64(answer.MinIntervalMs)
	}
	var err error
	if len(refused) > 0 {
		err = fmt.Errorf("the provider refused the reply to %s; it is dropped", strings.Join(refused, ", "))
	}
	if snapshot.missionKey != cache.Mission.Key {
		return err
	}
	mergeMissionView(&cache.Mission, answer.Mission, now)
	mergeInbox(cache, answer.Inbox)
	return err
}

// mergeMissionView keeps the view a sync answered, bounded as the sheet
// prints it.
func mergeMissionView(mission *sessionMissionState, view collab.MissionView, now time.Time) {
	mission.Known = true
	mission.Title = boundedLine(view.Title, collab.MaxTitleLength)
	mission.Plan = nil
	for _, step := range view.Plan.Steps {
		if len(mission.Plan) == sessionPlanMaxSteps {
			break
		}
		if text := planText(step.Text); text != "" {
			mission.Plan = append(mission.Plan, planStep{Key: boundedLine(step.Key, collab.MaxTokenLength), Status: step.Status, Text: text})
		}
	}
	mission.OpenDecisions = nil
	for _, decision := range view.OpenDecisions {
		if len(mission.OpenDecisions) == collab.MaxOpenDecisions {
			break
		}
		if missionIDPattern.MatchString(decision.ID) {
			mission.OpenDecisions = append(mission.OpenDecisions, collab.OpenDecision{ID: decision.ID, Question: boundedText(decision.Question, collab.MaxQuestionLength)})
		}
	}
	mission.SheetAt = now
}

// mergeInbox queues the messages of a sync answer that no hook delivered,
// within sessionInboxMax messages and sessionInboxBytesMax bytes. A message
// left out is not reported as delivered: the next sync answers it again.
func mergeInbox(cache *sessionCache, inbox []inboxMessage) {
	inboxBytes := 0
	for _, message := range cache.Inbox {
		inboxBytes += inboxMessageBytes(message)
	}
	for _, message := range inbox {
		if len(cache.Inbox) >= sessionInboxMax {
			break
		}
		if !missionIDPattern.MatchString(message.ID) || containsString(cache.Seen, message.ID) || inboxHolds(cache.Inbox, message.ID) {
			continue
		}
		boundInboxMessage(&message)
		size := inboxMessageBytes(message)
		if len(cache.Inbox) > 0 && inboxBytes+size > sessionInboxBytesMax {
			break
		}
		inboxBytes += size
		cache.Inbox = append(cache.Inbox, message)
	}
}

// boundInboxMessage cuts a fetched message to what one hook can print: the
// rendering still sees that a cut text was longer.
func boundInboxMessage(message *inboxMessage) {
	message.Text = boundedText(message.Text, inboxTextMax)
	message.Kind = collab.InboxKind(boundedLine(string(message.Kind), 32))
	message.CreatedAt = boundedLine(message.CreatedAt, 64)
	if message.Decision == nil {
		return
	}
	if !missionIDPattern.MatchString(message.Decision.ID) {
		message.Decision = nil
		return
	}
	message.Decision.Question = boundedText(message.Decision.Question, collab.MaxQuestionLength+1)
	message.Decision.Answer = boundedText(message.Decision.Answer, inboxTextMax)
	message.Decision.Option = boundedLine(message.Decision.Option, 64)
}

func inboxMessageBytes(message inboxMessage) int {
	size := len(message.ID) + len(message.Kind) + len(message.Text) + len(message.CreatedAt)
	if message.Decision != nil {
		size += len(message.Decision.ID) + len(message.Decision.Question) + len(message.Decision.Answer) + len(message.Decision.Option)
	}
	return size
}

func inboxHolds(inbox []inboxMessage, id string) bool {
	for _, message := range inbox {
		if message.ID == id {
			return true
		}
	}
	return false
}

// without is values without the members of drop, in order.
func without(values, drop []string) []string {
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if !containsString(drop, value) {
			kept = append(kept, value)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// syncSession publishes the session through missions.sync and merges the
// answer. The session lock is held to read and to merge, never across the
// call. A session that names no mission publishes nothing and answers nil.
//
// What a sync sends leaves the cache only once the provider answered ok: a
// failed call and any other outcome keep the acknowledgements, the replies
// and the decisions for the next sync, which sends them again; the provider
// applies each id once. A sync the provider refuses as invalid while it
// carries the plan goes once more without the plan, so a plan the provider
// refuses never blocks every later sync; when that one passes, the plan
// counts as sent until it changes.
func (e *Env) syncSession(ctx context.Context, files sessionFiles) error {
	var request collab.MissionsSyncInput
	var snapshot syncSnapshot
	publishable := false
	dir := e.sessionsDir()
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			return nil, nil
		}
		refreshMissionKey(cache, dir)
		if publishable = sessionPublishable(cache); publishable {
			request, snapshot = buildSyncRequest(cache, e.Now())
		}
		return cache, nil
	}); err != nil || !publishable {
		return err
	}
	envelope, callErr := e.call(ctx, collab.ContractMissions, collab.OperationSync, request)
	var planRefused error
	if callErr == nil && envelope.Outcome == collab.OutcomeInvalid && request.Plan != nil && ctx.Err() == nil {
		first := outcomeError(envelope)
		request.Plan = nil
		if envelope, callErr = e.call(ctx, collab.ContractMissions, collab.OperationSync, request); callErr == nil && envelope.Outcome == collab.OutcomeOK {
			planRefused = fmt.Errorf("the provider refused the plan; it goes again once it changes: %w", first)
		}
	}
	var answer collab.MissionsSyncResult
	if callErr == nil && envelope.Outcome != collab.OutcomeOK {
		callErr = outcomeError(envelope)
	}
	if callErr == nil {
		if err := json.Unmarshal(envelope.Result, &answer); err != nil {
			callErr = fmt.Errorf("decode the missions.sync result: %w", err)
		} else if answer.Mission.Key != request.Mission.Key {
			callErr = errors.New("the missions.sync result names another mission")
		}
	}
	var refused error
	err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			return nil, nil
		}
		if callErr == nil {
			refused = mergeSyncAnswer(cache, snapshot, answer, e.Now())
		} else {
			cache.LastSyncAt = e.Now()
		}
		return cache, nil
	})
	return errors.Join(callErr, planRefused, refused, err)
}

// readCheckpoint reads the mission's checkpoint through memory.mission for
// the sheet. A workspace that binds no memory answers unsupported, which is
// no failure. Not found clears the checkpoint; any other failure keeps the
// one the cache holds.
func (e *Env) readCheckpoint(ctx context.Context, files sessionFiles) error {
	cache, err := files.read()
	if err != nil || cache == nil || !sessionPublishable(cache) {
		return err
	}
	key := cache.Mission.Key
	input := collab.MemoryMissionInput{Mission: key, Identity: collab.MemoryIdentity{Repository: cache.Repository}}
	envelope, err := e.call(ctx, collab.ContractMemory, collab.OperationMission, input)
	if err != nil {
		return err
	}
	var checkpoint *missionCheckpoint
	switch envelope.Outcome {
	case collab.OutcomeUnsupported:
		return nil
	case collab.OutcomeNotFound:
	case collab.OutcomeOK:
		var result collab.MemoryRecordResult
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			return fmt.Errorf("decode the memory.mission result: %w", err)
		}
		checkpoint = &missionCheckpoint{
			Revision:  boundedLine(result.Record.Revision, collab.MaxTokenLength),
			UpdatedAt: boundedLine(firstString(result.Record.Freshness.UpdatedAt, result.Record.Provenance.RecordedAt), 64),
			Content:   boundedBytes(result.Record.Content, sessionSheetMax),
		}
	default:
		return outcomeError(envelope)
	}
	err = lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil || cache.Mission.Key != key {
			return nil, nil
		}
		cache.Mission.Checkpoint = checkpoint
		return cache, nil
	})
	return err
}
