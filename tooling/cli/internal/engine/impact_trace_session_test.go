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

// An --impacted run records why it selected what it did, in the session's
// retained event log and on the live JSONL stream alike: every seed, every
// edge, the baseline, the commit the diff measured against, and the changed
// files no commit records. Before it, the evidence was printed only
// under --verbose on a terminal and capped at ten edges, so a hosted run that
// selected more than a local run for the same commit left no record of why.
func TestRunRecordsTheImpactedSelectionTrace(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure", "the-selection-explains-every-project-it-holds")
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
	write(".gitignore", ".putnami/\n", 0o644)
	write("putnami.workspace.json", `{"name":"impact-trace","includes":["app","lib","extension"]}`, 0o644)
	write("lib/putnami.json", `{"name":"lib","extensions":["/extension"]}`, 0o644)
	write("app/putnami.json", `{"name":"app","dependencies":["lib"],"extensions":["/extension"]}`, 0o644)
	write("extension/putnami.json", `{"name":"@putnami/impact-trace"}`, 0o644)
	write("extension/putnami.extension.json", `{
		"name":"@putnami/impact-trace","version":"1.0.0","cliContract":4,
		"commands":{"build":{"run":[{"id":"build","task":"mark"}]}},
		"tasks":{"mark":{"kind":"command","command":`+taskCommand(t, fixtureproc.Program{})+`,"cache":false}}
	}`, 0o644)
	initCLISelectionGitRepo(t, root)
	runCLISelectionGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runCLISelectionGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runCLISelectionGit(t, root, "checkout", "-b", "feature")
	mergeBase := strings.TrimSpace(runCLISelectionGit(t, root, "rev-parse", "main"))
	// An uncommitted change: the shape a bootstrap-rewritten file has.
	write("lib/lib.txt", "changed\n", 0o644)
	workspace.InvalidateLoadCache(root)

	var live bytes.Buffer
	renderer := output.NewRenderer(output.Config{Output: "jsonl", Command: "build", Out: &live, Err: &live})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := New().Run(ctx, Request{
		WorkspaceRoot: root, Config: wsproto.Load(root), Commands: []string{"build"},
		Global: GlobalFlags{Output: "jsonl", Impacted: true, Projects: impactedProjectsSentinel, NoCache: true, MaxParallel: 1},
	}, renderer)
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("impacted engine run: exit=%d err=%v\n%s", result.ExitCode, err, live.String())
	}

	var trace workspace.ImpactTraceRecord
	var sessionID string
	opened, traced, started := -1, -1, -1
	for i, line := range strings.Split(strings.TrimSpace(live.String()), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode live record %q: %v", line, err)
		}
		switch {
		case record.Record == protocolcli.RecordTaskStart && started < 0:
			started = i
		case record.Event["type"] == "scheduler:parallel":
			opened = i
			sessionID, _ = record.Event["sessionId"].(string)
		case record.Event["type"] == workspace.ImpactTraceRecordType:
			if traced >= 0 {
				t.Fatalf("the trace was recorded twice:\n%s", live.String())
			}
			traced = i
			encoded, err := json.Marshal(record.Event)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &trace); err != nil {
				t.Fatalf("decode the trace %s: %v", encoded, err)
			}
			if record.Identity == nil || record.Identity.Key != "/workspace:session-events" {
				t.Errorf("trace identity = %+v, want the workspace session-events identity", record.Identity)
			}
		}
	}
	if traced < 0 {
		t.Fatalf("no %s record on the live stream:\n%s", workspace.ImpactTraceRecordType, live.String())
	}
	if !(opened >= 0 && opened < traced && traced < started) {
		t.Errorf("record order: scheduler:parallel=%d trace=%d first task:start=%d, want the trace between them", opened, traced, started)
	}

	if trace.Baseline != "origin/main" || trace.BaselineSource != "trunk" || trace.DiffBase != mergeBase {
		t.Errorf("trace baseline = %q (%q) at %q, want origin/main (trunk) at %s", trace.Baseline, trace.BaselineSource, trace.DiffBase, mergeBase)
	}
	if want := []string{"lib/lib.txt"}; !reflect.DeepEqual(trace.UncommittedFiles, want) || !reflect.DeepEqual(trace.ChangedFiles, want) {
		t.Errorf("changedFiles = %v, uncommittedFiles = %v; want both %v", trace.ChangedFiles, trace.UncommittedFiles, want)
	}
	if want := []workspace.ImpactTraceSeed{{Project: "/lib", File: "lib/lib.txt", Kind: "path-owner", Via: "lib"}}; !reflect.DeepEqual(trace.Seeds, want) {
		t.Errorf("seeds = %+v, want %+v", trace.Seeds, want)
	}
	if want := []workspace.ImpactTraceEdge{{Project: "/app", From: "/lib", Kind: "dependency"}}; !reflect.DeepEqual(trace.Edges, want) {
		t.Errorf("edges = %+v, want %+v", trace.Edges, want)
	}

	// The retained event log is the session record: the same bytes, and a
	// stream the contract's own validator accepts.
	artifact, err := os.ReadFile(filepath.Join(root, ".putnami", "sessions", sessionID, protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatalf("read the session's event log: %v", err)
	}
	if !bytes.Equal(live.Bytes(), artifact) {
		t.Error("the live stream differs from the retained session event log")
	}
	if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
		t.Fatalf("stream violations: %+v", violations)
	}
}

// The scheduler delivers the engine's pre-run records after the opening
// event and never without a session to record them: a run with no handler
// drops them rather than panicking, and a run with one keeps the stream valid.
func TestSchedulerRecordsOpeningSessionEventsAfterTheOpeningEvent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	session, err := workspace_state.NewSessionStore(root).Create()
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(session.Close)

	var live bytes.Buffer
	renderer := output.WithSessionRecording(output.NewRenderer(output.Config{
		Output: "jsonl", Command: "build", Out: &live, Err: &live,
	}), session, false)
	recorder, ok := renderer.(interface {
		RecordSessionEvent(jobs.SessionRecord)
	})
	if !ok {
		t.Fatalf("renderer %T does not record session events", renderer)
	}
	opening := []jobs.SessionRecord{
		{Type: workspace.ImpactTraceRecordType, Data: workspace.ImpactTraceRecord{Baseline: "origin/main"}.EventData()},
		{Type: "second:record"},
	}
	ws := &workspace.Workspace{Name: "opening-records", Root: root, Graph: workspace.BuildGraph(nil)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobs.RunPlan(ctx, jobs.RunRequest{
		Workspace: ws, Config: jobs.SchedulerConfig{MaxParallel: 1, NoCache: true},
		Renderer: renderer, SessionEvents: recorder.RecordSessionEvent, OpeningSessionEvents: opening,
	})
	jobs.RunPlan(ctx, jobs.RunRequest{
		Workspace: ws, Config: jobs.SchedulerConfig{MaxParallel: 1, NoCache: true},
		Renderer:             output.WithoutSessionArtifact(output.NewRenderer(output.Config{Output: "jsonl", Command: "build", Out: &bytes.Buffer{}})),
		OpeningSessionEvents: opening,
	})

	var types []string
	for _, line := range strings.Split(strings.TrimSpace(live.String()), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if kind, ok := record.Event["type"].(string); ok {
			types = append(types, kind)
		}
	}
	if want := []string{"scheduler:parallel", workspace.ImpactTraceRecordType, "second:record"}; !reflect.DeepEqual(types, want) {
		t.Errorf("session event order = %v, want %v", types, want)
	}
	artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatalf("read machine artifact: %v", err)
	}
	if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
		t.Fatalf("stream violations: %+v\nlive=%s", violations, live.Bytes())
	}
}
