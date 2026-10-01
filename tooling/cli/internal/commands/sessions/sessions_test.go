package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

func TestSessionsList(t *testing.T) {
	t.Run("shows empty message when no sessions", func(t *testing.T) {
		dir := t.TempDir()
		err := SessionsList(dir, "")
		if err != nil {
			t.Fatalf("SessionsList() error: %v", err)
		}
	})

	t.Run("lists sessions with metadata", func(t *testing.T) {
		dir := t.TempDir()
		sessDir := filepath.Join(dir, ".putnami", "sessions", "20260101-120000-abc123")
		os.MkdirAll(sessDir, 0o755)

		meta := &workspace_state.SessionMetadata{
			ID:        "20260101-120000-abc123",
			StartTime: "2026-01-01T12:00:00Z",
			EndTime:   "2026-01-01T12:00:05Z",
			Duration:  5000,
			Commands:  []string{"build"},
			Stats: &workspace_state.SessionStats{
				Total:     3,
				Succeeded: 3,
			},
		}
		data, _ := json.MarshalIndent(meta, "", "  ")
		os.WriteFile(filepath.Join(sessDir, "session.json"), data, 0o644)

		err := SessionsList(dir, "")
		if err != nil {
			t.Fatalf("SessionsList() error: %v", err)
		}
	})

	t.Run("outputs jsonl format", func(t *testing.T) {
		dir := t.TempDir()
		sessDir := filepath.Join(dir, ".putnami", "sessions", "20260101-120000-abc123")
		os.MkdirAll(sessDir, 0o755)

		meta := &workspace_state.SessionMetadata{
			ID:        "20260101-120000-abc123",
			StartTime: "2026-01-01T12:00:00Z",
			EndTime:   "2026-01-01T12:00:05Z",
			Duration:  5000,
			Commands:  []string{"build"},
			Stats:     &workspace_state.SessionStats{Total: 1, Succeeded: 1},
		}
		data, _ := json.MarshalIndent(meta, "", "  ")
		os.WriteFile(filepath.Join(sessDir, "session.json"), data, 0o644)

		err := SessionsList(dir, "jsonl")
		if err != nil {
			t.Fatalf("SessionsList() jsonl error: %v", err)
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

		meta := &workspace_state.SessionMetadata{
			ID:        sessID,
			StartTime: "2026-01-01T12:00:00Z",
			EndTime:   "2026-01-01T12:00:05Z",
			Duration:  5000,
			Commands:  []string{"build"},
			Stats: &workspace_state.SessionStats{
				Total:     2,
				Succeeded: 1,
				Failed:    1,
			},
			Git: &workspace_state.SessionGitInfo{
				Branch: "main",
			},
		}
		data, _ := json.MarshalIndent(meta, "", "  ")
		os.WriteFile(filepath.Join(sessDir, "session.json"), data, 0o644)

		plan := &workspace_state.PlanSnapshot{
			SessionID: sessID,
			Commands:  []string{"build"},
			Jobs: []workspace_state.PlanJobEntry{
				{Key: "app:build", Project: "app", Job: "build", Extension: "@putnami/typescript", Cache: true},
			},
		}
		planData, _ := json.MarshalIndent(plan, "", "  ")
		os.WriteFile(filepath.Join(sessDir, "plan.json"), planData, 0o644)

		// Write some events
		events := `{"type":"job:end","jobKey":"app:build","data":{"status":"success","project":"app","job":"build"}}
{"type":"job:end","jobKey":"lib:build","data":{"status":"failed","project":"lib","job":"build","error":"compilation failed"}}
`
		os.WriteFile(filepath.Join(sessDir, "events.jsonl"), []byte(events), 0o644)

		err := SessionsInspect(dir, []string{sessID}, "")
		if err != nil {
			t.Fatalf("SessionsInspect() error: %v", err)
		}
	})

	t.Run("handles latest alias", func(t *testing.T) {
		dir := t.TempDir()
		sessID := "20260101-120000-abc123"
		sessDir := filepath.Join(dir, ".putnami", "sessions", sessID)
		os.MkdirAll(sessDir, 0o755)

		meta := &workspace_state.SessionMetadata{
			ID:        sessID,
			StartTime: "2026-01-01T12:00:00Z",
			EndTime:   "2026-01-01T12:00:05Z",
			Duration:  5000,
			Commands:  []string{"test"},
			Stats:     &workspace_state.SessionStats{Total: 1, Succeeded: 1},
		}
		data, _ := json.MarshalIndent(meta, "", "  ")
		os.WriteFile(filepath.Join(sessDir, "session.json"), data, 0o644)

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
		latency := int64(14)
		meta := &workspace_state.SessionMetadata{
			ID:        sessID,
			StartTime: "2026-01-01T12:00:00Z",
			EndTime:   "2026-01-01T12:00:01Z",
			Duration:  1000,
			Commands:  []string{"lint"},
			Stats:     &workspace_state.SessionStats{Total: 1, Succeeded: 1},
			Jobs: []workspace_state.SessionJobEntry{{
				Key:                 "/app:lint~format",
				Project:             "app",
				Job:                 "lint~format",
				TaskKind:            "lint-format",
				Extension:           "@putnami/typescript",
				Status:              "success",
				Outcome:             "success",
				TaskWallMs:          40,
				SpawnToFirstEventMs: &latency,
			}},
		}
		data, _ := json.MarshalIndent(meta, "", "  ")
		os.WriteFile(filepath.Join(sessDir, "session.json"), data, 0o644)

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
