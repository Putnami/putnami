package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/local-collaboration/internal/store"
)

// missionsWriterVariable makes the test binary a missions writer instead of
// a test run: it reads a writerPlan from the variable, reports "ready" on its
// standard output, waits for one line on its standard input, then runs the
// plan against the store and prints its writerReport.
const missionsWriterVariable = "PUTNAMI_LOCAL_MISSIONS_WRITER"

func TestMain(m *testing.M) {
	if plan := os.Getenv(missionsWriterVariable); plan != "" {
		os.Exit(runMissionsWriter(plan, os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

// writerPlan is what one writer process does: syncs of its own session,
// which take the owner files both see, the ask both writers send, a reply of
// its own to the message both reply to, then asks of its own.
type writerPlan struct {
	Workspace string `json:"workspace"`
	Role      string `json:"role"`
	Message   string `json:"message"`
	Syncs     int    `json:"syncs"`
	Asks      int    `json:"asks"`
}

// writerOutcome is one call's answer, as a writer reports it.
type writerOutcome struct {
	Outcome  collab.Outcome `json:"outcome"`
	Reason   string         `json:"reason,omitempty"`
	ID       string         `json:"id,omitempty"`
	Replayed bool           `json:"replayed"`
}

type writerReport struct {
	Syncs  []writerOutcome `json:"syncs"`
	Shared writerOutcome   `json:"shared"`
	Reply  writerOutcome   `json:"reply"`
	Asks   []writerOutcome `json:"asks"`
}

func runMissionsWriter(encoded string, stdin io.Reader, stdout io.Writer) int {
	var plan writerPlan
	if err := json.Unmarshal([]byte(encoded), &plan); err != nil {
		return 2
	}
	if _, err := io.WriteString(stdout, "ready\n"); err != nil {
		return 2
	}
	if _, err := bufio.NewReader(stdin).ReadString('\n'); err != nil {
		return 2
	}
	report, err := writeMissions(New(), plan)
	if err != nil {
		return 2
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		return 2
	}
	return 0
}

// writeMissions runs one writer's plan through collab.Serve, as a routed
// call reaches the provider.
func writeMissions(p *Provider, plan writerPlan) (writerReport, error) {
	var report writerReport
	ask := func(key string) (writerOutcome, error) {
		arguments := `{"mission":{"repository":"acme/app","key":"issue-7"},"question":"Ship the change?","options":[{"id":"yes","label":"Ship it"},{"id":"no","label":"Wait"}],"idempotencyKey":"` + key + `"}`
		response, err := serveMissions(p, plan.Workspace, collab.OperationAsk, arguments)
		if err != nil || response.Outcome != collab.OutcomeOK {
			return outcomeOf(response), err
		}
		var result collab.MissionsAskResult
		if err := json.Unmarshal(response.Result, &result); err != nil {
			return writerOutcome{}, err
		}
		return writerOutcome{Outcome: response.Outcome, ID: result.Decision.ID, Replayed: result.Replayed}, nil
	}
	for i := 0; i < plan.Syncs; i++ {
		arguments := `{"session":{"engine":"host-a","id":"writer-` + plan.Role + `"},"mission":` + missionArguments +
			`,"event":"post_tool_use","state":"working","observedAt":"2026-09-24T07:00:0` + strconv.Itoa(i) + `Z"}`
		response, err := serveMissions(p, plan.Workspace, collab.OperationSync, arguments)
		if err != nil {
			return report, err
		}
		report.Syncs = append(report.Syncs, outcomeOf(response))
	}
	var err error
	if report.Shared, err = ask("shared"); err != nil {
		return report, err
	}
	response, err := serveMissions(p, plan.Workspace, collab.OperationReply, `{"message":"`+plan.Message+`","reply":"from `+plan.Role+`"}`)
	if err != nil {
		return report, err
	}
	report.Reply = outcomeOf(response)
	if response.Outcome == collab.OutcomeOK {
		var result collab.MissionsReplyResult
		if err := json.Unmarshal(response.Result, &result); err != nil {
			return report, err
		}
		report.Reply.ID, report.Reply.Replayed = result.Message.ID, result.Replayed
	}
	for i := 0; i < plan.Asks; i++ {
		outcome, err := ask(plan.Role + "-" + strconv.Itoa(i))
		if err != nil {
			return report, err
		}
		report.Asks = append(report.Asks, outcome)
	}
	return report, nil
}

func outcomeOf(response collab.Response) writerOutcome {
	outcome := writerOutcome{Outcome: response.Outcome}
	if response.Error != nil {
		outcome.Reason = response.Error.Reason
	}
	return outcome
}

func serveMissions(p *Provider, workspace, operation, arguments string) (collab.Response, error) {
	payload, err := json.Marshal(extension.ToolCallRequest{
		Name:          "local-collaboration.missions." + operation,
		Arguments:     json.RawMessage(arguments),
		WorkspaceRoot: workspace,
		Provider:      &extension.ToolProviderCall{Contract: collab.ContractMissions, Version: 1, Operation: operation},
	})
	if err != nil {
		return collab.Response{}, err
	}
	var out bytes.Buffer
	if err := collab.Serve(context.Background(), bytes.NewReader(payload), &out, p.Handlers()); err != nil {
		return collab.Response{}, err
	}
	var result extension.ToolCallResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Content) != 1 {
		return collab.Response{}, fmt.Errorf("the provider wrote no tool result: %s", out.String())
	}
	response, diags := collab.ParseResponse([]byte(result.Content[0].Text))
	if diags != nil {
		return collab.Response{}, fmt.Errorf("the provider answered an invalid response: %s", collab.FormatDiagnostics(diags))
	}
	return *response, nil
}

// TestConcurrentProcessesAskAndReplyOnce runs two writer processes against
// one store at once: this test and a copy of its binary. Both sync while the
// owner's message files and an answer file wait, both ask the same decision
// under one key, both reply to one message with different texts, and each
// asks decisions of its own. Each owner file is taken once, exactly one ask
// records the shared decision and the other replays it, exactly one reply
// lands and the other is message.closed, and no decision or message is lost
// or issued twice.
func TestConcurrentProcessesAskAndReplyOnce(t *testing.T) {
	spectest.Proves(t, "collaboration/provider-contract", "missions-provider-semantics", "the-local-provider-asks-and-replies-once-across-concurrent-processes")
	h := newMissionsHarness(t)
	writeOwnerFile(t, h.store, store.MessagesDir, "gate.json",
		ownerMessage{Mission: store.MissionKey{Repository: "acme/app", Key: "issue-7"}, Kind: collab.InboxKindPrompt, Text: "Does the gate pass?"})
	message := takenMessage(t, h.provider, h.store, "gate.json")
	answered := h.ask("answered").Decision.ID
	writeOwnerFile(t, h.store, store.AnswersDir, answered+".json", ownerAnswer{Option: "yes"})
	const files = 12
	for i := 0; i < files; i++ {
		writeOwnerFile(t, h.store, store.MessagesDir, "note-"+strconv.Itoa(i)+".json",
			ownerMessage{Mission: store.MissionKey{Repository: "acme/app", Key: "issue-7"}, Text: "Note " + strconv.Itoa(i) + "."})
	}
	const asks, syncs = 20, 3

	plan, _ := json.Marshal(writerPlan{Workspace: h.workspace, Role: "b", Message: message, Syncs: syncs, Asks: asks})
	other := exec.Command(os.Args[0], "-test.run=^$")
	other.Env = append(os.Environ(), missionsWriterVariable+"="+string(plan))
	start, err := other.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := other.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(time.Minute, func() { _ = other.Process.Kill() })
	defer timer.Stop()
	reader := bufio.NewReader(output)
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the other writer answered %q (%v)", line, err)
	}
	if _, err := io.WriteString(start, "go\n"); err != nil {
		t.Fatal(err)
	}
	ours, err := writeMissions(New(), writerPlan{Workspace: h.workspace, Role: "a", Message: message, Syncs: syncs, Asks: asks})
	if err != nil {
		t.Fatal(err)
	}
	var theirs writerReport
	if err := json.NewDecoder(reader).Decode(&theirs); err != nil {
		t.Fatalf("the other writer reported nothing: %v", err)
	}
	if err := other.Wait(); err != nil {
		t.Fatalf("the other writer: %v", err)
	}

	for _, outcome := range append(ours.Syncs, theirs.Syncs...) {
		if outcome.Outcome != collab.OutcomeOK {
			t.Fatalf("a sync answered %+v", outcome)
		}
	}
	if len(ours.Syncs) != syncs || len(theirs.Syncs) != syncs {
		t.Fatalf("the writers synced %d and %d times, want %d each", len(ours.Syncs), len(theirs.Syncs), syncs)
	}
	if ours.Shared.Outcome != collab.OutcomeOK || theirs.Shared.Outcome != collab.OutcomeOK || ours.Shared.ID != theirs.Shared.ID ||
		ours.Shared.Replayed == theirs.Shared.Replayed {
		t.Fatalf("the shared ask answered %+v and %+v; want one decision, recorded once and replayed once", ours.Shared, theirs.Shared)
	}
	winner, loser := ours.Reply, theirs.Reply
	text := "from a"
	if winner.Outcome != collab.OutcomeOK {
		winner, loser, text = theirs.Reply, ours.Reply, "from b"
	}
	if winner.Outcome != collab.OutcomeOK || winner.Replayed || loser.Outcome != collab.OutcomeConflict || loser.Reason != collab.ReasonMessageClosed {
		t.Fatalf("the two replies answered %+v and %+v; want one replied and one %s", ours.Reply, theirs.Reply, collab.ReasonMessageClosed)
	}
	ids := map[string]bool{ours.Shared.ID: true}
	for _, outcome := range append(ours.Asks, theirs.Asks...) {
		if outcome.Outcome != collab.OutcomeOK || outcome.Replayed || ids[outcome.ID] {
			t.Fatalf("an ask of its own answered %+v; want a new decision", outcome)
		}
		ids[outcome.ID] = true
	}
	held := h.missions()
	if len(held.Decisions) != 2*asks+2 || held.Next.Decision != 2*asks+2 || len(ids) != 2*asks+1 {
		t.Fatalf("the store holds %d decisions, counter %d, after %d distinct asks and one before", len(held.Decisions), held.Next.Decision, len(ids))
	}
	taken := map[string]int{}
	answers := 0
	messageIDs := map[string]bool{}
	for _, message := range held.Messages {
		if messageIDs[message.ID] {
			t.Fatalf("the message id %s was issued twice", message.ID)
		}
		messageIDs[message.ID] = true
		if message.Source != nil {
			taken[message.Source.File]++
		}
		if message.Decision == answered {
			answers++
		}
	}
	if len(held.Messages) != files+2 || held.Next.Message != files+2 || answers != 1 || len(taken) != files+1 {
		t.Fatalf("the store holds %d messages, counter %d, %d answers to %s and %d files taken; want each file and the answer once",
			len(held.Messages), held.Next.Message, answers, answered, len(taken))
	}
	for file, count := range taken {
		if count != 1 {
			t.Fatalf("the owner file %s was taken %d times", file, count)
		}
	}
	if decision := held.Decisions[decisionIndex(held, answered)]; decision.State != collab.DecisionStateAnswered || decision.Option != "yes" {
		t.Fatalf("the answered decision is %+v", decision)
	}
	if reply := held.Messages[messageIndex(held, message)].Reply; reply != text {
		t.Fatalf("the message holds the reply %q; the reply that landed was %q", reply, text)
	}
}
