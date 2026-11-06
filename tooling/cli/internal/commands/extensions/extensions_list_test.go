package extensions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

func TestSortedStringKeys(t *testing.T) {
	got := sortedStringKeys(map[string]string{"c": "", "a": "", "b": ""})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtensionsList_TextEmpty(t *testing.T) {
	dir := t.TempDir()
	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsListPrepared(dir, &wsproto.Config{}, "", nil) })
	if err != nil {
		t.Fatalf("ExtensionsListPrepared: %v", err)
	}
	if !strings.Contains(out, "EXTENSION") || !strings.Contains(out, "CONSTRAINT") {
		t.Errorf("output = %q, want table header", out)
	}
}

func TestExtensionsList_TextWithConfigAndLock(t *testing.T) {
	dir := t.TempDir()
	cfg := &wsproto.Config{
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{"@putnami/go": "^1.0.0"}},
	}
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "putnami.lock.json"), map[string]any{
		"version":    4,
		"extensions": map[string]any{"@putnami/go": map[string]any{"version": "1.2.3"}},
	})

	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsListPrepared(dir, cfg, "", nil) })
	if err != nil {
		t.Fatalf("ExtensionsListPrepared: %v", err)
	}
	if !strings.Contains(out, "@putnami/go") {
		t.Errorf("output = %q, want extension name", out)
	}
	if !strings.Contains(out, "1.2.3") {
		t.Errorf("output = %q, want installed version from lock", out)
	}
	if !strings.Contains(out, "config") {
		t.Errorf("output = %q, want config source", out)
	}
}

func TestExtensionsList_JSONLSortedAndShapes(t *testing.T) {
	dir := t.TempDir()
	cfg := &wsproto.Config{
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{
			"@putnami/zeta": "",
			"@putnami/go":   "^1.0.0",
		}},
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return ExtensionsListPrepared(dir, cfg, "jsonl", nil) })
	if err != nil {
		t.Fatalf("ExtensionsListPrepared: %v", err)
	}
	lines := sharedtest.ResultRecords(t, out, "extensions list")
	if len(lines) != 2 {
		t.Fatalf("got %d records, want 2: %s", len(lines), out)
	}
	type entry struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	var first entry
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatalf("parse line: %v", err)
	}
	// Sorted output: @putnami/go before @putnami/zeta.
	if first.Name != "@putnami/go" {
		t.Errorf("first entry = %q, want @putnami/go (sorted)", first.Name)
	}
	if first.Source != "config" {
		t.Errorf("source = %q, want config", first.Source)
	}
}

func TestExtensionsRemove_RequiresName(t *testing.T) {
	err := ExtensionsRemove(t.TempDir(), &wsproto.Config{}, nil)
	if err == nil || !strings.Contains(err.Error(), "extension name required") {
		t.Fatalf("err = %v, want name-required error", err)
	}
}

func TestExtensionsRemove_RemovesLockEntry(t *testing.T) {
	dir := t.TempDir()
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, "putnami.lock.json"), map[string]any{
		"version":    4,
		"extensions": map[string]any{"@putnami/go": map[string]any{"version": "1.0.0"}},
	})

	out, err := sharedtest.CaptureStdout(t, func() error {
		return ExtensionsRemove(dir, &wsproto.Config{}, []string{"@putnami/go"})
	})
	if err != nil {
		t.Fatalf("ExtensionsRemove: %v", err)
	}
	if !strings.Contains(out, "Removed @putnami/go") {
		t.Errorf("output = %q, want removed message", out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "putnami.lock.json"))
	if strings.Contains(string(data), "@putnami/go") {
		t.Errorf("lock file still references removed extension: %s", data)
	}
}

// TestExtensionsList_UnreadableLockIsExplained pins the unreadable-lock notice.
// The listing stays tolerant — it still renders every configured extension — but a
// lock it cannot read makes the INSTALLED column blank, which is the SAME table
// a workspace with nothing installed produces. A lock below the format floor is
// the common way to reach that state, so the reason must appear rather than be
// inferred from a dash.
func TestExtensionsList_UnreadableLockIsExplained(t *testing.T) {
	dir := t.TempDir()
	cfg := &wsproto.Config{
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{"@fake/ext": "^1.0.0"}},
	}
	// belowFloorLock (artifacts_test.go) is written verbatim rather than built
	// from a map: the point is a lock this CLI's reader rejects, so the bytes
	// must not go through the current writer. It pins @fake/ext at 1.0.0 — the
	// version that must NOT surface below.
	sharedtest.WriteLock(t, dir, "putnami.lock.json", belowFloorLock)

	var out string
	var listErr error
	stderr := sharedtest.CaptureStderr(t, func() {
		out, listErr = sharedtest.CaptureStdout(t, func() error { return ExtensionsListPrepared(dir, cfg, "", nil) })
	})
	// Control flow is deliberately unchanged: the command still succeeds and
	// still lists what config declares.
	if listErr != nil {
		t.Fatalf("ExtensionsListPrepared must stay tolerant of an unreadable lock, got: %v", listErr)
	}
	row := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "@fake/ext") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("listing dropped the configured extension:\n%s", out)
	}
	// NAME CONSTRAINT INSTALLED SOURCE — the blank INSTALLED is the symptom the
	// notice explains, and it must still be blank (the outdated lock is not read).
	if fields := strings.Fields(row); len(fields) < 4 || fields[2] != "-" {
		t.Errorf("row = %q, want the INSTALLED column blank (an outdated lock must not be read)", row)
	}
	if !strings.Contains(stderr, "putnami.lock.json") {
		t.Errorf("stderr does not name the lock file:\n%s", stderr)
	}
	if !strings.Contains(stderr, "putnami install") {
		t.Errorf("stderr does not carry the lock error's remedy:\n%s", stderr)
	}
}
