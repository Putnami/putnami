package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/sessionrefs"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// referenceFixtureScript is a cached task that reports one diagnostic, which
// its cache entry keeps, and writes its declared output.
const referenceFixtureScript = `#!/bin/sh
echo run >> "$1"
printf '%s\n' '{"v":2,"type":"diagnostic","severity":"warning","message":"from the run"}'
: > out.txt
`

// referenceFixture writes a workspace whose app project builds with one
// cached task that reports a diagnostic, and returns its root.
func referenceFixture(t *testing.T) string {
	t.Helper()
	root := rootWithoutRepository(t)
	tools := t.TempDir()
	script := filepath.Join(tools, "log.sh")
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(script, []byte(referenceFixtureScript), 0o755)
	enabled := true
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: "@test/log", Version: "1.0.0", CLIContract: 4,
		Commands: map[string]extensionproto.CommandDefinition{
			"build": {Run: []extensionproto.PipelineStep{{ID: "build", Task: "log"}}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"log": {
				Kind: "command", Command: script, Args: []string{filepath.Join(tools, "runs")}, Cwd: "{projectRoot}",
				Inputs: map[string]extensionproto.TaskInputPort{"sources": {From: "project", Files: []string{"src.txt"}}},
				Cache:  &extensionproto.TaskCachePolicy{Enabled: &enabled},
				Declares: &extensionproto.TaskDeclaration{Outputs: map[string]extensionproto.DeclaredOutput{
					"out": {Kind: extensionproto.OutputKindFile, Root: extensionproto.OutputRootProject, Path: "out.txt"},
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "putnami.workspace.json"), []byte(`{"name":"by-reference","includes":["app","extension"]}`), 0o644)
	write(filepath.Join(root, "app", "putnami.json"), []byte(`{"name":"app","extensions":["/extension"]}`), 0o644)
	write(filepath.Join(root, "app", "src.txt"), []byte("source\n"), 0o644)
	write(filepath.Join(root, "extension", "putnami.json"), []byte(`{"name":"@test/log"}`), 0o644)
	write(filepath.Join(root, "extension", "putnami.extension.json"), manifest, 0o644)
	workspace.InvalidateLoadCache(root)
	return root
}

// taskRecords is the type and message of every task:event record of data, a
// session stream, and the key of the entry a task:end names, or "".
func taskRecords(t *testing.T, data []byte) ([]string, string) {
	t.Helper()
	var events []string
	key := ""
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		var record struct {
			Record string          `json:"record"`
			Event  json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("record %s: %v", line, err)
		}
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, line); len(violations) != 0 {
			t.Fatalf("record %s: %+v", line, violations)
		}
		if _, reference, ok := output.SplitCacheReference(line); ok {
			key = strings.TrimPrefix(reference.Entry, "sha256:")
		}
		if record.Record == protocolcli.RecordTaskEvent {
			var event struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(record.Event, &event); err != nil {
				t.Fatal(err)
			}
			events = append(events, event.Type+":"+event.Message)
		}
	}
	return events, key
}

// TestARealHitIsRecordedByReferenceAndReadBack runs one cached task three
// times through the engine. The second run, a local hit, declares that its
// session records by reference, writes the task:end naming the entry in place
// of the replayed diagnostic, and pins the entry's result. The third, a hit
// whose environment says its sessions travel, as a run nested in a portable
// execution's would, records every record. A reader of the second session
// reads the third's records, before and after a collection that evicts every
// entry of the store.
func TestARealHitIsRecordedByReferenceAndReadBack(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "readers-rebuild-the-replayed-records")
	if runtime.GOOS == "windows" {
		t.Skip("the fixture task is a POSIX shell script")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("the fixture task runs through /bin/sh")
	}
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv("PUTNAMI_STORE_DIR", storeRoot)
	t.Setenv(scheduler.RecordSessionsWholeEnv, "")
	t.Setenv(protocolcli.SessionReporterEnv, "")
	t.Setenv(protocolcli.LogReporterEnv, "")
	root := referenceFixture(t)
	sessionDir := func(session protocolcli.SessionFile) string {
		return filepath.Join(root, ".putnami", "sessions", session.SessionID)
	}

	code, stderr, first := buildOnce(t, root)
	if code != ExitSuccess || first.Run.Reuse.LocalCache != 0 {
		t.Fatalf("first build = exit %d, %d hits (stderr %q), want a success that runs the task", code, first.Run.Reuse.LocalCache, stderr)
	}
	executed, err := os.ReadFile(filepath.Join(sessionDir(first), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	if events, referenced := taskRecords(t, executed); referenced != "" || !slices.Contains(events, "diagnostic:from the run") {
		t.Fatalf("the executed task recorded %v (referenced %q), want its diagnostic and no reference", events, referenced)
	}

	code, stderr, second := buildOnce(t, root)
	if code != ExitSuccess || second.Run.Reuse.LocalCache != 1 {
		t.Fatalf("second build = exit %d, %d hits (stderr %q), want a success served from the cache", code, second.Run.Reuse.LocalCache, stderr)
	}
	dir := sessionDir(second)
	recorded, err := os.ReadFile(filepath.Join(dir, protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	events, key := taskRecords(t, recorded)
	if !sessionrefs.Declared(dir) || key == "" || slices.Contains(events, "diagnostic:from the run") {
		t.Fatalf("the hit recorded %v (declared %v, entry %q), want its task:end naming the entry in place of its diagnostic", events, sessionrefs.Declared(dir), key)
	}
	pins, _ := filepath.Glob(filepath.Join(storeRoot, "pins", "*.json"))
	objects, _ := filepath.Glob(filepath.Join(storeRoot, "pins", "objects", "*.json"))
	if len(pins) != 1 || len(objects) != 1 {
		t.Fatalf("the store holds pins %v and kept results %v, want one of each", pins, objects)
	}

	t.Setenv(scheduler.RecordSessionsWholeEnv, "1")
	code, stderr, third := buildOnce(t, root)
	if code != ExitSuccess || third.Run.Reuse.LocalCache != 1 {
		t.Fatalf("third build = exit %d, %d hits (stderr %q), want a success served from the cache", code, third.Run.Reuse.LocalCache, stderr)
	}
	whole, err := os.ReadFile(filepath.Join(sessionDir(third), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	wantEvents, referenced := taskRecords(t, whole)
	if sessionrefs.Declared(sessionDir(third)) || referenced != "" || !slices.Contains(wantEvents, "diagnostic:from the run") {
		t.Fatalf("a hit whose sessions travel recorded %v (entry %q), want every record", wantEvents, referenced)
	}

	read := func() []string {
		t.Helper()
		entries := sessionrefs.Of(dir)
		var resolved []byte
		for _, line := range bytes.Split(bytes.TrimSuffix(recorded, []byte{'\n'}), []byte{'\n'}) {
			data, err := sessionrefs.Resolved(entries, line)
			if err != nil {
				t.Fatal(err)
			}
			resolved = append(resolved, data...)
		}
		events, key := taskRecords(t, resolved)
		if key != "" {
			t.Fatal("a resolved task:end still names its entry")
		}
		return events
	}
	if got := read(); !slices.Equal(got, wantEvents) {
		t.Fatalf("a reader reads the hit's events as %v, want those of the hit recorded whole %v", got, wantEvents)
	}
	if _, err := store.RunGC([]string{storeRoot}, store.GCOptions{MaxBytes: 1, Now: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if store.NewLocalStore(storeRoot).ReadTaskEntry(key) != nil {
		t.Fatal("collection kept the entry")
	}
	if got := read(); !slices.Equal(got, wantEvents) {
		t.Fatalf("once collection evicted the entry, a reader reads the hit's events as %v, want %v", got, wantEvents)
	}
}
