package epic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	epicFeature    = "tooling/epic"
	epicWaves      = "epic-waves"
	epicStatus     = "epic-status"
	epicCloudChild = "epic-cloud-child"
)

// workspace writes a workspace root with the example epic plan, every issue
// given a task 41 to 44, in .putnami/40/epic.json, and launches, when not
// empty, in .putnami/40/launches.json. sessions sets the envelope's
// sessions at once.
func workspace(t *testing.T, sessions int, launches ...Launch) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "putnami.workspace.json"), []byte(`{"options": {"@putnami/contributor": {"version": 1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "estimate", "testdata", "epic.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan map[string]any
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	plan["envelope"].(map[string]any)["sessions"] = sessions
	for i, issue := range plan["issues"].([]any) {
		issue.(map[string]any)["task"] = map[string]any{"source": "github:acme/app", "id": []string{"41", "42", "43", "44"}[i]}
	}
	dir := filepath.Join(root, ".putnami", "40")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dir, "epic.json"), plan)
	if len(launches) > 0 {
		writeJSONFile(t, filepath.Join(dir, LaunchesFile), Launches{ProtocolVersion: 1, Epic: "40", Launches: launches})
	}
	return root
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func launchContext(t *testing.T, members map[string]any) string {
	t.Helper()
	members["protocolVersion"] = 2
	path := filepath.Join(t.TempDir(), "context.json")
	writeJSONFile(t, path, members)
	return path
}

func launch(issue, branch, base string) Launch {
	return Launch{Issue: issue, Target: LaunchTargetLocal, Branch: branch, Base: base, RecordedAt: "2026-11-02T10:00:00Z"}
}

// TestTheEpicPlanPrintsTextOrTheJSONResult runs epic plan on the default
// path in text and in JSON, and pins its usage errors.
func TestTheEpicPlanPrintsTextOrTheJSONResult(t *testing.T) {
	t.Parallel()
	root := workspace(t, 2)
	run := func(contextFile string, args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := PlanCommand{Root: root, ContextFile: contextFile, Stdout: &stdout, Stderr: &stderr}.Run(args)
		return code, stdout.String(), stderr.String()
	}
	code, stdout, stderr := run("", "--epic", "40")
	want := "Epic " + filepath.Join(".putnami", "40", "epic.json") + ": Plans are sized and launched in waves\n" +
		"  Estimate: L, 52.0M to 147.1M tokens, 181 to 546 minutes\n" +
		"  Envelope: 330.0M tokens, 640 minutes, 2 sessions at once\n" +
		"Wave 1\n" +
		"  read-contract (task 41): M, 17.6M to 48.5M tokens, 44 to 144 minutes\n" +
		"  plan-command (task 42): S, 5.6M to 16.7M tokens, 31 to 86 minutes\n" +
		"Wave 2\n" +
		"  status-command (task 43): M, 23.2M to 65.2M tokens, 75 to 230 minutes, after read-contract, plan-command\n" +
		"Wave 3\n" +
		"  skill (task 44): S, 5.6M to 16.7M tokens, 31 to 86 minutes, after status-command\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
	}

	code, stdout, stderr = run(launchContext(t, map[string]any{"params": map[string]any{"output": "json"}}), "--epic=#40")
	var result struct {
		Epic  string     `json:"epic"`
		Waves [][]string `json:"waves"`
	}
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &result) != nil || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %q; want one JSON line", code, stdout, stderr)
	}
	if result.Epic != "40" || !reflect.DeepEqual(result.Waves, [][]string{{"read-contract", "plan-command"}, {"status-command"}, {"skill"}}) {
		t.Fatalf("result %+v", result)
	}

	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "give --epic <id> to read .putnami/<id>/epic.json, or --plan-file <file>"},
		{[]string{"--epic", "../x"}, 2, "--epic must be an epic id"},
		{[]string{"--epic", "1", "--epic", "2"}, 2, "--epic given twice"},
		{[]string{"--issue", "1"}, 2, `unexpected argument "--issue"`},
		{[]string{"--epic", "41"}, 1, filepath.Join(".putnami", "41", "epic.json") + ": no plan file"},
		{[]string{"--epic", "39", "--plan-file", filepath.Join(root, ".putnami", "40", "epic.json")}, 1, `the plan is for epic "40", not "39"`},
	} {
		code, stdout, stderr := run("", tc.args...)
		if code != tc.code || stdout != "" || !strings.Contains(stderr, tc.want) || !strings.HasPrefix(stderr, "epic plan: ") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit %d and %q", tc.args, code, stdout, stderr, tc.code, tc.want)
		}
	}
	spectest.Proves(t, epicFeature, epicWaves, "the-epic-plan-prints-text-or-the-json-result")
}

// provider answers the collaboration calls of a status from fixed state and
// counts them.
type provider struct {
	tasks       map[string]collab.TaskState
	sessions    map[string][]collab.MissionSession
	proposals   map[string]collab.Proposal
	heads       map[string]string
	unsupported map[string]bool
	// outcomes answers a call with an outcome other than ok, by
	// contract.operation, or by tasks.get:<id> for one task.
	outcomes map[string]collab.Outcome
	// broken fails a contract.operation with a transport error; hang blocks
	// it until its context ends.
	broken map[string]bool
	hang   map[string]bool
	calls  map[string]int
}

func (p *provider) call(ctx context.Context, contract, operation string, input json.RawMessage) (collab.Envelope, error) {
	name := contract + "." + operation
	p.calls[name]++
	if p.unsupported[contract] {
		return collab.Envelope{Outcome: collab.OutcomeUnsupported, Error: &collab.Error{Message: contract + " is not bound"}}, nil
	}
	if p.broken[name] {
		return collab.Envelope{}, errors.New("connection reset")
	}
	if p.hang[name] {
		<-ctx.Done()
		return collab.Envelope{}, ctx.Err()
	}
	key := name
	if name == "tasks.get" {
		var in collab.TaskRefInput
		_ = json.Unmarshal(input, &in)
		if _, one := p.outcomes[name+":"+in.Ref.ID]; one {
			key = name + ":" + in.Ref.ID
		}
	}
	if outcome, set := p.outcomes[key]; set {
		return collab.Envelope{Outcome: outcome, Error: &collab.Error{Message: key + " answered " + string(outcome)}}, nil
	}
	var result any
	switch contract + "." + operation {
	case "tasks.get":
		var in collab.TaskRefInput
		_ = json.Unmarshal(input, &in)
		result = collab.TaskResult{Task: collab.Task{Ref: in.Ref, State: p.tasks[in.Ref.ID]}}
	case "missions.status":
		var in collab.MissionsStatusInput
		_ = json.Unmarshal(input, &in)
		sessions := p.sessions[in.Mission.Key]
		if sessions == nil {
			sessions = []collab.MissionSession{}
		}
		result = collab.MissionsStatusResult{Mission: collab.MissionView{Key: in.Mission.Key}, Sessions: sessions}
	case "proposals.find":
		var in collab.ProposalFindInput
		_ = json.Unmarshal(input, &in)
		items := []collab.Proposal{}
		if found, ok := p.proposals[in.Change.Head]; ok && found.Change.Base == in.Change.Base {
			items = append(items, found)
		}
		result = collab.ProposalListResult{Items: items}
	case "proposals.status":
		var in collab.ProposalRefInput
		_ = json.Unmarshal(input, &in)
		for _, found := range p.proposals {
			if found.Ref == in.Ref {
				found.Change.HeadCommit = p.heads[in.Ref.ID]
				result = collab.ProposalStatusResult{Proposal: found}
			}
		}
	default:
		return collab.Envelope{Outcome: collab.OutcomeInvalid}, nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return collab.Envelope{}, err
	}
	return collab.Envelope{Contract: contract, Operation: operation, Outcome: collab.OutcomeOK, Result: data}, nil
}

// proposal is a proposal of head against main.
func proposal(id, head string, state collab.ProposalState) collab.Proposal {
	return collab.Proposal{Ref: collab.Ref{Source: "github:acme/app", ID: id}, URL: "https://example.test/pull/" + id,
		Change: collab.Change{Base: "main", Head: head}, State: state}
}

func session(stage collab.LoopStage, branch string, tokens int64) collab.MissionSession {
	return collab.MissionSession{Session: collab.SessionRef{Engine: "claude", ID: branch}, State: collab.SessionStateWorking,
		Stage: stage, Branch: branch, Usage: &collab.SessionUsage{InputTokens: tokens / 2, CacheReadTokens: tokens - tokens/2}}
}

func newProvider() *provider {
	return &provider{tasks: map[string]collab.TaskState{}, sessions: map[string][]collab.MissionSession{},
		proposals: map[string]collab.Proposal{}, heads: map[string]string{}, unsupported: map[string]bool{},
		outcomes: map[string]collab.Outcome{}, broken: map[string]bool{}, hang: map[string]bool{}, calls: map[string]int{}}
}

func runStatus(t *testing.T, root string, p *provider) (StatusResult, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := StatusCommand{Root: root, ContextFile: launchContext(t, map[string]any{"params": map[string]any{"output": "json"}}),
		Repository: "acme/app", Call: p.call, Stdout: &stdout, Stderr: &stderr}
	if code := command.Run(context.Background(), []string{"--epic", "40"}); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var result StatusResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	writeStatusText(&text, result)
	return result, text.String()
}

func issueByKey(t *testing.T, result StatusResult, key string) IssueStatus {
	t.Helper()
	for _, issue := range result.Issues {
		if issue.Key == key {
			return issue
		}
	}
	t.Fatalf("no issue %s", key)
	return IssueStatus{}
}

// TestTheStatusReadsEachChildThroughTheContracts reads a task, a mission's
// sessions and a proposal for each launched issue, and derives its state and
// tokens.
func TestTheStatusReadsEachChildThroughTheContracts(t *testing.T) {
	t.Parallel()
	root := workspace(t, 2, launch("read-contract", "b/read", "main"), launch("plan-command", "b/plan", "main"))
	p := newProvider()
	p.tasks["41"], p.tasks["42"] = collab.TaskStateInProgress, collab.TaskStateDone
	p.sessions["issue-41"] = []collab.MissionSession{session(collab.LoopStageCheck, "b/read", 10_000_000), session(collab.LoopStageCode, "b/read", 2_000_000)}
	p.sessions["issue-42"] = []collab.MissionSession{session(collab.LoopStageMerge, "b/plan", 5_000_000)}
	p.proposals["b/read"] = proposal("101", "b/read", collab.ProposalStateDraft)
	p.proposals["b/plan"] = proposal("102", "b/plan", collab.ProposalStateMerged)

	result, text := runStatus(t, root, p)
	read := issueByKey(t, result, "read-contract")
	if read.State != StateInProgress || read.Stage != collab.LoopStageCheck || read.Sessions != 2 || read.Tokens != 12_000_000 ||
		!read.UsageReported || read.Share != 48_500_000 || read.OverShare || read.Proposal == nil || read.Proposal.Ref.ID != "101" {
		t.Fatalf("read-contract %+v", read)
	}
	if plan := issueByKey(t, result, "plan-command"); plan.State != StateMerged || plan.TaskState != collab.TaskStateDone || plan.Tokens != 5_000_000 {
		t.Fatalf("plan-command %+v", plan)
	}
	if skill := issueByKey(t, result, "skill"); skill.State != StatePlanned || skill.UsageReported || skill.Proposal != nil {
		t.Fatalf("skill %+v", skill)
	}
	if result.Tokens != 17_000_000 || result.Running != 1 || result.EnvelopeSpent || len(result.Unavailable) != 0 {
		t.Fatalf("result %+v", result)
	}
	if p.calls["tasks.get"] != 4 || p.calls["missions.status"] != 4 || p.calls["proposals.find"] != 2 {
		t.Fatalf("calls %v, want a task and a mission per issue, a proposal per launched issue", p.calls)
	}
	for _, line := range []string{
		"  Spent: 17.0M of 330.0M tokens; 1 of 2 sessions running\n",
		"  read-contract (task 41): in progress at check, https://example.test/pull/101, 12.0M tokens of a 48.5M share\n",
		"  plan-command (task 42): merged at merge, https://example.test/pull/102, 5.0M tokens of a 16.7M share\n",
		"  skill (task 44): planned, no usage reported, share 16.7M\n",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("text %q lacks %q", text, line)
		}
	}

	p = newProvider()
	p.unsupported[collab.ContractMissions] = true
	result, text = runStatus(t, root, p)
	if p.calls["missions.status"] != 1 || len(result.Unavailable) != 1 || result.Unavailable[0].Issue != "" ||
		result.Unavailable[0].Outcome != string(collab.OutcomeUnsupported) || issueByKey(t, result, "read-contract").UsageReported {
		t.Fatalf("calls %v, unavailable %+v; want one missions call reported once", p.calls, result.Unavailable)
	}
	if !strings.Contains(text, "Not read\n  missions.status answered unsupported: missions is not bound\n") {
		t.Fatalf("text %q", text)
	}

	// Without the origin's repository, no mission is read, and the status
	// says so once.
	p = newProvider()
	var stdout, stderr bytes.Buffer
	command := StatusCommand{Root: root, Call: p.call, Stdout: &stdout, Stderr: &stderr}
	if code := command.Run(context.Background(), []string{"--epic", "40"}); code != 0 || p.calls["missions.status"] != 0 ||
		!strings.Contains(stdout.String(), "Not read\n  missions.status answered unsupported: the origin remote names no repository\n") {
		t.Fatalf("exit %d, calls %v, stdout %q, stderr %q", code, p.calls, stdout.String(), stderr.String())
	}

	// A closed early proposal on the branch never hides the one that
	// replaced it.
	closed, open, merged := proposal("100", "b/read", collab.ProposalStateClosed), proposal("101", "b/read", collab.ProposalStateOpen),
		proposal("102", "b/read", collab.ProposalStateMerged)
	for _, tc := range []struct {
		items []collab.Proposal
		want  string
	}{
		{[]collab.Proposal{closed, open}, "101"},
		{[]collab.Proposal{closed, open, merged}, "102"},
		{[]collab.Proposal{closed}, "100"},
	} {
		if got, ok := preferred(tc.items); !ok || got.Ref.ID != tc.want {
			t.Errorf("preferred %+v = %s, want %s", tc.items, got.Ref.ID, tc.want)
		}
	}
	if _, ok := preferred(nil); ok {
		t.Error("preferred found a proposal in an empty list")
	}
	spectest.Proves(t, epicFeature, epicStatus, "the-status-reads-each-child-through-the-contracts")
}

// TestAChildLaunchesOnMergedOrOneReadyParent launches an issue whose
// dependencies merged, or whose one unmerged dependency is ready, on that
// dependency's branch, and names the rebase once that parent merged.
func TestAChildLaunchesOnMergedOrOneReadyParent(t *testing.T) {
	t.Parallel()
	root := workspace(t, 2, launch("read-contract", "b/read", "main"), launch("plan-command", "b/plan", "main"))
	p := newProvider()
	p.proposals["b/read"] = proposal("101", "b/read", collab.ProposalStateOpen)
	p.proposals["b/plan"] = proposal("102", "b/plan", collab.ProposalStateMerged)
	result, text := runStatus(t, root, p)
	want := []Launchable{{Issue: "status-command", Base: "b/read", StacksOn: "read-contract"}}
	if !reflect.DeepEqual(result.Launchable, want) {
		t.Fatalf("launchable %+v, want %+v", result.Launchable, want)
	}
	if !strings.Contains(text, "Launch now\n  status-command: from b/read, stacked on read-contract\n") {
		t.Fatalf("text %q", text)
	}

	p.proposals["b/plan"] = proposal("102", "b/plan", collab.ProposalStateDraft)
	if result, _ := runStatus(t, root, p); len(result.Launchable) != 0 {
		t.Fatalf("launchable %+v with two unmerged dependencies, want none", result.Launchable)
	}

	root = workspace(t, 2, launch("read-contract", "b/read", "main"), launch("plan-command", "b/plan", "main"),
		launch("status-command", "b/status", "b/read"))
	p = newProvider()
	p.proposals["b/read"] = proposal("101", "b/read", collab.ProposalStateMerged)
	p.proposals["b/plan"] = proposal("102", "b/plan", collab.ProposalStateMerged)
	p.proposals["b/status"] = proposal("103", "b/status", collab.ProposalStateDraft)
	p.heads["101"] = "0123abc"
	result, text = runStatus(t, root, p)
	status := issueByKey(t, result, "status-command")
	if status.State != StateInProgress || status.Rebase != "git rebase --onto main 0123abc" || status.Proposal == nil || status.Proposal.Ref.ID != "103" {
		t.Fatalf("status-command %+v", status)
	}
	if !strings.Contains(text, "Rebase\n  status-command: git rebase --onto main 0123abc\n") {
		t.Fatalf("text %q", text)
	}

	// A stack is never two levels deep: skill waits while the ready parent it
	// needs is itself stacked on an unmerged branch.
	root = workspace(t, 3, launch("read-contract", "b/read", "main"), launch("plan-command", "b/plan", "main"),
		launch("status-command", "b/status", "b/read"))
	p = newProvider()
	p.proposals["b/read"] = proposal("101", "b/read", collab.ProposalStateOpen)
	p.proposals["b/plan"] = proposal("102", "b/plan", collab.ProposalStateMerged)
	status103 := proposal("103", "b/status", collab.ProposalStateOpen)
	status103.Change.Base = "b/read"
	p.proposals["b/status"] = status103
	if result, _ := runStatus(t, root, p); issueByKey(t, result, "status-command").State != StateReady || len(result.Launchable) != 0 {
		t.Fatalf("launchable %+v; want skill to wait for read-contract's merge", result.Launchable)
	}
	spectest.Proves(t, epicFeature, epicStatus, "a-child-launches-on-merged-or-one-ready-parent")
}

// TestASpentEnvelopeLaunchesNothing launches nothing once the children
// reported the envelope's tokens, nor past the envelope's sessions at once,
// and flags a child more than 50 % over its share.
func TestASpentEnvelopeLaunchesNothing(t *testing.T) {
	t.Parallel()
	root := workspace(t, 2)
	p := newProvider()
	p.sessions["issue-41"] = []collab.MissionSession{session(collab.LoopStageCode, "b/read", 330_000_000)}
	result, text := runStatus(t, root, p)
	if !result.EnvelopeSpent || len(result.Launchable) != 0 || !issueByKey(t, result, "read-contract").OverShare {
		t.Fatalf("result %+v; want a spent envelope, nothing to launch and an issue over its share", result)
	}
	if !strings.Contains(text, "The envelope is spent: launch nothing new.") || !strings.Contains(text, "more than 50 % over its share") {
		t.Fatalf("text %q", text)
	}

	p = newProvider()
	if result, _ := runStatus(t, root, p); !reflect.DeepEqual(result.Launchable, []Launchable{{Issue: "read-contract", Base: "main"}, {Issue: "plan-command", Base: "main"}}) {
		t.Fatalf("launchable %+v, want both first-wave issues", result.Launchable)
	}
	root = workspace(t, 1, launch("read-contract", "b/read", "main"))
	if result, _ := runStatus(t, root, p); result.Running != 1 || len(result.Launchable) != 0 {
		t.Fatalf("running %d, launchable %+v; want one session at once and none free", result.Running, result.Launchable)
	}

	// A ready child whose session still works takes a session; a child whose
	// sessions ended does not, and says so.
	root = workspace(t, 1, launch("read-contract", "b/read", "main"))
	p = newProvider()
	p.proposals["b/read"] = proposal("101", "b/read", collab.ProposalStateOpen)
	p.sessions["issue-41"] = []collab.MissionSession{session(collab.LoopStageValidate, "b/read", 1_000_000)}
	if result, _ := runStatus(t, root, p); result.Running != 1 || len(result.Launchable) != 0 {
		t.Fatalf("running %d, launchable %+v; want the ready child to take the one session", result.Running, result.Launchable)
	}
	ended := session(collab.LoopStageCode, "b/read", 1_000_000)
	ended.State = collab.SessionStateEnded
	p.sessions["issue-41"] = []collab.MissionSession{ended}
	p.proposals["b/read"] = proposal("101", "b/read", collab.ProposalStateDraft)
	result, text = runStatus(t, root, p)
	if result.Running != 0 || !reflect.DeepEqual(result.Launchable, []Launchable{{Issue: "plan-command", Base: "main"}}) ||
		!strings.Contains(text, "  read-contract (task 41): in progress at code, no live session, ") {
		t.Fatalf("running %d, launchable %+v, text %q; want the ended session to free its slot", result.Running, result.Launchable, text)
	}

	// A ready child whose mission is not read holds a session; one whose
	// session ended frees it.
	p = newProvider()
	p.proposals["b/read"] = proposal("101", "b/read", collab.ProposalStateOpen)
	p.unsupported[collab.ContractMissions] = true
	if result, _ := runStatus(t, root, p); issueByKey(t, result, "read-contract").State != StateReady ||
		result.Running != 1 || len(result.Launchable) != 0 {
		t.Fatalf("running %d, launchable %+v; want the unread ready child to hold the one session", result.Running, result.Launchable)
	}
	p.unsupported[collab.ContractMissions] = false
	p.sessions["issue-41"] = []collab.MissionSession{ended}
	result, _ = runStatus(t, root, p)
	if read := issueByKey(t, result, "read-contract"); read.State != StateReady || !read.SessionsRead || read.Live != 0 ||
		result.Running != 0 || !reflect.DeepEqual(result.Launchable, []Launchable{{Issue: "plan-command", Base: "main"}}) {
		t.Fatalf("running %d, launchable %+v; want the ready child's ended session to free its slot", result.Running, result.Launchable)
	}
	spectest.Proves(t, epicFeature, epicStatus, "a-spent-envelope-launches-nothing")
}

// TestADeniedOrMissingAnswerIsListed stops calling a contract that answered
// denied, and lists a per-issue not_found with its issue while the other
// issues are still read.
func TestADeniedOrMissingAnswerIsListed(t *testing.T) {
	t.Parallel()
	root := workspace(t, 2, launch("read-contract", "b/read", "main"))
	p := newProvider()
	p.outcomes["proposals.find"] = collab.OutcomeDenied
	p.outcomes["tasks.get:42"] = collab.OutcomeNotFound
	result, text := runStatus(t, root, p)
	if p.calls["proposals.find"] != 1 || p.calls["tasks.get"] != 4 {
		t.Fatalf("calls %v; want one proposals.find and every task read", p.calls)
	}
	want := []Unavailable{
		{Contract: collab.ContractProposals, Operation: collab.OperationFind, Outcome: string(collab.OutcomeDenied), Message: "proposals.find answered denied"},
		{Contract: collab.ContractTasks, Operation: collab.OperationGet, Issue: "plan-command", Outcome: string(collab.OutcomeNotFound), Message: "tasks.get:42 answered not_found"},
	}
	if !reflect.DeepEqual(result.Unavailable, want) {
		t.Fatalf("unavailable %+v, want %+v", result.Unavailable, want)
	}
	for _, line := range []string{
		"  proposals.find answered denied: proposals.find answered denied\n",
		"  tasks.get for plan-command answered not_found: tasks.get:42 answered not_found\n",
	} {
		if !strings.Contains(text, line) {
			t.Errorf("text %q lacks %q", text, line)
		}
	}
}

// TestATransportErrorStopsTheStatus exits 1 when a provider cannot be
// reached: a status built on a partial read would look complete.
func TestATransportErrorStopsTheStatus(t *testing.T) {
	t.Parallel()
	root := workspace(t, 2)
	p := newProvider()
	p.broken["missions.status"] = true
	var stdout, stderr bytes.Buffer
	command := StatusCommand{Root: root, Repository: "acme/app", Call: p.call, Stdout: &stdout, Stderr: &stderr}
	if code := command.Run(context.Background(), []string{"--epic", "40"}); code != 1 || stdout.Len() != 0 ||
		!strings.Contains(stderr.String(), "missions.status: connection reset") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

// TestTheReadStopsAtItsBudget lists what it read once the budget runs out,
// and names the call where it stopped.
func TestTheReadStopsAtItsBudget(t *testing.T) {
	t.Parallel()
	root := workspace(t, 2, launch("read-contract", "b/read", "main"))
	p := newProvider()
	p.hang["proposals.find"] = true
	var stdout, stderr bytes.Buffer
	command := StatusCommand{Root: root, Repository: "acme/app", Call: p.call, Budget: 200 * time.Millisecond, Stdout: &stdout, Stderr: &stderr}
	if code := command.Run(context.Background(), []string{"--epic", "40"}); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Not read\n  proposals.find for read-contract answered unavailable: the status stopped reading after 200ms\n") {
		t.Fatalf("stdout %q", stdout.String())
	}
	if p.calls["proposals.find"] != 1 {
		t.Fatalf("calls %v; want no call after the budget ran out", p.calls)
	}
}

// TestAClosedDependencyBlocksItsDependents names the dependency closed
// without a merge on each issue that waits for it, and launches none of them.
func TestAClosedDependencyBlocksItsDependents(t *testing.T) {
	t.Parallel()
	root := workspace(t, 2, launch("read-contract", "b/read", "main"), launch("plan-command", "b/plan", "main"))
	p := newProvider()
	p.proposals["b/read"] = proposal("101", "b/read", collab.ProposalStateClosed)
	p.proposals["b/plan"] = proposal("102", "b/plan", collab.ProposalStateMerged)
	result, text := runStatus(t, root, p)
	if status := issueByKey(t, result, "status-command"); !reflect.DeepEqual(status.BlockedBy, []string{"read-contract"}) {
		t.Fatalf("status-command %+v", status)
	}
	if len(result.Launchable) != 0 || !strings.Contains(text, "blocked: read-contract closed without a merge") {
		t.Fatalf("launchable %+v, text %q", result.Launchable, text)
	}
	if skill := issueByKey(t, result, "skill"); skill.BlockedBy != nil {
		t.Fatalf("skill %+v; only a direct dependency blocks", skill)
	}
}

// TestACloudLaunchIsRecordedAndReported accepts a launch record whose target
// is a cloud session, follows that child like a local one, and reports its
// target in JSON and in text.
func TestACloudLaunchIsRecordedAndReported(t *testing.T) {
	t.Parallel()
	cloud := launch("read-contract", "b/read", "main")
	cloud.Target = LaunchTargetCloud
	root := workspace(t, 2, cloud, launch("plan-command", "b/plan", "main"))
	p := newProvider()
	p.tasks["41"] = collab.TaskStateInProgress
	p.sessions["issue-41"] = []collab.MissionSession{session(collab.LoopStageCode, "b/read", 3_000_000)}
	result, text := runStatus(t, root, p)
	read := issueByKey(t, result, "read-contract")
	if read.Target != LaunchTargetCloud || read.State != StateInProgress || read.Branch != "b/read" || read.Tokens != 3_000_000 {
		t.Fatalf("read-contract %+v", read)
	}
	if plan := issueByKey(t, result, "plan-command"); plan.Target != LaunchTargetLocal {
		t.Fatalf("plan-command %+v", plan)
	}
	if skill := issueByKey(t, result, "skill"); skill.Target != "" {
		t.Fatalf("skill %+v; an issue not launched has no target", skill)
	}
	if !strings.Contains(text, "  read-contract (task 41): in progress at code in a cloud session, branch b/read, 3.0M tokens of a 48.5M share\n") {
		t.Fatalf("text %q", text)
	}
	if !strings.Contains(text, "  plan-command (task 42): launched, branch b/plan, no usage reported, share 16.7M\n") {
		t.Fatalf("text %q; a local child carries no cloud note", text)
	}

	// Once the epic session published the cloud child's proposal, the
	// status shows the proposal and drops the cloud note; JSON keeps it.
	p.proposals["b/read"] = proposal("101", "b/read", collab.ProposalStateDraft)
	result, text = runStatus(t, root, p)
	if read := issueByKey(t, result, "read-contract"); read.Target != LaunchTargetCloud || read.Proposal == nil {
		t.Fatalf("read-contract %+v", read)
	}
	if !strings.Contains(text, "  read-contract (task 41): in progress at code, https://example.test/pull/101, 3.0M tokens of a 48.5M share\n") {
		t.Fatalf("text %q", text)
	}
	spectest.Proves(t, epicFeature, epicStatus, "a-cloud-launch-is-recorded-and-reported")
}

// TestALaunchRecordIsChecked refuses a launch record that names an unknown
// issue, an unknown target or another epic.
func TestALaunchRecordIsChecked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		launch Launch
		want   string
	}{
		{launch("nothing", "b/x", "main"), `launches[0].issue: "nothing" names no issue of the epic`},
		{Launch{Issue: "skill", Target: "remote", Branch: "b/x", Base: "main", RecordedAt: "2026-11-02T10:00:00Z"}, `launches[0].target: must be "local" or "cloud"`},
		{Launch{Issue: "skill", Branch: "b/x", Base: "main", RecordedAt: "2026-11-02T10:00:00Z"}, `launches[0].target: must be "local" or "cloud"`},
		{Launch{Issue: "skill", Target: LaunchTargetLocal, Branch: "b x", Base: "main", RecordedAt: "2026-11-02T10:00:00Z"}, "launches[0].branch: must be a branch name"},
		{Launch{Issue: "skill", Target: LaunchTargetLocal, Branch: "b/x", Base: "main", RecordedAt: "yesterday"}, "launches[0].recordedAt: must be an RFC 3339 time"},
	} {
		root := workspace(t, 2, tc.launch)
		var stdout, stderr bytes.Buffer
		code := StatusCommand{Root: root, Call: newProvider().call, Stdout: &stdout, Stderr: &stderr}.Run(context.Background(), []string{"--epic", "40"})
		if code != 1 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%+v: exit %d, stderr %q; want %q", tc.launch, code, stderr.String(), tc.want)
		}
	}
}
