package agentsession

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// TestEverySyncRequestHoldsTheContractRules builds the request of caches at
// the edges of each bound and parses it as the CLI route does.
func TestEverySyncRequestHoldsTheContractRules(t *testing.T) {
	spectest.Proves(t, sessionFeature, stopRequirement, "stop-publishes-in-one-sync")
	base := func() *sessionCache {
		return &sessionCache{Engine: claude, Session: claudeSession, Repository: sessionRepo, Branch: sessionBranch,
			Mission: sessionMissionState{Key: sessionBranchKey}, State: collab.SessionStateWorking, LastEvent: sessionEventToolUse}
	}
	for _, tc := range []struct {
		name  string
		edit  func(*sessionCache)
		check func(t *testing.T, request collab.MissionsSyncInput)
	}{
		{"new mission", nil, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.Mission.Title != sessionBranch || r.LastMessage != "" || r.Plan != nil {
				t.Errorf("%+v", r)
			}
		}},
		{"known mission", func(c *sessionCache) { c.Mission.Known = true }, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.Mission.Title != "" {
				t.Errorf("title %q", r.Mission.Title)
			}
		}},
		{"waiting without an end", func(c *sessionCache) { c.State = collab.SessionStateWaiting }, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.State != collab.SessionStateHandedBack || r.WaitUntil != "" {
				t.Errorf("state %s until %q", r.State, r.WaitUntil)
			}
		}},
		{"waiting", func(c *sessionCache) {
			c.State, c.WaitUntil = collab.SessionStateWaiting, sessionEpoch.Add(time.Minute)
		}, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.State != collab.SessionStateWaiting || r.WaitUntil != "2026-10-21T10:01:00Z" {
				t.Errorf("state %s until %q", r.State, r.WaitUntil)
			}
		}},
		{"failed without an error", func(c *sessionCache) { c.State = collab.SessionStateFailed }, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.Error != "unknown" {
				t.Errorf("error %q", r.Error)
			}
		}},
		{"error on another state", func(c *sessionCache) { c.Error = "stale" }, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.Error != "" {
				t.Errorf("error %q", r.Error)
			}
		}},
		{"branch with a control character", func(c *sessionCache) { c.Branch = "feat/a\u0085b" }, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.Branch != "" {
				t.Errorf("branch %q", r.Branch)
			}
		}},
		{"long branch", func(c *sessionCache) { c.Branch = strings.Repeat("b", collab.MaxBranchLength+1) }, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.Branch != "" {
				t.Errorf("branch of %d characters", len(r.Branch))
			}
		}},
		{"stop", func(c *sessionCache) {
			c.LastEvent, c.LastMessage = sessionEventStop, strings.Repeat("m", collab.MaxLastMessageLength)
		}, func(t *testing.T, r collab.MissionsSyncInput) {
			if len(r.LastMessage) != collab.MaxLastMessageLength {
				t.Errorf("last message of %d characters", len(r.LastMessage))
			}
		}},
		{"model from a transcript", func(c *sessionCache) { c.Model = "model\x1b[31m-a" + strings.Repeat("x", 200) }, func(t *testing.T, r collab.MissionsSyncInput) {
			if !strings.HasPrefix(r.Model, "model-a") || len([]rune(r.Model)) != collab.MaxModelLength {
				t.Errorf("model %q", r.Model)
			}
		}},
		{"full queues", func(c *sessionCache) {
			for i := range 2 * collab.MaxListMembers {
				c.Acks = append(c.Acks, "msg-"+strconv.Itoa(i))
			}
			for i := range 2 * collab.MaxSyncReplies {
				c.Replies = append(c.Replies, messageReply{Message: "msg-" + strconv.Itoa(i), Reply: strings.Repeat("r", collab.MaxReplyLength)})
				c.Decisions = append(c.Decisions, "dec-"+strconv.Itoa(i))
			}
		}, func(t *testing.T, r collab.MissionsSyncInput) {
			if len(r.Delivered) != collab.MaxListMembers || len(r.Replies) != collab.MaxSyncReplies || len(r.Decisions) != collab.MaxSyncDecisions {
				t.Errorf("%d delivered, %d replies, %d decisions", len(r.Delivered), len(r.Replies), len(r.Decisions))
			}
		}},
		{"cleared plan", func(c *sessionCache) { c.PlanDigest = planDigest(nil) }, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.Plan == nil || r.Plan.Steps == nil || len(r.Plan.Steps) != 0 {
				t.Errorf("plan %+v", r.Plan)
			}
		}},
		{"full plan", func(c *sessionCache) {
			steps := make([]wholePlanStep, 0, 2*sessionPlanMaxSteps)
			for i := range 2 * sessionPlanMaxSteps {
				steps = append(steps, wholePlanStep{text: strconv.Itoa(i) + strings.Repeat(" step\n", 100), status: "in_progress"})
			}
			c.Plan = replacePlan(steps)
			c.PlanDigest = planDigest(c.Plan)
		}, func(t *testing.T, r collab.MissionsSyncInput) {
			if r.Plan == nil || len(r.Plan.Steps) != sessionPlanMaxSteps {
				t.Errorf("plan %+v", r.Plan)
			}
		}},
	} {
		cache := base()
		if tc.edit != nil {
			tc.edit(cache)
		}
		request, _ := buildSyncRequest(cache, sessionEpoch.Add(1500*time.Millisecond))
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, diags := collab.ParseRequest(collab.ContractMissions, 1, collab.OperationSync, data); diag.HasErrors(diags) {
			t.Errorf("%s: the request is invalid: %v", tc.name, diags)
			continue
		}
		if request.ObservedAt != "2026-10-21T10:00:01.5Z" {
			t.Errorf("%s: observed at %q", tc.name, request.ObservedAt)
		}
		tc.check(t, request)
	}
}

// TestTheSyncPaceStaysWithinBounds pins the throttle: the provider sets the
// pace, within the contract's floor and ceiling.
func TestTheSyncPaceStaysWithinBounds(t *testing.T) {
	spectest.Proves(t, sessionFeature, wakeRequirement, "the-idle-wake-ends-after-its-window")
	for ms, want := range map[int64]time.Duration{
		0:         sessionSyncInterval,
		1:         5 * time.Second,
		45_000:    45 * time.Second,
		3_600_000: 10 * time.Minute,
	} {
		if got := syncInterval(&sessionCache{MinIntervalMS: ms}); got != want {
			t.Errorf("syncInterval(%d) = %v, want %v", ms, got, want)
		}
	}
}

// TestMergeKeepsAMessageOnce pins that a message delivered once never comes
// again, and that a sync about a mission the session left changes only what
// the sync sent.
func TestMergeKeepsAMessageOnce(t *testing.T) {
	spectest.Proves(t, sessionFeature, stopRequirement, "a-waiting-message-continues-the-turn-once")
	cache := &sessionCache{Repository: sessionRepo, Mission: sessionMissionState{Key: sessionBranchKey}, Seen: []string{promptID}, Acks: []string{promptID}}
	request, snapshot := buildSyncRequest(cache, sessionEpoch)
	answer := collab.MissionsSyncResult{
		Mission:       collab.MissionView{Key: sessionBranchKey, Title: "T", OpenDecisions: []collab.OpenDecision{{ID: "bad id", Question: "q"}, {ID: decisionID, Question: "q"}}},
		Inbox:         []inboxMessage{promptMessage(), infoMessage(), infoMessage()},
		MinIntervalMs: 7000,
	}
	if err := mergeSyncAnswer(cache, snapshot, answer, sessionEpoch); err != nil {
		t.Fatal(err)
	}
	if len(cache.Inbox) != 1 || cache.Inbox[0].ID != infoID || cache.Acks != nil || cache.MinIntervalMS != 7000 || len(cache.Mission.OpenDecisions) != 1 {
		t.Errorf("inbox %+v, acks %v, pace %d, decisions %v", cache.Inbox, cache.Acks, cache.MinIntervalMS, cache.Mission.OpenDecisions)
	}
	if !slices.Equal(request.Delivered, []string{promptID}) {
		t.Errorf("delivered %v", request.Delivered)
	}
	// The session moved to another mission during the call.
	cache.Mission = sessionMissionState{Key: "issue-9"}
	cache.Acks = []string{"msg-x"}
	_, snapshot = buildSyncRequest(cache, sessionEpoch)
	cache.Mission = sessionMissionState{Key: "issue-10"}
	if err := mergeSyncAnswer(cache, snapshot, collab.MissionsSyncResult{Mission: collab.MissionView{Key: "issue-9", Title: "Nine"}, Inbox: []inboxMessage{promptMessage()}}, sessionEpoch); err != nil {
		t.Fatal(err)
	}
	if cache.Mission.Known || cache.Mission.Title != "" || cache.Acks != nil {
		t.Errorf("an answer about another mission changed %+v", cache.Mission)
	}
}
