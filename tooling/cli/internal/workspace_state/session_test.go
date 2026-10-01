package workspace_state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/cli"
)

func TestNewSession_CreatesDirectory(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	defer sess.Close()

	if sess.ID == "" {
		t.Error("session ID is empty")
	}

	// Check directory exists
	if _, err := os.Stat(sess.Dir()); os.IsNotExist(err) {
		t.Errorf("session dir does not exist: %s", sess.Dir())
	}

	// Check events.jsonl was created
	eventsPath := filepath.Join(sess.Dir(), "events.jsonl")
	if _, err := os.Stat(eventsPath); os.IsNotExist(err) {
		t.Errorf("events.jsonl does not exist: %s", eventsPath)
	}
}

func TestSession_IDFormat(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	defer sess.Close()

	// Format: YYYYMMDD-HHMMSS-random
	parts := strings.Split(sess.ID, "-")
	if len(parts) != 3 {
		t.Errorf("expected 3 parts in session ID, got %d: %s", len(parts), sess.ID)
	}
	if len(parts[0]) != 8 {
		t.Errorf("expected 8-char date, got %d: %s", len(parts[0]), parts[0])
	}
	if len(parts[1]) != 6 {
		t.Errorf("expected 6-char time, got %d: %s", len(parts[1]), parts[1])
	}
	if len(parts[2]) != 6 {
		t.Errorf("expected 6-char random hex, got %d: %s", len(parts[2]), parts[2])
	}
}

// TestSession_EventStreamIsTheRecordedFile pins the recorder half of the contract: the
// session is its stream's only producer, both writers put exactly the record and
// one LF on disk, and FinalizeV2 sets the stream's final marker before
// session.json exists.
func TestSession_EventStreamIsTheRecordedFile(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := sess.Events().Subscribe("recorder-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	machine := []byte(`{"protocolVersion":2,"record":"task:start"}`)
	if err := sess.AppendMachineOutputLine(machine); err != nil {
		t.Fatal(err)
	}
	event := SessionEvent{Time: "2026-09-17T12:00:00Z", Type: "job:end", JobKey: "proj:build"}
	if err := sess.AppendEvent(event); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(event)
	want := append(append(append(append([]byte(nil), machine...), '\n'), legacy...), '\n')

	if _, final := sess.Events().Extent(); final {
		t.Fatal("the stream was final before the session finalized")
	}
	if err := sess.FinalizeV2(&cli.SessionFile{Commands: []string{"build"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	end, final := sess.Events().Extent()
	if !final || end.Offset != int64(len(want)) || end.Records != 2 {
		t.Fatalf("stream extent = %+v final=%v, want %d bytes / 2 records, final", end, final, len(want))
	}
	got, err := os.ReadFile(filepath.Join(sess.Dir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("events.jsonl = %q, want %q", got, want)
	}
	if err := sess.AppendMachineOutputLine([]byte(`{"late":true}`)); err != nil {
		t.Fatalf("a record after the final marker = %v, want a silent no-op", err)
	}
	if data, _ := os.ReadFile(filepath.Join(sess.Dir(), "events.jsonl")); string(data) != string(want) {
		t.Fatal("a record was appended after the final marker")
	}
}

func TestSession_AppendEvent(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	// Append multiple events
	for i := 0; i < 3; i++ {
		err := sess.AppendEvent(SessionEvent{
			Type:   "job:end",
			JobKey: "proj:build",
			Data:   map[string]any{"status": "success", "index": i},
		})
		if err != nil {
			t.Fatalf("AppendEvent %d failed: %v", i, err)
		}
	}

	sess.Close()

	// Read events file and verify
	data, err := os.ReadFile(filepath.Join(sess.Dir(), "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 event lines, got %d", len(lines))
	}

	// Verify each line is valid JSON
	for i, line := range lines {
		var event SessionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i, err)
		}
		if event.Type != "job:end" {
			t.Errorf("line %d type = %q, want job:end", i, event.Type)
		}
		if event.Time == "" {
			t.Errorf("line %d time is empty", i)
		}
	}
}

// The recording writers, version 2 — the only ones left after a later change
// deleted WritePlan/Finalize. What these pin is the split the writers own: the
// CALLER supplies the document, and the session stamps exactly the members it
// alone knows (id, start/end times, the run's wall duration, the git block).
// The reader's v1 fallback is pinned in internal/commands, on documents already
// on disk.

func TestSession_WritePlanV2(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	defer sess.Close()

	snapshot := &cli.SessionPlanFile{
		ProtocolVersion: cli.ResultProtocolVersion,
		// Deliberately wrong: the writer owns the session id and must overwrite
		// whatever the caller passed, or a recorded plan could name another run.
		SessionID: "not-this-session",
		Commands:  []string{"build", "test"},
		Tasks: []cli.PlannedTask{
			{
				Identity: cli.TaskIdentity{
					Key:      "/proj:build~transpile",
					Scope:    cli.TaskScopeProject,
					Project:  cli.ProjectIdentity{ID: "/proj", Name: "proj"},
					Task:     cli.TaskRef{Name: "build~transpile", Command: "build", Kind: "transpile"},
					Provider: cli.ProviderIdentity{Extension: "@putnami/typescript"},
				},
				Cache: true,
			},
			{
				Identity: cli.TaskIdentity{
					Key:      "/proj:test~run",
					Scope:    cli.TaskScopeProject,
					Project:  cli.ProjectIdentity{ID: "/proj", Name: "proj"},
					Task:     cli.TaskRef{Name: "test~run", Command: "test", Kind: "test-run"},
					Provider: cli.ProviderIdentity{Extension: "@putnami/typescript"},
				},
				DependsOn: []string{"/proj:build~transpile"},
				After:     []string{"/proj:lint~format"},
			},
		},
	}

	if err := sess.WritePlanV2(snapshot); err != nil {
		t.Fatalf("WritePlanV2 failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sess.Dir(), "plan.json"))
	if err != nil {
		t.Fatalf("read plan.json: %v", err)
	}
	if violations := cli.ValidateDocument(cli.DocumentSessionPlanFile, data); len(violations) != 0 {
		t.Fatalf("recorded plan violates the contract: %v\n%s", violations, data)
	}

	var parsed cli.SessionPlanFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse plan.json: %v", err)
	}
	if parsed.ProtocolVersion != cli.ResultProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", parsed.ProtocolVersion, cli.ResultProtocolVersion)
	}
	if parsed.SessionID != sess.ID {
		t.Errorf("sessionId = %q, want the writing session's %q", parsed.SessionID, sess.ID)
	}
	if len(parsed.Tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2", len(parsed.Tasks))
	}
	if len(parsed.Tasks[1].After) != 1 || parsed.Tasks[1].After[0] != "/proj:lint~format" {
		t.Errorf("tasks[1].after = %v, want the serialize edge", parsed.Tasks[1].After)
	}
}

// TestSession_WritePlanV2NilIsANoOp keeps a caller with nothing to record from
// truncating a plan someone else wrote.
func TestSession_WritePlanV2NilIsANoOp(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	defer sess.Close()

	if err := sess.WritePlanV2(nil); err != nil {
		t.Fatalf("WritePlanV2(nil): %v", err)
	}
	if _, err := os.Stat(filepath.Join(sess.Dir(), "plan.json")); !os.IsNotExist(err) {
		t.Errorf("plan.json exists after a nil write: %v", err)
	}
}

func TestSession_FinalizeV2(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	meta := &cli.SessionFile{
		Commands: []string{"build"},
		Run: cli.RunSummary{
			Outcome:  cli.RunOutcomeFailure,
			ExitCode: cli.ExitFailure,
			Counts:   cli.RunCounts{Total: 7, Succeeded: 3, Failed: 1, Skipped: 3},
			Reuse:    cli.RunReuse{LocalCache: 1, Coalesced: 2},
			Failures: []cli.TaskFailure{{
				Identity: cli.TaskIdentity{
					Key:      "/proj:build",
					Scope:    cli.TaskScopeProject,
					Project:  cli.ProjectIdentity{ID: "/proj", Name: "proj"},
					Task:     cli.TaskRef{Name: "build", Command: "build", Kind: "build"},
					Provider: cli.ProviderIdentity{Extension: "@putnami/typescript"},
				},
				Error: cli.ResultError{Code: "failure", Message: "build failed"},
			}},
		},
	}

	if err := sess.FinalizeV2(meta, "", "main"); err != nil {
		t.Fatalf("FinalizeV2 failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sess.Dir(), "session.json"))
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	if violations := cli.ValidateDocument(cli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("recorded session violates the contract: %v\n%s", violations, data)
	}

	var parsed cli.SessionFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse session.json: %v", err)
	}
	if parsed.ProtocolVersion != cli.ResultProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", parsed.ProtocolVersion, cli.ResultProtocolVersion)
	}
	if parsed.SessionID != sess.ID {
		t.Errorf("sessionId = %q, want %q", parsed.SessionID, sess.ID)
	}
	if parsed.StartTime == "" || parsed.EndTime == "" {
		t.Errorf("startTime/endTime = %q/%q, want both stamped", parsed.StartTime, parsed.EndTime)
	}
	// v2 has no top-level duration member: the run summary's is the recorded
	// wall, and the writer owns it.
	if parsed.Run.DurationMs < 0 {
		t.Errorf("run.durationMs = %d, want >= 0", parsed.Run.DurationMs)
	}
	if parsed.Run.Counts.Total != 7 || parsed.Run.Counts.Failed != 1 {
		t.Errorf("run.counts = %+v, want the caller's histogram", parsed.Run.Counts)
	}
	if parsed.Run.Reuse.LocalCache != 1 || parsed.Run.Reuse.Coalesced != 2 {
		t.Errorf("run.reuse = %+v, want reuse counted apart from the verdict", parsed.Run.Reuse)
	}
	if len(parsed.Commands) != 1 || parsed.Commands[0] != "build" {
		t.Errorf("commands = %v, want [build]", parsed.Commands)
	}
}

func TestSession_FinalizeV2IncludesTaskRecords(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	spawn := int64(13)
	meta := &cli.SessionFile{
		Commands: []string{"lint", "test"},
		Run: cli.RunSummary{
			Outcome:  cli.RunOutcomeSuccess,
			ExitCode: cli.ExitSuccess,
			Counts:   cli.RunCounts{Total: 2, Succeeded: 2},
		},
		Tasks: []cli.TaskRecord{
			{
				Identity: cli.TaskIdentity{
					Key:      "/a:lint~format",
					Scope:    cli.TaskScopeProject,
					Project:  cli.ProjectIdentity{ID: "/a", Name: "a"},
					Task:     cli.TaskRef{Name: "lint~format", Command: "lint", Kind: "lint-format"},
					Provider: cli.ProviderIdentity{Extension: "@putnami/typescript"},
				},
				Status:              cli.TaskStatusSuccess,
				Reuse:               cli.TaskReuseNone,
				DurationMs:          40,
				TaskWallMs:          47,
				SpawnToFirstEventMs: spawn,
			},
			{
				Identity: cli.TaskIdentity{
					Key:      "/z:test~test",
					Scope:    cli.TaskScopeProject,
					Project:  cli.ProjectIdentity{ID: "/z", Name: "z"},
					Task:     cli.TaskRef{Name: "test~test", Command: "test", Kind: "test-run"},
					Provider: cli.ProviderIdentity{Extension: "@putnami/typescript"},
				},
				Status:     cli.TaskStatusSuccess,
				Reuse:      cli.TaskReuseLocalCache,
				DurationMs: 88,
				TaskWallMs: 91,
			},
		},
	}

	if err := sess.FinalizeV2(meta, "", ""); err != nil {
		t.Fatalf("FinalizeV2: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(sess.Dir(), "session.json"))
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	if violations := cli.ValidateDocument(cli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("recorded session violates the contract: %v\n%s", violations, data)
	}
	var parsed cli.SessionFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse session.json: %v", err)
	}
	if len(parsed.Tasks) != 2 {
		t.Fatalf("tasks = %#v, want two terminal records", parsed.Tasks)
	}
	if parsed.Tasks[0].Identity.Key != "/a:lint~format" || parsed.Tasks[0].TaskWallMs != 47 {
		t.Errorf("measured task record = %#v", parsed.Tasks[0])
	}
	if parsed.Tasks[0].SpawnToFirstEventMs != spawn {
		t.Errorf("spawn-to-first-event = %d, want %d", parsed.Tasks[0].SpawnToFirstEventMs, spawn)
	}
	// Absent, not zero-valued: omitempty keeps "no subprocess event observed"
	// distinguishable from "observed at 0ms".
	if parsed.Tasks[1].SpawnToFirstEventMs != 0 {
		t.Errorf("eventless task latency = %d, want absent", parsed.Tasks[1].SpawnToFirstEventMs)
	}
	if parsed.Tasks[1].Reuse != cli.TaskReuseLocalCache {
		t.Errorf("reuse = %q, want %q", parsed.Tasks[1].Reuse, cli.TaskReuseLocalCache)
	}
}

func TestSession_FinalizeV2IncludesSchedulerAndCache(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	type reservationRecord struct {
		Job       string `json:"job"`
		CPU       int    `json:"cpu"`
		MemoryMiB int    `json:"memoryMiB"`
	}
	meta := &cli.SessionFile{
		Commands: []string{"build"},
		Run:      cli.RunSummary{Outcome: cli.RunOutcomeSuccess, ExitCode: cli.ExitSuccess},
		Scheduler: map[string]any{
			"parallel": map[string]any{
				"mode":            "auto",
				"workers":         8,
				"cpuCapacity":     6,
				"memoryUsableMiB": 12288,
			},
			"heavyJobRatio": 0.25,
			"resourceReservations": []reservationRecord{{
				Job:       "/app:test~test",
				CPU:       2,
				MemoryMiB: 768,
			}},
		},
		Cache: map[string]any{
			"providerSummaryRestoredBytes": int64(4_200_000),
			"providerSummaryUploadedBytes": int64(2_100_000),
		},
	}
	if err := sess.FinalizeV2(meta, "", ""); err != nil {
		t.Fatalf("FinalizeV2 failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sess.Dir(), "session.json"))
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse session.json: %v", err)
	}
	sched, ok := raw["scheduler"].(map[string]any)
	if !ok {
		t.Fatalf("scheduler missing from session.json: %v", raw["scheduler"])
	}
	parallel, ok := sched["parallel"].(map[string]any)
	if !ok {
		t.Fatalf("scheduler.parallel missing: %v", sched)
	}
	if parallel["mode"] != "auto" {
		t.Errorf("scheduler.parallel.mode = %v, want auto", parallel["mode"])
	}
	if parallel["cpuCapacity"] != float64(6) || parallel["memoryUsableMiB"] != float64(12288) {
		t.Errorf("scheduler.parallel capacity lost: %v", parallel)
	}
	reservations, ok := sched["resourceReservations"].([]any)
	if !ok || len(reservations) != 1 {
		t.Fatalf("scheduler resource reservations missing: %v", sched["resourceReservations"])
	}
	reservationData, err := json.Marshal(reservations[0])
	if err != nil {
		t.Fatalf("marshal scheduler resource reservation: %v", err)
	}
	var reservation reservationRecord
	if err := json.Unmarshal(reservationData, &reservation); err != nil {
		t.Fatalf("decode scheduler resource reservation: %v", err)
	}
	if reservation.Job != "/app:test~test" || reservation.CPU != 2 || reservation.MemoryMiB != 768 {
		t.Errorf("scheduler resource reservation lost: %+v", reservation)
	}
	cacheMeta, ok := raw["cache"].(map[string]any)
	if !ok {
		t.Fatalf("cache missing from session.json: %v", raw["cache"])
	}
	if cacheMeta["providerSummaryRestoredBytes"] != float64(4_200_000) {
		t.Errorf("cache.providerSummaryRestoredBytes = %v, want 4200000", cacheMeta["providerSummaryRestoredBytes"])
	}
}

// TestSession_FinalizeV2OmitsSchedulerAndCacheWhenUnset keeps a local-only run
// from writing "scheduler": null / "cache": null, which a consumer would have to
// tell apart from a real report.
func TestSession_FinalizeV2OmitsSchedulerAndCacheWhenUnset(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	if err := sess.FinalizeV2(&cli.SessionFile{
		Commands: []string{"build"},
		Run:      cli.RunSummary{Outcome: cli.RunOutcomeSuccess, ExitCode: cli.ExitSuccess},
	}, "", ""); err != nil {
		t.Fatalf("FinalizeV2 failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sess.Dir(), "session.json"))
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse session.json: %v", err)
	}
	for _, key := range []string{"scheduler", "cache"} {
		if _, ok := raw[key]; ok {
			t.Errorf("%s key should be omitted when nothing was recorded", key)
		}
	}
}

// TestSession_FinalizeV2RederivesCPUAllocation covers the one member a caller
// derives from a duration this writer then REPLACES. The producer
// (machine.Run.SessionFile) divides the scheduler's wall; FinalizeV2 stamps the
// wider whole-session wall, and the contract ties allocatedMs to whichever
// durationMs the summary ends up stating. Carrying the producer's figure across
// the re-stamp made every finalized session with a cpu block fail
// cli.result.count_mismatch at run.cpu.allocatedMs — invisible to a test that
// validates the document before it is finalized.
func TestSession_FinalizeV2RederivesCPUAllocation(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	// A whole-session wall knowably wider than the scheduler wall below.
	const schedulerWallMs = 5156
	sess.StartTime = time.Now().Add(-(schedulerWallMs + 4) * time.Millisecond)

	const millicores = 10_000 // ten cores
	const producerAllocatedMs = schedulerWallMs * millicores / 1000

	meta := &cli.SessionFile{
		Commands: []string{"build"},
		Run: cli.RunSummary{
			Outcome:    cli.RunOutcomeSuccess,
			ExitCode:   cli.ExitSuccess,
			Counts:     cli.RunCounts{Total: 1, Succeeded: 1},
			DurationMs: schedulerWallMs,
			CPU: &cli.RunCPU{
				AllocatedMillicores: millicores,
				AllocatedSource:     cli.CPUAllocationCgroupQuota,
				AllocatedMs:         producerAllocatedMs,
				ActualMs:            12_000,
				Executions:          3,
			},
		},
	}

	if err := sess.FinalizeV2(meta, "", ""); err != nil {
		t.Fatalf("FinalizeV2 failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(sess.Dir(), "session.json"))
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	if violations := cli.ValidateDocument(cli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("finalized session with a cpu block violates the contract: %v\n%s", violations, data)
	}

	var parsed cli.SessionFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse session.json: %v", err)
	}
	if parsed.Run.CPU == nil {
		t.Fatalf("run.cpu missing from the finalized session: %s", data)
	}
	if parsed.Run.DurationMs <= schedulerWallMs {
		t.Fatalf("run.durationMs = %d, want the wider stamped wall (> %d)", parsed.Run.DurationMs, schedulerWallMs)
	}
	if want := parsed.Run.DurationMs * millicores / 1000; parsed.Run.CPU.AllocatedMs != want {
		t.Errorf("run.cpu.allocatedMs = %d, want %d over the stamped wall", parsed.Run.CPU.AllocatedMs, want)
	}
	if parsed.Run.CPU.AllocatedMs == producerAllocatedMs {
		t.Errorf("run.cpu.allocatedMs kept the producer's figure %d, measured over a wall the session no longer reports", producerAllocatedMs)
	}
	// Everything else the producer measured is left alone: only the derived
	// budget moves with the wall.
	if parsed.Run.CPU.ActualMs != 12_000 || parsed.Run.CPU.Executions != 3 {
		t.Errorf("run.cpu = %+v, want the measured members untouched", *parsed.Run.CPU)
	}
}

func TestSessionStore_CreateAndList(t *testing.T) {
	wsRoot := t.TempDir()
	store := NewSessionStore(wsRoot)

	// Create a few sessions
	sess1, err := store.Create()
	if err != nil {
		t.Fatalf("Create 1 failed: %v", err)
	}
	sess1.Close()

	sess2, err := store.Create()
	if err != nil {
		t.Fatalf("Create 2 failed: %v", err)
	}
	sess2.Close()

	sessions, err := store.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestSessionStore_UpdateLatest(t *testing.T) {
	wsRoot := t.TempDir()
	store := NewSessionStore(wsRoot)

	sess, err := store.Create()
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	sess.Close()

	if err := store.UpdateLatest(sess); err != nil {
		t.Fatalf("UpdateLatest failed: %v", err)
	}

	latestID := store.LatestID()
	if latestID != sess.ID {
		t.Errorf("LatestID = %q, want %q", latestID, sess.ID)
	}

	// A later session replaces the link.
	next, err := store.Create()
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	next.Close()
	if err := store.UpdateLatest(next); err != nil {
		t.Fatalf("UpdateLatest over an existing link failed: %v", err)
	}
	if latestID := store.LatestID(); latestID != next.ID {
		t.Errorf("LatestID after replace = %q, want %q", latestID, next.ID)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "latest")); err != nil {
		t.Errorf("latest does not resolve after replace: %v", err)
	}
}

func TestSessionStore_Prune(t *testing.T) {
	wsRoot := t.TempDir()
	store := NewSessionStore(wsRoot)
	store.maxSessions = 2

	// Create 4 sessions
	for i := 0; i < 4; i++ {
		sess, err := store.Create()
		if err != nil {
			t.Fatalf("Create %d failed: %v", i, err)
		}
		sess.Close()
	}

	if err := store.Prune(); err != nil {
		t.Fatalf("Prune failed: %v", err)
	}

	sessions, err := store.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions after prune, got %d", len(sessions))
	}
}

func TestSessionStore_PrunePreservesLatestTarget(t *testing.T) {
	wsRoot := t.TempDir()
	store := NewSessionStore(wsRoot)
	store.maxSessions = 3

	ids := []string{
		"20260101-000000-oldest",
		"20260101-000001-middle",
		"20260101-000002-newer",
		"20260101-000003-newest",
	}
	for _, id := range ids {
		if err := os.MkdirAll(filepath.Join(store.Root(), id), 0o755); err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
	}
	latest := &Session{ID: ids[0], dir: filepath.Join(store.Root(), ids[0])}
	if err := store.UpdateLatest(latest); err != nil {
		t.Fatalf("UpdateLatest: %v", err)
	}

	if err := store.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	sessions, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{ids[3], ids[2], ids[0]}
	if strings.Join(sessions, ",") != strings.Join(want, ",") {
		t.Fatalf("sessions after prune = %v, want %v", sessions, want)
	}
	if store.LatestID() != ids[0] {
		t.Fatalf("LatestID = %q, want preserved target %q", store.LatestID(), ids[0])
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "latest")); err != nil {
		t.Fatalf("latest symlink is dangling after prune: %v", err)
	}
}

func TestSession_ConcurrentAppendEvent(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}

	const goroutines = 20
	const eventsPerGoroutine = 50

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < eventsPerGoroutine; j++ {
				err := sess.AppendEvent(SessionEvent{
					Type:   "job:event",
					JobKey: fmt.Sprintf("proj-%d:build", id),
					Data:   map[string]any{"step": j},
				})
				if err != nil {
					t.Errorf("AppendEvent goroutine %d step %d: %v", id, j, err)
				}
			}
		}(i)
	}
	wg.Wait()
	sess.Close()

	// Verify all events were written as valid JSONL
	data, err := os.ReadFile(filepath.Join(sess.Dir(), "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	expected := goroutines * eventsPerGoroutine
	if len(lines) != expected {
		t.Errorf("expected %d event lines, got %d", expected, len(lines))
	}

	// Every line must be valid JSON (no interleaving)
	for i, line := range lines {
		var event SessionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Errorf("line %d is not valid JSON: %v (line: %q)", i, err, line[:min(len(line), 100)])
		}
	}
}

func TestSessionStore_PruneNoError_EmptyDir(t *testing.T) {
	wsRoot := t.TempDir()
	store := NewSessionStore(wsRoot)

	// Should not error when sessions dir doesn't exist
	if err := store.Prune(); err != nil {
		t.Errorf("Prune on empty should not error: %v", err)
	}
}

func TestSessionStore_LastBuildSHA_MissingState(t *testing.T) {
	wsRoot := t.TempDir()
	store := NewSessionStore(wsRoot)

	got, err := store.LastBuildSHA("main", []string{"build"}, nil)
	if err != nil {
		t.Fatalf("LastBuildSHA: %v", err)
	}
	if got != "" {
		t.Errorf("LastBuildSHA = %q, want empty", got)
	}
}

func TestSessionStore_RecordSuccessfulBuild_KeyedByBranchAndCommands(t *testing.T) {
	wsRoot := t.TempDir()
	store := NewSessionStore(wsRoot)

	if err := store.RecordSuccessfulBuild("main", []string{"build"}, nil, "sha-main-build-1"); err != nil {
		t.Fatalf("RecordSuccessfulBuild main: %v", err)
	}
	if err := store.RecordSuccessfulBuild("main", []string{"lint"}, nil, "sha-main-lint"); err != nil {
		t.Fatalf("RecordSuccessfulBuild main lint: %v", err)
	}
	if err := store.RecordSuccessfulBuild("feature", []string{"build"}, nil, "sha-feature"); err != nil {
		t.Fatalf("RecordSuccessfulBuild feature: %v", err)
	}
	if err := store.RecordSuccessfulBuild("main", []string{"build"}, nil, "sha-main-build-2"); err != nil {
		t.Fatalf("RecordSuccessfulBuild main update: %v", err)
	}

	mainBuildSHA, err := store.LastBuildSHA("main", []string{"build"}, nil)
	if err != nil {
		t.Fatalf("LastBuildSHA main build: %v", err)
	}
	if mainBuildSHA != "sha-main-build-2" {
		t.Errorf("main build LastBuildSHA = %q, want sha-main-build-2", mainBuildSHA)
	}

	mainLintSHA, err := store.LastBuildSHA("main", []string{"lint"}, nil)
	if err != nil {
		t.Fatalf("LastBuildSHA main lint: %v", err)
	}
	if mainLintSHA != "sha-main-lint" {
		t.Errorf("main lint LastBuildSHA = %q, want sha-main-lint", mainLintSHA)
	}

	featureSHA, err := store.LastBuildSHA("feature", []string{"build"}, nil)
	if err != nil {
		t.Fatalf("LastBuildSHA feature: %v", err)
	}
	if featureSHA != "sha-feature" {
		t.Errorf("feature LastBuildSHA = %q, want sha-feature", featureSHA)
	}

	missingSHA, err := store.LastBuildSHA("feature", []string{"lint"}, nil)
	if err != nil {
		t.Fatalf("LastBuildSHA feature lint: %v", err)
	}
	if missingSHA != "" {
		t.Errorf("feature lint LastBuildSHA = %q, want empty", missingSHA)
	}

	if _, err := os.Stat(filepath.Join(wsRoot, ".putnami", "sessions", "last-build.json")); err != nil {
		t.Errorf("last-build.json not written: %v", err)
	}
}

func TestSessionStore_RecordSuccessfulBuild_KeyedByCommandParams(t *testing.T) {
	wsRoot := t.TempDir()
	store := NewSessionStore(wsRoot)

	linuxParams := map[string]any{"target": "linux/amd64"}
	darwinParams := map[string]any{"target": "darwin/arm64"}

	if err := store.RecordSuccessfulBuild("main", []string{"build"}, linuxParams, "sha-linux"); err != nil {
		t.Fatalf("RecordSuccessfulBuild linux: %v", err)
	}

	gotLinux, err := store.LastBuildSHA("main", []string{"build"}, linuxParams)
	if err != nil {
		t.Fatalf("LastBuildSHA linux: %v", err)
	}
	if gotLinux != "sha-linux" {
		t.Errorf("linux LastBuildSHA = %q, want sha-linux", gotLinux)
	}

	gotDarwin, err := store.LastBuildSHA("main", []string{"build"}, darwinParams)
	if err != nil {
		t.Fatalf("LastBuildSHA darwin: %v", err)
	}
	if gotDarwin != "" {
		t.Errorf("darwin LastBuildSHA = %q, want empty", gotDarwin)
	}
}
