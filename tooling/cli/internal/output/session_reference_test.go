package output

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

const (
	referenceTestKey    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	referenceTestDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// quietRenderer is a renderer that displays nothing.
type quietRenderer struct{}

func (quietRenderer) Start([]*jobmodel.ScheduledJob)                                 {}
func (quietRenderer) JobStart(*jobmodel.ScheduledJob)                                {}
func (quietRenderer) JobEvent(*jobmodel.ScheduledJob, jobmodel.RawJobEvent)          {}
func (quietRenderer) JobComplete(*jobmodel.ScheduledJob, *jobmodel.JobResult)        {}
func (quietRenderer) Finish(map[string]*jobmodel.JobResult, jobmodel.SessionOutcome) {}

// reusedTestResult is the result a hit on the entry of a test task that ran
// three cases serves: the entry's diagnostic and result event, its result data
// holding the cases, then the artifact event the hit recovered this run.
func reusedTestResult(t *testing.T) *jobmodel.JobResult {
	t.Helper()
	const cases = `[{"name":"TestA","suite":"example.com/a","status":"passed","durationMs":12},` +
		`{"name":"TestB","suite":"example.com/a","status":"failed","durationMs":3,"output":"want 1, got 2"},` +
		`{"name":"TestC","suite":"example.com/a","status":"skipped","durationMs":0}]`
	var result jobmodel.JobResult
	if err := json.Unmarshal([]byte(`{"status":"success","data":{"testSummary":{"total":3,"passed":1,"failed":1,"skipped":1},"testCases":`+cases+`}}`), &result); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`{"v":2,"type":"diagnostic","time":"2026-11-27T10:00:00.5Z","severity":"warning","message":"deprecated call"}`,
		`{"v":2,"type":"result","data":{"status":"success","data":{"testSummary":{"total":3,"passed":1,"failed":1,"skipped":1},"testCases":` + cases + `}}}`,
		`{"v":2,"type":"artifact","id":"putnami-evidence","name":"Evidence fragments","kind":"report","path":"putnami-evidence.jsonl"}`,
	} {
		event, ok := jobmodel.ParseRawEvent(line)
		if !ok {
			t.Fatalf("parse runtime event: %s", line)
		}
		result.Events = append(result.Events, event)
	}
	result.EntryRecords = &jobmodel.EntryRecords{Key: referenceTestKey, Result: referenceTestDigest, Events: 2}
	result.CacheKey = referenceTestKey
	result.MarkReuse(jobmodel.ReuseLocalCache)
	return &result
}

// recordReusedTask records, as the scheduler replays it, one task result
// served: its start, the first marked of its events marked as the entry's,
// and its end. It returns the session's events.jsonl and the results the
// recorder pinned.
func recordReusedTask(t *testing.T, result *jobmodel.JobResult, byReference bool, marked int) ([]byte, []PinnedReference) {
	t.Helper()
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer := WithSessionRecording(quietRenderer{}, session, false)
	var pinned []PinnedReference
	if byReference && !RecordReusedTasksByReference(renderer, func(results []PinnedReference) error { pinned = results; return nil }) {
		t.Fatal("the session recorder does not record by reference")
	}
	job := makeRendererTestJob()
	job.Project.ID = "/packages/my-app"
	renderer.Start([]*scheduler.ScheduledJob{job})
	renderer.JobStart(job)
	for i, event := range result.Events {
		event.FromCacheEntry = i < marked
		renderer.JobEvent(job, event)
	}
	renderer.JobComplete(job, result)
	renderer.Finish(map[string]*scheduler.JobResult{job.Key(): result}, scheduler.SessionOutcome{})
	session.Close()
	data, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	return data, pinned
}

func streamLines(data []byte) [][]byte {
	return bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
}

// recordKinds names each record of data by its kind, and a task:event by its
// event's type too.
func recordKinds(t *testing.T, data []byte) []string {
	t.Helper()
	var kinds []string
	for _, line := range streamLines(data) {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("record %s: %v", line, err)
		}
		kind := record.Record
		if eventType, ok := record.Event["type"].(string); ok {
			kind += "/" + eventType
		}
		kinds = append(kinds, kind)
	}
	return kinds
}

// TestAReusedTaskIsRecordedByItsCacheEntry records a hit by reference: the
// stream holds the task's start, the artifact the hit recovered and its end,
// whose cacheReference, its last member, names the entry, its result, the
// two events and three cases it stands for, the projection that rebuilds them
// and the digest of the records it rebuilds. Every record is a valid one, and
// the recorder pins the entry's result once the session ends.
func TestAReusedTaskIsRecordedByItsCacheEntry(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-reused-task-records-its-cache-entry")
	whole, _ := recordReusedTask(t, reusedTestResult(t), false, 2)
	data, pinned := recordReusedTask(t, reusedTestResult(t), true, 2)

	if got, want := recordKinds(t, data), []string{"task:start", "task:event/artifact", "task:end", "session:end"}; !slices.Equal(got, want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	for _, line := range streamLines(data) {
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, line); len(violations) != 0 {
			t.Fatalf("record %s: %+v", line, violations)
		}
	}
	endLine := streamLines(data)[2]
	var end protocolcli.SessionStreamRecord
	if err := json.Unmarshal(endLine, &end); err != nil {
		t.Fatal(err)
	}
	split, reference, ok := SplitCacheReference(endLine)
	if !ok || bytes.Contains(split, []byte("cacheReference")) || len(protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, split)) != 0 {
		t.Fatalf("the task:end splits as %s (%v), want a valid task:end and the reference it carries", split, ok)
	}
	result := reusedTestResult(t)
	rebuilt, ok := CachedRecords(split, result.Events[:2], jobmodel.TaskResultOf(nil, result).TestCases)
	if !ok {
		t.Fatal("the task:end rebuilds no records")
	}
	want := protocolcli.TaskCacheReference{
		Entry: "sha256:" + referenceTestKey, Result: referenceTestDigest, Events: 2, TestCases: 3,
		Projection: CachedRecordsProjection, Records: CachedRecordsDigest(rebuilt),
	}
	if end.CacheReference == nil || *end.CacheReference != want || *reference != want {
		t.Fatalf("cacheReference = %+v, split %+v, want %+v", end.CacheReference, reference, want)
	}
	if end.Task == nil || end.Task.Reuse != protocolcli.TaskReuseLocalCache || end.Task.InputDigest != "sha256:"+referenceTestKey {
		t.Fatalf("task:end task = %+v, want the reused task keyed on the entry's key", end.Task)
	}
	if !slices.Equal(pinned, []PinnedReference{{Key: referenceTestKey, Result: referenceTestDigest}}) {
		t.Fatalf("pinned = %v, want the entry's key and result", pinned)
	}
	want9 := []string{"task:start", "task:event/diagnostic", "task:event/result", "task:event/artifact", "test:case", "test:case", "test:case", "task:end", "session:end"}
	if got := recordKinds(t, whole); !slices.Equal(got, want9) || len(data) >= len(whole) {
		t.Fatalf("the task recorded whole holds %v in %d bytes, by reference %d bytes", got, len(whole), len(data))
	}
}

// TestASessionWhoseReadersCannotResolveWritesTheRecords records a hit through
// the machine renderer, whose --output=jsonl artifact keeps every record, and
// a hit whose replay marked fewer events than its entry supplied: both are
// recorded whole.
func TestASessionWhoseReadersCannotResolveWritesTheRecords(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-session-whose-readers-cannot-resolve-writes-the-records")
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var live bytes.Buffer
	if RecordReusedTasksByReference(WithSessionRecording(newMachineV2Renderer(&live, "test", true), session, false), func([]PinnedReference) error { return nil }) {
		t.Fatal("the machine renderer's recorder records by reference")
	}
	session.Close()

	partial, pinned := recordReusedTask(t, reusedTestResult(t), true, 1)
	want := []string{"task:start", "task:event/result", "task:event/artifact", "task:event/diagnostic", "test:case", "test:case", "test:case", "task:end", "session:end"}
	if got := recordKinds(t, partial); !slices.Equal(got, want) {
		t.Fatalf("a hit whose replay marked one of two entry events is recorded as %v, want %v", got, want)
	}
	if bytes.Contains(partial, []byte(`"cacheReference"`)) || len(pinned) != 0 {
		t.Fatalf("a hit recorded whole names its entry (pinned %v)", pinned)
	}

	executed := reusedTestResult(t)
	executed.Reuse, executed.EntryRecords = jobmodel.ReuseNone, nil
	data, _ := recordReusedTask(t, executed, true, 0)
	if bytes.Contains(data, []byte(`"cacheReference"`)) || bytes.Count(data, []byte(`"record":"test:case"`)) != 3 {
		t.Fatal("an executed task is not recorded whole")
	}
}

// TestCachedRecordsAreTheRecordsOfATaskRecordedWhole rebuilds, from a
// reused task's end line and its entry's events and cases, the records the
// recorder writes for it whole, in order, each stamped with the end's time
// when it carries none of its own.
func TestCachedRecordsAreTheRecordsOfATaskRecordedWhole(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "readers-rebuild-the-replayed-records")
	result := reusedTestResult(t)
	data, _ := recordReusedTask(t, result, true, 2)
	end, _, ok := SplitCacheReference(streamLines(data)[2])
	if !ok {
		t.Fatal("the task:end carries no reference")
	}
	records, ok := CachedRecords(end, result.Events[:2], jobmodel.TaskResultOf(nil, result).TestCases)
	if !ok || len(records) != 5 {
		t.Fatalf("rebuilt %d records (%v), want two events and three cases", len(records), ok)
	}
	var head struct{ Time string }
	if err := json.Unmarshal(end, &head); err != nil {
		t.Fatal(err)
	}
	for i, line := range records {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		wantTime := head.Time
		if i == 0 {
			wantTime = "2026-11-27T10:00:00.5Z"
		}
		if record.Time != wantTime || record.Identity == nil || record.Identity.Key != "/packages/my-app:build" {
			t.Fatalf("record %d = %s, want the task's identity at %s", i, line, wantTime)
		}
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, line); len(violations) != 0 {
			t.Fatalf("record %s: %+v", line, violations)
		}
	}
	if bytes.Contains(records[1], []byte(`"testCases"`)) {
		t.Fatal("the rebuilt result event carries the cases a test:case record carries")
	}
	if _, ok := CachedRecords(streamLines(data)[0], nil, nil); ok {
		t.Fatal("a task:start rebuilt records")
	}
}
