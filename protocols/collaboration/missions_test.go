package collaboration

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func syncInput() *MissionsSyncInput {
	return &MissionsSyncInput{
		Session: SessionRef{Engine: "host-a", ID: "s-1"}, Mission: MissionName{Key: "issue-596"},
		Event: "stop", State: SessionStateWorking, ObservedAt: "2026-10-26T09:00:00Z",
	}
}

func syncResult() *MissionsSyncResult {
	return &MissionsSyncResult{
		Session: SessionStatus{State: SessionStateWorking},
		Mission: MissionView{Key: "issue-596", Plan: MissionPlan{Steps: []PlanStep{}}, OpenDecisions: []OpenDecision{}},
		Inbox:   []InboxMessage{}, Decisions: []DecisionStatus{}, Replies: []ReplyStatus{},
		MinIntervalMs: MinSyncIntervalMs, Now: "2026-10-26T09:00:01Z",
	}
}

func askInput() *MissionsAskInput {
	return &MissionsAskInput{
		Mission: MissionName{Key: "issue-596"}, Question: "Ship now?",
		Options:        []DecisionOption{{ID: "yes", Label: "Ship"}, {ID: "no", Label: "Wait"}},
		IdempotencyKey: "issue-596/ask/ship",
	}
}

// TestMissions_InputRules covers the request rules no fixture shows.
func TestMissions_InputRules(t *testing.T) {
	cases := []struct {
		name  string
		input func() any
		codes []string
	}{
		{"a minimal sync", func() any { return syncInput() }, nil},
		{"waitUntil outside waiting", func() any { in := syncInput(); in.WaitUntil = "2026-10-26T10:00:00Z"; return in }, []string{ErrorCodeInvalidValue}},
		{"waitUntil while waiting", func() any {
			in := syncInput()
			in.State, in.WaitUntil = SessionStateWaiting, "2026-10-26T10:00:00+02:00"
			return in
		}, nil},
		{"error outside failed", func() any { in := syncInput(); in.Error = "boom"; return in }, []string{ErrorCodeInvalidValue}},
		{"an empty plan clears it", func() any { in := syncInput(); in.Plan = &MissionPlan{Steps: []PlanStep{}}; return in }, nil},
		{"a plan without steps", func() any { in := syncInput(); in.Plan = &MissionPlan{}; return in }, []string{ErrorCodeRequired}},
		{"a repeated step key", func() any {
			in := syncInput()
			in.Plan = &MissionPlan{Steps: []PlanStep{{Key: "a", Status: PlanStepTodo, Text: "x"}, {Key: "a", Status: PlanStepDone, Text: "y"}}}
			return in
		}, []string{ErrorCodeInvalidValue}},
		{"an unknown step status", func() any {
			in := syncInput()
			in.Plan = &MissionPlan{Steps: []PlanStep{{Key: "a", Status: "blocked", Text: "x"}}}
			return in
		}, []string{ErrorCodeInvalidState}},
		{"a step text on two lines", func() any {
			in := syncInput()
			in.Plan = &MissionPlan{Steps: []PlanStep{{Key: "a", Status: PlanStepTodo, Text: "x\ny"}}}
			return in
		}, []string{ErrorCodeInvalidValue}},
		{"a repeated delivered id", func() any { in := syncInput(); in.Delivered = []string{"m1", "m1"}; return in }, []string{ErrorCodeInvalidValue}},
		{"too many decisions", func() any {
			in := syncInput()
			for i := range MaxSyncDecisions + 1 {
				in.Decisions = append(in.Decisions, maximalID(i))
			}
			return in
		}, []string{ErrorCodeTooLarge}},
		{"two replies to one message", func() any {
			in := syncInput()
			in.Replies = []MessageReply{{Message: "m1", Reply: "a"}, {Message: "m1", Reply: "b"}}
			return in
		}, []string{ErrorCodeInvalidValue}},
		{"an empty reply", func() any { in := syncInput(); in.Replies = []MessageReply{{Message: "m1"}}; return in }, []string{ErrorCodeRequired}},
		{"an uppercase event", func() any { in := syncInput(); in.Event = "Stop"; return in }, []string{ErrorCodeInvalidValue}},
		{"an uppercase engine", func() any { in := syncInput(); in.Session.Engine = "Host"; return in }, []string{ErrorCodeInvalidValue}},
		{"a long session id", func() any { in := syncInput(); in.Session.ID = fill(MaxSessionIDLength + 1); return in }, []string{ErrorCodeInvalidValue}},
		{"an unknown session state", func() any { in := syncInput(); in.State = "sleeping"; return in }, []string{ErrorCodeInvalidState}},
		{"a model on two lines", func() any { in := syncInput(); in.Model = "a\nb"; return in }, []string{ErrorCodeInvalidValue}},
		{"a last message on two lines", func() any { in := syncInput(); in.LastMessage = "a\nb"; return in }, nil},
		{"a last message with NUL", func() any { in := syncInput(); in.LastMessage = "a\x00b"; return in }, []string{ErrorCodeInvalidValue}},
		{"a long last message", func() any { in := syncInput(); in.LastMessage = fill(MaxLastMessageLength + 1); return in }, []string{ErrorCodeTooLarge}},
		{"a mission key with a space", func() any { in := syncInput(); in.Mission.Key = "issue 596"; return in }, []string{ErrorCodeInvalidValue}},
		{"a recommendation naming no option", func() any {
			in := askInput()
			in.Recommendation = &DecisionRecommendation{Option: "maybe"}
			return in
		}, []string{ErrorCodeInvalidValue}},
		{"a repeated option", func() any { in := askInput(); in.Options[1].ID = "yes"; return in }, []string{ErrorCodeInvalidValue}},
		{"six options", func() any {
			in := askInput()
			in.Options = nil
			for i := range MaxDecisionOptions + 1 {
				in.Options = append(in.Options, DecisionOption{ID: numbered(i, 3), Label: "x"})
			}
			return in
		}, []string{ErrorCodeInvalidValue}},
		{"a long question", func() any { in := askInput(); in.Question = fill(MaxQuestionLength + 1); return in }, []string{ErrorCodeTooLarge}},
		{"a question on two lines", func() any { in := askInput(); in.Question = "Ship now?\nOr wait?"; return in }, nil},
		{"a bad idempotency key", func() any { in := askInput(); in.IdempotencyKey = "a b"; return in }, []string{ErrorCodeInvalidValue}},
		{"an empty reply input", func() any { return &MissionsReplyInput{Message: "m1"} }, []string{ErrorCodeRequired}},
		{"a long message id", func() any { return &MissionsReplyInput{Message: maximalID(0) + "a", Reply: "x"} }, []string{ErrorCodeInvalidValue}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := distinctCodes(ValidateInput(tc.input())); !slices.Equal(got, tc.codes) {
				t.Fatalf("codes = %v, want %v", got, tc.codes)
			}
		})
	}
}

// TestMissions_ResultRules covers the result rules no fixture shows.
func TestMissions_ResultRules(t *testing.T) {
	answer := func(text string) InboxMessage {
		return InboxMessage{ID: "m1", Kind: InboxKindAnswer, Text: text, CreatedAt: "2026-10-26T09:00:00Z",
			Decision: &InboxDecision{ID: "d1", Question: "q", Answer: "a"}}
	}
	cases := []struct {
		name   string
		result func() any
		codes  []string
	}{
		{"an empty sync", func() any { return syncResult() }, nil},
		{"an answer without its decision", func() any {
			r := syncResult()
			message := answer("x")
			message.Decision = nil
			r.Inbox = []InboxMessage{message}
			return r
		}, []string{ErrorCodeRequired}},
		{"information naming a decision", func() any {
			r := syncResult()
			message := answer("x")
			message.Kind = InboxKindInfo
			r.Inbox = []InboxMessage{message}
			return r
		}, []string{ErrorCodeInvalidValue}},
		{"an unknown inbox kind", func() any {
			r := syncResult()
			r.Inbox = []InboxMessage{{ID: "m1", Kind: "note", Text: "x", CreatedAt: "2026-10-26T09:00:00Z"}}
			return r
		}, []string{ErrorCodeInvalidValue}},
		{"a repeated inbox id", func() any {
			r := syncResult()
			r.Inbox = []InboxMessage{answer("x"), answer("y")}
			return r
		}, []string{ErrorCodeInvalidValue}},
		{"inbox texts above the content bound", func() any {
			r := syncResult()
			for i := range 9 {
				message := answer(strings.Repeat("x", MaxInboxTextLength))
				message.ID = maximalID(i)
				r.Inbox = append(r.Inbox, message)
			}
			return r
		}, []string{ErrorCodeTooLarge}},
		{"a replied entry with a reason", func() any {
			r := syncResult()
			r.Replies = []ReplyStatus{{Message: "m1", State: ReplyStateReplied, Reason: "x"}}
			return r
		}, []string{ErrorCodeInvalidValue}},
		{"an unknown reply state", func() any {
			r := syncResult()
			r.Replies = []ReplyStatus{{Message: "m1", State: "lost"}}
			return r
		}, []string{ErrorCodeInvalidState}},
		{"an unknown decision state", func() any {
			r := syncResult()
			r.Decisions = []DecisionStatus{{ID: "d1", State: "closed"}}
			return r
		}, []string{ErrorCodeInvalidState}},
		{"an interval below the floor", func() any { r := syncResult(); r.MinIntervalMs = MinSyncIntervalMs - 1; return r }, []string{ErrorCodeInvalidValue}},
		{"an interval above the ceiling", func() any { r := syncResult(); r.MinIntervalMs = MaxSyncIntervalMs + 1; return r }, []string{ErrorCodeInvalidValue}},
		{"null open decisions", func() any { r := syncResult(); r.Mission.OpenDecisions = nil; return r }, []string{ErrorCodeRequired}},
		{"a null plan", func() any { r := syncResult(); r.Mission.Plan.Steps = nil; return r }, []string{ErrorCodeRequired}},
		{"null decisions and replies", func() any { r := syncResult(); r.Decisions, r.Replies = nil, nil; return r }, []string{ErrorCodeRequired}},
		{"a mission url with credentials", func() any { r := syncResult(); r.Mission.URL = "https://u:p@h/x"; return r }, []string{ErrorCodeInvalidURL}},
		{"no clock", func() any { r := syncResult(); r.Now = ""; return r }, []string{ErrorCodeRequired}},
		{"an open decision with an answer", func() any {
			return &MissionsAskResult{Decision: MissionDecision{ID: "d1", State: DecisionStateOpen, Answer: "yes"}}
		}, []string{ErrorCodeInvalidState}},
		{"an answered decision", func() any {
			return &MissionsAskResult{Decision: MissionDecision{ID: "d1", State: DecisionStateAnswered, Answer: "yes"}, Replayed: true}
		}, nil},
		{"a refused reply result", func() any {
			return &MissionsReplyResult{Message: RepliedMessage{ID: "m1", State: ReplyStateRefused}}
		}, []string{ErrorCodeInvalidState}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := distinctCodes(ValidateResult(tc.result())); !slices.Equal(got, tc.codes) {
				t.Fatalf("codes = %v, want %v", got, tc.codes)
			}
		})
	}
}

// TestServe_ANilMissionsListIsTheEmptyList: a sync handler that has nothing
// to report may leave its lists nil; Serve answers the empty lists the
// contract requires.
func TestServe_ANilMissionsListIsTheEmptyList(t *testing.T) {
	handlers := map[OperationKey]Handler{
		{ContractMissions, 1, OperationSync}: func(_ context.Context, call Call) (any, *Failure) {
			in := call.Input.(*MissionsSyncInput)
			return &MissionsSyncResult{
				Session: SessionStatus{State: in.State}, Mission: MissionView{Key: in.Mission.Key},
				MinIntervalMs: MinSyncIntervalMs, Now: "2026-10-26T09:00:01Z",
			}, nil
		},
	}
	arguments, err := json.Marshal(syncInput())
	if err != nil {
		t.Fatal(err)
	}
	response := serveCall(t, routed(ContractMissions, OperationSync, string(arguments)), handlers)
	if response.Outcome != OutcomeOK {
		t.Fatalf("outcome = %s: %+v", response.Outcome, response.Error)
	}
	if _, diags := ParseResult(ContractMissions, 1, OperationSync, response.Result); diags != nil {
		t.Fatalf("the result is not valid: %v", diags)
	}
	for _, member := range []string{`"inbox":[]`, `"decisions":[]`, `"replies":[]`, `"steps":[]`, `"openDecisions":[]`} {
		if !strings.Contains(string(response.Result), member) {
			t.Errorf("the result %s lacks %s", response.Result, member)
		}
	}
}
