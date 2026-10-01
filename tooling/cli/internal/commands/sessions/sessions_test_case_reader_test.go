package sessions

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// recordTestCaseSession writes a session whose one task reported two test
// cases, through the stream records the engine persists. withCases false drops
// the test:case records, leaving the stream a reader saw before they existed.
func recordTestCaseSession(t *testing.T, job *jobs.ScheduledJob, result *jobs.JobResult, withCases bool) (*workspace_state.SessionStore, string) {
	t.Helper()
	store := workspace_state.NewSessionStore(t.TempDir())
	session, err := workspace_state.NewSession(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	planned := []*jobs.ScheduledJob{job}
	results := map[string]*jobs.JobResult{job.Key(): result}
	commands := []string{"test"}
	if err := session.WritePlanV2(machine.SessionPlanFile(session.ID, commands, planned)); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	task := machine.Task{Identity: machine.Identity(job), Result: jobs.TaskResultOf(job, result)}
	ends := machine.TaskEndRecords(task, at)
	if !withCases {
		ends = ends[len(ends)-1:]
	}
	for _, record := range append([]protocolcli.SessionStreamRecord{machine.TaskStart(job, at)}, ends...) {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.AppendMachineOutputLine(line); err != nil {
			t.Fatal(err)
		}
	}

	view := machine.RunFrom(jobs.ReduceRun(planned, results, jobs.SessionOutcome{}), planned, results)
	if err := session.FinalizeV2(view.SessionFile(commands, json.RawMessage(`{"criticalPath":{"durationMs":30}}`), nil), "", ""); err != nil {
		t.Fatal(err)
	}
	return store, filepath.Base(session.Dir())
}

// TestSessionReadersSkipTestCaseRecords proves the sessions readers treat a
// test:case record as a record they do not project: inspect lists task
// terminals and diagnostics exactly as if it were absent, while the reader
// keeps the record whole for the JSONL dump.
func TestSessionReadersSkipTestCaseRecords(t *testing.T) {
	job := &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: "/api", Name: "@acme/api", Path: "api"},
		Extension: &extension.ExtensionDescription{Name: "go"},
		JobDef:    &extension.JobDefinition{Name: "test", CommandName: "test", Cache: true},
	}
	result := &jobs.JobResult{
		Status:   "failed",
		Duration: 30 * time.Millisecond,
		TaskWall: 31 * time.Millisecond,
		Error:    &jobs.JobError{Message: "1 test failed"},
	}
	if err := json.Unmarshal([]byte(`{"testCases":[`+
		`{"name":"TestA","suite":"example.com/api","status":"failed","output":"want 1, got 2"},`+
		`{"name":"TestB","suite":"example.com/api","status":"skipped"}]}`), &result.Data); err != nil {
		t.Fatal(err)
	}
	withStore, withID := recordTestCaseSession(t, job, result, true)
	withoutStore, withoutID := recordTestCaseSession(t, job, result, false)

	with, err := readSessionEvents(withStore, withID)
	if err != nil {
		t.Fatal(err)
	}
	without, err := readSessionEvents(withoutStore, withoutID)
	if err != nil {
		t.Fatal(err)
	}
	var cases []string
	for _, event := range with {
		if event.Record == protocolcli.RecordTestCase {
			if event.TestCase == nil || event.Identity == nil || event.Identity.Key != job.Key() {
				t.Fatalf("test:case read back without its case or task: %+v", event)
			}
			cases = append(cases, event.TestCase.Name)
		}
	}
	if !reflect.DeepEqual(cases, []string{"TestA", "TestB"}) || len(with) != len(without)+2 {
		t.Fatalf("read back test cases %v from %d records, want TestA, TestB beside the %d others", cases, len(with), len(without))
	}

	var terminals []recordedTaskEnd
	for _, event := range with {
		if terminal, ok := taskEndFromSessionEvent(event); ok {
			terminals = append(terminals, terminal)
		}
		if diagnostic, ok := diagnosticFromSessionEvent(event); ok {
			t.Fatalf("a record projected as a diagnostic: %+v", diagnostic)
		}
	}
	if len(terminals) != 1 || terminals[0].key != job.Key() || terminals[0].status != protocolcli.TaskStatusFailed {
		t.Fatalf("task terminals = %+v, want the one failed task:end", terminals)
	}
}
