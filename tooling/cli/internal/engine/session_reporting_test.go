package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runnerproto "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/sessionreporter"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// TestNativeSessionReporterProvider is a real provider subprocess using the
// published parser and acknowledgement contract. Its only external boundary is
// a local directory standing in for the remote receiver's durable storage.
func TestNativeSessionReporterProvider(t *testing.T) {
	if os.Getenv("REPORTER_TEST_HELPER") != "1" {
		return
	}
	root := os.Getenv("REPORTER_TEST_ROOT")
	// The log reporter's provider keeps its files beside the session reporter's
	// under a "log-" prefix, and holds only its own token.
	prefix, token, own, foreign := "", "reporter-test-secret", protocolcli.SessionReporterTokenEnv, protocolcli.LogReporterTokenEnv
	if os.Getenv("REPORTER_TEST_NAME") == "log" {
		prefix, token, own, foreign = "log-", "log-test-secret", protocolcli.LogReporterTokenEnv, protocolcli.SessionReporterTokenEnv
	}
	file := func(name string) string { return filepath.Join(root, prefix+name) }
	exists := func(name string) bool {
		_, err := os.Stat(file(name))
		return err == nil
	}
	// A killed subscriber stays dead: every respawn after the kill exits before
	// acknowledging anything.
	if exists("dead") {
		os.Exit(28)
	}
	_ = os.WriteFile(file("provider.pid"), []byte(fmt.Sprint(os.Getpid())), 0o600)
	if os.Getenv(own) != token {
		os.Exit(21)
	}
	if os.Getenv(protocolcli.SessionReporterEnv)+os.Getenv(protocolcli.LogReporterEnv)+os.Getenv(foreign) != "" {
		os.Exit(22)
	}
	if os.Getenv("REPORTER_TEST_MANAGED") == "1" {
		out, err := exec.Command("reporter-compiler", "--version").Output()
		if err != nil || strings.TrimSpace(string(out)) != "1.2.3" || os.Getenv("REPORTER_COMPILER_ROOT") == "" {
			os.Exit(27)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), protocolcli.SessionReportingLineBytes)
	for scanner.Scan() {
		chunk, err := protocolcli.ParseSessionReportingChunk(scanner.Bytes())
		if err != nil || prefix != "" && chunk.Artifact != "events.jsonl" {
			os.Exit(23)
		}
		trace, err := os.OpenFile(file("trace.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(24)
		}
		_, _ = trace.Write(append(append([]byte{}, scanner.Bytes()...), '\n'))
		_ = trace.Close()
		if exists("crash") {
			os.Exit(29)
		}
		// exit-on-plan exits at every plan.json frame, as a receiver whose
		// parser does not know the artifact does.
		if chunk.Artifact == "plan.json" && exists("exit-on-plan") {
			os.Exit(30)
		}
		ack := chunk.Ack()
		// offline-after-live acknowledges the chunk that carries task:start,
		// then refuses every later chunk. refuse-plan refuses every plan.json
		// chunk without retry and acknowledges the other artifacts.
		goOffline := false
		if exists("offline") {
			ack.OK, ack.Code = false, "unavailable"
			fmt.Fprintln(os.Stderr, "provider diagnostic "+token)
		} else if chunk.Artifact == "plan.json" && exists("refuse-plan") {
			ack.OK, ack.Code = false, "unsupported_artifact"
		} else {
			path := file("received-" + chunk.Artifact)
			f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				os.Exit(25)
			}
			if _, err := f.WriteAt(chunk.Data, chunk.Offset); err != nil {
				os.Exit(26)
			}
			_ = f.Sync()
			_ = f.Close()
			if bytes.Contains(chunk.Data, []byte(`"record":"task:start"`)) {
				_ = os.WriteFile(file("live"), []byte("received"), 0o600)
				goOffline = exists("offline-after-live")
			}
			if chunk.Final {
				_ = os.WriteFile(path+".final", []byte("closed"), 0o600)
			}
		}
		_ = json.NewEncoder(os.Stdout).Encode(ack)
		if goOffline {
			_ = os.WriteFile(file("offline"), nil, 0o600)
		}
		if ack.OK && chunk.Artifact == "events.jsonl" && chunk.Final {
			os.Exit(0)
		}
	}
	os.Exit(0)
}

// reporterCompiler names the managed toolchain TestNativeSessionReportingManagedToolchain
// places: this test binary, which answers its version probe in TestMain.
const reporterCompiler = "reporter-compiler"

// reporterFixture builds a workspace whose test task runs task, a program
// whose relative paths are relative to the workspace root.
func reporterFixture(t *testing.T, task fixtureproc.Program) (string, Request) {
	t.Helper()
	root := t.TempDir()
	taskPath := fixtureproc.Write(t, filepath.Join(t.TempDir(), "test-exec"), task)
	write := func(path string, data []byte) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", []byte(`{"name":"reporter-proof","includes":["app","extension"]}`))
	write("app/putnami.json", []byte(`{"name":"app","extensions":["/extension"]}`))
	write("extension/putnami.json", []byte(`{"name":"@test/reporter"}`))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cacheEnabled := false
	manifest := extensionproto.Manifest{
		Name: "@test/reporter", Version: "1.0.0", CLIContract: 4,
		Commands: map[string]extensionproto.CommandDefinition{
			"test":             {Run: []extensionproto.PipelineStep{{ID: "test", Task: "test-exec"}}},
			"session-reporter": {Visibility: "internal", Run: []extensionproto.PipelineStep{{ID: "reporter", Task: "reporter-exec"}}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"test-exec":     {Kind: "command", Command: taskPath, Cwd: "{workspaceRoot}", Cache: &extensionproto.TaskCachePolicy{Enabled: &cacheEnabled}},
			"reporter-exec": {Kind: "command", Command: executable, Args: []string{"-test.run=^TestNativeSessionReporterProvider$"}, Env: map[string]string{"REPORTER_TEST_HELPER": "1", "REPORTER_TEST_ROOT": root}},
		},
	}
	data, _ := json.Marshal(manifest)
	write("extension/putnami.extension.json", data)
	workspace.InvalidateLoadCache(root)
	t.Setenv(protocolcli.SessionReporterEnv, "@test/reporter")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "reporter-test-secret")
	t.Setenv(protocolcli.LogReporterEnv, "")
	t.Setenv(protocolcli.LogReporterTokenEnv, "")
	return root, Request{WorkspaceRoot: root, Config: wsproto.Load(root), Commands: []string{"test"}, Global: GlobalFlags{Projects: "app", NoCache: true}, Stdout: io.Discard}
}

// taskRanWithoutReporter fails t unless the test task, recording its runs in
// root/executions, ran once and saw neither the reporter's token nor its
// selection: both are withheld from tasks.
func taskRanWithoutReporter(t *testing.T, root string) {
	t.Helper()
	runs := fixtureproc.Runs(t, filepath.Join(root, "executions"))
	if len(runs) != 1 {
		t.Fatalf("job executions = %d, want 1", len(runs))
	}
	for _, name := range []string{protocolcli.SessionReporterTokenEnv, protocolcli.SessionReporterEnv} {
		if value, ok := runs[0].LookupEnv(name); ok && value != "" {
			t.Fatalf("the task saw %s", name)
		}
	}
}

// liveReportingContext shortens the reporter's live batching interval for the
// tests that wait on live delivery. Production keeps
// sessionreporter.EventsBatchInterval; only tests may reach this seam.
func liveReportingContext() context.Context {
	return sessionreporter.WithEventsBatchInterval(context.Background(), 20*time.Millisecond)
}

func TestNativeSessionReportingWithholdsCredentialsFromHooks(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "credential-boundary", "explicit-secret-is-withheld-from-hooks-and-tasks")
	requireSh(t)
	root, req := reporterFixture(t, fixtureproc.Program{Record: "executions"})
	check := `test -z "$PUTNAMI_SESSION_REPORTER_TOKEN$PUTNAMI_SESSION_REPORTER" && echo denied >> hook-checks`
	req.Hooks = &wsproto.HooksConfig{CLI: &wsproto.HookPhaseConfig{Before: []string{check}, After: []string{check}}}
	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("credential withholding failed: %d %v", result.ExitCode, err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "hook-checks")); string(data) != "denied\ndenied\n" {
		t.Fatalf("hooks did not execute: %q", data)
	}
	taskRanWithoutReporter(t, root)
	verifyReporterArtifacts(t, root, workspace_state.NewSessionStore(root).LatestID())
}

func TestNativeSessionReportingPlacement(t *testing.T) {
	t.Run("absent runner reports one local fallback with placement provenance", func(t *testing.T) {
		root, req := reporterFixture(t, fixtureproc.Program{Record: "executions"})
		req.Global.Where = "remote"
		result, err := New().Run(context.Background(), req, discardEvents{})
		if err != nil || result.ExitCode != ExitSuccess {
			t.Fatalf("remote fallback: exit=%d err=%v", result.ExitCode, err)
		}
		verifyReporterArtifacts(t, root, workspace_state.NewSessionStore(root).LatestID())
		data, err := os.ReadFile(filepath.Join(root, "received-session.json"))
		if err != nil {
			t.Fatal(err)
		}
		var session protocolcli.SessionFile
		if err := json.Unmarshal(data, &session); err != nil {
			t.Fatal(err)
		}
		if session.Placement == nil || session.Placement.Requested != "remote" || session.Placement.Actual != "local" {
			t.Fatalf("reported placement: %+v", session.Placement)
		}
		taskRanWithoutReporter(t, root)
	})
	t.Run("installed runner never executes or reports locally", func(t *testing.T) {
		root, req := reporterFixture(t, fixtureproc.Program{Record: "executions"})
		req.Global.Where = "remote"
		path := filepath.Join(root, "extension", "putnami.extension.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var manifest extensionproto.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		manifest.Commands[runnerproto.ProviderCommandName] = extensionproto.CommandDefinition{Visibility: "internal", Run: []extensionproto.PipelineStep{{ID: "runner", Task: "test-exec"}}}
		data, err = json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		workspace.InvalidateLoadCache(root)
		// The fixture is not a repository, so the portable seam cannot capture
		// its source. That failure is reported precisely: it never becomes a
		// local execution, a local session, or a reported session.
		var result SessionResult
		stderr := captureStderr(t, func() { result, err = New().Run(context.Background(), req, discardEvents{}) })
		if err != nil || result.ExitCode != ExitError || !strings.Contains(stderr, "remote execution: capture source") {
			t.Fatalf("installed runner: exit=%d err=%v diagnostics=%s", result.ExitCode, err, stderr)
		}
		for _, marker := range []string{"executions", "trace.jsonl", "received-session.json"} {
			if _, err := os.Stat(filepath.Join(root, marker)); !os.IsNotExist(err) {
				t.Fatalf("failed remote placement reached %s: %v", marker, err)
			}
		}
	})
}

func TestNativeSessionReportingManagedToolchain(t *testing.T) {
	root, req := reporterFixture(t, fixtureproc.Program{})
	managedRoot := t.TempDir()
	// The compiler is this test binary: it answers the version probe and
	// otherwise runs as the provider.
	fixtureproc.Binary(t, filepath.Join(managedRoot, "toolchains", "compiler", "compiler-1.2.3", "bin", reporterCompiler))
	t.Setenv("PUTNAMI_HOME", managedRoot)
	locked := lockfile.NewLockFile()
	locked.SetToolchain("compiler", lockfile.LockEntry{Version: "1.2.3", Integrities: map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): "test-integrity"}})
	if err := lockfile.WriteLockFile(root, locked); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "extension", "putnami.extension.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest extensionproto.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Runtime = &extensionproto.RuntimeDefinition{Executable: "unused-runtime", Toolchains: map[string]extensionproto.RuntimeToolchain{
		"compiler": {Lock: "compiler", Candidates: []extensionproto.RuntimeToolchainCandidate{{From: "putnami-home", Path: "toolchains/compiler/compiler-{version}/bin/reporter-compiler"}}, Probe: extensionproto.RuntimeToolchainProbe{Args: []string{"--version"}, Expect: "{version}"}, PrependPath: true, Environment: map[string]extensionproto.RuntimeToolchainEnvironment{"REPORTER_COMPILER_ROOT": {From: "ancestor", Levels: 2}}},
	}}
	provider, ok := manifest.Tasks["reporter-exec"]
	if !ok {
		t.Fatal("missing provider")
	}
	provider.Toolchains = []string{"compiler"}
	provider.Env["REPORTER_TEST_MANAGED"] = "1"
	provider.Command = reporterCompiler
	manifest.Tasks["reporter-exec"] = provider
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	var result SessionResult
	stderr := captureStderr(t, func() { result, err = New().Run(context.Background(), req, discardEvents{}) })
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("managed reporter diagnostics: %s", stderr)
			id := workspace_state.NewSessionStore(root).LatestID()
			checkpoint, readErr := os.ReadFile(filepath.Join(root, ".putnami", "sessions", id, sessionreporter.StateFile))
			t.Logf("managed reporter checkpoint (first 4096 bytes): %.4096s; read error: %v", checkpoint, readErr)
		}
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("managed reporter: exit=%d err=%v", result.ExitCode, err)
	}
	verifyReporterArtifacts(t, root, workspace_state.NewSessionStore(root).LatestID())
}

func TestNativeSessionReportingSuccessAndFailure(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "live-events-and-finalized-success-failure-cancellation")
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			// The job cannot finish until the real provider sees task:start. This
			// distinguishes live delivery from an after-the-DAG upload.
			root, req := reporterFixture(t, fixtureproc.Program{Record: "executions", WaitFor: []string{"live"}, Exit: exit})
			result, err := New().Run(liveReportingContext(), req, discardEvents{})
			if err != nil || result.ExitCode != exit {
				t.Fatalf("engine exit=%d error=%v", result.ExitCode, err)
			}
			verifyReporterArtifacts(t, root, workspace_state.NewSessionStore(root).LatestID())
			taskRanWithoutReporter(t, root)
		})
	}
}

func TestNativeSessionReportingCancellation(t *testing.T) {
	abort.Reset()
	t.Cleanup(abort.Reset)
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "live-events-and-finalized-success-failure-cancellation")
	root, req := reporterFixture(t, fixtureproc.Program{Record: "started", Sleep: 30 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan bool, 1)
	go func() {
		deadline := time.NewTimer(30 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline.C:
				started <- false
				cancel()
				return
			case <-ticker.C:
				if _, err := os.Stat(filepath.Join(root, "started")); err == nil {
					started <- true
					abort.Record(syscall.SIGTERM)
					cancel()
					return
				}
			}
		}
	}()
	result, err := New().Run(ctx, req, discardEvents{})
	if !<-started {
		t.Fatal("task did not reach its cancellation readiness point")
	}
	if err != nil || result.ExitCode != ExitSignalReceived {
		t.Fatalf("canceled graph exit=%d error=%v", result.ExitCode, err)
	}
	verifyReporterArtifacts(t, root, workspace_state.NewSessionStore(root).LatestID())
}

func TestNativeSessionReportingOutageAndReplay(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "durable-replay", "outage-retains-identical-frame-and-replay-runs-no-workload")
	root, req := reporterFixture(t, fixtureproc.Program{Record: "executions"})
	if err := os.WriteFile(filepath.Join(root, "offline"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var result SessionResult
	stderr := captureStderr(t, func() { result, _ = New().Run(context.Background(), req, discardEvents{}) })
	if result.ExitCode != 0 || !strings.Contains(stderr, "reporting incomplete") || strings.Contains(stderr, "reporter-test-secret") {
		t.Fatalf("outage changed verdict or exposed secret: exit=%d stderr=%s", result.ExitCode, stderr)
	}
	sessionID := workspace_state.NewSessionStore(root).LatestID()
	dir := filepath.Join(root, ".putnami", "sessions", sessionID)
	stateBytes, err := os.ReadFile(filepath.Join(dir, sessionreporter.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint struct {
		Pending     *protocolcli.SessionReportingChunk `json:"pending"`
		Complete    bool                               `json:"complete"`
		PlanOmitted string                             `json:"planOmitted"`
	}
	if json.Unmarshal(stateBytes, &checkpoint) != nil || checkpoint.Pending == nil || checkpoint.Complete {
		t.Fatalf("no pending frame: %s", stateBytes)
	}
	// The outage refused plan.json first, without retry: plan.json is
	// best-effort and closes as refused, so replay resumes at the frame the
	// outage left pending.
	if checkpoint.PlanOmitted != "refused" || checkpoint.Pending.Artifact == "plan.json" {
		t.Fatalf("outage checkpoint: %s", stateBytes)
	}
	outage := len(traceLines(t, filepath.Join(root, "trace.jsonl")))
	if err := os.Remove(filepath.Join(root, "offline")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(protocolcli.SessionReporterEnv, "@test/reporter")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "reporter-test-secret")
	if err := New().ReplaySession(context.Background(), root, req.Config, sessionID); err != nil {
		t.Fatal(err)
	}
	verifyReporterDelivery(t, root, sessionID, "refused")
	lines := traceLines(t, filepath.Join(root, "trace.jsonl"))
	if len(lines) <= outage || !bytes.Equal(lines[outage], lines[outage-1]) {
		t.Fatalf("pending identity changed on replay: %s", bytes.Join(lines, []byte("\n")))
	}
	if runs := fixtureproc.Runs(t, filepath.Join(root, "executions")); len(runs) != 1 {
		t.Fatalf("replay ran the workload: %d executions, want 1", len(runs))
	}
}

// TestNativeSessionReportingSendsThePlanBeforeTheFirstTask holds the task until
// the receiver closed plan.json: plan.json leaves while the graph runs, before
// the first task:start record, as the bytes the session persisted.
func TestNativeSessionReportingSendsThePlanBeforeTheFirstTask(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "plan-precedes-every-other-frame")
	root, req := reporterFixture(t, fixtureproc.Program{Record: "executions", WaitFor: []string{"received-plan.json.final"}})
	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("engine exit=%d error=%v", result.ExitCode, err)
	}
	sessionID := workspace_state.NewSessionStore(root).LatestID()
	verifyReporterArtifacts(t, root, sessionID)
	var plan protocolcli.SessionPlanFile
	data, err := os.ReadFile(filepath.Join(sessionDir(root, sessionID), "plan.json"))
	if err != nil || json.Unmarshal(data, &plan) != nil || plan.SessionID != sessionID || len(plan.Tasks) != 1 {
		t.Fatalf("the delivered plan.json is not the session's plan: %s (%v)", data, err)
	}
	taskRanWithoutReporter(t, root)
}

// TestNativeSessionReportingARefusedPlanLeavesTheRunUnchanged runs against a
// receiver that refuses plan.json, or exits at it as a receiver that does not
// know the artifact does: the verdict, the other artifacts and the delivery
// evidence stay as without plan.json, and no diagnostic is printed.
func TestNativeSessionReportingARefusedPlanLeavesTheRunUnchanged(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "an-omitted-plan-leaves-delivery-and-the-verdict-unchanged")
	for _, tc := range []struct {
		marker, omitted string
		// frames is how many plan.json frames reach the receiver: one refusal,
		// or one per attempt when the receiver exits.
		frames int
	}{
		{"refuse-plan", "refused", 1},
		{"exit-on-plan", "undeliverable", 3},
	} {
		for _, exit := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s exit %d", tc.marker, exit), func(t *testing.T) {
				root, req := reporterFixture(t, fixtureproc.Program{Record: "executions", Exit: exit})
				if err := os.WriteFile(filepath.Join(root, tc.marker), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				var result SessionResult
				var err error
				stderr := captureStderr(t, func() { result, err = New().Run(context.Background(), req, discardEvents{}) })
				if err != nil || result.ExitCode != exit || strings.Contains(stderr, "incomplete") {
					t.Fatalf("an omitted plan.json changed the run: exit=%d err=%v diagnostics=%s", result.ExitCode, err, stderr)
				}
				sessionID := workspace_state.NewSessionStore(root).LatestID()
				verifyReporterDelivery(t, root, sessionID, tc.omitted)
				frames := 0
				for _, line := range traceLines(t, filepath.Join(root, "trace.jsonl")) {
					if chunk, err := protocolcli.ParseSessionReportingChunk(line); err == nil && chunk.Artifact == "plan.json" {
						frames++
					}
				}
				if frames != tc.frames {
					t.Fatalf("plan.json frames = %d, want %d", frames, tc.frames)
				}
				if got := subscriberEvidence(t, root, sessionID)[protocolcli.SessionReporterCommand].Evidence; got != protocolcli.SubscriberEvidenceDelivered {
					t.Fatalf("session reporter evidence %q", got)
				}
				taskRanWithoutReporter(t, root)
			})
		}
	}
}

// TestNativeSessionReportingReplaySendsThePlanFirst replays a session no
// reporter received: replay sends plan.json first, as the run would have, and
// runs no workload.
func TestNativeSessionReportingReplaySendsThePlanFirst(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "durable-replay", "replay-sends-an-undelivered-plan-first")
	root, req := reporterFixture(t, fixtureproc.Program{Record: "executions"})
	t.Setenv(protocolcli.SessionReporterEnv, "")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "")
	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("engine exit=%d error=%v", result.ExitCode, err)
	}
	if _, err := os.Stat(filepath.Join(root, "trace.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("an unselected reporter received frames: %v", err)
	}
	sessionID := workspace_state.NewSessionStore(root).LatestID()
	t.Setenv(protocolcli.SessionReporterEnv, "@test/reporter")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "reporter-test-secret")
	if err := New().ReplaySession(context.Background(), root, req.Config, sessionID); err != nil {
		t.Fatal(err)
	}
	verifyReporterArtifacts(t, root, sessionID)
	if runs := fixtureproc.Runs(t, filepath.Join(root, "executions")); len(runs) != 1 {
		t.Fatalf("replay ran the workload: %d executions, want 1", len(runs))
	}
}

// verifyReporterArtifacts fails t unless the session reporter delivered the
// session's plan.json, session.json and events.jsonl unchanged, closed each,
// and closed plan.json before any other frame reached the receiver.
func verifyReporterArtifacts(t *testing.T, root, sessionID string) {
	t.Helper()
	verifyReporterDelivery(t, root, sessionID, "")
}

// verifyReporterDelivery is verifyReporterArtifacts for a session whose
// checkpoint omits plan.json as planOmitted says, "" when it does not: an
// omitted plan.json is never closed, and every plan.json frame the receiver
// read, refused ones included, precedes every other frame.
func verifyReporterDelivery(t *testing.T, root, sessionID, planOmitted string) {
	t.Helper()
	if sessionID == "" {
		t.Fatal("engine produced no session")
	}
	checkpoint, err := os.ReadFile(filepath.Join(root, ".putnami", "sessions", sessionID, sessionreporter.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Complete    bool                               `json:"complete"`
		Pending     *protocolcli.SessionReportingChunk `json:"pending"`
		PlanOmitted string                             `json:"planOmitted"`
	}
	if err := json.Unmarshal(checkpoint, &state); err != nil {
		t.Fatal(err)
	}
	if !state.Complete || state.Pending != nil {
		t.Fatal("provider exited after final ACK but reporting remained incomplete")
	}
	if state.PlanOmitted != planOmitted {
		t.Fatalf("checkpoint omits plan.json as %q, want %q", state.PlanOmitted, planOmitted)
	}
	artifacts := []string{"session.json", "events.jsonl"}
	if planOmitted == "" {
		artifacts = append(artifacts, "plan.json")
	} else if _, err := os.Stat(filepath.Join(root, "received-plan.json.final")); err == nil {
		t.Fatal("the receiver closed an omitted plan.json")
	}
	for _, artifact := range artifacts {
		want, err := os.ReadFile(filepath.Join(root, ".putnami", "sessions", sessionID, artifact))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root, "received-"+artifact))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("reporter changed %s", artifact)
		}
		if _, err := os.Stat(filepath.Join(root, "received-"+artifact+".final")); err != nil {
			t.Fatalf("%s not finalized: %v", artifact, err)
		}
	}
	trace, err := os.ReadFile(filepath.Join(root, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var sessionFinal, planFinal, otherFrame bool
	for _, line := range bytes.Split(bytes.TrimSpace(trace), []byte("\n")) {
		chunk, err := protocolcli.ParseSessionReportingChunk(line)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Artifact == "plan.json" {
			if otherFrame {
				t.Fatal("a plan.json frame followed another artifact's frame")
			}
			planFinal = planFinal || chunk.Final
		} else {
			// Every events.jsonl frame, the one that carries the first
			// task:start record included, follows the plan.json final marker.
			if planOmitted == "" && !planFinal {
				t.Fatalf("%s reached the receiver before plan.json closed", chunk.Artifact)
			}
			otherFrame = true
		}
		if chunk.Artifact == "session.json" && chunk.Final {
			sessionFinal = true
		}
		if chunk.Artifact == "events.jsonl" && chunk.Final && !sessionFinal {
			t.Fatal("events closed before persisted session")
		}
	}
}

func TestNativeSessionReportingSelectionAndReplayValidation(t *testing.T) {
	root, req := reporterFixture(t, fixtureproc.Program{})
	t.Setenv(protocolcli.SessionReporterEnv, "@test/missing")
	var result SessionResult
	stderr := captureStderr(t, func() { result, _ = New().Run(context.Background(), req, discardEvents{}) })
	if result.ExitCode != 0 || !strings.Contains(stderr, "reporter unavailable") {
		t.Fatalf("unavailable reporter: %d %s", result.ExitCode, stderr)
	}
	for _, id := range []string{"", "latest", "../outside", "absent"} {
		if err := New().ReplaySession(context.Background(), root, req.Config, id); err == nil {
			t.Fatalf("replay accepted %q", id)
		}
	}
}
