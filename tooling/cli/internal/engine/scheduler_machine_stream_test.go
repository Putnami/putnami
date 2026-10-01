package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// Observe writes synchronously: inspecting a captured stream only after Run
// returns cannot prove the named plan was readable before task execution.
type openingSchedulerObserver struct {
	bytes.Buffer
	pending []byte
	observe func([]byte)
}

func (w *openingSchedulerObserver) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.pending = append(w.pending, p...)
	for {
		line, rest, found := bytes.Cut(w.pending, []byte{'\n'})
		if !found {
			break
		}
		w.observe(line)
		w.pending = rest
	}
	return n, err
}

func TestRunOpeningSchedulerEventNamesPersistedPlanBeforeTasks(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "opening-session-identity", "the-opening-event-names-the-written-plan-before-tasks-and-prepares-once")
	for _, commands := range [][]string{{"build"}, {"build", "publish"}} {
		t.Run(strings.Join(commands, ","), func(t *testing.T) {
			root := t.TempDir()
			write := func(rel, content string, mode os.FileMode) {
				t.Helper()
				path := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			write("putnami.workspace.json", `{"name":"opening-session","includes":["app","extension"]}`, 0o644)
			write("app/putnami.json", `{"name":"app","extensions":["/extension"]}`, 0o644)
			write("extension/putnami.json", `{"name":"@putnami/opening-session"}`, 0o644)
			write("extension/putnami.extension.json", `{
				"name":"@putnami/opening-session","version":"1.0.0","cliContract":4,
				"commands":{
					"build":{"run":[{"id":"build","task":"mark"}]},
					"publish":{"run":[{"id":"publish","task":"mark"}]}
				},
				"tasks":{"mark":{"kind":"command","command":`+taskCommand(t, fixtureproc.Program{Record: filepath.Join(root, "tasks.log")})+`,"cache":false}}
			}`, 0o644)
			workspace.InvalidateLoadCache(root)
			hooks, ran := hookRecorder(t)
			var sessionID string
			openingEvents, taskStarts := 0, 0
			live := &openingSchedulerObserver{}
			live.observe = func(line []byte) {
				var record protocolcli.SessionStreamRecord
				if err := json.Unmarshal(line, &record); err != nil {
					t.Errorf("decode live record: %v", err)
					return
				}
				if record.Record == protocolcli.RecordTaskStart {
					taskStarts++
					if openingEvents != 1 || sessionID == "" {
						t.Error("task started before the opening session identity")
					}
				}
				if record.Event["type"] != "scheduler:parallel" {
					return
				}
				openingEvents++
				if taskStarts != 0 {
					t.Error("opening scheduler event arrived after a task start")
				}
				if _, err := os.Stat(filepath.Join(root, "tasks.log")); !os.IsNotExist(err) {
					t.Errorf("task marker exists before the opening event: %v", err)
				}
				if got := strings.Join(ran(), ","); got != "cli-before,command-before" {
					t.Errorf("preparation hooks at opening event = %q", got)
				}
				sessionID, _ = record.Event["sessionId"].(string)
				if sessionID == "" {
					t.Error("opening scheduler event omitted the recorded session ID")
					return
				}
				data, err := os.ReadFile(filepath.Join(root, ".putnami", "sessions", sessionID, "plan.json"))
				if err != nil {
					t.Errorf("plan not readable at opening event: %v", err)
					return
				}
				if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionPlanFile, data); len(violations) != 0 {
					t.Errorf("persisted plan violations: %+v", violations)
				}
				var plan protocolcli.SessionPlanFile
				if err := json.Unmarshal(data, &plan); err != nil {
					t.Errorf("decode persisted plan: %v", err)
				}
				if plan.SessionID != sessionID || !reflect.DeepEqual(plan.Commands, commands) || len(plan.Tasks) != len(commands) {
					t.Errorf("opening event identified the wrong plan: %+v", plan)
				}
			}
			renderer := output.NewRenderer(output.Config{
				Output: "jsonl", Command: strings.Join(commands, ","), Out: live, Err: live,
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := New().Run(ctx, Request{
				WorkspaceRoot: root, Config: wsproto.Load(root), Commands: commands, Hooks: hooks,
				Global: GlobalFlags{Output: "jsonl", Projects: "app", NoCache: true, MaxParallel: 1},
			}, renderer)
			if err != nil || result.ExitCode != ExitSuccess {
				t.Fatalf("real engine run: exit=%d err=%v\n%s", result.ExitCode, err, live.String())
			}
			if openingEvents != 1 || taskStarts != len(commands) || sessionID == "" {
				t.Fatalf("opening events=%d task starts=%d session=%q", openingEvents, taskStarts, sessionID)
			}
			if got := strings.Join(ran(), ","); got != "cli-before,command-before,command-after,cli-after" {
				t.Errorf("successful invocation repeated preparation hooks: %q", got)
			}
			if runs := fixtureproc.Runs(t, filepath.Join(root, "tasks.log")); len(runs) != len(commands) {
				t.Errorf("real task runs = %d, want %d", len(runs), len(commands))
			}
			artifact, err := os.ReadFile(filepath.Join(root, ".putnami", "sessions", sessionID, protocolcli.MachineOutputArtifactPath))
			if err != nil {
				t.Fatal(err)
			}
			if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
				t.Fatalf("real engine stream violations: %+v", violations)
			}
			if !bytes.Equal(live.Bytes(), artifact) {
				t.Error("finite live stream differs from the retained artifact")
			}
			final := lastStreamRecord(t, live.String())
			if final.MachineOutput == nil || final.MachineOutput.Artifact.SessionID != sessionID {
				t.Errorf("terminal artifact identity differs from opening session %q", sessionID)
			}
		})
	}
}

// TestSchedulerInitializesMachineRetentionBeforeAuditEvents exercises the real
// scheduler ordering. scheduler:parallel is emitted after Start and before any
// task; otherwise session:end summarizes a different selection than the
// complete artifact contains.
func TestSchedulerInitializesMachineRetentionBeforeAuditEvents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	session, err := workspace_state.NewSessionStore(root).Create()
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(session.Close)

	var live bytes.Buffer
	renderer := output.WithSessionRecording(output.NewRenderer(output.Config{
		Output:  "jsonl",
		Command: "build",
		Out:     &live,
		Err:     &live,
	}), session, false)
	recorder, ok := renderer.(interface {
		RecordSessionEvent(jobs.SessionRecord)
	})
	if !ok {
		t.Fatalf("renderer %T does not record scheduler events", renderer)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobs.RunPlan(ctx, jobs.RunRequest{
		Workspace: &workspace.Workspace{
			Name:  "machine-order",
			Root:  root,
			Graph: workspace.BuildGraph(nil),
		},
		Plan:          nil,
		Config:        jobs.SchedulerConfig{MaxParallel: 1, NoCache: true},
		Renderer:      renderer,
		SessionEvents: recorder.RecordSessionEvent,
	})

	artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatalf("read machine artifact: %v", err)
	}
	if !bytes.Contains(live.Bytes(), []byte("scheduler:parallel")) ||
		!bytes.Contains(artifact, []byte("scheduler:parallel")) {
		t.Fatalf("opening scheduler audit event missing from live/artifact streams\nlive=%s\nartifact=%s", live.Bytes(), artifact)
	}
	if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
		t.Fatalf("real scheduler stream violations: %+v\nlive=%s\nartifact=%s", violations, live.Bytes(), artifact)
	}
}

func TestExecutionRendererWithoutSessionMakesNoArtifactClaim(t *testing.T) {
	t.Parallel()
	var live bytes.Buffer
	renderer := executionSessionRenderer(output.NewRenderer(output.Config{
		Output:  "jsonl",
		Command: "install",
		Out:     &live,
		Err:     &live,
	}), nil, false)
	renderer.Start(nil)
	renderer.Finish(nil, jobs.SessionOutcome{})

	lines := bytes.Split(bytes.TrimSpace(live.Bytes()), []byte{'\n'})
	final := lines[len(lines)-1]
	if bytes.Contains(final, []byte(`"machineOutput"`)) || bytes.Contains(final, []byte("unrecorded")) {
		t.Fatalf("intentional no-session run made an impossible artifact claim: %s", final)
	}
}
