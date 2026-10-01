package jobs

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// testCasesResult builds the JobResult a solo run produces from one runtime
// result event, so the decode runs on the JSON-decoded shape a subprocess
// really delivers.
func testCasesResult(t *testing.T, status, data string) *JobResult {
	t.Helper()
	event := parseTestEvent(t, `{"v":2,"type":"result","data":{"status":"`+status+`","data":`+data+`}}`)
	result := ExtractResult(event.Data)
	result.Events = []RawJobEvent{event}
	return result
}

func TestTaskResultOf_DecodesBoundedTestCasesInRunnerOrder(t *testing.T) {
	job := canonicalTestJob("a", "test", "test-exec")
	result := testCasesResult(t, "FAILED", `{
		"testSummary": {"total": 3, "passed": 1, "failed": 1, "skipped": 1},
		"testCases": [
			{"name": "TestA", "suite": "example.com/a", "status": "passed", "durationMs": 4, "output": "passing noise"},
			{"name": "TestB/sub", "suite": "example.com/a", "status": "failed", "durationMs": 7, "output": "want 1, got 2", "file": "a/b_test.go", "line": 12},
			{"name": "TestC", "suite": "example.com/a", "status": "skipped", "output": "needs docker"}
		]
	}`)

	task := TaskResultOf(job, result)
	want := []protocolcli.TestCase{
		{Name: "TestA", Suite: "example.com/a", Status: "passed", DurationMs: 4},
		{Name: "TestB/sub", Suite: "example.com/a", Status: "failed", DurationMs: 7, Output: "want 1, got 2", File: "a/b_test.go", Line: 12},
		{Name: "TestC", Suite: "example.com/a", Status: "skipped", Output: "needs docker"},
	}
	if !reflect.DeepEqual(task.TestCases, want) {
		t.Fatalf("test cases = %+v\nwant %+v", task.TestCases, want)
	}
	if task.TestCasesDropped != 0 {
		t.Fatalf("dropped = %d, want 0", task.TestCasesDropped)
	}
	if task.Status != TaskStatusFailed || task.Tests == nil || task.Tests.Total != 3 {
		t.Fatalf("the verdict or the summary moved: status=%s tests=%+v", task.Status, task.Tests)
	}
	if _, still := result.Data["testCases"]; !still {
		t.Fatal("decoding mutated the result payload the cache entry persists")
	}
}

// TestTaskResultOf_DecodesTestCasesBesideCanonicalRecords pins the batch and
// warm-cache path: the member result carries typed records AND its slice of
// the wire, and the cases come from the wire.
func TestTaskResultOf_DecodesTestCasesBesideCanonicalRecords(t *testing.T) {
	job := canonicalTestJob("a", "test", "test-exec")
	result := testCasesResult(t, "OK", `{"testCases": [{"name": "TestA", "suite": "example.com/a", "status": "passed", "durationMs": 3}]}`)
	result.Canonical = &TaskResult{Metrics: []TaskMetric{{Name: "tests", Value: 1, Unit: "count"}}}
	task := TaskResultOf(job, result)
	if len(task.TestCases) != 1 || task.TestCases[0].Name != "TestA" || len(task.Metrics) != 1 {
		t.Fatalf("canonical member lost its cases or records: %+v", task)
	}
}

func TestTaskResultOf_CountsEveryReportedCaseWithoutARecord(t *testing.T) {
	job := canonicalTestJob("a", "test", "test-exec")
	cases := make([]string, 0, 1005)
	for i := range 1005 {
		cases = append(cases, fmt.Sprintf(`{"name":"TestP%04d","suite":"example.com/a","status":"passed"}`, i))
	}
	result := testCasesResult(t, "OK", `{"testCasesDropped": 3, "testCases": [`+strings.Join(cases, ",")+`]}`)

	task := TaskResultOf(job, result)
	if len(task.TestCases) != protocolcli.TestCaseMaxPerTask {
		t.Fatalf("kept %d cases, want the contract bound %d", len(task.TestCases), protocolcli.TestCaseMaxPerTask)
	}
	if task.TestCasesDropped != 8 {
		t.Fatalf("dropped = %d, want 3 from the producer + 5 over the bound", task.TestCasesDropped)
	}
	if task.TestCases[0].Name != "TestP0000" || task.TestCases[999].Name != "TestP0999" {
		t.Fatalf("kept cases lost runner order: first=%s last=%s", task.TestCases[0].Name, task.TestCases[999].Name)
	}
}

func TestTaskResultOf_MalformedTestCasesNeverFailTheTask(t *testing.T) {
	job := canonicalTestJob("a", "test", "test-exec")
	for name, tc := range map[string]struct {
		data        string
		wantKept    int
		wantDropped int
	}{
		"not an array counts nothing": {data: `{"testCases": "TestA passed"}`},
		"object counts nothing":       {data: `{"testCases": {"name": "TestA"}}`},
		"null counts nothing":         {data: `{"testCases": null}`},
		"each bad entry counts once": {
			data: `{"testCases": [
				{"name": "TestA", "suite": "s", "status": "passed"},
				"TestB",
				{"name": 5, "suite": "s", "status": "passed"},
				null,
				{"name": "TestC", "suite": "s", "status": "flaky"},
				{"name": "", "suite": "s", "status": "failed"}
			]}`,
			wantKept:    1,
			wantDropped: 5,
		},
		"negative producer count":     {data: `{"testCasesDropped": -1}`},
		"fractional producer count":   {data: `{"testCasesDropped": 2.5}`},
		"string producer count":       {data: `{"testCasesDropped": "3"}`},
		"out-of-range producer count": {data: `{"testCasesDropped": 1e12}`},
		"producer count alone":        {data: `{"testCasesDropped": 4}`, wantDropped: 4},
	} {
		t.Run(name, func(t *testing.T) {
			task := TaskResultOf(job, testCasesResult(t, "OK", tc.data))
			if task.Status != TaskStatusSuccess {
				t.Fatalf("a malformed optional payload changed the verdict to %s", task.Status)
			}
			if len(task.TestCases) != tc.wantKept || task.TestCasesDropped != tc.wantDropped {
				t.Fatalf("kept=%d dropped=%d, want kept=%d dropped=%d",
					len(task.TestCases), task.TestCasesDropped, tc.wantKept, tc.wantDropped)
			}
			if tc.wantKept == 0 && task.TestCases != nil {
				t.Fatalf("no kept case must read as nil, got %#v", task.TestCases)
			}
		})
	}
}

// TestTaskSummaryOf_SkipsTestCases keeps the decode off the scheduler's
// counting projection, which reads no record at all.
func TestTaskSummaryOf_SkipsTestCases(t *testing.T) {
	job := canonicalTestJob("a", "test", "test-exec")
	result := testCasesResult(t, "OK", `{"testCasesDropped": 2, "testCases": [{"name": "TestA", "suite": "s", "status": "passed"}]}`)
	if task := TaskSummaryOf(job, result); task.TestCases != nil || task.TestCasesDropped != 0 {
		t.Fatalf("the counting projection decoded test cases: %+v", task)
	}
}

// TestTaskResultOf_BoundsHoldAfterSanitization pins that a decoded case holds
// its bounds as the stream writes it: the sanitizer turns a control byte into
// the 3-byte U+FFFD and empties an escape-only string, so a case bounded before
// sanitization could break the output bound or carry an empty name.
func TestTaskResultOf_BoundsHoldAfterSanitization(t *testing.T) {
	job := canonicalTestJob("a", "test", "test-exec")
	secret := "gh" + "p_" + strings.Repeat("b", 36)
	cases, err := json.Marshal([]protocolcli.TestCase{
		{Name: "TestBell", Suite: "s", Status: "failed", Output: strings.Repeat("\a", protocolcli.TestCaseMaxOutputBytes)},
		{Name: "\x1b[31m\x1b[0m", Suite: "s", Status: "failed", Output: "colored name"},
		{Name: "TestSecret", Suite: "s", Status: "failed", Output: "token " + secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := testCasesResult(t, "FAILED", `{"testCases": `+string(cases)+`}`)

	task := TaskResultOf(job, result)
	if len(task.TestCases) != 2 || task.TestCasesDropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want the escape-only name dropped and counted", len(task.TestCases), task.TestCasesDropped)
	}
	bell := task.TestCases[0]
	if len(bell.Output) > protocolcli.TestCaseMaxOutputBytes || !bell.OutputTruncated {
		t.Fatalf("sanitized output is %d bytes (truncated=%v), want at most %d and truncated",
			len(bell.Output), bell.OutputTruncated, protocolcli.TestCaseMaxOutputBytes)
	}
	if again := protocolcli.SanitizeMachineOutputString(bell.Output); again != bell.Output {
		t.Fatal("the record sanitizer changes a sanitized, bounded output again")
	}
	if strings.Contains(task.TestCases[1].Output, secret) {
		t.Fatal("a secret in a failure output survived the decode")
	}
}
