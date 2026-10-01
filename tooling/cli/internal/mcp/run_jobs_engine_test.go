package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// run_jobs is an ADAPTER over engine.Run rather than a second copy of the job
// pipeline. These tests pin what that has to mean, and — more importantly —
// what it must never mean:
//
//  1. Not one byte reaches stdout. Stdout is the JSON-RPC frame stream; the
//     EventSink discards and the engine's own notices are captured
//     (engine.Request.Stdout).
//  2. No successful-run marker is written, locally or remotely. Markers are
//     keyed by (branch, commands, params) with no provenance, so an agent's run
//     would otherwise silently narrow what a later human or CI `--impacted` run
//     considers changed.
//  3. Cache keys are unchanged: CommandParams stays the empty, non-nil map the
//     code before this adapter passed, because a param value's Go type is part
//     of every key.
//  4. A run that never executed is an explicit error, not a green summary.
//
// The telemetry observer is absent by construction: this package never names it,
// and internal/engine/seam_test.go enforces module-wide that only the terminal
// adapter injects one (ADR 0001 §4).

// jobFixture writes a workspace whose two projects both run a real `build` and
// `lint` through a local extension, so run_jobs has something to plan, schedule
// and fail on. lint fails in "app" only, with one diagnostic, which is what
// makes the failure and diagnostic projections observable end to end.
func jobFixture(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
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
		`{"name":"fixture-ws","includes":["packages/app","packages/lib"],"extensions":["/tools/demo"]}`, 0o644)
	write("packages/app/putnami.json",
		`{"name":"app","type":"application","dependencies":["lib"],"extensions":["@putnami/demo"]}`, 0o644)
	write("packages/lib/putnami.json",
		`{"name":"lib","type":"library","extensions":["@putnami/demo"]}`, 0o644)
	write("packages/app/demo.activate", "", 0o644)
	write("packages/lib/demo.activate", "", 0o644)
	write("tools/demo/putnami.json", `{"name":"@putnami/demo"}`, 0o644)

	// The job runs with cwd = the project directory, which is the only signal
	// the script needs to fail in exactly one project.
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
	// The store is isolated once for the whole binary by TestMain
	// (PUTNAMI_STORE_DIR), so the fixture needs no t.Setenv and stays
	// compatible with t.Parallel.
	return dir
}

func jobFixtureServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := jobFixture(t)
	return NewServer(Options{WorkspaceRoot: dir, Config: wsproto.Load(dir), ServerVersion: "test"}), dir
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	func() {
		defer func() {
			os.Stdout = orig
			_ = w.Close()
		}()
		fn()
	}()
	out := <-done
	_ = r.Close()
	return out
}

// toolCall is the tools/call params object, typed so the run_jobs helpers below
// decode both directions of the wire into shapes instead of untyped maps.
type toolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolDiagnostics is the get_diagnostics answer, as an agent receives it.
//
// There is no local mirror of the run_jobs answer beside it any more: since v1
// was removed, run_jobs answers with the contract's own v2 document, so the
// helpers below decode straight into protocolcli.MCPResult. A second
// hand-written shape would be exactly the drift the versioned contract exists
// to remove.
type toolDiagnostics struct {
	Count int            `json:"count"`
	Diags []toolDiagnost `json:"diagnostics"`
}

type toolDiagnost struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Project  string `json:"project"`
	Job      string `json:"job"`
}

// callTool runs one tools/call over a real session and decodes the text block
// into out. It returns the raw text and whether the tool reported isError, so a
// caller can assert on either the value or the refusal.
func callTool(t *testing.T, srv *Server, name, args string, out any) (string, bool) {
	t.Helper()
	resps := runSession(t, srv, req(1, "tools/call", toolCall{Name: name, Arguments: json.RawMessage(args)}))
	if len(resps) != 1 {
		t.Fatalf("%s responses = %d, want 1", name, len(resps))
	}
	res := resultOf(t, resps[0])
	isErr, _ := res["isError"].(bool)
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("%s result has no content: %v", name, res)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !isErr && out != nil {
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("decode %s result: %v\n%s", name, err, text)
		}
	}
	return text, isErr
}

// callRunJobs runs run_jobs and fails the test if the tool refused. It decodes
// the answer as the v2 MCP document, and fails when the document does not say
// so: a run_jobs answer without protocolVersion would be a v1 document, and
// there is no emitter left that can produce one.
func callRunJobs(t *testing.T, srv *Server, args string) protocolcli.MCPResult {
	t.Helper()
	var result protocolcli.MCPResult
	text, isErr := callTool(t, srv, "run_jobs", args, &result)
	if isErr {
		t.Fatalf("run_jobs %s reported an error: %s", args, text)
	}
	if result.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Fatalf("run_jobs %s answered protocolVersion %d, want %d:\n%s",
			args, result.ProtocolVersion, protocolcli.ResultProtocolVersion, text)
	}
	return result
}

// refuseRunJobs runs run_jobs expecting a refusal, and returns its message.
func refuseRunJobs(t *testing.T, srv *Server, args string) string {
	t.Helper()
	text, isErr := callTool(t, srv, "run_jobs", args, nil)
	if !isErr {
		t.Fatalf("run_jobs %s was accepted; want a refusal.\n%s", args, text)
	}
	return text
}

// TestRunJobs_WritesNothingToStdout is acceptance criterion R2. The engine
// prints on the human stdout stream at several stages ("No jobs matched", the
// --plan table, the auto-selection line); every one of them would desynchronize
// the JSON-RPC framing. Three shapes are exercised: a real execution, a plan,
// and the notice-producing empty case.
func TestRunJobs_WritesNothingToStdout(t *testing.T) {
	srv, _ := jobFixtureServer(t)
	empty := NewServer(Options{
		WorkspaceRoot: t.TempDir(),
		Config:        wsproto.Load(t.TempDir()),
		ServerVersion: "test",
	})

	var stderr string
	got := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			callRunJobs(t, srv, `{"commands":["build"]}`)
			callTool(t, srv, "run_jobs", `{"commands":["build"],"dryRun":true}`, nil)
			// The workspace-less server makes the engine emit its notices.
			callTool(t, empty, "run_jobs", `{"commands":["build"]}`, nil)
		})
	})
	if got != "" {
		t.Fatalf("run_jobs wrote %d bytes to stdout, which is the JSON-RPC frame stream:\n%q", len(got), got)
	}
	// MCP stays local-only, silently. Leaving CacheTrust unset would make the
	// engine fail closed to local-only AND warn about it once per tool call.
	if strings.Contains(stderr, "remote cache") {
		t.Errorf("run_jobs announced a degraded remote cache; MCP is deliberately local-only:\n%s", stderr)
	}
}

// MCP run_jobs is an explicitly local adapter. A caller's configured remote
// cache must neither start a provider nor contact its endpoint; unlike the
// terminal, MCP also emits no degradation notice because it never requested
// remote authority in the first place.
func TestRunJobs_ConfiguredRemoteCacheDoesNotContactAProvider(t *testing.T) {
	srv, _ := jobFixtureServer(t)
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(endpoint.Close)
	t.Setenv("PUTNAMI_CACHE_URL", endpoint.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", "")

	stderr := captureStderr(t, func() {
		result := callRunJobs(t, srv, `{"commands":["build"]}`)
		if result.Run == nil || result.Run.Counts.Failed != 0 {
			t.Fatalf("local run_jobs failed: %+v", result.Run)
		}
	})
	if got := requests.Load(); got != 0 {
		t.Fatalf("run_jobs contacted the configured remote endpoint %d times", got)
	}
	if strings.Contains(stderr, "remote cache") {
		t.Fatalf("local-only MCP announced or initialized remote cache:\n%s", stderr)
	}
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns
// everything written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	func() {
		defer func() {
			os.Stderr = orig
			_ = w.Close()
		}()
		fn()
	}()
	out := <-done
	_ = r.Close()
	return out
}

// TestRunJobs_RecordsNoSuccessfulRunMarker is the run-marker decision of this
// adapter, with its own positive control so it cannot pass vacuously. An
// MCP-shaped selection never satisfies shouldRecordSuccessfulBuild; the
// identical run with --all does. Markers feed `--impacted` and remote skip-hit
// decisions and carry no provenance, so an agent's tool call must not write
// one.
func TestRunJobs_RecordsNoSuccessfulRunMarker(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "preview-no-effects", "plan-only-and-dry-runs-execute-nothing")
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-side-effect-boundary", "dry-run-executes-nothing")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable on this platform")
	}
	srv, dir := jobFixtureServer(t)
	initFixtureGitRepo(t, dir)

	sessStore := workspace_state.NewSessionStore(dir)
	// The key an MCP run would write under: the same commands, and the same
	// empty non-nil params map the adapter passes as CommandParams.
	markerParams := map[string]any{}

	for _, args := range []string{
		`{"commands":["build"]}`,
		`{"commands":["build"],"projects":["app","lib"]}`,
	} {
		callRunJobs(t, srv, args)
		sha, err := sessStore.LastBuildSHA("main", []string{"build"}, markerParams)
		if err != nil {
			t.Fatalf("last build sha: %v", err)
		}
		if sha != "" {
			t.Fatalf("run_jobs %s recorded a successful-run marker (%q); an MCP tool call must not "+
				"move what a later --impacted run considers changed", args, sha)
		}
	}

	// Positive control: the same workspace, the same commands, run through the
	// engine the way a terminal `--all` invocation does, DOES record. Without
	// this the assertions above would also pass if markers were simply broken.
	var global engine.GlobalFlags
	global.All = true
	global.Projects = "*"
	discardEngineNotices(t, func(stdout *strings.Builder) {
		result, err := engine.New().Run(context.Background(), engine.Request{
			WorkspaceRoot: dir,
			Config:        wsproto.Load(dir),
			Commands:      []string{"build"},
			Global:        global,
			CommandParams: markerParams,
			Stdout:        stdout,
		}, discardSink{})
		if err != nil || result.ExitCode != engine.ExitSuccess {
			t.Fatalf("control run exit = %d, err = %v", result.ExitCode, err)
		}
	})
	sha, err := sessStore.LastBuildSHA("main", []string{"build"}, markerParams)
	if err != nil {
		t.Fatalf("last build sha (control): %v", err)
	}
	if sha == "" {
		t.Fatal("the --all control run recorded no marker either; the assertions above prove nothing")
	}
}

// TestRunJobs_FailureCarriesCanonicalDiagnostics proves the reduction reaches
// the wire: counts, the failure identity, and the diagnostic's file:line:col —
// all now derived by jobs.SessionReducer instead of MCP's deleted tally loop —
// plus the get_diagnostics readback that depends on them.
func TestRunJobs_FailureCarriesCanonicalDiagnostics(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "diagnostic-recall", "the-latest-executed-run-is-recalled-without-rerunning")
	srv, _ := jobFixtureServer(t)

	result := callRunJobs(t, srv, `{"commands":["lint"]}`)
	run := result.Run
	if run == nil {
		t.Fatalf("run_jobs answered no run summary: %+v", result)
	}
	if run.Outcome != protocolcli.RunOutcomeFailure {
		t.Fatalf("lint run reported %q even though app's lint failed: %+v", run.Outcome, run)
	}
	// Total and Failed are deterministic; the SIBLING project's verdict is not.
	// With fail-fast scheduling the second lint can be canceled before it
	// finishes, so it lands in either counts.succeeded or counts.canceled. v2
	// counts BOTH — unlike the v1 wire, which had no canceled bucket at all —
	// so the histogram still adds up to total either way and the sibling's
	// timing cannot be asserted. It is the same race this adapter avoided in the
	// equivalence test by keeping its cases single-verdict. The substance below
	// — failure identity and the diagnostic's file:line:col — is unaffected.
	if run.Counts.Total != 2 || run.Counts.Failed != 1 {
		t.Errorf("counts = %+v; want total=2 failed=1", run.Counts)
	}
	if run.Counts.Succeeded+run.Counts.Canceled != 1 {
		t.Errorf("counts = %+v; the one non-failing sibling must be counted as succeeded or canceled", run.Counts)
	}
	if len(run.Failures) != 1 {
		t.Fatalf("failures = %+v, want exactly one", run.Failures)
	}
	failure := run.Failures[0]
	// "lint~lint" is <command>~<step id>, which is the canonical plan name the
	// v2 identity carries in task.name — the same value the v1 wire reported as
	// "job", now beside the project it belongs to instead of duplicated into it.
	if failure.Identity.Project.Name != "app" || failure.Identity.Task.Name != "lint~lint" {
		t.Errorf("failure identity = %+v, want app/lint~lint", failure.Identity)
	}
	if len(failure.Diagnostics) != 1 {
		t.Fatalf("failure diagnostics = %+v, want exactly one", failure.Diagnostics)
	}
	assertDemoV2Diagnostic(t, failure.Diagnostics[0])

	// get_diagnostics reads the same run back without re-running anything. Its
	// answer is NOT the v2 document: it is the off-wire diagnostic list the run
	// record kept, which is what carries the project/job provenance.
	var stored toolDiagnostics
	callTool(t, srv, "get_diagnostics", `{"severity":"error"}`, &stored)
	if len(stored.Diags) != 1 {
		t.Fatalf("get_diagnostics = %+v, want the single error diagnostic", stored.Diags)
	}
	assertDemoDiagnostic(t, stored.Diags[0])
}

// assertDemoV2Diagnostic is assertDemoDiagnostic on the contract's diagnostic
// shape. The v2 diagnostic carries no project/job members — the enclosing
// failure's typed identity names the task — so the provenance is asserted on
// the get_diagnostics readback instead.
func assertDemoV2Diagnostic(t *testing.T, d protocolcli.Diagnostic) {
	t.Helper()
	if d.File != "packages/app/src/main.ts" || d.Line != 7 || d.Column != 2 {
		t.Errorf("diagnostic location = %+v, want packages/app/src/main.ts:7:2", d)
	}
	if d.Severity != "error" || d.Code != "DEMO001" || d.Message != "demo lint failure" {
		t.Errorf("diagnostic payload = %+v", d)
	}
}

func assertDemoDiagnostic(t *testing.T, d toolDiagnost) {
	t.Helper()
	// The path is workspace-relative: the shared event pipeline rewrites the
	// project-relative one the job emitted, exactly as it does for every surface.
	if d.File != "packages/app/src/main.ts" || d.Line != 7 || d.Column != 2 {
		t.Errorf("diagnostic location = %+v, want packages/app/src/main.ts:7:2", d)
	}
	if d.Severity != "error" || d.Code != "DEMO001" || d.Message != "demo lint failure" {
		t.Errorf("diagnostic payload = %+v", d)
	}
	if d.Project != "app" || d.Job != "lint~lint" {
		t.Errorf("diagnostic provenance = %+v, want app/lint~lint", d)
	}
}

// TestRunJobs_NoJobsMatchedIsAnExplicitError is a DIVERGENCE CLOSED by this
// adapter (R3). A workspace where nothing serves the requested command used to
// answer {"success":true,"total":0} — a green summary for a build that never
// ran, which is the same green-on-broken failure class the engine's guards
// exist to stop. It is now the engine's exit-2 refusal, surfaced with the
// reason the engine printed on the stream MCP captures.
func TestRunJobs_NoJobsMatchedIsAnExplicitError(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan", "a-missing-selected-command-is-rejected")
	srv := newFixtureServer(t) // no extensions: nothing can serve "build"

	text := refuseRunJobs(t, srv, `{"commands":["build"]}`)
	for _, want := range []string{"no jobs ran", "exit 2", "No jobs matched"} {
		if !strings.Contains(text, want) {
			t.Errorf("error %q does not mention %q", text, want)
		}
	}

	// The dry-run shape refuses identically: a plan of nothing is not a plan.
	refuseRunJobs(t, srv, `{"commands":["build"],"dryRun":true}`)
}

// TestRunJobs_UnknownProjectIsRejectedBeforeAnythingRuns keeps the earlier
// error for a typo'd selector. Handing unknown names to the engine's filter
// instead would run the SUBSET that did resolve — a silent partial run of
// something nobody asked for.
func TestRunJobs_UnknownProjectIsRejectedBeforeAnythingRuns(t *testing.T) {
	t.Parallel()
	srv, _ := jobFixtureServer(t)
	text := refuseRunJobs(t, srv, `{"commands":["build"],"projects":["app","nope"]}`)
	if !strings.Contains(text, "projects not found: nope") {
		t.Errorf("error = %q, want it to name the missing project", text)
	}
}

// TestRunJobs_ImpactedFailsClosed pins ImpactedStrict. The engine's lenient
// default falls back to running EVERY project when the baseline cannot be
// resolved; for a tool call that is an unbounded run nobody requested, and the
// earlier code returned an error instead.
func TestRunJobs_ImpactedFailsClosed(t *testing.T) {
	t.Parallel()
	srv, _ := jobFixtureServer(t) // not a git repo: no baseline resolves
	refuseRunJobs(t, srv, `{"commands":["build"],"impacted":true,"baseline":"origin/nope"}`)
}

// TestRunJobs_RejectsLongLivedCommands guards the one hazard routing through the
// engine introduces: `serve` makes the engine enter WATCH mode, which never
// returns even when its child exits. The MCP transport is a single synchronous
// stdio loop, so such a call wedges the entire session rather than one tool.
// Only `serve` is refused — it is the one command the engine promotes to watch
// mode, so it never returns. `run` stays accepted; see
// TestRejectLongLivedCommands_ScopedToTheWatchPromotion for that half.
func TestRunJobs_RejectsLongLivedCommands(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-side-effect-boundary", "long-lived-and-externally-mutating-jobs-are-refused")
	srv, _ := jobFixtureServer(t)
	text := refuseRunJobs(t, srv, `{"commands":["serve"]}`)
	if !strings.Contains(text, "blocks the whole session") {
		t.Errorf("serve refusal = %q, want it to explain the transport constraint", text)
	}
	if !strings.Contains(text, "watch mode") {
		t.Errorf("serve refusal = %q, want it to name the watch promotion that causes it", text)
	}
}

// TestRunJobs_EmptySelectionStaysASuccessfulNoOp keeps the one earlier shape
// that legitimately answered success with zero counts: nothing impacted is
// nothing to do, not a failure.
func TestRunJobs_EmptySelectionStaysASuccessfulNoOp(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable on this platform")
	}
	srv, dir := jobFixtureServer(t)
	initFixtureGitRepo(t, dir)

	result := callRunJobs(t, srv, `{"commands":["build"],"impacted":true,"baseline":"HEAD"}`)
	if result.Run == nil || result.Run.Outcome != protocolcli.RunOutcomeSuccess || result.Run.Counts.Total != 0 {
		t.Errorf("result = %+v, want a zero-count success", result.Run)
	}

	// The dry-run shape answers an empty plan rather than refusing: the engine
	// returns success with a nil plan when nothing was selected.
	var plan protocolcli.MCPResult
	callTool(t, srv, "run_jobs", `{"commands":["build"],"impacted":true,"baseline":"HEAD","dryRun":true}`, &plan)
	if plan.Plan == nil || !plan.Plan.DryRun || len(plan.Plan.Tasks) != 0 {
		t.Errorf("dry-run plan = %+v, want an empty plan", plan.Plan)
	}
}

// TestRunJobs_EmptySelectionV2Document is the v2 half of the no-op shape: the
// default answer for an empty selection is a v2 document with a zero-count
// success, not an error and not a v1 summary.
func TestRunJobs_EmptySelectionV2Document(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable on this platform")
	}
	srv, dir := jobFixtureServer(t)
	initFixtureGitRepo(t, dir)

	out, err := srv.toolRunJobs(context.Background(), json.RawMessage(`{"commands":["build"],"impacted":true,"baseline":"HEAD"}`))
	if err != nil {
		t.Fatalf("toolRunJobs: %v", err)
	}
	doc, ok := out.(*protocolcli.MCPResult)
	if !ok {
		t.Fatalf("empty selection answered %T, want the v2 MCP document by default", out)
	}
	if doc.Run == nil || doc.Run.Outcome != protocolcli.RunOutcomeSuccess || doc.Run.Counts.Total != 0 {
		t.Errorf("v2 empty-selection run = %+v, want a zero-count success", doc.Run)
	}
}

// discardEngineNotices runs fn with a human-notice sink, and fails the test if
// anything reached the process stdout while it ran.
func discardEngineNotices(t *testing.T, fn func(*strings.Builder)) {
	t.Helper()
	var notices strings.Builder
	if leaked := captureStdout(t, func() { fn(&notices) }); leaked != "" {
		t.Fatalf("engine wrote %q to os.Stdout despite Request.Stdout", leaked)
	}
}

func initFixtureGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-m", "initial"},
		{"branch", "-M", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestRejectLongLivedCommands_ScopedToTheWatchPromotion pins BOTH halves of the
// refusal this adapter adds. The policy exists for one reason — the engine
// promotes `serve` to watch mode (internal/engine/execute.go:110), so it never
// returns even when its child exits, and the synchronous MCP stdio loop then
// wedges for the whole session.
//
// `run` must stay accepted: it is not promoted to watch, so it terminates
// exactly as it did on MCP's earlier fork. Refusing it would narrow a
// published tool's accepted input for a hazard this change does not create —
// and forwarding a workload's exit code is a legitimate agent use.
func TestRejectLongLivedCommands_ScopedToTheWatchPromotion(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/context-mcp-discovery", "mcp-side-effect-boundary", "long-lived-and-externally-mutating-jobs-are-refused")
	for _, tc := range []struct {
		name     string
		commands []string
		refused  bool
	}{
		{name: "serve is refused", commands: []string{"serve"}, refused: true},
		{name: "serve refused among others", commands: []string{"build", "serve"}, refused: true},
		{name: "qualify is refused", commands: []string{"build", "qualify"}, refused: true},
		{name: "run is accepted", commands: []string{"run"}, refused: false},
		{name: "ordinary commands accepted", commands: []string{"lint", "test", "build"}, refused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectLongLivedCommands(tc.commands)
			if tc.refused && err == nil {
				t.Fatalf("rejectLongLivedCommands(%v) = nil, want a refusal", tc.commands)
			}
			if !tc.refused && err != nil {
				t.Fatalf("rejectLongLivedCommands(%v) = %v, want it accepted", tc.commands, err)
			}
		})
	}
}

// TestRunJobs_PlanningALongLivedCommandIsAllowed pins that the refusal guards
// EXECUTION only. Planning schedules nothing and cannot wedge the session, so an
// agent can still ask what `serve` would run; refusing the plan too would hide
// the workspace's shape for no safety gain.
func TestRunJobs_PlanningALongLivedCommandIsAllowed(t *testing.T) {
	t.Parallel()
	srv := newFixtureServer(t)

	text, _ := callTool(t, srv, "run_jobs", `{"commands":["serve"],"dryRun":true}`, nil)
	if strings.Contains(text, "cannot run") {
		t.Fatalf("planning `serve` hit the long-lived refusal, which guards execution only:\n%s", text)
	}
}

// TestRunJobs_DoesNotRotateTheLatestSessionPointer pins a cross-slice defect a
// whole-epic integration review found. Routing MCP through the engine
// inherited session recording wholesale, so an agent's run_jobs landing after
// a human's build silently became `latest`, the session every
// `--session latest` reader opens — and session metadata carries no origin
// field, so nobody could tell afterwards.
//
// The session is still written; only the pointer is withheld. The positive
// control asserts a session WAS created, so this cannot pass by recording
// nothing at all.
func TestRunJobs_DoesNotRotateTheLatestSessionPointer(t *testing.T) {
	t.Parallel()
	srv, dir := jobFixtureServer(t)
	sessStore := workspace_state.NewSessionStore(dir)

	// A human's build owns `latest`. Seed it, then confirm it is real.
	seeded, err := sessStore.Create()
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := sessStore.UpdateLatest(seeded); err != nil {
		t.Fatalf("seed latest: %v", err)
	}
	if got := sessStore.LatestID(); got != seeded.ID {
		t.Fatalf("control: seeded latest = %q, want %q", got, seeded.ID)
	}

	before, err := os.ReadDir(filepath.Join(dir, ".putnami", "sessions"))
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	callRunJobs(t, srv, `{"commands":["build"]}`)

	// Positive control: the agent's run really did record a session, so the
	// assertion below is about the POINTER, not about nothing having happened.
	after, err := os.ReadDir(filepath.Join(dir, ".putnami", "sessions"))
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(after) <= len(before) {
		t.Fatalf("run_jobs recorded no session (%d -> %d entries); this test then proves nothing "+
			"— it must assert the pointer is withheld, not that the run was silent", len(before), len(after))
	}

	if got := sessStore.LatestID(); got != seeded.ID {
		t.Errorf("run_jobs moved `latest` to %q (was %q), so an agent tool call would become "+
			"the session every --session latest reader opens.", got, seeded.ID)
	}
}
