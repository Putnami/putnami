package collaboration

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// Maximal documents: every member at its bound, filled with a character the
// encoder escapes to six bytes, so no valid document of the same shape
// encodes larger.

func fill(n int) string { return strings.Repeat("<", n) }

// numbered is n characters ending in i, so the members of one list differ.
func numbered(i, n int) string {
	suffix := strconv.Itoa(i)
	return fill(n-len(suffix)) + suffix
}

func maximalRef(i int) Ref {
	return Ref{Source: "x:" + numbered(i, MaxTokenLength-2), ID: numbered(i, MaxTokenLength)}
}

func maximalURL(i int) string {
	suffix := strconv.Itoa(i)
	return "https://h/" + strings.Repeat("&", MaxURLLength-len("https://h/")-len(suffix)) + suffix
}

const maximalTimestamp = "2026-09-24T08:00:00.123456789+00:00"

var maximalCommit = strings.Repeat("a", 64)

func maximalTask() Task {
	parent := maximalRef(1)
	task := Task{
		Ref: maximalRef(0), Key: fill(MaxTokenLength), Revision: fill(MaxTokenLength), URL: maximalURL(0), Title: fill(MaxTitleLength),
		Body: fill(MaxBodyBytes), State: TaskStateOpen, ProviderState: fill(MaxLabelLength), Parent: &parent,
		Holder: fill(MaxTokenLength), CreatedAt: maximalTimestamp, UpdatedAt: maximalTimestamp,
	}
	for i := range MaxLabels {
		task.Labels = append(task.Labels, numbered(i, MaxLabelLength))
	}
	for i := range MaxListMembers {
		task.Assignees = append(task.Assignees, numbered(i, MaxTokenLength))
	}
	return task
}

func maximalProposal() Proposal {
	proposal := Proposal{
		Ref: maximalRef(0), Revision: fill(MaxTokenLength), URL: maximalURL(0),
		Change: Change{Repository: fill(MaxTokenLength), Base: fill(MaxTokenLength), Head: numbered(1, MaxTokenLength), HeadCommit: maximalCommit},
		Title:  fill(MaxTitleLength), Body: fill(MaxBodyBytes), State: ProposalStateOpen, ProviderState: fill(MaxLabelLength),
		UpdatedAt: maximalTimestamp,
	}
	for i := range MaxLabels {
		proposal.Labels = append(proposal.Labels, numbered(i, MaxLabelLength))
	}
	for i := range MaxListMembers {
		proposal.Assignees = append(proposal.Assignees, numbered(i, MaxTokenLength))
	}
	return proposal
}

// maximalComment is a comment on a file line with every member at its bound.
func maximalComment() ProposalComment {
	review, line := maximalRef(1), math.MaxInt64
	return ProposalComment{
		Ref: maximalRef(0), URL: maximalURL(0), Author: fill(MaxTokenLength), SubmittedAt: maximalTimestamp, Review: &review,
		Path: fill(MaxLocatorLength), Line: &line, Commit: maximalCommit, Body: fill(MaxBodyBytes),
	}
}

func maximalRecord() MemoryRecord {
	record := MemoryRecord{
		Ref: maximalRef(0), Revision: fill(MaxTokenLength), Kind: MemoryKindMission,
		Identity: MemoryIdentity{Workspace: fill(MaxTokenLength), Repository: fill(MaxTokenLength), Scope: fill(MaxTokenLength), Mission: fill(MaxTokenLength)},
		Title:    fill(MaxTitleLength), Content: fill(MaxContentBytes),
		Provenance: Provenance{RecordedAt: maximalTimestamp, RecordedBy: fill(MaxTokenLength)},
		Freshness:  Freshness{UpdatedAt: maximalTimestamp, RetrievedAt: maximalTimestamp},
	}
	for i := range MaxListMembers {
		record.Sources = append(record.Sources, maximalRef(i))
		record.Evidence = append(record.Evidence, Evidence{Kind: EvidenceKindGate, Locator: numbered(i, MaxLocatorLength), Digest: "sha256:" + strings.Repeat("b", 64)})
	}
	return record
}

func maximalStatus() ProposalStatusResult {
	status := ProposalStatusResult{Proposal: maximalProposal(), Checks: Checks{State: ChecksStateFailing, Commit: maximalCommit, Detail: fill(MaxTitleLength * 4)}}
	for i := range MaxListMembers {
		status.Checks.Items = append(status.Checks.Items, Check{Name: numbered(i, MaxTitleLength), State: CheckStateFailing, URL: maximalURL(i)})
		status.Reviews = append(status.Reviews, Review{Ref: maximalRef(i), URL: maximalURL(i), Verdict: ReviewVerdictComment,
			Commit: maximalCommit, Author: fill(MaxTokenLength), SubmittedAt: maximalTimestamp})
	}
	return status
}

// maximalID is the longest bare missions id, ending in i. It is also the
// longest option id.
func maximalID(i int) string {
	suffix := strconv.Itoa(i)
	return strings.Repeat("a", 64-len(suffix)) + suffix
}

const maximalUTC = "2026-09-24T08:00:00.123456789Z"

func maximalSession() SessionRef {
	return SessionRef{Engine: strings.Repeat("e", 32), ID: strings.Repeat("s", 128)}
}

func maximalMissionName() MissionName {
	return MissionName{Repository: fill(MaxTokenLength), Key: fill(MaxTokenLength), Title: fill(MaxTitleLength)}
}

func maximalPlan() MissionPlan {
	plan := MissionPlan{Steps: []PlanStep{}}
	for i := range MaxListMembers {
		plan.Steps = append(plan.Steps, PlanStep{Key: numbered(i, MaxTokenLength), Status: PlanStepDoing, Text: fill(MaxPlanStepTextLength)})
	}
	return plan
}

// maximalSyncResult fills every list to its bound. The inbox texts share
// MaxContentBytes evenly.
func maximalSyncResult() *MissionsSyncResult {
	result := &MissionsSyncResult{
		Session: SessionStatus{State: SessionStateWaiting},
		Mission: MissionView{Key: fill(MaxTokenLength), Title: fill(MaxTitleLength), URL: maximalURL(0), Plan: maximalPlan(), OpenDecisions: []OpenDecision{}},
		Inbox:   []InboxMessage{}, Decisions: []DecisionStatus{}, Replies: []ReplyStatus{},
		MinIntervalMs: MaxSyncIntervalMs, Now: maximalTimestamp,
	}
	for i := range MaxOpenDecisions {
		result.Mission.OpenDecisions = append(result.Mission.OpenDecisions, OpenDecision{ID: maximalID(i), Question: fill(MaxQuestionLength)})
	}
	share := MaxContentBytes / MaxInboxMessages
	text := (share - MaxQuestionLength) / 2
	for i := range MaxInboxMessages {
		result.Inbox = append(result.Inbox, InboxMessage{
			ID: maximalID(i), Kind: InboxKindAnswer, Text: fill(text), CreatedAt: maximalTimestamp,
			Decision: &InboxDecision{ID: maximalID(i), Question: fill(MaxQuestionLength), Answer: fill(share - MaxQuestionLength - text), Option: maximalID(i)},
		})
	}
	for i := range MaxSyncDecisions {
		result.Decisions = append(result.Decisions, DecisionStatus{ID: maximalID(i), State: DecisionStateAcknowledged})
	}
	for i := range MaxSyncReplies {
		result.Replies = append(result.Replies, ReplyStatus{Message: maximalID(i), State: ReplyStateRefused, Reason: ReasonMessageNotFound})
	}
	return result
}

func maximalSyncInput() *MissionsSyncInput {
	plan := maximalPlan()
	input := &MissionsSyncInput{
		Session: maximalSession(), Mission: maximalMissionName(), Event: strings.Repeat("e", 32),
		State: SessionStateFailed, ObservedAt: maximalUTC, Model: fill(MaxModelLength), Branch: fill(MaxBranchLength),
		LastMessage: fill(MaxLastMessageLength), Error: fill(MaxErrorLength), Plan: &plan, Stage: LoopStageValidate,
		Usage: &SessionUsage{InputTokens: MaxUsageTokens, OutputTokens: MaxUsageTokens, CacheWriteTokens: MaxUsageTokens, CacheReadTokens: MaxUsageTokens},
	}
	for i := range MaxListMembers {
		input.Delivered = append(input.Delivered, maximalID(i))
	}
	for i := range MaxSyncDecisions {
		input.Decisions = append(input.Decisions, maximalID(i))
	}
	for i := range MaxSyncReplies {
		input.Replies = append(input.Replies, MessageReply{Message: maximalID(i), Reply: fill(MaxReplyLength)})
	}
	return input
}

func maximalAskInput() *MissionsAskInput {
	input := &MissionsAskInput{
		Mission: maximalMissionName(), Question: fill(MaxQuestionLength), Context: fill(MaxContextLength),
		Recommendation: &DecisionRecommendation{Option: maximalID(0), Reason: fill(MaxExplanationLength)},
		Blocks:         fill(MaxExplanationLength), IdempotencyKey: strings.Repeat("a", 128),
	}
	for i := range MaxDecisionOptions {
		input.Options = append(input.Options, DecisionOption{ID: maximalID(i), Label: fill(MaxOptionLabelLength), Consequences: fill(MaxExplanationLength)})
	}
	return input
}

// TestMaxDocumentBytesHoldsEveryMaximalDocument: a request, and a result of
// one item, whose members are all within their bounds always parse, so only a
// list page can outgrow the bound, and a page of one item never does.
func TestMaxDocumentBytesHoldsEveryMaximalDocument(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "bounded-documents", "every-maximal-document-fits-the-bound")
	cursor := strings.Repeat("<", MaxCursorLength)
	checkpoint := &MemoryCheckpointInput{
		Mission: fill(MaxTokenLength), Identity: MemoryIdentity{Workspace: fill(MaxTokenLength), Repository: fill(MaxTokenLength), Scope: fill(MaxTokenLength)},
		Precondition: Precondition{ExpectedRevision: fill(MaxTokenLength)}, IdempotencyKey: strings.Repeat("a", 128),
		Title: fill(MaxTitleLength), Content: fill(MaxContentBytes),
	}
	record := maximalRecord()
	checkpoint.Sources, checkpoint.Evidence = record.Sources, record.Evidence
	results := map[string]any{
		"tasks.get":          &TaskResult{Task: maximalTask()},
		"tasks.find":         &TaskListResult{Items: []Task{maximalTask()}, Page: Page{Next: cursor}},
		"proposals.find":     &ProposalListResult{Items: []Proposal{maximalProposal()}, Page: Page{Next: cursor}},
		"proposals.status":   func() *ProposalStatusResult { s := maximalStatus(); return &s }(),
		"proposals.comments": &ProposalCommentListResult{Items: []ProposalComment{maximalComment()}, Page: Page{Next: cursor}},
		"memory.mission":     &MemoryRecordResult{Record: record},
		"memory.checkpoint":  &MemoryCheckpointResult{Record: record, Replayed: true},
		"memory.context":     &MemoryListResult{Items: []MemoryRecord{record}, Page: Page{Next: cursor}},
		"missions.sync":      maximalSyncResult(),
		"missions.ask": &MissionsAskResult{Decision: MissionDecision{
			ID: maximalID(0), State: DecisionStateAnswered, URL: maximalURL(0), Answer: fill(MaxInboxTextLength), Option: maximalID(0),
		}, Replayed: true},
	}
	for name, result := range results {
		t.Run(name, func(t *testing.T) {
			if diags := ValidateResult(result); diag.HasErrors(diags) {
				t.Fatalf("the maximal document is not valid: %s", formatDiagnostics(diags))
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > maxResultBytes {
				t.Fatalf("the maximal result encodes to %d bytes, above the %d-byte result bound", len(encoded), maxResultBytes)
			}
			response, err := json.Marshal(Response{Outcome: OutcomeOK, Result: encoded})
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseResponse(response); diags != nil {
				t.Fatalf("the maximal response does not parse: %v", diags)
			}
		})
	}
	t.Run("memory.checkpoint request", func(t *testing.T) {
		encoded, err := json.Marshal(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if _, diags := ParseRequest(ContractMemory, 1, OperationCheckpoint, encoded); diags != nil {
			t.Fatalf("the maximal checkpoint request (%d bytes) does not parse: %v", len(encoded), diags)
		}
	})
	for operation, input := range map[string]any{OperationSync: maximalSyncInput(), OperationAsk: maximalAskInput()} {
		t.Run("missions."+operation+" request", func(t *testing.T) {
			encoded, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if _, diags := ParseRequest(ContractMissions, 1, operation, encoded); diags != nil {
				t.Fatalf("the maximal %s request (%d bytes) does not parse: %v", operation, len(encoded), diags)
			}
		})
	}
}

// pagedTasks is a list handler over n tasks with bodies of the given size,
// paged by position so a cursor never depends on the page size.
func pagedTasks(n, body int, calls *int) Handler {
	return func(_ context.Context, call Call) (any, *Failure) {
		*calls++
		in := call.Input.(*TaskFindInput)
		start := 0
		if in.Page.Cursor != "" {
			start, _ = strconv.Atoi(in.Page.Cursor)
		}
		end := min(start+in.Page.Size, n)
		result := &TaskListResult{}
		for i := start; i < end; i++ {
			result.Items = append(result.Items, Task{Ref: Ref{Source: "local:x", ID: strconv.Itoa(i)}, Revision: "r1",
				Title: "t", Body: strings.Repeat("x", body), State: TaskStateOpen})
		}
		if end < n {
			result.Page.Next = strconv.Itoa(end)
		}
		return result, nil
	}
}

// TestServe_ShortensAPageThatWouldExceedTheDocumentBound: a page of the
// largest size with maximal bodies is larger than any document may be. Serve
// answers the items that fit and the cursor after them, so the traversal
// still covers every task exactly once.
func TestServe_ShortensAPageThatWouldExceedTheDocumentBound(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "bounded-documents", "serve-shortens-a-page-above-the-bound")
	calls := 0
	handlers := map[OperationKey]Handler{{ContractTasks, 1, OperationFind}: pagedTasks(MaxPageSize, MaxBodyBytes, &calls)}
	seen := map[string]int{}
	cursor, pages := "", 0
	for {
		arguments := `{"page":{"size":` + strconv.Itoa(MaxPageSize) + `}}`
		if cursor != "" {
			arguments = `{"page":{"size":` + strconv.Itoa(MaxPageSize) + `,"cursor":"` + cursor + `"}}`
		}
		response := serveCall(t, routed(ContractTasks, OperationFind, arguments), handlers)
		if response.Outcome != OutcomeOK {
			t.Fatalf("page %d: outcome %s %+v", pages, response.Outcome, response.Error)
		}
		var page TaskListResult
		if err := json.Unmarshal(response.Result, &page); err != nil {
			t.Fatal(err)
		}
		if pages == 0 && (len(page.Items) == MaxPageSize || page.Page.Next == "") {
			t.Fatalf("the first page carried %d items and next %q; it cannot hold them all", len(page.Items), page.Page.Next)
		}
		for _, task := range page.Items {
			seen[task.Ref.ID]++
		}
		pages++
		if cursor = page.Page.Next; cursor == "" {
			break
		}
	}
	if len(seen) != MaxPageSize {
		t.Fatalf("the traversal saw %d of %d tasks", len(seen), MaxPageSize)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("task %s was listed %d times", id, n)
		}
	}
	if calls > 2*pages {
		t.Fatalf("%d handler calls for %d pages; a page is asked again at most once", calls, pages)
	}
}

// TestServe_AListHandlerThatIgnoresThePageSizeIsRefused: a handler that keeps
// answering the page that does not fit is refused after one repeat, never
// asked forever.
func TestServe_AListHandlerThatIgnoresThePageSizeIsRefused(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "bounded-documents", "serve-refuses-a-handler-that-ignores-the-page-size")
	calls := 0
	whole := pagedTasks(MaxPageSize, MaxBodyBytes, new(int))
	handlers := map[OperationKey]Handler{{ContractTasks, 1, OperationFind}: func(ctx context.Context, call Call) (any, *Failure) {
		calls++
		all := *call.Input.(*TaskFindInput)
		all.Page = &PageRequest{Size: MaxPageSize}
		return whole(ctx, Call{Input: &all})
	}}
	response := serveCall(t, routed(ContractTasks, OperationFind, `{"page":{"size":100}}`), handlers)
	if response.Outcome != OutcomeUnavailable || response.Error.Reason != ReasonProviderInvalidResponse {
		t.Fatalf("outcome %s %+v", response.Outcome, response.Error)
	}
	if calls != 2 {
		t.Fatalf("the handler was called %d times, want the page and one repeat", calls)
	}
}
