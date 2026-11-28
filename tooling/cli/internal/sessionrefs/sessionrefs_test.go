package sessionrefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	extensionmodel "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/jobs"
	workspacemodel "go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

var (
	testKey    = strings.Repeat("a", 64)
	testDigest = "sha256:" + strings.Repeat("c", 64)
)

// testEntries is a store holding the entries a test names, by key.
type testEntries map[string]*store.TaskEntry

func (e testEntries) ReadReferencedResult(key, result string) *store.EntryResult {
	if entry := e[key]; entry != nil && entry.ResultDigest == result {
		return entry.Result
	}
	return nil
}

type quietRenderer struct{}

func (quietRenderer) Start([]*jobs.ScheduledJob)                             {}
func (quietRenderer) JobStart(*jobs.ScheduledJob)                            {}
func (quietRenderer) JobEvent(*jobs.ScheduledJob, jobs.RawJobEvent)          {}
func (quietRenderer) JobComplete(*jobs.ScheduledJob, *jobs.JobResult)        {}
func (quietRenderer) Finish(map[string]*jobs.JobResult, jobs.SessionOutcome) {}

// testEntry is the task-owned entry of a test task that ran three cases: a
// diagnostic and the task's result event, and result data holding the cases.
func testEntry(t *testing.T) *store.TaskEntry {
	t.Helper()
	const cases = `[{"name":"TestA","suite":"example.com/a","status":"passed","durationMs":12},` +
		`{"name":"TestB","suite":"example.com/a","status":"failed","durationMs":3,"output":"want 1, got 2"},` +
		`{"name":"TestC","suite":"example.com/a","status":"skipped","durationMs":0}]`
	var executed jobs.JobResult
	if err := json.Unmarshal([]byte(`{"status":"success","data":{"testSummary":{"total":3,"passed":1,"failed":1,"skipped":1},"testCases":`+cases+`}}`), &executed); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`{"v":2,"type":"diagnostic","time":"2026-11-27T10:00:00.5Z","severity":"warning","message":"deprecated call"}`,
		`{"v":2,"type":"result","data":{"status":"success","data":{"testSummary":{"total":3,"passed":1,"failed":1,"skipped":1},"testCases":` + cases + `}}}`,
	} {
		event, ok := jobs.ParseRawEvent(line)
		if !ok {
			t.Fatalf("parse runtime event: %s", line)
		}
		executed.Events = append(executed.Events, event)
	}
	return &store.TaskEntry{Key: testKey, Result: cachekey.EntryResultFromJobResult(&executed), ResultDigest: testDigest}
}

// recordHit records, as the scheduler replays it, one task entry served: its
// start, the entry's events marked as the entry's, the artifact the hit
// recovered, and its end. It returns the session's events.jsonl.
func recordHit(t *testing.T, entry *store.TaskEntry, byReference bool) []byte {
	t.Helper()
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer := output.WithSessionRecording(quietRenderer{}, session, false)
	if byReference && !output.RecordReusedTasksByReference(renderer, func([]output.PinnedReference) error { return nil }) {
		t.Fatal("the session recorder does not record by reference")
	}
	job := &jobs.ScheduledJob{
		Project:   &workspacemodel.Project{ID: "/packages/my-app", Name: "my-app", Path: "packages/my-app"},
		Extension: &extensionmodel.ExtensionDescription{Name: "@putnami/go", Path: "/ext"},
		JobDef:    &extensionmodel.JobDefinition{Name: "test", ExtensionName: "@putnami/go"},
	}
	result := cachekey.JobResultFromEntryResult(entry.Result)
	result.EntryRecords = &jobs.EntryRecords{Key: entry.Key, Result: entry.ResultDigest, Events: len(result.Events)}
	result.Events = append(result.Events, cachekey.TaskArtifactEvent(jobs.TaskArtifact{
		ID: cachekey.EvidenceFragmentArtifactID, Name: "Evidence fragments", Kind: "report", Path: cachekey.EvidenceFragmentFilename,
	}))
	result.CacheKey = entry.Key
	result.MarkReuse(jobs.ReuseLocalCache)

	renderer.Start([]*jobs.ScheduledJob{job})
	renderer.JobStart(job)
	for i, event := range result.Events {
		event.FromCacheEntry = i < result.EntryRecords.Events
		renderer.JobEvent(job, event)
	}
	renderer.JobComplete(job, result)
	renderer.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
	session.Close()
	data, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func lines(data []byte) [][]byte {
	return bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
}

// resolve is data as a reader of the session reads it.
func resolve(t *testing.T, entries Entries, data []byte) []byte {
	t.Helper()
	var out []byte
	for _, line := range lines(data) {
		resolved, err := Resolved(entries, line)
		if err != nil {
			t.Fatalf("resolve %s: %v", line, err)
		}
		out = append(out, resolved...)
	}
	return out
}

// kinds names each record of data by its kind, and a task:event by its
// event's type too.
func kinds(t *testing.T, data []byte) []string {
	t.Helper()
	var out []string
	for _, line := range lines(data) {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("record %s: %v", line, err)
		}
		kind := record.Record
		if eventType, ok := record.Event["type"].(string); ok {
			kind += "/" + eventType
		}
		out = append(out, kind)
	}
	return out
}

// normalized is the records of data before its session:end, each as JSON
// without its time, sorted: the two orders a task's events reach a stream in
// are the same records.
func normalized(t *testing.T, data []byte) []string {
	t.Helper()
	var out []string
	for _, line := range lines(data) {
		var record map[string]json.RawMessage
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("record %s: %v", line, err)
		}
		if string(record["record"]) == `"`+protocolcli.RecordSessionEnd+`"` {
			continue
		}
		delete(record, "time")
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(encoded))
	}
	slices.Sort(out)
	return out
}

// TestReadersRebuildTheRecordsAReusedTaskReplayed resolves a stream recorded
// by reference: it holds every record of the same task recorded whole, the
// rebuilt ones right before the task:end, which no longer names the entry,
// and two resolutions read the same bytes.
func TestReadersRebuildTheRecordsAReusedTaskReplayed(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "readers-rebuild-the-replayed-records")
	entry := testEntry(t)
	whole := recordHit(t, entry, false)
	data := recordHit(t, entry, true)
	if !bytes.Contains(data, []byte(`"cacheReference"`)) {
		t.Fatal("the hit was not recorded by reference")
	}

	resolved := resolve(t, testEntries{testKey: entry}, data)
	if got, want := normalized(t, resolved), normalized(t, whole); !reflect.DeepEqual(got, want) {
		t.Fatalf("the resolved stream holds\n%s\nwant the records of the stream recorded whole\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	want := []string{"task:start", "task:event/artifact", "task:event/diagnostic", "task:event/result", "test:case", "test:case", "test:case", "task:end", "session:end"}
	if got := kinds(t, resolved); !slices.Equal(got, want) {
		t.Fatalf("resolved records = %v, want %v", got, want)
	}
	if bytes.Contains(resolved, []byte(`"cacheReference"`)) {
		t.Fatal("a resolved task:end still names its entry")
	}
	for _, line := range lines(resolved) {
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, line); len(violations) != 0 {
			t.Fatalf("resolved record %s: %+v", line, violations)
		}
	}
	if again := resolve(t, testEntries{testKey: entry}, data); !bytes.Equal(again, resolved) {
		t.Fatal("two resolutions of one stream differ")
	}
}

// TestAReferenceThatDoesNotResolveRebuildsNothing reads a reference whose
// entry is gone or whose result changed: nothing is rebuilt, and the task:end
// reads alone as it is, its reference included, so a reader sees which task's
// records are not there.
func TestAReferenceThatDoesNotResolveRebuildsNothing(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "readers-rebuild-the-replayed-records")
	entry := testEntry(t)
	end := lines(recordHit(t, entry, true))[2]

	changed := *entry
	changed.ResultDigest = "sha256:" + strings.Repeat("d", 64)
	for name, entries := range map[string]Entries{
		"no store":       nil,
		"gone":           testEntries{},
		"result changed": testEntries{testKey: &changed},
	} {
		if records, _, err := Resolve(entries, end); !errors.Is(err, ErrUnavailable) || records != nil {
			t.Fatalf("%s: rebuilt %d records (%v), want %v", name, len(records), err, ErrUnavailable)
		}
		got, err := Resolved(entries, end)
		if err != nil || !bytes.Equal(got, append(slices.Clone(end), '\n')) {
			t.Fatalf("%s: read as %s (%v), want the task:end alone as it is", name, got, err)
		}
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, bytes.TrimSuffix(got, []byte{'\n'})); len(violations) != 0 {
			t.Fatalf("%s: %+v", name, violations)
		}
	}
}

// TestASessionResolvesInItsWorkspacesStore names the store a session
// directory's references resolve in: the workspace's own, or none for a
// directory outside a workspace's sessions.
func TestASessionResolvesInItsWorkspacesStore(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "readers-rebuild-the-replayed-records")
	storeRoot := t.TempDir()
	t.Setenv("PUTNAMI_STORE_DIR", storeRoot)
	ws := t.TempDir()
	dir := filepath.Join(ws, ".putnami", "sessions", "20261127-120000-abc123")
	local, ok := Of(dir).(*store.LocalStore)
	if !ok || local.Root() != storeRoot {
		t.Fatalf("the session resolves in %v, want the store at %s", Of(dir), storeRoot)
	}
	if Of(t.TempDir()) != nil {
		t.Fatal("a directory outside a workspace's sessions resolves in a store")
	}
}
