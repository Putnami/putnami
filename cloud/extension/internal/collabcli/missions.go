package collabcli

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.putnami.dev/client"
	agencyapiclient "go.putnami.dev/cloud/clients/agency-api/go"
	collab "go.putnami.dev/protocol/collaboration"
)

// askReplay is the replay rule every ask names: the decision its key recorded
// replays in any state.
const askReplay = "any"

// maxErrorBytes is the error agency-api keeps for a failed session, in bytes.
// The contract bounds it in characters, so a longer error is cut at a
// character boundary.
const maxErrorBytes = 1 << 10

// repeatSame is how a caller settles a missions write whose outcome is
// unknown: every one replays the same document safely.
func repeatSame(operation string) string {
	return "repeat missions." + operation + " with the same document"
}

// missionsHandlers serves missions versions 1 and 2 from agency-api. Both
// versions answer sync, ask, reply and status; version 2 adds start, report
// and runs. Each operation makes one call to the workspace's agency routes.
func (p *Provider) missionsHandlers() map[collab.OperationKey]collab.Handler {
	key := func(version int, operation string) collab.OperationKey {
		return collab.OperationKey{Contract: collab.ContractMissions, Version: version, Operation: operation}
	}
	handlers := map[collab.OperationKey]collab.Handler{}
	for _, version := range []int{1, 2} {
		handlers[key(version, collab.OperationSync)] = agencyHandler(p, missionsSync)
		handlers[key(version, collab.OperationAsk)] = agencyHandler(p, missionsAsk)
		handlers[key(version, collab.OperationReply)] = agencyHandler(p, missionsReply)
		handlers[key(version, collab.OperationStatus)] = agencyHandler(p, missionsStatus)
	}
	handlers[key(2, collab.OperationStart)] = agencyHandler(p, missionsStart)
	handlers[key(2, collab.OperationReport)] = agencyHandler(p, missionsReport)
	handlers[key(2, collab.OperationRuns)] = agencyHandler(p, missionsRuns)
	return handlers
}

func missionsSync(ctx context.Context, _ collab.Call, agency agencyCall, in *collab.MissionsSyncInput) (any, *collab.Failure) {
	body, ok := syncBody(*in)
	if !ok {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the missions.sync request carries a time that is not RFC 3339")
	}
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.SessionSyncResult, error) {
		return agency.api.CreateV1WorkspacesAgencyAgentSessionsSync(callCtx, agencyapiclient.CreateV1WorkspacesAgencyAgentSessionsSyncInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyAgentSessionsSyncPath{Workspace: agency.workspace.WorkspaceID,
				Session: in.Session.Engine + ":" + in.Session.ID},
			Body: body})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationSync, repeatSame(collab.OperationSync), collab.ReasonIdempotencyMismatch)
	}
	result, ok := syncResult(answer)
	if !ok {
		return nil, invalidAgencyAnswer(collab.OperationSync, repeatSame(collab.OperationSync))
	}
	return result, nil
}

func missionsAsk(ctx context.Context, _ collab.Call, agency agencyCall, in *collab.MissionsAskInput) (any, *collab.Failure) {
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.DecisionResult, error) {
		return agency.api.CreateV1WorkspacesAgencyDecisions(callCtx, agencyapiclient.CreateV1WorkspacesAgencyDecisionsInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyDecisionsPath{Workspace: agency.workspace.WorkspaceID}, Body: askBody(*in)})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationAsk, repeatSame(collab.OperationAsk), collab.ReasonIdempotencyMismatch)
	}
	result, ok := askResult(answer)
	if !ok {
		return nil, invalidAgencyAnswer(collab.OperationAsk, repeatSame(collab.OperationAsk))
	}
	return result, nil
}

func missionsReply(ctx context.Context, _ collab.Call, agency agencyCall, in *collab.MissionsReplyInput) (any, *collab.Failure) {
	reply := in.Reply
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.MessageResult, error) {
		return agency.api.CreateV1WorkspacesAgencyMessagesReply(callCtx, agencyapiclient.CreateV1WorkspacesAgencyMessagesReplyInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyMessagesReplyPath{Workspace: agency.workspace.WorkspaceID, Message: in.Message},
			Body: agencyapiclient.MessageReplyBody{Reply: &reply}})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationReply, repeatSame(collab.OperationReply), collab.ReasonMessageClosed)
	}
	if answer == nil || answer.Message == nil || value(answer.Message.Id) != in.Message {
		return nil, invalidAgencyAnswer(collab.OperationReply, repeatSame(collab.OperationReply))
	}
	return &collab.MissionsReplyResult{
		Message:  collab.RepliedMessage{ID: in.Message, State: collab.ReplyStateReplied},
		Replayed: answer.Replayed != nil && *answer.Replayed,
	}, nil
}

// missionsStatus reads one of the caller's own missions and a page of its
// sessions. The route takes the contract's request and answers its result,
// so the documents cross it unchanged.
func missionsStatus(ctx context.Context, _ collab.Call, agency agencyCall, in *collab.MissionsStatusInput) (any, *collab.Failure) {
	var body agencyapiclient.MissionsStatusInput
	if err := convert(in, &body); err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the missions.status request cannot be sent: %v", err)
	}
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.MissionsStatusResult, error) {
		return agency.api.CreateV1WorkspacesAgencyMissionsStatus(callCtx, agencyapiclient.CreateV1WorkspacesAgencyMissionsStatusInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyMissionsStatusPath{Workspace: agency.workspace.WorkspaceID}, Body: body})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationStatus, "", collab.ReasonRevisionConflict)
	}
	result := &collab.MissionsStatusResult{}
	if answer == nil || convert(answer, result) != nil || result.Mission.Key != in.Mission.Key {
		return nil, invalidAgencyAnswer(collab.OperationStatus, "")
	}
	// An empty list agency-api leaves out is the contract's empty list.
	if result.Sessions == nil {
		result.Sessions = []collab.MissionSession{}
	}
	if result.Mission.Plan.Steps == nil {
		result.Mission.Plan.Steps = []collab.PlanStep{}
	}
	if result.Mission.OpenDecisions == nil {
		result.Mission.OpenDecisions = []collab.OpenDecision{}
	}
	return result, nil
}

// missionsStart starts a run of a mission. The route takes the contract's
// start document and answers the run, so the documents cross it unchanged.
func missionsStart(ctx context.Context, _ collab.Call, agency agencyCall, in *collab.MissionsStartInput) (any, *collab.Failure) {
	var body agencyapiclient.MissionsStartInput
	if err := convert(in, &body); err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the missions.start request cannot be sent: %v", err)
	}
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.MissionsRunResult, error) {
		return agency.api.CreateV1WorkspacesAgencyMissionsStart(callCtx, agencyapiclient.CreateV1WorkspacesAgencyMissionsStartInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyMissionsStartPath{Workspace: agency.workspace.WorkspaceID}, Body: body})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationStart, repeatSame(collab.OperationStart), collab.ReasonIdempotencyMismatch)
	}
	result := &collab.MissionsRunResult{}
	if answer == nil || convert(answer, result) != nil || result.Run.ID == "" || result.Run.Kind != in.Kind {
		return nil, invalidAgencyAnswer(collab.OperationStart, repeatSame(collab.OperationStart))
	}
	return result, nil
}

// missionsReport ends a run once. The route takes the contract's report
// document and answers the run.
func missionsReport(ctx context.Context, _ collab.Call, agency agencyCall, in *collab.MissionsReportInput) (any, *collab.Failure) {
	var body agencyapiclient.MissionsReportInput
	if err := convert(in, &body); err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the missions.report request cannot be sent: %v", err)
	}
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.MissionsRunResult, error) {
		return agency.api.CreateV1WorkspacesAgencyMissionsReport(callCtx, agencyapiclient.CreateV1WorkspacesAgencyMissionsReportInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyMissionsReportPath{Workspace: agency.workspace.WorkspaceID}, Body: body})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationReport, repeatSame(collab.OperationReport), collab.ReasonRunClosed)
	}
	result := &collab.MissionsRunResult{}
	if answer == nil || convert(answer, result) != nil || result.Run.ID != in.Run {
		return nil, invalidAgencyAnswer(collab.OperationReport, repeatSame(collab.OperationReport))
	}
	return result, nil
}

// missionsRuns reads a page of a mission's runs.
func missionsRuns(ctx context.Context, _ collab.Call, agency agencyCall, in *collab.MissionsRunsInput) (any, *collab.Failure) {
	var body agencyapiclient.MissionsRunsInput
	if err := convert(in, &body); err != nil {
		return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "the missions.runs request cannot be sent: %v", err)
	}
	answer, err := send(ctx, agency, func(callCtx context.Context) (*agencyapiclient.MissionsRunsResult, error) {
		return agency.api.CreateV1WorkspacesAgencyMissionsRuns(callCtx, agencyapiclient.CreateV1WorkspacesAgencyMissionsRunsInput{
			Path: agencyapiclient.CreateV1WorkspacesAgencyMissionsRunsPath{Workspace: agency.workspace.WorkspaceID}, Body: body})
	})
	if err != nil {
		return nil, agencyFailure(err, collab.OperationRuns, "", collab.ReasonRevisionConflict)
	}
	result := &collab.MissionsRunsResult{}
	if answer == nil || convert(answer, result) != nil {
		return nil, invalidAgencyAnswer(collab.OperationRuns, "")
	}
	if result.Items == nil {
		result.Items = []collab.Run{}
	}
	return result, nil
}

// syncBody is the route's body for a sync. The session id in the path is the
// engine, a colon and the host's id. A stage, a usage or a report of missed
// syncs the sync does not carry is left out, so the route keeps the one it
// holds.
func syncBody(in collab.MissionsSyncInput) (agencyapiclient.SessionSyncBody, bool) {
	observed, err := time.Parse(time.RFC3339Nano, in.ObservedAt)
	if err != nil {
		return agencyapiclient.SessionSyncBody{}, false
	}
	state := string(in.State)
	engine, event := in.Session.Engine, in.Event
	body := agencyapiclient.SessionSyncBody{
		Mission: missionName(in.Mission), Engine: &engine, Event: &event, State: &state, ObservedAt: &observed,
		Model: optional(in.Model), Branch: optional(in.Branch), LastMessage: optional(in.LastMessage),
		Error: optional(cutBytes(in.Error, maxErrorBytes)),
	}
	if in.WaitUntil != "" {
		waitUntil, err := time.Parse(time.RFC3339Nano, in.WaitUntil)
		if err != nil {
			return agencyapiclient.SessionSyncBody{}, false
		}
		body.WaitUntil = client.Some(waitUntil)
	}
	if in.Stage != "" {
		stage := string(in.Stage)
		body.Stage = &stage
	}
	if usage := in.Usage; usage != nil {
		body.Usage = client.Some(agencyapiclient.SessionUsage{InputTokens: &usage.InputTokens,
			OutputTokens: &usage.OutputTokens, CacheWriteTokens: &usage.CacheWriteTokens, CacheReadTokens: &usage.CacheReadTokens})
	}
	if missed := in.MissedSyncs; missed != nil {
		lastAt, err := time.Parse(time.RFC3339Nano, missed.LastAt)
		if err != nil {
			return agencyapiclient.SessionSyncBody{}, false
		}
		reason := missedReason(missed.Reason)
		body.MissedSyncs = client.Some(agencyapiclient.SessionMissedSyncs{Count: &missed.Count, Reason: &reason, LastAt: &lastAt})
	}
	if in.Plan != nil {
		steps := make([]agencyapiclient.PlanStep, 0, len(in.Plan.Steps))
		for _, step := range in.Plan.Steps {
			key, text, status := step.Key, step.Text, string(step.Status)
			steps = append(steps, agencyapiclient.PlanStep{Key: &key, Text: &text, Status: &status})
		}
		body.Plan = client.Some(agencyapiclient.SessionPlan{Steps: &steps})
	}
	if len(in.Delivered) > 0 {
		delivered := make([]agencyapiclient.DeliveredMessage, 0, len(in.Delivered))
		for _, id := range in.Delivered {
			message, state := id, "delivered"
			delivered = append(delivered, agencyapiclient.DeliveredMessage{Message: &message, State: &state})
		}
		body.Delivered = &delivered
	}
	if len(in.Decisions) > 0 {
		decisions := append([]string{}, in.Decisions...)
		body.Decisions = &decisions
	}
	if len(in.Replies) > 0 {
		replies := make([]agencyapiclient.SyncReply, 0, len(in.Replies))
		for _, reply := range in.Replies {
			message, text := reply.Message, reply.Reply
			replies = append(replies, agencyapiclient.SyncReply{Message: &message, Reply: &text})
		}
		body.Replies = &replies
	}
	return body, true
}

// syncResult reads the route's answer as the contract's result. The inbox
// keeps its first messages within the contract's bounds; the rest wait for a
// later sync, once the host delivered the first ones.
func syncResult(answer *agencyapiclient.SessionSyncResult) (*collab.MissionsSyncResult, bool) {
	if answer == nil || answer.Session == nil || answer.Session.State == nil || answer.Mission == nil ||
		answer.Mission.Key == nil || answer.Now == nil {
		return nil, false
	}
	result := &collab.MissionsSyncResult{
		Session: collab.SessionStatus{State: collab.SessionState(*answer.Session.State)},
		Mission: collab.MissionView{Key: *answer.Mission.Key, Title: value(answer.Mission.Title),
			Plan: collab.MissionPlan{Steps: []collab.PlanStep{}}, OpenDecisions: []collab.OpenDecision{}},
		Inbox:         []collab.InboxMessage{},
		Decisions:     []collab.DecisionStatus{},
		Replies:       []collab.ReplyStatus{},
		MinIntervalMs: syncInterval(answer.MinIntervalMs),
		Now:           timestamp(*answer.Now),
	}
	for _, step := range list(answer.Mission.Plan) {
		result.Mission.Plan.Steps = append(result.Mission.Plan.Steps, collab.PlanStep{Key: value(step.Key),
			Status: collab.PlanStepStatus(value(step.Status)), Text: value(step.Text)})
	}
	for _, decision := range list(answer.Mission.OpenDecisions) {
		if len(result.Mission.OpenDecisions) == collab.MaxOpenDecisions {
			break
		}
		result.Mission.OpenDecisions = append(result.Mission.OpenDecisions, collab.OpenDecision{ID: value(decision.Id),
			Question: value(decision.Question)})
	}
	total := 0
	for _, message := range list(answer.Inbox) {
		if message.CreatedAt == nil {
			return nil, false
		}
		item := collab.InboxMessage{ID: value(message.Id), Kind: collab.InboxKind(value(message.Kind)), Text: value(message.Text),
			CreatedAt: timestamp(*message.CreatedAt)}
		size := len(item.Text)
		if decision, ok := message.Decision.Value(); ok {
			item.Decision = inboxDecision(decision)
			size += len(item.Decision.Question) + len(item.Decision.Answer)
		}
		if len(result.Inbox) == collab.MaxInboxMessages || total+size > collab.MaxContentBytes {
			break
		}
		total += size
		result.Inbox = append(result.Inbox, item)
	}
	for _, decision := range list(answer.NamedDecisions) {
		result.Decisions = append(result.Decisions, collab.DecisionStatus{ID: value(decision.Id),
			State: collab.DecisionState(value(decision.State))})
	}
	for _, reply := range list(answer.Replies) {
		result.Replies = append(result.Replies, collab.ReplyStatus{Message: value(reply.Message),
			State: collab.ReplyState(value(reply.State)), Reason: value(reply.Reason)})
	}
	return result, true
}

// inboxDecision is the decision an answer message answers. The answer is the
// owner's text, or the chosen option's label when the owner wrote none.
func inboxDecision(decision agencyapiclient.InboxDecision) *collab.InboxDecision {
	answer := value(decision.Text)
	if answer == "" {
		answer = value(decision.Answer)
	}
	out := &collab.InboxDecision{ID: value(decision.Id), Question: value(decision.Question), Answer: answer}
	if answer != "" {
		out.Option = value(decision.Option)
	}
	return out
}

func askBody(in collab.MissionsAskInput) agencyapiclient.DecisionAskBody {
	options := make([]agencyapiclient.DecisionOption, 0, len(in.Options))
	for _, option := range in.Options {
		id, label := option.ID, option.Label
		options = append(options, agencyapiclient.DecisionOption{Id: &id, Label: &label, Consequences: optional(option.Consequences)})
	}
	// The missions contract replays an ask under its key in any state, and
	// refuses the key with another ask; agency-api applies that rule only to
	// an ask that names it.
	question, key, replay := in.Question, in.IdempotencyKey, askReplay
	body := agencyapiclient.DecisionAskBody{Mission: missionName(in.Mission), Question: &question,
		Context: optional(in.Context), Options: &options, Blocks: optional(in.Blocks), IdempotencyKey: &key, Replay: &replay}
	if recommendation := in.Recommendation; recommendation != nil {
		option := recommendation.Option
		body.Recommendation = client.Some(agencyapiclient.DecisionRecommendation{Option: &option, Reason: optional(recommendation.Reason)})
	}
	return body
}

// askResult reads the decision the route answers. Once there is an answer, it
// is the owner's text, or the chosen option's label when the owner wrote
// none.
func askResult(answer *agencyapiclient.DecisionResult) (*collab.MissionsAskResult, bool) {
	if answer == nil || answer.Decision == nil || answer.Decision.Id == nil || answer.Decision.State == nil {
		return nil, false
	}
	decision := answer.Decision
	result := &collab.MissionsAskResult{
		Decision: collab.MissionDecision{ID: *decision.Id, State: collab.DecisionState(*decision.State)},
		Replayed: answer.Replayed != nil && *answer.Replayed,
	}
	given, ok := decision.Answer.Value()
	if !ok || result.Decision.State == collab.DecisionStateOpen {
		return result, true
	}
	text, option := value(given.Text), value(given.Option)
	if text == "" {
		for _, candidate := range list(decision.Options) {
			if option != "" && value(candidate.Id) == option {
				text = value(candidate.Label)
			}
		}
	}
	result.Decision.Answer = text
	if text != "" {
		result.Decision.Option = option
	}
	return result, true
}

func missionName(mission collab.MissionName) *agencyapiclient.MissionName {
	repository, key := mission.Repository, mission.Key
	return &agencyapiclient.MissionName{Repository: &repository, Key: &key, Title: optional(mission.Title)}
}

// syncInterval keeps the pace agency-api answers within the contract's
// bounds.
func syncInterval(ms *int64) int {
	switch {
	case ms == nil || *ms < collab.MinSyncIntervalMs:
		return collab.MinSyncIntervalMs
	case *ms > collab.MaxSyncIntervalMs:
		return collab.MaxSyncIntervalMs
	}
	return int(*ms)
}

func timestamp(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

// cutBytes keeps the longest prefix of text within limit bytes that ends on a
// character boundary.
func cutBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// missedReason is the reason of a report of missed syncs as the route takes
// it: its control characters, which the contract allows and the route
// refuses, become spaces, and the line is trimmed.
func missedReason(reason string) string {
	cleaned := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, reason))
	if cleaned == "" {
		return "unknown"
	}
	return cleaned
}
