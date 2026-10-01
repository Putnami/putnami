package machine

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func testCaseJob() *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: "/a", Name: "a"},
		Extension: &extension.ExtensionDescription{Name: "test-ext"},
		JobDef:    &extension.JobDefinition{Name: "test", ExtensionName: "test-ext", CommandName: "test"},
	}
}

// resultRecordData is the part of a result task:event these tests read.
type resultRecordData struct {
	Event struct {
		Status string `json:"status"`
		Data   struct {
			TestSummary      json.RawMessage `json:"testSummary"`
			TestCases        json.RawMessage `json:"testCases"`
			TestCasesDropped int             `json:"testCasesDropped"`
			BatchResults     []struct {
				ProjectID string `json:"projectId"`
				Data      struct {
					TestSummary json.RawMessage `json:"testSummary"`
					TestCases   json.RawMessage `json:"testCases"`
				} `json:"data"`
			} `json:"batchResults"`
		} `json:"data"`
	} `json:"event"`
}

func decodeResultRecord(t *testing.T, record protocolcli.SessionStreamRecord) resultRecordData {
	t.Helper()
	var decoded resultRecordData
	if err := json.Unmarshal(encodeJSON(t, record), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// eventOf builds a RawJobEvent whose payload is the JSON object payload, the
// shape the CLI holds after decoding a runtime line.
func eventOf(t *testing.T, eventType, payload string) jobs.RawJobEvent {
	t.Helper()
	event := jobs.RawJobEvent{Type: eventType}
	if err := json.Unmarshal([]byte(payload), &event.Data); err != nil {
		t.Fatal(err)
	}
	return event
}

func encodeJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestTaskEndRecords_CasesPrecedeTheTerminalInRunnerOrder(t *testing.T) {
	identity := identityOfKey("/a:test")
	cases := []protocolcli.TestCase{
		{Name: "TestB/sub", Suite: "example.com/a", Status: "failed", DurationMs: 7, Output: "want 1, got 2", File: "a/b_test.go", Line: 12},
		{Name: "TestC", Suite: "example.com/a", Status: "skipped", Output: "needs docker"},
	}
	task := Task{Identity: identity, Result: jobs.TaskResult{
		Status:           jobs.TaskStatusFailed,
		TestCases:        cases,
		TestCasesDropped: 3,
	}}
	at := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

	records := TaskEndRecords(task, at)
	if len(records) != 3 {
		t.Fatalf("records = %d, want 2 test:case + 1 task:end", len(records))
	}
	for i, want := range cases {
		record := records[i]
		if record.Record != protocolcli.RecordTestCase || record.TestCase == nil || !reflect.DeepEqual(*record.TestCase, want) {
			t.Fatalf("record %d = %+v, want the test:case of %+v", i, record, want)
		}
		if record.Identity == nil || *record.Identity != identity {
			t.Fatalf("record %d identity = %+v, want the task's %+v", i, record.Identity, identity)
		}
		if record.Event != nil || record.Task != nil || record.Run != nil {
			t.Fatalf("record %d carries another variant's member: %+v", i, record)
		}
	}
	end := records[2]
	if end.Record != protocolcli.RecordTaskEnd || end.Task == nil || end.Task.TestCasesDropped != 3 {
		t.Fatalf("last record = %+v, want the task:end with testCasesDropped 3", end)
	}
	for i, record := range records {
		line := encodeJSON(t, record)
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, line); len(violations) != 0 {
			t.Fatalf("record %d violates the contract: %+v\n%s", i, violations, line)
		}
	}

	if only := TaskEndRecords(Task{Identity: identity, Result: jobs.TaskResult{Status: jobs.TaskStatusSuccess}}, at); len(only) != 1 ||
		only[0].Record != protocolcli.RecordTaskEnd || only[0].Task.TestCasesDropped != 0 {
		t.Fatalf("a task without cases closes with %+v, want its task:end alone", only)
	}
}

func TestTaskEvent_StripsResultTestCasesWithoutTouchingTheResult(t *testing.T) {
	job := testCaseJob()
	parsed, ok := jobs.ParseRawEvent(`{"v":2,"type":"result","data":{"status":"FAILED","data":{` +
		`"testSummary":{"total":1,"passed":0,"failed":1,"skipped":0},"testCasesDropped":2,` +
		`"testCases":[{"name":"TestA","suite":"s","status":"failed","output":"boom"}]}}}`)
	if !ok {
		t.Fatal("result event did not parse")
	}
	result := jobs.ExtractResult(parsed.Data)
	// A result the CLI synthesizes (a cache replay, a batch member projection)
	// names its type on the event only.
	synthesized := eventOf(t, jobs.EventTypeResult, `{"status":"OK","data":{"testCases":[{"name":"TestA"}]}}`)

	for name, event := range map[string]jobs.RawJobEvent{"parsed": parsed, "synthesized": synthesized} {
		t.Run(name, func(t *testing.T) {
			before := encodeJSON(t, event.Data)
			record := decodeResultRecord(t, TaskEvent(job, event, time.Now()))
			if record.Event.Data.TestCases != nil {
				t.Fatalf("task:event still carries the cases: %s", record.Event.Data.TestCases)
			}
			if !bytes.Equal(encodeJSON(t, event.Data), before) {
				t.Fatal("the strip wrote to the event payload it was given")
			}
		})
	}

	record := decodeResultRecord(t, TaskEvent(job, parsed, time.Now()))
	if record.Event.Data.TestSummary == nil || record.Event.Data.TestCasesDropped != 2 || record.Event.Status != "FAILED" {
		t.Fatalf("the strip removed more than testCases: %+v", record.Event)
	}
	if _, present := result.Data["testCases"]; !present {
		t.Fatal("the strip wrote to the task's result data")
	}

	log := eventOf(t, jobs.EventTypeLog, `{"type":"log","data":{"testCases":"not a result"}}`)
	if got := TaskEvent(job, log, time.Now()).Event; reflect.ValueOf(got).Pointer() != reflect.ValueOf(log.Data).Pointer() {
		t.Fatal("a non-result event was copied or changed")
	}
}

func TestBatchTaskEvent_StripsEveryMemberTestCasesWithoutTouchingTheWire(t *testing.T) {
	job := testCaseJob()
	parsed, ok := jobs.ParseRawEvent(`{"v":2,"type":"result","data":{"status":"OK","data":{"batchResults":[` +
		`{"projectId":"/a","status":"success","data":{"testSummary":{"total":1,"passed":1,"failed":0,"skipped":0},` +
		`"testCases":[{"name":"TestA","suite":"s","status":"passed"}]}},` +
		`{"projectId":"/b","status":"success","data":{"testSummary":{"total":0,"passed":0,"failed":0,"skipped":0}}}` +
		`]}}}`)
	if !ok {
		t.Fatal("batch result event did not parse")
	}
	wire := encodeJSON(t, parsed.Data)

	record := decodeResultRecord(t, BatchTaskEvent(job, parsed, time.Now()))
	members := record.Event.Data.BatchResults
	if len(members) != 2 || members[0].ProjectID != "/a" || members[1].ProjectID != "/b" {
		t.Fatalf("members = %+v, want /a and /b in wire order", members)
	}
	for _, member := range members {
		if member.Data.TestCases != nil {
			t.Fatalf("the -batch task:event still carries %s's cases: %s", member.ProjectID, member.Data.TestCases)
		}
		if member.Data.TestSummary == nil {
			t.Fatalf("the strip removed %s's testSummary", member.ProjectID)
		}
	}
	if !bytes.Equal(encodeJSON(t, parsed.Data), wire) {
		t.Fatal("the strip wrote to the batch wire the splitter reads")
	}
}
