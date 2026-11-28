package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// TestSessionsInspectNamesTheReusedTasksItCannotRebuild inspects a session
// whose reused tasks name a cache entry this CLI cannot rebuild as their
// producer did, recorded by another projection, and one the store no longer
// holds: each task:end shows as recorded, its cacheReference included, and
// inspect says, in text and in its data, which tasks it shows without their
// records and why.
func TestSessionsInspectNamesTheReusedTasksItCannotRebuild(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-reference-this-cli-cannot-rebuild-is-named")
	storeRoot := t.TempDir()
	t.Setenv("PUTNAMI_STORE_DIR", storeRoot)
	key, goneKey := strings.Repeat("e", 64), strings.Repeat("f", 64)
	var result store.EntryResult
	if err := json.Unmarshal([]byte(`{"status":"success","events":[{"v":2,"type":"diagnostic","data":{"type":"diagnostic","severity":"warning","message":"deprecated call"}}]}`), &result); err != nil {
		t.Fatal(err)
	}
	entry, err := store.NewLocalStore(storeRoot).IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
		Key: key, Result: &result, Metadata: &store.EntryMetadata{Extension: "@putnami/go", Task: "build", Project: "a"},
	})
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	gone := &store.TaskEntry{Key: goneKey, Result: &result, ResultDigest: "sha256:" + strings.Repeat("d", 64)}

	identity := func(project string) string {
		return `{"key":"/` + project + `:build","scope":"project","project":{"id":"/` + project + `","name":"` + project + `"},"task":{"name":"build","command":"build","kind":"go-build"},"provider":{"extension":"@putnami/go"}}`
	}
	end := func(project, key string) string {
		return `{"protocolVersion":2,"record":"task:end","time":"2026-11-28T10:00:02Z","identity":` + identity(project) +
			`,"task":{"identity":` + identity(project) + `,"inputDigest":"sha256:` + key + `","status":"success","reuse":"local-cache","exitCode":0,"durationMs":0}}`
	}
	lines := []string{
		withCacheReference(t, end("a", key), key, entry, output.CachedRecordsProjection+1),
		withCacheReference(t, end("b", goneKey), goneKey, gone, output.CachedRecordsProjection),
	}

	root := t.TempDir()
	sessions := workspace_state.NewSessionStore(root)
	const id = "20261128-100000-abc123"
	dir := filepath.Join(sessions.Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSessionDocument(t, dir, "session.json", &protocolcli.SessionFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion, SessionID: id, Commands: []string{"build"},
	})
	if err := os.WriteFile(filepath.Join(dir, sessionstream.EventsFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	events, unresolved, err := readSessionEventsNoting(sessions, id)
	if err != nil || len(events) != 2 || unresolved != (unresolvedReferences{otherCLI: 1, unavailable: 1}) {
		t.Fatalf("read %d events and %+v (%v), want both task:ends as recorded, one per kind", len(events), unresolved, err)
	}
	data, err := json.Marshal(sessionInspectData(&workspace_state.SessionMetadata{ID: id}, nil, events, unresolved))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		UnresolvedReferences struct {
			NotResolvableByThisCLI int `json:"notResolvableByThisCli"`
			Unavailable            int `json:"unavailable"`
		} `json:"unresolvedReferences"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.UnresolvedReferences.NotResolvableByThisCLI != 1 || decoded.UnresolvedReferences.Unavailable != 1 {
		t.Fatalf("inspect data = %s (%v), want the two counts", data, err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return SessionsInspect(root, []string{id}, "") })
	if err != nil || !strings.Contains(out, "1 reused task(s) are not resolvable by this CLI") || !strings.Contains(out, "1 reused task(s) name a cache entry the store no longer holds") {
		t.Fatalf("inspect printed (%v)\n%s\nwant a note for each reused task it shows without its records", err, out)
	}

	whole, err := json.Marshal(sessionInspectData(&workspace_state.SessionMetadata{ID: id}, nil, events, unresolvedReferences{}))
	if err != nil || strings.Contains(string(whole), "unresolvedReferences") {
		t.Fatalf("a session whose references all resolve reports unresolved ones: %s", whole)
	}
}
