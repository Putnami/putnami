package lifecycle

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// readJSONArray reads a JSON file and returns the named field as a []string.
func readJSONArray(t *testing.T, path, field string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	arr, _ := raw[field].([]any)
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestAddProjectToWorkspaceConfig(t *testing.T) {
	tests := []struct {
		name      string
		initial   string
		add       string
		wantField string
		wantList  []string
	}{
		{
			name:      "adds to existing includes",
			initial:   `{"includes":["packages/a"]}`,
			add:       "packages/b",
			wantField: "includes",
			wantList:  []string{"packages/a", "packages/b"},
		},
		{
			name:      "uses projects field when includes absent",
			initial:   `{"projects":["packages/a"]}`,
			add:       "packages/b",
			wantField: "projects",
			wantList:  []string{"packages/a", "packages/b"},
		},
		{
			name:      "dedupes an already-listed path",
			initial:   `{"includes":["packages/a"]}`,
			add:       "packages/a",
			wantField: "includes",
			wantList:  []string{"packages/a"},
		},
		{
			name:      "creates includes when neither field present",
			initial:   `{"name":"ws"}`,
			add:       "packages/a",
			wantField: "includes",
			wantList:  []string{"packages/a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, wsproto.WorkspaceConfigFilename)
			if err := os.WriteFile(cfgPath, []byte(tt.initial), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}

			if err := addProjectToWorkspaceConfig(dir, tt.add); err != nil {
				t.Fatalf("addProjectToWorkspaceConfig: %v", err)
			}

			got := readJSONArray(t, cfgPath, tt.wantField)
			if len(got) != len(tt.wantList) {
				t.Fatalf("%s = %v, want %v", tt.wantField, got, tt.wantList)
			}
			for i := range got {
				if got[i] != tt.wantList[i] {
					t.Errorf("%s[%d] = %q, want %q", tt.wantField, i, got[i], tt.wantList[i])
				}
			}
		})
	}
}

func TestEnsureWorkspaceExtension(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, wsproto.WorkspaceConfigFilename)
	if err := os.WriteFile(cfgPath, []byte(`{"extensions":["go"]}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Adding a new extension appends it.
	if err := ensureWorkspaceExtension(dir, "ts"); err != nil {
		t.Fatalf("ensureWorkspaceExtension: %v", err)
	}
	if got := readJSONArray(t, cfgPath, "extensions"); len(got) != 2 {
		t.Fatalf("extensions = %v, want [go ts]", got)
	}

	// Adding an already-present extension is a no-op (no duplicate).
	if err := ensureWorkspaceExtension(dir, "ts"); err != nil {
		t.Fatalf("ensureWorkspaceExtension (dup): %v", err)
	}
	if got := readJSONArray(t, cfgPath, "extensions"); len(got) != 2 {
		t.Errorf("extensions after dup = %v, want 2 entries", got)
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns what was
// written. It mirrors captureStdout but for the stderr stream used by
// best-effort diagnostic notes.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	origStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = writer
	defer func() {
		os.Stderr = origStderr
		_ = writer.Close()
		_ = reader.Close()
	}()
	readDone := sharedtest.DrainCapturedStream(reader)
	fn()

	os.Stderr = origStderr
	if err := writer.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	result := <-readDone
	if result.Err != nil {
		t.Fatalf("read stderr: %v", result.Err)
	}
	return string(result.Data)
}

func TestCaptureStderr_DrainsBeyondPipeCapacity(t *testing.T) {
	want := strings.Repeat("e", 256*1024)
	got := captureStderr(t, func() {
		if _, err := io.WriteString(os.Stderr, want); err != nil {
			t.Fatalf("write stderr capacity probe: %v", err)
		}
	})
	if got != want {
		t.Fatalf("captured %d bytes, want %d", len(got), len(want))
	}
}
