package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/mcp"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// An earlier change deleted MCP's parallel job pipeline — its own
// workspace load, extension discovery, project selection, planner call,
// SchedulerConfig and result reducer — and made run_jobs an adapter over
// engine.Run. THIS test is the deliverable of that change: proof that the two
// adapters, given the same inputs, produce the same plan and the same result.
//
// It lives in internal/cli because this is the only package that can reach both
// sides: internal/mcp cannot import internal/cli (cli → mcp), and the terminal
// mapping is terminalRequest here. Both sides are the shipping code —
// ParseArgs + terminalRequest for the terminal, the real MCP tool call over a
// real JSON-RPC session for the agent — so agreement is a property of what
// ships, not of a re-derivation written for the test.
//
// The fixture's tasks are cache:false on purpose: a cached second run would
// report `cached` where the first reported `succeeded`, and the comparison would
// be measuring the cache rather than the two adapters.

// Both surfaces speak ONE contract since an earlier change deleted the v1 emitters,
// so this test decodes them into the contract's own types instead of a pair of
// hand-written mirrors. That is a stronger comparison than the v1 pair allowed:
// the envelope's run member and the MCP result's run member are the SAME
// protocolcli.RunSummary, so the assertion is struct equality over the whole
// verdict rather than a field-by-field re-listing that could omit one.

// runSummaryDigest renders the verdict fields this test compares, for failure
// messages.
func runSummaryDigest(run *protocolcli.RunSummary) string {
	if run == nil {
		return "<no run summary>"
	}
	return fmt.Sprintf("outcome=%s exit=%d total=%d succeeded=%d failed=%d canceled=%d skipped=%d "+
		"localCache=%d remoteCache=%d coalesced=%d",
		run.Outcome, run.ExitCode, run.Counts.Total, run.Counts.Succeeded, run.Counts.Failed,
		run.Counts.Canceled, run.Counts.Skipped,
		run.Reuse.LocalCache, run.Reuse.RemoteCache, run.Reuse.Coalesced)
}

// sameRunVerdict reports whether two surfaces agree on the whole verdict: the
// outcome, the exit code and both histograms. RunCounts and RunReuse are
// comparable structs, so a member added to either is compared automatically.
func sameRunVerdict(a, b *protocolcli.RunSummary) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Outcome == b.Outcome && a.ExitCode == b.ExitCode &&
		a.Counts == b.Counts && a.Reuse == b.Reuse
}

// failureKeys lists a run's failures by their derived identity key, sorted.
func failureKeys(run *protocolcli.RunSummary) []string {
	if run == nil {
		return nil
	}
	keys := make([]string, 0, len(run.Failures))
	for _, failure := range run.Failures {
		keys = append(keys, failure.Identity.Key)
	}
	sort.Strings(keys)
	return keys
}

// TestMCPAndTerminalAgreeOnThePlan compares the two adapters' PLANS: same
// commands, same projects, same job keys in the same order. Planning executes
// nothing, so this half is exact — it is the DAG both surfaces would schedule.
func TestMCPAndTerminalAgreeOnThePlan(t *testing.T) {
	requireShell(t)
	wsRoot := writeEquivalenceFixture(t)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")

	for _, commands := range [][]string{{"build"}, {"lint"}, {"build", "lint"}} {
		t.Run(strings.Join(commands, ","), func(t *testing.T) {
			terminalPlan := terminalPlanJobs(t, wsRoot,
				[]string{strings.Join(commands, ","), "--projects", "app,lib", "--plan"})

			var plan protocolcli.MCPResult
			callMCPRunJobs(t, wsRoot, runJobsArguments(t, commands, []string{"app", "lib"}, true), &plan)
			if plan.Plan == nil {
				t.Fatalf("run_jobs dryRun answered no plan: %+v", plan)
			}
			mcpPlan := make([]string, 0, len(plan.Plan.Tasks))
			for _, task := range plan.Plan.Tasks {
				mcpPlan = append(mcpPlan, task.Identity.Key)
			}

			if len(terminalPlan) != 2*len(commands) {
				t.Fatalf("terminal planned %v; the fixture should give one job per project per command", terminalPlan)
			}
			if !equalStringSlices(terminalPlan, mcpPlan) {
				t.Fatalf("plans diverged.\n  terminal: %v\n  mcp:      %v", terminalPlan, mcpPlan)
			}
		})
	}
}

// TestMCPAndTerminalAgreeOnResults compares what the two adapters REPORT for
// runs that really execute: the six mutually-exclusive counts and the identity
// of every failure.
//
// The two cases are deliberately single-verdict. A run where one job fails while
// a sibling is still running has no deterministic outcome — the scheduler cancels
// pending work on the first failure, so the sibling lands on "succeeded" or
// "canceled" depending on timing. Comparing that would test the race, not the
// adapters.
func TestMCPAndTerminalAgreeOnResults(t *testing.T) {
	requireShell(t)
	wsRoot := writeEquivalenceFixture(t)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")

	t.Run("all jobs pass", func(t *testing.T) {
		var result protocolcli.MCPResult
		callMCPRunJobs(t, wsRoot, runJobsArguments(t, []string{"build"}, []string{"app", "lib"}, false), &result)
		terminal := terminalJSONSummary(t, wsRoot, []string{"build", "--projects", "app,lib", "--output=json"})

		if !sameRunVerdict(result.Run, terminal.Run) {
			t.Fatalf("verdicts diverged.\n  terminal: %s\n  mcp:      %s",
				runSummaryDigest(terminal.Run), runSummaryDigest(result.Run))
		}
		if result.Run.Counts.Succeeded != 2 {
			t.Fatalf("succeeded = %d, want 2 — the fixture stopped building", result.Run.Counts.Succeeded)
		}
		if result.Run.Outcome != protocolcli.RunOutcomeSuccess {
			t.Errorf("mcp reported %q for an all-passing run: %s",
				result.Run.Outcome, runSummaryDigest(result.Run))
		}
	})

	t.Run("a job fails", func(t *testing.T) {
		var result protocolcli.MCPResult
		callMCPRunJobs(t, wsRoot, runJobsArguments(t, []string{"lint"}, []string{"app"}, false), &result)
		terminal := terminalJSONSummary(t, wsRoot, []string{"lint", "--projects", "app", "--output=json"})

		if !sameRunVerdict(result.Run, terminal.Run) {
			t.Fatalf("verdicts diverged.\n  terminal: %s\n  mcp:      %s",
				runSummaryDigest(terminal.Run), runSummaryDigest(result.Run))
		}
		if result.Run.Counts.Failed != 1 {
			t.Fatalf("failed = %d, want 1 — the fixture stopped failing", result.Run.Counts.Failed)
		}
		if result.Run.Outcome != protocolcli.RunOutcomeFailure {
			t.Errorf("mcp reported %q for a run with a failing task", result.Run.Outcome)
		}
		// The envelope's status IS the run's outcome in v2, so a surface that
		// reported one and not the other would be caught here rather than by a
		// consumer reading whichever member it happened to trust.
		if terminal.Status != terminal.Run.Outcome {
			t.Errorf("terminal envelope status %q disagrees with run.outcome %q",
				terminal.Status, terminal.Run.Outcome)
		}

		mcpFailures := failureKeys(result.Run)
		terminalFailures := failureKeys(terminal.Run)
		if len(mcpFailures) != 1 {
			t.Fatalf("mcp listed %v failures, want exactly one", mcpFailures)
		}
		if !equalStringSlices(mcpFailures, terminalFailures) {
			t.Fatalf("failures diverged.\n  terminal: %v\n  mcp:      %v", terminalFailures, mcpFailures)
		}
	})
}

// terminalPlanJobs runs the REAL terminal mapping — ParseArgs then
// terminalRequest — through the engine's --plan stage and returns the planned
// job keys in plan order. The human notice stream is captured rather than
// printed so the test asserts on structure instead of on a rendered table.
func terminalPlanJobs(t *testing.T, wsRoot string, argv []string) []string {
	t.Helper()
	parsed := ParseArgs(argv, nil, nil)
	req := terminalRequest(parsed, wsproto.Load(wsRoot), wsRoot, nil, false)

	var notices strings.Builder
	req.Stdout = &notices

	var result engine.SessionResult
	out := captureStdoutStderr(t, func() {
		var err error
		result, err = engine.New().Run(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("terminal --plan: %v", err)
		}
	})
	if result.ExitCode != ExitSuccess {
		t.Fatalf("terminal --plan exit = %d\n%s%s", result.ExitCode, notices.String(), out)
	}
	keys := make([]string, 0, len(result.Plan))
	for _, job := range result.Plan {
		keys = append(keys, job.Key())
	}
	return keys
}

func runJobsArguments(t *testing.T, commands, projects []string, dryRun bool) string {
	t.Helper()
	arguments, err := json.Marshal(struct {
		Commands []string `json:"commands"`
		Projects []string `json:"projects"`
		DryRun   bool     `json:"dryRun"`
	}{commands, projects, dryRun})
	if err != nil {
		t.Fatalf("marshal run_jobs arguments: %v", err)
	}
	return string(arguments)
}

// callMCPRunJobs drives one full `putnami mcp` session: JSON-RPC frames in,
// JSON-RPC frames out, and the tool result decoded into out. It also asserts the
// transport invariant on the way past — nothing may reach the process stdout,
// because in production that IS the frame stream.
func callMCPRunJobs(t *testing.T, wsRoot, arguments string, out any) {
	t.Helper()
	srv := mcp.NewServer(mcp.Options{
		WorkspaceRoot: wsRoot,
		Config:        wsproto.Load(wsRoot),
		ServerVersion: "test",
	})
	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"run_jobs","arguments":` + arguments + "}}\n")

	var frames bytes.Buffer
	leaked := captureStdout(t, func() {
		if err := srv.Serve(context.Background(), bytes.NewReader(request), &frames); err != nil {
			t.Fatalf("mcp Serve: %v", err)
		}
	})
	if leaked != "" {
		t.Fatalf("the MCP session wrote %q to os.Stdout, which is the JSON-RPC frame stream", leaked)
	}

	scanner := bufio.NewScanner(bytes.NewReader(frames.Bytes()))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !scanner.Scan() {
		t.Fatal("mcp session produced no response frame")
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		t.Fatalf("decode response frame %q: %v", scanner.Bytes(), err)
	}
	if len(response.Error) > 0 {
		t.Fatalf("run_jobs JSON-RPC error: %s", response.Error)
	}
	if len(response.Result.Content) == 0 {
		t.Fatalf("run_jobs returned no content: %s", scanner.Bytes())
	}
	if response.Result.IsError {
		t.Fatalf("run_jobs failed: %s", response.Result.Content[0].Text)
	}
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), out); err != nil {
		t.Fatalf("decode run_jobs result: %v\n%s", err, response.Result.Content[0].Text)
	}
}

// terminalJSONSummary runs the real `putnami … --output=json` command and
// returns the envelope an agent or CI would read.
func terminalJSONSummary(t *testing.T, wsRoot string, argv []string) protocolcli.ResultV2 {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	func() {
		defer func() {
			os.Stdout = orig
			_ = w.Close()
		}()
		app, appErr := NewApp()
		if appErr != nil {
			t.Fatalf("NewApp: %v", appErr)
		}
		t.Chdir(wsRoot)
		// A non-zero code is expected for the failing case; the envelope carries
		// the verdict this test compares.
		app.Run(context.Background(), argv)
	}()
	stdout := <-done
	_ = r.Close()

	var envelope protocolcli.ResultV2
	decodeLastJSONObject(t, stdout, &envelope)
	if envelope.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Fatalf("terminal envelope protocolVersion = %d, want %d — a document WITHOUT it is version 1:\n%s",
			envelope.ProtocolVersion, protocolcli.ResultProtocolVersion, stdout)
	}
	return envelope
}

// decodeLastJSONObject decodes the final top-level JSON object on stdout into
// out. The --output=json envelope is pretty-printed and is the last thing
// written, so the last line that opens an object at column zero begins it;
// anything a notice printed earlier is skipped rather than turning this into a
// strict-stdout assertion the terminal has never made.
func decodeLastJSONObject(t *testing.T, out string, into any) {
	t.Helper()
	start := strings.LastIndex(out, "\n{")
	switch {
	case start >= 0:
		start++
	case strings.HasPrefix(out, "{"):
		start = 0
	default:
		t.Fatalf("no JSON object on stdout:\n%s", out)
	}
	if err := json.NewDecoder(strings.NewReader(out[start:])).Decode(into); err != nil {
		t.Fatalf("decode --output=json envelope: %v\n%s", err, out[start:])
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeEquivalenceFixture builds a two-project workspace whose lint fails in
// "app" and passes in "lib", through a local extension. Both tasks are
// cache:false so repeated runs execute rather than replay.
func writeEquivalenceFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}

	write("putnami.workspace.json",
		`{"name":"equivalence-ws","includes":["packages/app","packages/lib"],"extensions":["/tools/demo"]}`, 0o644)
	write("packages/app/putnami.json",
		`{"name":"app","type":"application","dependencies":["lib"],"extensions":["@putnami/demo"]}`, 0o644)
	write("packages/lib/putnami.json",
		`{"name":"lib","type":"library","extensions":["@putnami/demo"]}`, 0o644)
	write("packages/app/demo.activate", "", 0o644)
	write("packages/lib/demo.activate", "", 0o644)
	write("tools/demo/putnami.json", `{"name":"@putnami/demo"}`, 0o644)
	write("tools/demo/ok.sh", "#!/bin/sh\nexit 0\n", 0o755)
	write("tools/demo/lint.sh", `#!/bin/sh
if [ "$(basename "$PWD")" = "app" ]; then
  printf '%s\n' '{"v":2,"type":"diagnostic","severity":"error","code":"DEMO001","message":"demo lint failure","location":{"file":"src/main.ts","line":7,"column":2}}'
  exit 1
fi
exit 0
`, 0o755)
	write("tools/demo/putnami.extension.json", `{
  "name": "@putnami/demo",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "build": {
      "description": "Demo build fixture.",
      "activationFiles": ["demo.activate"],
      "run": [{ "id": "build", "task": "build-task" }]
    },
    "lint": {
      "description": "Demo lint fixture.",
      "activationFiles": ["demo.activate"],
      "run": [{ "id": "lint", "task": "lint-task" }]
    }
  },
  "tasks": {
    "build-task": {
      "kind": "command",
      "command": "{extensionRoot}/ok.sh",
      "cache": false,
      "timeoutMs": 30000
    },
    "lint-task": {
      "kind": "command",
      "command": "{extensionRoot}/lint.sh",
      "cache": false,
      "timeoutMs": 30000
    }
  }
}`, 0o644)

	workspace.InvalidateLoadCache(dir)
	return dir
}
