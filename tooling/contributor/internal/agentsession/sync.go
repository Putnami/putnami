package agentsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// missed is the count of missed syncs the request carried.
	missed int64
}

// buildSyncRequest is the missions.sync request of the cache. The plan goes
// when it differs from the last plan sent to the session's mission. An empty
// plan goes only to a mission this session sent a plan to, so it does not
// clear a plan another session published there. A plan the provider refused
// and a plan a failed call carried count as sent: in those two cases an empty
// plan can still clear another session's plan. The last message goes with a
// Stop only.
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
	if slices.Contains(collab.ValidLoopStages, cache.Stage) {
		request.Stage = cache.Stage
	}
	if cache.Usage != nil {
		totals := cache.Usage.Totals
		request.Usage = &collab.SessionUsage{
			InputTokens: tokenCount(totals.InputTokens), OutputTokens: tokenCount(totals.OutputTokens),
			CacheWriteTokens: tokenCount(totals.CacheWriteTokens), CacheReadTokens: tokenCount(totals.CacheReadTokens),
		}
	}
	snapshot := syncSnapshot{missionKey: cache.Mission.Key}
	// The contract takes the branch as one line: a branch with a control
	// character, which git allows above ASCII, goes without one.
	if utf8.RuneCountInString(cache.Branch) <= collab.MaxBranchLength && !strings.ContainsFunc(cache.Branch, unicode.IsControl) {
		request.Branch = cache.Branch
	}
	request.Mission.Title = syncTitle(cache)
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
	if planUnsent(cache) {
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
	if cache.Missed != nil {
		missed := *cache.Missed
		request.MissedSyncs, snapshot.missed = &missed, missed.Count
	}
	return request, snapshot
}

// planUnsent reports whether the next sync carries the plan: it differs from
// the last plan sent to the session's mission, and it is not empty, or this
// session sent a plan to that mission.
func planUnsent(cache *sessionCache) bool {
	return cache.PlanDigest != cache.PlanSynced && (cache.PlanSynced != "" || len(cache.Plan) > 0)
}

// recordMissedSync counts one failed sync, with its reason and time.
func recordMissedSync(cache *sessionCache, failure error, now time.Time) {
	count := int64(1)
	if cache.Missed != nil {
		count = min(cache.Missed.Count+1, collab.MaxMissedSyncs)
	}
	cache.Missed = &collab.MissedSyncs{Count: count, Reason: missedReason(failure), LastAt: now.UTC().Format(time.RFC3339Nano)}
}

// settleMissedSyncs clears the missed syncs a successful sync carried. A
// sync that failed while this one ran stays counted for the next one.
func settleMissedSyncs(cache *sessionCache, carried int64) {
	if cache.Missed == nil {
		return
	}
	if left := cache.Missed.Count - carried; left > 0 {
		cache.Missed.Count = left
		return
	}
	cache.Missed = nil
}

// missedReason is the one-line reason a failed sync reports: the reason
// code and message of an answer that is not ok, timeout for a call that
// timed out, else the error.
func missedReason(failure error) string {
	var outcome *outcomeFailure
	reason := failure.Error()
	switch {
	case errors.As(failure, &outcome):
		reason = string(outcome.envelope.Outcome)
		if outcome.envelope.Error != nil {
			reason = firstString(outcome.envelope.Error.Reason, reason)
			if message := lineText(outcome.envelope.Error.Message); message != "" {
				reason += ": " + message
			}
		}
	case errors.Is(failure, context.DeadlineExceeded):
		reason = "timeout: the provider did not answer in time"
	}
	return firstString(boundedLine(reason, collab.MaxMissedSyncReasonLength), "unknown")
}

// mergeSyncAnswer applies a sync answer to the cache as it is now. A message
// already delivered is never queued again, and an answer about a mission the
// session left changes nothing but the pace and what the sync sent; the plan
// it carried does not count as sent to the mission the session moved to. It
// answers the replies the provider refused: a refused reply is final, so the
// cache drops it and sends no other reply to that message.
func mergeSyncAnswer(cache *sessionCache, snapshot syncSnapshot, answer collab.MissionsSyncResult, now time.Time) error {
	cache.LastSyncAt = now
	if snapshot.planSent && snapshot.missionKey == cache.Mission.Key {
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
		case !slices.Contains(snapshot.replies, reply.Message) || !answered:
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

// syncTitle is the title a sync or a decision ask names for the session's
// mission: the session's name while the mission is unknown, untitled or still
// titled by its key, else the key's title for a mission no sync answered yet,
// else none, and the mission keeps its own.
func syncTitle(cache *sessionCache) string {
	keyTitle := boundedLine(collab.MissionKeyTitle(cache.Mission.Key), collab.MaxTitleLength)
	if !cache.Mission.Known || cache.Mission.Title == "" || cache.Mission.Title == keyTitle {
		if name := boundedLine(cache.Name, collab.MaxTitleLength); name != "" {
			return name
		}
	}
	if !cache.Mission.Known {
		return keyTitle
	}
	return ""
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
		if !missionIDPattern.MatchString(message.ID) || slices.Contains(cache.Seen, message.ID) || inboxHolds(cache.Inbox, message.ID) {
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
		if !slices.Contains(drop, value) {
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
// Each sync first reads the usage the session's transcripts added, and
// carries the session's stage and usage.
//
// What a sync sends leaves the cache only once the provider answered ok: a
// failed call and any other outcome keep the acknowledgements, the replies
// and the decisions for the next sync, which sends them again; the provider
// applies each id once. A sync the provider refuses as invalid while it
// carries the plan or missed syncs goes once more without either, so a plan
// or a member the provider refuses never blocks every later sync; when that
// one passes, the plan counts as sent until it changes and the missed syncs
// are dropped.
//
// Every failed sync counts in the cache's missed syncs, with its reason;
// the next sync carries them, and an ok answer clears what it carried.
func (e *Env) syncSession(ctx context.Context, files sessionFiles) error {
	var request collab.MissionsSyncInput
	var snapshot syncSnapshot
	publishable := false
	if err := lockedUpdate(files, func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			return nil, nil
		}
		request, snapshot, publishable = e.prepareSync(cache)
		return cache, nil
	}); err != nil || !publishable {
		return err
	}
	return e.sendSync(ctx, files, request, snapshot)
}

// prepareSync refreshes the mission key of the cache, which its caller holds
// under the session lock, and, when the session names a mission, reads its
// usage and answers its sync request. It answers false for a session that
// names no mission.
func (e *Env) prepareSync(cache *sessionCache) (collab.MissionsSyncInput, syncSnapshot, bool) {
	refreshMissionKey(cache, e.sessionsDir())
	if !sessionPublishable(cache) {
		return collab.MissionsSyncInput{}, syncSnapshot{}, false
	}
	refreshUsage(cache)
	request, snapshot := buildSyncRequest(cache, e.Now())
	return request, snapshot, true
}

// sendSync sends a request prepareSync built, without the session lock, and
// merges the answer under it.
func (e *Env) sendSync(ctx context.Context, files sessionFiles, request collab.MissionsSyncInput, snapshot syncSnapshot) error {
	envelope, callErr := e.call(ctx, collab.ContractMissions, collab.OperationSync, request)
	var planRefused error
	if callErr == nil && envelope.Outcome == collab.OutcomeInvalid && (request.Plan != nil || request.MissedSyncs != nil) && ctx.Err() == nil {
		first := outcomeError(envelope)
		plan, missed := request.Plan != nil, request.MissedSyncs != nil
		request.Plan, request.MissedSyncs = nil, nil
		if envelope, callErr = e.call(ctx, collab.ContractMissions, collab.OperationSync, request); callErr == nil && envelope.Outcome == collab.OutcomeOK {
			var refused []string
			if plan {
				refused = append(refused, "the plan, which goes again once it changes")
			}
			if missed {
				refused = append(refused, fmt.Sprintf("the report of %d missed syncs, which is dropped", snapshot.missed))
			}
			planRefused = fmt.Errorf("the sync went without %s: %w", strings.Join(refused, " and "), first)
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
			settleMissedSyncs(cache, snapshot.missed)
		} else {
			markSyncFailed(cache, snapshot, e.Now())
			recordMissedSync(cache, callErr, e.Now())
		}
		return cache, nil
	})
	return errors.Join(callErr, planRefused, refused, err)
}

// planUncertain is the PlanSynced of a mission a failed sync may have sent a
// plan to. It equals no digest, so the next sync sends the plan again, even
// empty.
const planUncertain = "uncertain"

// markSyncFailed applies a failed sync to the cache: the call may have
// applied the plan it carried to the session's mission, so the next sync
// sends the plan again, whatever it is.
func markSyncFailed(cache *sessionCache, snapshot syncSnapshot, now time.Time) {
	cache.LastSyncAt = now
	if snapshot.planSent && snapshot.missionKey == cache.Mission.Key {
		cache.PlanSynced = planUncertain
	}
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
