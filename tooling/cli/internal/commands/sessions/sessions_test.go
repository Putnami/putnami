package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// writeSessionDocument records doc as sessDir/name, the bytes a session
// writes.
func writeSessionDocument(t *testing.T, sessDir, name string, doc any) {
	t.Helper()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSessionsList(t *testing.T) {
	t.Run("shows empty message when no sessions", func(t *testing.T) {
		dir := t.TempDir()
		err := SessionsListRevision(dir, "", "")
		if err != nil {
			t.Fatalf("SessionsListRevision() error: %v", err)
		}
	})

	t.Run("lists sessions with metadata", func(t *testing.T) {
		dir := t.TempDir()
		writeRecordedSession(t, dir, "20260101-120000-abc123", recordedV2(t, "20260101-120000-abc123", "2026-01-01T12:00:00Z", 0, 0))

		err := SessionsListRevision(dir, "", "")
		if err != nil {
			t.Fatalf("SessionsListRevision() error: %v", err)
		}
	})

	t.Run("outputs jsonl format", func(t *testing.T) {
		dir := t.TempDir()
		writeRecordedSession(t, dir, "20260101-120000-abc123", recordedV2(t, "20260101-120000-abc123", "2026-01-01T12:00:00Z", 0, 0))

		out, err := sharedtest.CaptureStdout(t, func() error { return SessionsListRevision(dir, "jsonl", "") })
		if err != nil {
			t.Fatalf("SessionsListRevision() jsonl error: %v", err)
		}
		var rows []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("one session must list as a JSON array, got %q: %v", out, err)
		}
		if len(rows) != 1 || rows[0].ID != "20260101-120000-abc123" {
			t.Fatalf("rows = %v, want the one session", rows)
		}
	})

	// The structured result reads this output as one JSON document: a human
	// message here failed `sessions list --output=json` on a fresh workspace.
	t.Run("lists no session as an empty JSON array", func(t *testing.T) {
		for _, revision := range []string{"", "0123456789abcdef0123456789abcdef01234567"} {
			out, err := sharedtest.CaptureStdout(t, func() error {
				return SessionsListRevision(t.TempDir(), "jsonl", revision)
			})
			if err != nil {
				t.Fatalf("SessionsListRevision(%q) error: %v", revision, err)
			}
			if strings.TrimSpace(out) != "[]" {
				t.Fatalf("SessionsListRevision(%q) = %q, want []", revision, out)
			}
		}
	})
}

func TestSessionsInspect(t *testing.T) {
	t.Run("requires session ID", func(t *testing.T) {
		dir := t.TempDir()
		err := SessionsInspect(dir, nil, "")
		if err == nil {
			t.Fatal("expected error when no session ID given")
		}
	})

	t.Run("errors on unknown session", func(t *testing.T) {
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, ".putnami", "sessions"), 0o755)
		err := SessionsInspect(dir, []string{"nonexistent"}, "")
		if err == nil {
			t.Fatal("expected error for nonexistent session")
		}
	})

	t.Run("inspects a real session", func(t *testing.T) {
		dir := t.TempDir()
		sessID := "20260101-120000-abc123"
		sessDir := filepath.Join(dir, ".putnami", "sessions", sessID)
		os.MkdirAll(sessDir, 0o755)

		app := v2Identity("/app", "app", "build", "build", "@putnami/typescript")
		lib := v2Identity("/lib", "lib", "build", "build", "@putnami/typescript")
		writeSessionDocument(t, sessDir, "session.json", &protocolcli.SessionFile{
			ProtocolVersion: protocolcli.ResultProtocolVersion,
			SessionID:       sessID,
			StartTime:       "2026-01-01T12:00:00Z",
			EndTime:         "2026-01-01T12:00:05Z",
			Commands:        []string{"build"},
			Git:             &protocolcli.SessionGit{Branch: "main"},
			Run: protocolcli.RunSummary{
				Outcome:    protocolcli.RunOutcomeFailure,
				ExitCode:   protocolcli.ExitFailure,
				Counts:     protocolcli.RunCounts{Total: 2, Succeeded: 1, Failed: 1},
				DurationMs: 5000,
			},
		})
		writeSessionDocument(t, sessDir, "plan.json", &protocolcli.SessionPlanFile{
			ProtocolVersion: protocolcli.ResultProtocolVersion,
			SessionID:       sessID,
			Commands:        []string{"build"},
			Tasks:           []protocolcli.PlannedTask{{Identity: app, Cache: true}},
		})

		var events []byte
		for _, task := range []protocolcli.TaskRecord{
			{Identity: app, Status: protocolcli.TaskStatusSuccess, Reuse: protocolcli.TaskReuseNone},
			{Identity: lib, Status: protocolcli.TaskStatusFailed, Reuse: protocolcli.TaskReuseNone,
				Error: &protocolcli.ResultError{Code: "failure", Message: "compilation failed"}},
		} {
			line, err := json.Marshal(workspace_state.SessionEvent{
				ProtocolVersion: protocolcli.ResultProtocolVersion,
				Record:          protocolcli.RecordTaskEnd,
				Time:            "2026-01-01T12:00:04Z",
				Task:            &task,
			})
			if err != nil {
				t.Fatal(err)
			}
			events = append(append(events, line...), '\n')
		}
		os.WriteFile(filepath.Join(sessDir, "events.jsonl"), events, 0o644)

		err := SessionsInspect(dir, []string{sessID}, "")
		if err != nil {
			t.Fatalf("SessionsInspect() error: %v", err)
		}
	})

	t.Run("handles latest alias", func(t *testing.T) {
		dir := t.TempDir()
		sessID := "20260101-120000-abc123"
		sessDir := filepath.Join(dir, ".putnami", "sessions", sessID)
		writeRecordedSession(t, dir, sessID, recordedV2(t, sessID, "2026-01-01T12:00:00Z", 0, 0))

		// Create latest symlink
		latestLink := filepath.Join(dir, ".putnami", "sessions", "latest")
		os.Symlink(sessDir, latestLink)

		err := SessionsInspect(dir, []string{"latest"}, "")
		if err != nil {
			t.Fatalf("SessionsInspect(latest) error: %v", err)
		}
	})

	t.Run("json exposes task economics rows", func(t *testing.T) {
		dir := t.TempDir()
		sessID := "20260101-120000-abc123"
		sessDir := filepath.Join(dir, ".putnami", "sessions", sessID)
		os.MkdirAll(sessDir, 0o755)
		writeSessionDocument(t, sessDir, "session.json", &protocolcli.SessionFile{
			ProtocolVersion: protocolcli.ResultProtocolVersion,
			SessionID:       sessID,
			StartTime:       "2026-01-01T12:00:00Z",
			EndTime:         "2026-01-01T12:00:01Z",
			Commands:        []string{"lint"},
			Run: protocolcli.RunSummary{
				Outcome:    protocolcli.RunOutcomeSuccess,
				Counts:     protocolcli.RunCounts{Total: 1, Succeeded: 1},
				DurationMs: 1000,
			},
			Tasks: []protocolcli.TaskRecord{{
				Identity:            v2Identity("/app", "app", "lint~format", "lint-format", "@putnami/typescript"),
				Status:              protocolcli.TaskStatusSuccess,
				Reuse:               protocolcli.TaskReuseNone,
				TaskWallMs:          40,
				SpawnToFirstEventMs: 14,
			}},
		})

		out, err := sharedtest.CaptureStdout(t, func() error {
			return SessionsInspect(dir, []string{sessID}, "jsonl")
		})
		if err != nil {
			t.Fatalf("SessionsInspect() jsonl error: %v", err)
		}
		var inspection struct {
			Metadata workspace_state.SessionMetadata `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(out), &inspection); err != nil {
			t.Fatalf("decode inspect output: %v", err)
		}
		if len(inspection.Metadata.Jobs) != 1 || inspection.Metadata.Jobs[0].SpawnToFirstEventMs == nil {
			t.Fatalf("inspect metadata jobs = %#v", inspection.Metadata.Jobs)
		}
		if got := *inspection.Metadata.Jobs[0].SpawnToFirstEventMs; got != 14 {
			t.Errorf("spawnToFirstEventMs = %d, want 14", got)
		}
	})
}

func TestFormatDurationMs64(t *testing.T) {
	tests := []struct {
		ms       int64
		expected string
	}{
		{50, "50ms"},
		{500, "500ms"},
		{1500, "1.5s"},
		{65000, "1.1m"},
	}
	for _, tt := range tests {
		got := formatDurationMs64(tt.ms)
		if got != tt.expected {
			t.Errorf("formatDurationMs64(%d) = %q, want %q", tt.ms, got, tt.expected)
		}
	}
}

func TestFormatTimestamp(t *testing.T) {
	ts := "2026-01-15T10:30:45.123456789Z"
	got := formatTimestamp(ts)
	if got != "2026-01-15 10:30:45" {
		t.Errorf("formatTimestamp(%q) = %q, want %q", ts, got, "2026-01-15 10:30:45")
	}

	// Invalid timestamp returns as-is
	got2 := formatTimestamp("not-a-timestamp")
	if got2 != "not-a-timestamp" {
		t.Errorf("formatTimestamp(invalid) = %q, want %q", got2, "not-a-timestamp")
	}
}
