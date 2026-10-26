package agentsession

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
)

// The feature the tests of this package prove.
const sessionFeature = "tooling/agent-session-hooks"

const (
	claudeSession    = "ebc08b6f-8d54-47ac-9c30-e5d1b724ebc8"
	codexSession     = "01a15d8d-8dda-7830-92b9-2112bcd234d7"
	sessionBranch    = "feat/session-hooks"
	sessionBranchKey = "branch:" + sessionBranch
	sessionRepo      = "acme/widgets"
	sessionOrigin    = "git@example.com:Acme/Widgets.git"
	promptID         = "msg-prompt"
	infoID           = "msg-info"
	answerID         = "msg-answer"
	decisionID       = "dec-logo"
)

var sessionEpoch = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)

func promptMessage() inboxMessage {
	return inboxMessage{ID: promptID, Kind: collab.InboxKindPrompt, Text: "Also write b.txt.", CreatedAt: "2026-10-21T09:58:00Z"}
}

func infoMessage() inboxMessage {
	return inboxMessage{ID: infoID, Kind: collab.InboxKindInfo, Text: "The review moved to Friday.", CreatedAt: "2026-10-21T09:59:30.5Z"}
}

func answerMessage() inboxMessage {
	return inboxMessage{ID: answerID, Kind: collab.InboxKindAnswer, Text: "green", CreatedAt: "2026-10-21T10:01:00Z",
		Decision: &collab.InboxDecision{ID: decisionID, Question: "Which color should the logo be?", Answer: "green", Option: "green"}}
}

const (
	promptRendered = `The owner sent this message at 09:58 UTC (message id msg-prompt): "Also write b.txt."`
	infoRendered   = `The owner sent this information at 09:59 UTC (message id msg-info): "The review moved to Friday."`
	answerRendered = "The owner answered the open decision dec-logo at 10:01 UTC (message id msg-answer).\nQuestion:\n> Which color should the logo be?\nAnswer:\n> green"
)

// fakeMissions is a missions and memory provider behind a fake host channel.
// It holds every request to the contract's rules, as the CLI route does, and
// every result it answers to the result rules. It keeps a mission's state as
// a provider does: the last plan, the open decisions and an inbox whose
// messages leave once a sync reports them delivered.
type fakeMissions struct {
	t  *testing.T
	mu sync.Mutex

	calls   []string
	syncs   []collab.MissionsSyncInput
	invalid []string
	closes  int

	title  string
	plan   []collab.PlanStep
	open   []collab.OpenDecision
	inbox  []collab.InboxMessage
	refuse map[string]string
	pace   int

	// refusePlan answers invalid to a sync that carries a plan.
	refusePlan bool
	// broken fails each call on the channel while it is set.
	broken error
	// failSync answers this envelope to a sync instead of the result.
	failSync *collab.Envelope
	// memory answers memory.mission; nil means the workspace binds no memory.
	memory *collab.Envelope
	// onSync runs inside each sync, after the provider applied it.
	onSync func(in collab.MissionsSyncInput)
	// answer changes each sync result before it goes out.
	answer func(*collab.MissionsSyncResult)
}

func newFakeMissions(t *testing.T) *fakeMissions {
	f := &fakeMissions{t: t, refuse: map[string]string{}, pace: 30_000}
	t.Cleanup(func() {
		if len(f.invalid) > 0 {
			t.Errorf("the provider refused %d requests as invalid: %v", len(f.invalid), f.invalid)
		}
	})
	return f
}

func (f *fakeMissions) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeMissions) Call(_ context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, contract+"."+operation)
	if f.broken != nil {
		return collab.Envelope{}, f.broken
	}
	parsed, diags := collab.ParseRequest(contract, 1, operation, input)
	if diag.HasErrors(diags) {
		f.invalid = append(f.invalid, contract+"."+operation+": "+diags[0].Message)
		return f.envelope(contract, operation, collab.OutcomeInvalid, nil, &collab.Error{Message: diags[0].Message, Reason: collab.ReasonRequestInvalid}), nil
	}
	switch request := parsed.(type) {
	case *collab.MissionsSyncInput:
		return f.sync(*request), nil
	case *collab.MemoryMissionInput:
		if f.memory == nil {
			return f.envelope(contract, operation, collab.OutcomeUnsupported, nil, &collab.Error{Message: "no memory provider is bound", Reason: collab.ReasonBindingMissing}), nil
		}
		return *f.memory, nil
	}
	f.t.Errorf("the hook called %s.%s", contract, operation)
	return f.envelope(contract, operation, collab.OutcomeUnsupported, nil, &collab.Error{Message: "unexpected"}), nil
}

func (f *fakeMissions) envelope(contract, operation string, outcome collab.Outcome, result any, failure *collab.Error) collab.Envelope {
	envelope := collab.Envelope{Contract: contract, Version: 1, Operation: operation, Provider: &collab.ProviderIdentity{Name: "@acme/missions"}, Outcome: outcome, Error: failure}
	if result != nil {
		data, err := json.Marshal(result)
		if err != nil {
			f.t.Fatal(err)
		}
		if _, diags := collab.ParseResult(contract, 1, operation, data); diag.HasErrors(diags) {
			f.t.Fatalf("the fake answers an invalid %s.%s result: %v", contract, operation, diags)
		}
		envelope.Result = data
	}
	return envelope
}

func (f *fakeMissions) sync(in collab.MissionsSyncInput) collab.Envelope {
	if f.refusePlan && in.Plan != nil {
		f.syncs = append(f.syncs, in)
		return f.envelope(collab.ContractMissions, collab.OperationSync, collab.OutcomeInvalid, nil, &collab.Error{Message: "plan.steps[0].text is refused", Reason: collab.ReasonRequestInvalid})
	}
	f.syncs = append(f.syncs, in)
	if f.failSync != nil {
		return *f.failSync
	}
	if in.Plan != nil {
		f.plan = append([]collab.PlanStep{}, in.Plan.Steps...)
	}
	if f.title == "" {
		f.title = in.Mission.Title
	}
	kept := f.inbox[:0]
	for _, message := range f.inbox {
		if !slices.Contains(in.Delivered, message.ID) {
			kept = append(kept, message)
		}
	}
	f.inbox = kept
	result := collab.MissionsSyncResult{
		Session:   collab.SessionStatus{State: in.State},
		Mission:   collab.MissionView{Key: in.Mission.Key, Title: f.title, Plan: collab.MissionPlan{Steps: append([]collab.PlanStep{}, f.plan...)}, OpenDecisions: append([]collab.OpenDecision{}, f.open...)},
		Inbox:     append([]collab.InboxMessage{}, f.inbox...),
		Decisions: []collab.DecisionStatus{},
		Replies:   []collab.ReplyStatus{},
		Now:       "2026-10-21T10:00:00Z",
	}
	result.MinIntervalMs = f.pace
	for _, id := range in.Decisions {
		result.Decisions = append(result.Decisions, collab.DecisionStatus{ID: id, State: collab.DecisionStateOpen})
	}
	for _, reply := range in.Replies {
		if reason, refused := f.refuse[reply.Message]; refused {
			result.Replies = append(result.Replies, collab.ReplyStatus{Message: reply.Message, State: collab.ReplyStateRefused, Reason: reason})
			continue
		}
		result.Replies = append(result.Replies, collab.ReplyStatus{Message: reply.Message, State: collab.ReplyStateReplied})
	}
	if f.onSync != nil {
		f.onSync(in)
	}
	if f.answer != nil {
		f.answer(&result)
	}
	return f.envelope(collab.ContractMissions, collab.OperationSync, collab.OutcomeOK, result, nil)
}

// bindMemory makes memory.mission answer a checkpoint with content.
func (f *fakeMissions) bindMemory(key, revision, content, updatedAt string) {
	record := collab.MemoryRecordResult{Record: collab.MemoryRecord{
		Ref: collab.Ref{Source: "local:memory", ID: key}, Revision: revision, Kind: collab.MemoryKindMission,
		Identity: collab.MemoryIdentity{Repository: sessionRepo, Mission: key}, Content: content,
		Provenance: collab.Provenance{RecordedAt: updatedAt},
		Freshness:  collab.Freshness{UpdatedAt: updatedAt, RetrievedAt: updatedAt},
	}}
	envelope := f.envelope(collab.ContractMemory, collab.OperationMission, collab.OutcomeOK, record, nil)
	f.memory = &envelope
}

func (f *fakeMissions) recordedSyncs() []collab.MissionsSyncInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.syncs)
}

func (f *fakeMissions) lastSync(t *testing.T) collab.MissionsSyncInput {
	t.Helper()
	syncs := f.recordedSyncs()
	if len(syncs) == 0 {
		t.Fatal("no sync")
	}
	return syncs[len(syncs)-1]
}

func (f *fakeMissions) recordedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeMissions) post(messages ...inboxMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inbox = append(f.inbox, messages...)
}

// harness is one checkout on a branch with an origin, one Putnami home, a
// fake provider behind the host channel and a clock the test moves. A sleep
// moves the clock and is recorded.
type harness struct {
	t        *testing.T
	root     string
	now      time.Time
	provider *fakeMissions
	env      *Env
	branch   string
	origin   string
	gitCalls []string
	slept    []time.Duration
	environ  map[string]string
	// onSleep runs after a sleep moved the clock, with the index of that
	// sleep: what happens outside the hook while it sleeps.
	onSleep func(index int)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := canonicalRoot(t.TempDir())
	if err := os.WriteFile(filepath.Join(root, workspaceManifest), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, root: root, now: sessionEpoch, provider: newFakeMissions(t), branch: sessionBranch, origin: sessionOrigin, environ: map[string]string{}}
	h.env = &Env{
		Home: t.TempDir(), Root: root, Host: h.provider,
		Git: func(_ context.Context, _ string, args ...string) (string, error) {
			command := strings.Join(args, " ")
			h.gitCalls = append(h.gitCalls, command)
			switch {
			case command == "remote get-url origin" && h.origin != "":
				return h.origin, nil
			case command == "symbolic-ref --short -q HEAD" && h.branch != "":
				return h.branch, nil
			}
			return "", errors.New("exit status 1")
		},
		Now: func() time.Time { return h.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			h.slept = append(h.slept, d)
			h.now = h.now.Add(d)
			if h.onSleep != nil {
				h.onSleep(len(h.slept) - 1)
			}
			return nil
		},
		Getenv: func(key string) string { return h.environ[key] },
	}
	return h
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

// payload reads one captured hook payload and points it at the harness
// checkout and the given session.
func (h *harness) payload(engine, fixture, session string, edit func(map[string]any)) []byte {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "hooks", engine, fixture+".json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		h.t.Fatal(err)
	}
	payload["cwd"] = h.root
	payload["session_id"] = session
	if edit != nil {
		edit(payload)
	}
	out, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

// run runs `agent hook` with args on stdin and answers the exit code and both
// streams.
func (h *harness) run(args []string, stdin []byte) (int, string, string) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	code := Hook(context.Background(), h.env, args, bytes.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// hook runs `agent hook --engine <engine>` on one payload and answers what it
// printed.
func (h *harness) hook(engine, fixture, session string, edit func(map[string]any)) *hookOutput {
	h.t.Helper()
	return h.hookRaw(engine, h.payload(engine, fixture, session, edit))
}

func (h *harness) hookRaw(engine string, stdin []byte) *hookOutput {
	h.t.Helper()
	code, stdout, stderr := h.run([]string{"--engine", engine}, stdin)
	if code != 0 || stderr != "" {
		h.t.Fatalf("the hook exited %d with stderr %q", code, stderr)
	}
	if stdout == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 1 {
		h.t.Fatalf("the hook printed %d lines: %q", len(lines), stdout)
	}
	decoder := json.NewDecoder(strings.NewReader(lines[0]))
	decoder.DisallowUnknownFields()
	var output hookOutput
	if err := decoder.Decode(&output); err != nil {
		h.t.Fatalf("hook output %q: %v", lines[0], err)
	}
	return &output
}

// wake runs the idle wake on the Stop payload of the Claude Code session and
// answers the text it wakes the session with, or "" when it exited 0. It
// never prints on stdout.
func (h *harness) wake(edit func(map[string]any)) string {
	h.t.Helper()
	code, stdout, stderr := h.run([]string{"--engine", sessionEngineClaudeCode, "--idle-wake"}, h.payload(sessionEngineClaudeCode, "stop", claudeSession, edit))
	if stdout != "" {
		h.t.Fatalf("the idle wake printed %q", stdout)
	}
	switch code {
	case 0:
		if stderr != "" {
			h.t.Fatalf("the idle wake exited 0 with stderr %q", stderr)
		}
		return ""
	case ExitWake:
		if stderr == "" {
			h.t.Fatal("the idle wake exited 75 without a text")
		}
		return strings.TrimSuffix(stderr, "\n")
	}
	h.t.Fatalf("the idle wake exited %d: %q", code, stderr)
	return ""
}

func (h *harness) files(engine, session string) sessionFiles {
	return h.env.files(engine, session)
}

func (h *harness) cache(engine, session string) *sessionCache {
	h.t.Helper()
	cache, err := h.files(engine, session).read()
	if err != nil {
		h.t.Fatal(err)
	}
	if cache == nil {
		h.t.Fatalf("no cache for %s:%s", engine, session)
	}
	return cache
}

// seed changes the cache of a session under its lock.
func (h *harness) seed(engine, session string, change func(*sessionCache)) {
	h.t.Helper()
	if err := lockedUpdate(h.files(engine, session), func(cache *sessionCache) (*sessionCache, error) {
		if cache == nil {
			cache = &sessionCache{Engine: engine, Session: session}
		}
		change(cache)
		return cache, nil
	}); err != nil {
		h.t.Fatal(err)
	}
}

// hookLog answers the lines of hook.log.
func (h *harness) hookLog() []map[string]string {
	h.t.Helper()
	file, err := os.Open(filepath.Join(h.env.sessionsDir(), hookLogName))
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	var lines []map[string]string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var line map[string]string
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			h.t.Fatalf("hook.log line %q: %v", scanner.Text(), err)
		}
		lines = append(lines, line)
	}
	return lines
}

// lastMessage sets the agent's last message of a Stop payload.
func lastMessage(text string) func(map[string]any) {
	return func(payload map[string]any) { payload["last_assistant_message"] = text }
}

func contextOf(t *testing.T, out *hookOutput) string {
	t.Helper()
	if out == nil || out.HookSpecificOutput == nil {
		t.Fatalf("output = %+v, want additional context", out)
	}
	return out.HookSpecificOutput.AdditionalContext
}
