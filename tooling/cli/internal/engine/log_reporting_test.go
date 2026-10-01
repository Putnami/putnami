package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/sessionreporter"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// noReportingEnv fails a hook that can see any reporting selector or token.
const noReportingEnv = `test -z "$PUTNAMI_SESSION_REPORTER_TOKEN$PUTNAMI_SESSION_REPORTER$PUTNAMI_LOG_REPORTER_TOKEN$PUTNAMI_LOG_REPORTER"`

// reportingEnv names every reporting selector and token; no task sees one.
var reportingEnv = []string{protocolcli.SessionReporterTokenEnv, protocolcli.SessionReporterEnv, protocolcli.LogReporterTokenEnv, protocolcli.LogReporterEnv}

// logReporterFixture extends reporterFixture with a second extension,
// @test/logs, that declares only the log-reporter command. Its provider is the
// same real subprocess helper, keeping its own "log-" prefixed files and
// requiring its own token. Both capabilities are selected.
func logReporterFixture(t *testing.T, task fixtureproc.Program) (string, Request) {
	t.Helper()
	root, req := reporterFixture(t, task)
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
	write("putnami.workspace.json", []byte(`{"name":"reporter-proof","includes":["app","extension","logs"]}`))
	write("app/putnami.json", []byte(`{"name":"app","extensions":["/extension","/logs"]}`))
	write("logs/putnami.json", []byte(`{"name":"@test/logs"}`))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := extensionproto.Manifest{
		Name: "@test/logs", Version: "1.0.0", CLIContract: 4,
		Commands: map[string]extensionproto.CommandDefinition{
			protocolcli.LogReporterCommand: {Visibility: "internal", Run: []extensionproto.PipelineStep{{ID: "reporter", Task: "log-reporter-exec"}}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"log-reporter-exec": {Kind: "command", Command: executable, Args: []string{"-test.run=^TestNativeSessionReporterProvider$"}, Env: map[string]string{"REPORTER_TEST_HELPER": "1", "REPORTER_TEST_ROOT": root, "REPORTER_TEST_NAME": "log"}},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write("logs/putnami.extension.json", data)
	workspace.InvalidateLoadCache(root)
	req.Config = wsproto.Load(root)
	t.Setenv(protocolcli.LogReporterEnv, "@test/logs")
	t.Setenv(protocolcli.LogReporterTokenEnv, "log-test-secret")
	return root, req
}

// selectReporting re-selects both capabilities, as a fresh process would: the
// engine run captured and removed them from the environment.
func selectReporting(t *testing.T) {
	t.Helper()
	t.Setenv(protocolcli.SessionReporterEnv, "@test/reporter")
	t.Setenv(protocolcli.SessionReporterTokenEnv, "reporter-test-secret")
	t.Setenv(protocolcli.LogReporterEnv, "@test/logs")
	t.Setenv(protocolcli.LogReporterTokenEnv, "log-test-secret")
}

// taskRanOnceWithoutReporting fails t unless the test task, recording its runs
// in root/executions, ran once and saw no reporting selector or token.
func taskRanOnceWithoutReporting(t *testing.T, root string) {
	t.Helper()
	runs := fixtureproc.Runs(t, filepath.Join(root, "executions"))
	if len(runs) != 1 {
		t.Fatalf("job executions = %d, want 1", len(runs))
	}
	for _, name := range reportingEnv {
		if value, ok := runs[0].LookupEnv(name); ok && value != "" {
			t.Fatalf("the task saw %s", name)
		}
	}
}

func sessionDir(root, sessionID string) string {
	return filepath.Join(root, ".putnami", "sessions", sessionID)
}

func subscriberEvidence(t *testing.T, root, sessionID string) map[string]protocolcli.SessionSubscriberEvidence {
	t.Helper()
	evidence := map[string]protocolcli.SessionSubscriberEvidence{}
	file, err := sessionstream.ReadEvidence(sessionDir(root, sessionID))
	if os.IsNotExist(err) {
		return evidence
	}
	if err != nil {
		t.Fatalf("read subscribers.json: %v", err)
	}
	if file.SessionID != sessionID {
		t.Fatalf("subscribers.json belongs to %q, want %q", file.SessionID, sessionID)
	}
	for _, entry := range file.Subscribers {
		evidence[entry.Name] = entry
	}
	return evidence
}

func readCheckpoint(t *testing.T, root, sessionID, name string) (complete bool, pending *protocolcli.SessionReportingChunk) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sessionDir(root, sessionID), name))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Complete bool                               `json:"complete"`
		Pending  *protocolcli.SessionReportingChunk `json:"pending"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.Complete, state.Pending
}

func traceLines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(data), []byte("\n"))
}

// verifyLogReporterArtifacts asserts the log reporter received the persisted
// events.jsonl byte for byte, closed it, and never received session.json.
func verifyLogReporterArtifacts(t *testing.T, root, sessionID string) {
	t.Helper()
	if sessionID == "" {
		t.Fatal("engine produced no session")
	}
	if complete, pending := readCheckpoint(t, root, sessionID, sessionreporter.LogStateFile); !complete || pending != nil {
		t.Fatal("log reporter exited after its final ACK but its checkpoint remained incomplete")
	}
	want, err := os.ReadFile(filepath.Join(sessionDir(root, sessionID), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "log-received-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("log reporter changed events.jsonl")
	}
	if _, err := os.Stat(filepath.Join(root, "log-received-events.jsonl.final")); err != nil {
		t.Fatalf("log reporter events not finalized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "log-received-session.json")); !os.IsNotExist(err) {
		t.Fatalf("log reporter received session.json: %v", err)
	}
	for _, line := range traceLines(t, filepath.Join(root, "log-trace.jsonl")) {
		chunk, err := protocolcli.ParseSessionReportingChunk(line)
		if err != nil || chunk.Artifact != "events.jsonl" {
			t.Fatalf("log reporter frame %s: %v", line, err)
		}
	}
}

// assertUnselected asserts a capability left no provider, checkpoint or
// evidence behind.
func assertUnselected(t *testing.T, root, sessionID string, capability sessionreporter.Capability, prefix string, evidence map[string]protocolcli.SessionSubscriberEvidence) {
	t.Helper()
	for _, path := range []string{filepath.Join(root, prefix+"trace.jsonl"), filepath.Join(root, prefix+"provider.pid"), filepath.Join(sessionDir(root, sessionID), capability.StateFile)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unselected %s left %s: %v", capability.Label, path, err)
		}
	}
	if entry, ok := evidence[capability.Name]; ok {
		t.Fatalf("unselected %s recorded evidence %+v", capability.Label, entry)
	}
}

func TestLogReportingSelection(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "reporting-capabilities-deliver-and-fail-independently")
	for _, tc := range []struct {
		name         string
		session, log bool
	}{
		{"both", true, true},
		{"session reporter only", true, false},
		{"log reporter only", false, true},
		{"neither", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The job cannot finish until each selected provider has received
			// task:start: both deliver live, not after the graph.
			var markers []string
			if tc.session {
				markers = append(markers, "live")
			}
			if tc.log {
				markers = append(markers, "log-live")
			}
			// The markers are relative to the task's working directory, the
			// workspace root.
			root, req := logReporterFixture(t, fixtureproc.Program{Record: "executions", WaitFor: markers})
			if !tc.session {
				t.Setenv(protocolcli.SessionReporterEnv, "")
			}
			if !tc.log {
				t.Setenv(protocolcli.LogReporterEnv, "")
			}
			var result SessionResult
			var err error
			stderr := captureStderr(t, func() { result, err = New().Run(liveReportingContext(), req, discardEvents{}) })
			if err != nil || result.ExitCode != 0 || strings.Contains(stderr, "report") {
				t.Fatalf("engine exit=%d error=%v diagnostics=%s", result.ExitCode, err, stderr)
			}
			sessionID := workspace_state.NewSessionStore(root).LatestID()
			evidence := subscriberEvidence(t, root, sessionID)
			if tc.session {
				verifyReporterArtifacts(t, root, sessionID)
				if evidence[protocolcli.SessionReporterCommand].Evidence != protocolcli.SubscriberEvidenceDelivered {
					t.Fatalf("session reporter evidence %+v", evidence)
				}
			} else {
				assertUnselected(t, root, sessionID, sessionreporter.SessionReporter, "", evidence)
			}
			if tc.log {
				verifyLogReporterArtifacts(t, root, sessionID)
				if evidence[protocolcli.LogReporterCommand].Evidence != protocolcli.SubscriberEvidenceDelivered {
					t.Fatalf("log reporter evidence %+v", evidence)
				}
			} else {
				assertUnselected(t, root, sessionID, sessionreporter.LogReporter, "log-", evidence)
			}
			taskRanOnceWithoutReporting(t, root)
		})
	}
}

func TestLogReportingTokensReachOnlyTheirOwnProvider(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "credential-boundary", "each-token-reaches-only-its-own-provider")
	// Each provider exits 21 without its own token and 22 when it sees a
	// selector or the other capability's token; the task and both hooks fail
	// when they see any of the four.
	requireSh(t)
	root, req := logReporterFixture(t, fixtureproc.Program{Record: "executions"})
	check := noReportingEnv + " && echo denied >> hook-checks"
	req.Hooks = &wsproto.HooksConfig{CLI: &wsproto.HookPhaseConfig{Before: []string{check}, After: []string{check}}}
	var result SessionResult
	var err error
	stderr := captureStderr(t, func() { result, err = New().Run(context.Background(), req, discardEvents{}) })
	if err != nil || result.ExitCode != 0 || strings.Contains(stderr, "report") {
		t.Fatalf("credential isolation failed: exit=%d err=%v diagnostics=%s", result.ExitCode, err, stderr)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "hook-checks")); string(data) != "denied\ndenied\n" {
		t.Fatalf("hooks did not execute: %q", data)
	}
	taskRanOnceWithoutReporting(t, root)
	sessionID := workspace_state.NewSessionStore(root).LatestID()
	verifyReporterArtifacts(t, root, sessionID)
	verifyLogReporterArtifacts(t, root, sessionID)
}

func TestLogReportingMissingExtensionRunsLocally(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "reporting-capabilities-deliver-and-fail-independently")
	for _, exit := range []int{0, 1} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			root, req := logReporterFixture(t, fixtureproc.Program{Record: "executions", Exit: exit})
			t.Setenv(protocolcli.LogReporterEnv, "@test/missing")
			var result SessionResult
			var err error
			stderr := captureStderr(t, func() { result, err = New().Run(context.Background(), req, discardEvents{}) })
			if err != nil || result.ExitCode != exit {
				t.Fatalf("missing log reporter changed the verdict: exit=%d err=%v", result.ExitCode, err)
			}
			if strings.Count(stderr, "log report") != 1 || !strings.Contains(stderr, "log reporter unavailable") || strings.Contains(stderr, "session report") || strings.Contains(stderr, "secret") {
				t.Fatalf("want exactly one log reporter diagnostic, got %s", stderr)
			}
			sessionID := workspace_state.NewSessionStore(root).LatestID()
			verifyReporterArtifacts(t, root, sessionID)
			evidence := subscriberEvidence(t, root, sessionID)
			if evidence[protocolcli.SessionReporterCommand].Evidence != protocolcli.SubscriberEvidenceDelivered || evidence[protocolcli.LogReporterCommand].Evidence != protocolcli.SubscriberEvidenceLost {
				t.Fatalf("evidence %+v", evidence)
			}
			if _, err := os.Stat(filepath.Join(root, "log-trace.jsonl")); !os.IsNotExist(err) {
				t.Fatalf("a missing log reporter reached a provider: %v", err)
			}
			taskRanOnceWithoutReporting(t, root)
		})
	}
}

func TestLogReportingFailureStaysWithItsCapability(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "reporting-capabilities-deliver-and-fail-independently")
	capabilities := map[string]struct {
		capability sessionreporter.Capability
		prefix     string
	}{
		"session": {sessionreporter.SessionReporter, ""},
		"log":     {sessionreporter.LogReporter, "log-"},
	}
	for _, failing := range []string{"session", "log"} {
		other := map[string]string{"session": "log", "log": "session"}[failing]
		// offline refuses every chunk with a non-retryable NACK; crash exits
		// before acknowledging, on every respawn.
		for _, mode := range []string{"offline", "crash"} {
			for _, exit := range []int{0, 1} {
				t.Run(fmt.Sprintf("%s %s exit %d", failing, mode, exit), func(t *testing.T) {
					root, req := logReporterFixture(t, fixtureproc.Program{Record: "executions", Exit: exit})
					failed, delivered := capabilities[failing], capabilities[other]
					if err := os.WriteFile(filepath.Join(root, failed.prefix+mode), nil, 0o600); err != nil {
						t.Fatal(err)
					}
					var result SessionResult
					var err error
					stderr := captureStderr(t, func() { result, err = New().Run(context.Background(), req, discardEvents{}) })
					if err != nil || result.ExitCode != exit {
						t.Fatalf("%s %s changed the verdict: exit=%d err=%v", failed.capability.Label, mode, result.ExitCode, err)
					}
					if strings.Count(stderr, "incomplete") != 1 || !strings.Contains(stderr, failed.capability.Activity+" incomplete") || strings.Contains(stderr, "secret") {
						t.Fatalf("want one %s diagnostic without secrets, got %s", failed.capability.Activity, stderr)
					}
					sessionID := workspace_state.NewSessionStore(root).LatestID()
					if delivered.prefix == "" {
						verifyReporterArtifacts(t, root, sessionID)
					} else {
						verifyLogReporterArtifacts(t, root, sessionID)
					}
					if complete, _ := readCheckpoint(t, root, sessionID, failed.capability.StateFile); complete {
						t.Fatalf("failed %s checkpoint is complete", failed.capability.Label)
					}
					evidence := subscriberEvidence(t, root, sessionID)
					if evidence[delivered.capability.Name].Evidence != protocolcli.SubscriberEvidenceDelivered {
						t.Fatalf("%s evidence %+v", delivered.capability.Label, evidence)
					}
					if got := evidence[failed.capability.Name].Evidence; got == "" || got == protocolcli.SubscriberEvidenceDelivered {
						t.Fatalf("failed %s evidence %q", failed.capability.Label, got)
					}
				})
			}
		}
	}
}

func TestLogReportingReplayResumesOnlyTheIncompleteCapability(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "durable-replay", "replay-resumes-only-undelivered-capabilities")
	// The log provider acknowledges the live chunk that carries task:start, then
	// refuses everything after it: the log reporter stops at a non-zero
	// acknowledged offset while the session reporter delivers everything.
	root, req := logReporterFixture(t, fixtureproc.Program{Record: "executions", WaitFor: []string{"log-live"}})
	if err := os.WriteFile(filepath.Join(root, "log-offline-after-live"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var result SessionResult
	var err error
	stderr := captureStderr(t, func() { result, err = New().Run(liveReportingContext(), req, discardEvents{}) })
	if err != nil || result.ExitCode != 0 || strings.Count(stderr, "incomplete") != 1 || !strings.Contains(stderr, "log reporting incomplete") {
		t.Fatalf("log outage: exit=%d err=%v diagnostics=%s", result.ExitCode, err, stderr)
	}
	sessionID := workspace_state.NewSessionStore(root).LatestID()
	dir := sessionDir(root, sessionID)
	verifyReporterArtifacts(t, root, sessionID)
	before := subscriberEvidence(t, root, sessionID)
	if before[protocolcli.SessionReporterCommand].Evidence != protocolcli.SubscriberEvidenceDelivered || before[protocolcli.LogReporterCommand].Evidence != protocolcli.SubscriberEvidencePartial {
		t.Fatalf("evidence after the outage %+v", before)
	}
	complete, pending := readCheckpoint(t, root, sessionID, sessionreporter.LogStateFile)
	if complete || pending == nil || pending.Offset == 0 {
		t.Fatalf("log checkpoint after the outage: complete=%v pending=%+v, want a pending frame past offset 0", complete, pending)
	}
	sessionTrace, err := os.ReadFile(filepath.Join(root, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sessionCheckpoint, err := os.ReadFile(filepath.Join(dir, sessionreporter.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	logFrames := len(traceLines(t, filepath.Join(root, "log-trace.jsonl")))
	for _, marker := range []string{"log-offline", "log-offline-after-live"} {
		if err := os.Remove(filepath.Join(root, marker)); err != nil {
			t.Fatal(err)
		}
	}

	selectReporting(t)
	if err := New().ReplaySession(context.Background(), root, req.Config, sessionID); err != nil {
		t.Fatal(err)
	}
	verifyLogReporterArtifacts(t, root, sessionID)
	lines := traceLines(t, filepath.Join(root, "log-trace.jsonl"))
	if len(lines) <= logFrames || !bytes.Equal(lines[logFrames], lines[logFrames-1]) {
		t.Fatal("replay did not resend the refused frame's exact bytes first")
	}
	for _, line := range lines[logFrames:] {
		chunk, err := protocolcli.ParseSessionReportingChunk(line)
		if err != nil || chunk.Offset < pending.Offset {
			t.Fatalf("replay sent %s before the acknowledged offset %d: %v", line, pending.Offset, err)
		}
	}
	after := subscriberEvidence(t, root, sessionID)
	if after[protocolcli.LogReporterCommand].Evidence != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("log reporter evidence after replay %+v", after)
	}
	if !reflect.DeepEqual(after[protocolcli.SessionReporterCommand], before[protocolcli.SessionReporterCommand]) {
		t.Fatalf("replay rewrote the delivered session reporter entry: %+v, was %+v", after[protocolcli.SessionReporterCommand], before[protocolcli.SessionReporterCommand])
	}
	if data, _ := os.ReadFile(filepath.Join(root, "trace.jsonl")); !bytes.Equal(data, sessionTrace) {
		t.Fatal("replay resent frames to the delivered session reporter")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, sessionreporter.StateFile)); !bytes.Equal(data, sessionCheckpoint) {
		t.Fatal("replay rewrote the delivered session reporter checkpoint")
	}

	// Once every selected capability is delivered, replay sends nothing.
	logTrace, err := os.ReadFile(filepath.Join(root, "log-trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	selectReporting(t)
	if err := New().ReplaySession(context.Background(), root, req.Config, sessionID); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "log-trace.jsonl")); !bytes.Equal(data, logTrace) {
		t.Fatal("replay resent frames to a delivered log reporter")
	}
	if runs := fixtureproc.Runs(t, filepath.Join(root, "executions")); len(runs) != 1 {
		t.Fatalf("replay ran the workload: %d executions, want 1", len(runs))
	}
}
