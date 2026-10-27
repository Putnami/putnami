package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
)

// The feature and requirements the agent command tests prove.
const (
	agentFeature        = "tooling/agent-session-hooks"
	agentChannel        = "host-channel"
	agentFailOpen       = "fail-open"
	agentSessionBranch  = "feat/session-hooks"
	agentSessionID      = "ebc08b6f-8d54-47ac-9c30-e5d1b724ebc8"
	agentSessionMission = "branch:" + agentSessionBranch
)

// agentCheckout is a workspace checkout on agentSessionBranch whose origin
// names acme/widgets.
func agentCheckout(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{
		{"init", "-q", "-b", agentSessionBranch, dir},
		{"-C", dir, "remote", "add", "origin", "https://example.com/acme/widgets.git"},
	} {
		command := exec.Command("git", args...)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// launchAgent runs `agent-<command> <args>` in dir as putnami launches an
// interactive subcommand, with stdin and a Putnami home of its own.
func launchAgent(t *testing.T, executable, dir, home, stdin string, args []string, extraEnv ...string) languageResult {
	t.Helper()
	command := exec.Command(executable, append(args, "--putnamiContext", filepath.Join(dir, "absent-context.json"))...)
	command.Dir = dir
	command.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "PUTNAMI_HOME=" + home, "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}, extraEnv...)
	command.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	result := languageResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		result.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("launch the executable: %v", err)
	}
	return result
}

func sessionStartPayload(t *testing.T, dir string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"session_id": agentSessionID, "cwd": dir, "hook_event_name": "SessionStart", "source": "startup"})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestTheHookReachesTheProviderThroughTheHostChannel runs the built
// executable as the SessionStart hook of a Claude Code session: it syncs the
// session over the host channel putnami names in the environment, reads the
// checkpoint, and prints the mission sheet and the owner's waiting message.
func TestTheHookReachesTheProviderThroughTheHostChannel(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, agentFeature, agentChannel, "the-hook-reaches-the-provider-through-the-host-channel")
	executable := languageExecutable(t)
	dir, home := agentCheckout(t), t.TempDir()
	host := newFakeHost(t, func(params wire.CollaborationCallParams) any {
		if params.Contract == collab.ContractMemory {
			return collab.Envelope{Contract: params.Contract, Version: 1, Operation: params.Operation, Outcome: collab.OutcomeUnsupported, Error: &collab.Error{Message: "no memory provider is bound", Reason: collab.ReasonBindingMissing}}
		}
		parsed, diags := collab.ParseRequest(params.Contract, 1, params.Operation, params.Input)
		if diag.HasErrors(diags) {
			return collab.Envelope{Contract: params.Contract, Version: 1, Operation: params.Operation, Outcome: collab.OutcomeInvalid, Error: &collab.Error{Message: diags[0].Message, Reason: collab.ReasonRequestInvalid}}
		}
		request := parsed.(*collab.MissionsSyncInput)
		result, _ := json.Marshal(collab.MissionsSyncResult{
			Session:   collab.SessionStatus{State: request.State},
			Mission:   collab.MissionView{Key: request.Mission.Key, Title: request.Mission.Title, Plan: collab.MissionPlan{Steps: []collab.PlanStep{}}, OpenDecisions: []collab.OpenDecision{}},
			Inbox:     []collab.InboxMessage{{ID: "msg-prompt", Kind: collab.InboxKindPrompt, Text: "Also write b.txt.", CreatedAt: "2026-10-21T09:58:00Z"}},
			Decisions: []collab.DecisionStatus{}, Replies: []collab.ReplyStatus{}, MinIntervalMs: 30000, Now: "2026-10-21T10:00:00Z",
		})
		return collab.Envelope{Contract: params.Contract, Version: 1, Operation: params.Operation, Outcome: collab.OutcomeOK, Result: result}
	})
	got := launchAgent(t, executable, dir, home, sessionStartPayload(t, dir), []string{"agent-hook", "--engine", "claude-code"}, host.env()...)
	if got.code != 0 || got.stderr != "" {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	var output struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &output); err != nil || output.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("stdout %q: %v", got.stdout, err)
	}
	text := output.HookSpecificOutput.AdditionalContext
	for _, want := range []string{
		"Mission branch:feat/session-hooks of repository acme/widgets.",
		`The owner sent this message at 09:58 UTC (message id msg-prompt): "Also write b.txt."`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("context\n%s\ndoes not hold %q", text, want)
		}
	}
	connections, requests := host.seen()
	if connections != 1 || len(requests) != 2 {
		t.Fatalf("%d connections and %d requests, want 1 and 2", connections, len(requests))
	}
	var sync wire.CollaborationCallParams
	if err := json.Unmarshal(requests[0].Params, &sync); err != nil || sync.Contract != collab.ContractMissions || sync.Operation != collab.OperationSync {
		t.Fatalf("first request %s", requests[0].Params)
	}
	var input collab.MissionsSyncInput
	if err := json.Unmarshal(sync.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.Mission != (collab.MissionName{Repository: "acme/widgets", Key: agentSessionMission, Title: agentSessionBranch}) ||
		input.Session != (collab.SessionRef{Engine: "claude-code", ID: agentSessionID}) || input.Branch != agentSessionBranch {
		t.Errorf("sync %+v", input)
	}
	// The session store lives in the Putnami home, never in the checkout.
	if _, err := os.Stat(filepath.Join(home, "contributor", "sessions")); err != nil {
		t.Errorf("the session store: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("the hook wrote into the checkout: %v", entries)
	}
}

// TestAHookWithoutAChannelExitsZero runs the built hook with no host channel:
// it exits 0, prints no error and leaves one line in hook.log.
func TestAHookWithoutAChannelExitsZero(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, agentFeature, agentFailOpen, "a-run-without-a-channel-exits-0")
	executable := languageExecutable(t)
	dir, home := agentCheckout(t), t.TempDir()
	got := launchAgent(t, executable, dir, home, `{"session_id":"`+agentSessionID+`","cwd":"`+dir+`","hook_event_name":"Stop","prompt_id":"p1"}`, []string{"agent-hook", "--engine", "claude-code"})
	if got.code != 0 || got.stderr != "" || got.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	file, err := os.Open(filepath.Join(home, "contributor", "sessions", "hook.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lines := 0
	for scanner := bufio.NewScanner(file); scanner.Scan(); lines++ {
		var line map[string]string
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || line["event"] != "Stop" || !strings.Contains(line["error"], wire.SocketEnv) {
			t.Errorf("hook.log line %q", scanner.Text())
		}
	}
	if lines != 1 {
		t.Errorf("hook.log holds %d lines", lines)
	}
	// attach needs no channel: it writes the declaration of the checkout.
	attach := launchAgent(t, executable, dir, home, "", []string{"agent-attach", "--issue", "596"})
	if attach.code != 0 || attach.stdout != "The sessions of this checkout on branch feat/session-hooks now report to mission issue-596.\n" {
		t.Errorf("attach exit %d, stdout %q, stderr %q", attach.code, attach.stdout, attach.stderr)
	}
}
