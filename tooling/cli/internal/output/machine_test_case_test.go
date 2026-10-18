package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// testCaseStream is one recorded run: the live stream a machine renderer
// wrote and the complete events.jsonl artifact.
type testCaseStream struct {
	live     []byte
	artifact []byte
	records  []protocolcli.SessionStreamRecord
	lines    [][]byte
	final    protocolcli.BoundedSessionEndRecord
}

// recordTestCaseStream drives renderer through run, closes the session and
// reads both outputs back. It fails the test when the pair violates the
// whole-stream contract, which recomputes the live selection.
func recordTestCaseStream(t *testing.T, renderer jobs.Renderer, session *workspace_state.Session, live *bytes.Buffer, run func(jobs.Renderer)) testCaseStream {
	t.Helper()
	run(renderer)
	session.Close()
	artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	if live != nil {
		if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
			t.Fatalf("stream violations: %+v", violations)
		}
	}
	records, lines := parseOuterV2Records(t, artifact)
	stream := testCaseStream{artifact: artifact, records: records, lines: lines}
	if live != nil {
		stream.live = append([]byte(nil), live.Bytes()...)
		liveLines := bytes.Split(bytes.TrimSpace(stream.live), []byte{'\n'})
		if err := json.Unmarshal(liveLines[len(liveLines)-1], &stream.final); err != nil {
			t.Fatal(err)
		}
	}
	return stream
}

// assertCasesPrecedeTaskEnd checks that the task named key has exactly the
// named test:case records, in order, immediately before its task:end, and that
// each one is a valid session-stream record.
func assertCasesPrecedeTaskEnd(t *testing.T, stream testCaseStream, key string, names ...string) protocolcli.SessionStreamRecord {
	t.Helper()
	end := -1
	cases := 0
	for i, record := range stream.records {
		if record.Identity == nil || record.Identity.Key != key {
			continue
		}
		switch record.Record {
		case protocolcli.RecordTestCase:
			cases++
		case protocolcli.RecordTaskEnd:
			end = i
		}
	}
	if end < 0 {
		t.Fatalf("%s has no task:end", key)
	}
	if cases != len(names) || end < len(names) {
		t.Fatalf("%s has %d test:case records, want %d", key, cases, len(names))
	}
	for i, name := range names {
		index := end - len(names) + i
		record := stream.records[index]
		if record.Record != protocolcli.RecordTestCase || record.Identity.Key != key || record.TestCase == nil || record.TestCase.Name != name {
			t.Fatalf("record %d before %s's task:end = %s, want its test:case %q", len(names)-i, key, stream.lines[index], name)
		}
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, stream.lines[index]); len(violations) != 0 {
			t.Fatalf("test:case %q violates the contract: %+v\n%s", name, violations, stream.lines[index])
		}
	}
	return stream.records[end]
}

// assertNoTaskEventCarriesTestCases checks that every case travels once: no
// task:event payload, solo or batch, still holds a testCases member.
func assertNoTaskEventCarriesTestCases(t *testing.T, stream testCaseStream) {
	t.Helper()
	for i, record := range stream.records {
		if record.Record == protocolcli.RecordTaskEvent && bytes.Contains(stream.lines[i], []byte(`"testCases"`)) {
			t.Fatalf("task:event still carries test cases: %s", stream.lines[i])
		}
	}
}

// testCaseJob is a test task whose typed identity key equals its plan key.
func testCaseJob(id string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: "/" + id, Name: id, Path: id},
		Extension: &extension.ExtensionDescription{Name: "test-ext", Path: "/ext"},
		JobDef:    &extension.JobDefinition{Name: "test", ExtensionName: "test-ext", CommandName: "test"},
	}
}

func parsedResult(t *testing.T, line string) (jobs.RawJobEvent, *jobs.JobResult) {
	t.Helper()
	event, ok := jobmodel.ParseRawEvent(line)
	if !ok {
		t.Fatalf("runtime event did not parse: %s", line)
	}
	result := jobmodel.ExtractResult(event.Data)
	result.Events = []jobs.RawJobEvent{event}
	return event, result
}

const failedAndSkippedResult = `{"v":2,"type":"result","time":"2026-09-28T10:00:00Z","data":{"status":"FAILED",` +
	`"error":{"message":"1 test failed"},"data":{"testSummary":{"total":2,"passed":0,"failed":1,"skipped":1},"testCases":[` +
	`{"name":"TestFails","suite":"example.com/app","status":"failed","durationMs":5,"output":"want 1, got 2","file":"app/app_test.go","line":9},` +
	`{"name":"TestSkips","suite":"example.com/app","status":"skipped","output":"needs docker"}]}}}`

func TestMachineStreamEmitsTestCasesBeforeTheirTaskEnd(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "verbose"}[verbose], func(t *testing.T) {
			var live bytes.Buffer
			session, err := workspace_state.NewSession(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			renderer := WithSessionRecording(newMachineV2Renderer(&live, "test", true), session, verbose)
			job := testCaseJob("app")
			event, result := parsedResult(t, failedAndSkippedResult)

			stream := recordTestCaseStream(t, renderer, session, &live, func(renderer jobs.Renderer) {
				renderer.Start([]*jobs.ScheduledJob{job})
				renderer.JobStart(job)
				renderer.JobEvent(job, event)
				renderer.JobComplete(job, result)
				renderer.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
			})

			end := assertCasesPrecedeTaskEnd(t, stream, job.Key(), "TestFails", "TestSkips")
			if end.Task.Status != protocolcli.TaskStatusFailed || end.Task.TestCasesDropped != 0 {
				t.Fatalf("task:end = %+v, want failed with no dropped case", end.Task)
			}
			assertNoTaskEventCarriesTestCases(t, stream)
			if !bytes.Contains(stream.artifact, []byte(`"testSummary"`)) {
				t.Fatal("the strip removed the result's testSummary")
			}
			if _, still := result.Data["testCases"]; !still {
				t.Fatal("rendering wrote to the task's result data")
			}

			for _, name := range []string{"TestFails", "TestSkips"} {
				if live := bytes.Contains(stream.live, []byte(`"name":"`+name+`"`)); live != verbose {
					t.Fatalf("%s in the live stream = %v, want %v (debug detail)", name, live, verbose)
				}
			}
		})
	}
}

func TestMachineStreamCountsEveryCaseWithoutARecord(t *testing.T) {
	var live bytes.Buffer
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer := WithSessionRecording(newMachineV2Renderer(&live, "test", true), session, false)
	job := testCaseJob("app")
	cases := make([]string, 0, 1005)
	for i := range 1005 {
		cases = append(cases, fmt.Sprintf(`{"name":"TestP%04d","suite":"example.com/app","status":"passed","durationMs":1}`, i))
	}
	event, result := parsedResult(t, `{"v":2,"type":"result","data":{"status":"OK","data":{"testCasesDropped":3,"testCases":[`+strings.Join(cases, ",")+`]}}}`)

	stream := recordTestCaseStream(t, renderer, session, &live, func(renderer jobs.Renderer) {
		renderer.Start([]*jobs.ScheduledJob{job})
		renderer.JobStart(job)
		renderer.JobEvent(job, event)
		renderer.JobComplete(job, result)
		renderer.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
	})

	names := make([]string, 0, protocolcli.TestCaseMaxPerTask)
	for i := range protocolcli.TestCaseMaxPerTask {
		names = append(names, fmt.Sprintf("TestP%04d", i))
	}
	end := assertCasesPrecedeTaskEnd(t, stream, job.Key(), names...)
	if end.Task.TestCasesDropped != 8 {
		t.Fatalf("task:end testCasesDropped = %d, want 3 from the producer + 5 over the bound", end.Task.TestCasesDropped)
	}
	assertNoTaskEventCarriesTestCases(t, stream)
	if bytes.Contains(stream.live, []byte(`"record":"test:case"`)) {
		t.Fatal("normal live output admitted passed test cases")
	}
	if stream.final.MachineOutput.Elided.Ordinary.Records < protocolcli.TestCaseMaxPerTask {
		t.Fatalf("ordinary elision = %+v, want every passed case counted", stream.final.MachineOutput.Elided.Ordinary)
	}
}

func TestMachineStreamAttributesBatchTestCasesToTheirMember(t *testing.T) {
	var live bytes.Buffer
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer := WithSessionRecording(newMachineV2Renderer(&live, "test", true), session, true)
	a, b := testCaseJob("a"), testCaseJob("b")
	batchEvent, ok := jobmodel.ParseRawEvent(`{"v":2,"type":"result","data":{"status":"OK","data":{"batchResults":[` +
		`{"projectId":"/a","status":"failed","data":{"testCases":[{"name":"TestA","suite":"example.com/a","status":"failed","output":"boom"}]}},` +
		`{"projectId":"/b","status":"success","data":{"testCases":[{"name":"TestB","suite":"example.com/b","status":"passed"}]}}]}}}`)
	if !ok {
		t.Fatal("batch result event did not parse")
	}
	// The members' results as the batch split builds them: the member's slice
	// of the wire as Data, beside its canonical records.
	memberResult := func(status jobs.TaskStatus, data string) *jobs.JobResult {
		result := &jobs.JobResult{Status: string(status), Canonical: &jobs.TaskResult{Status: status}}
		if err := json.Unmarshal([]byte(data), &result.Data); err != nil {
			t.Fatal(err)
		}
		return result
	}
	resultA := memberResult(jobs.TaskStatusFailed, `{"testCases":[{"name":"TestA","suite":"example.com/a","status":"failed","output":"boom"}]}`)
	resultB := memberResult(jobs.TaskStatusSuccess, `{"testCases":[{"name":"TestB","suite":"example.com/b","status":"passed"}]}`)

	stream := recordTestCaseStream(t, renderer, session, &live, func(renderer jobs.Renderer) {
		batch, ok := renderer.(interface {
			BatchJobEvent(*jobs.ScheduledJob, jobs.RawJobEvent)
		})
		if !ok {
			t.Fatalf("renderer %T does not record batch events", renderer)
		}
		renderer.Start([]*jobs.ScheduledJob{a, b})
		renderer.JobStart(a)
		renderer.JobStart(b)
		batch.BatchJobEvent(a, batchEvent)
		renderer.JobComplete(a, resultA)
		renderer.JobComplete(b, resultB)
		renderer.Finish(map[string]*jobs.JobResult{a.Key(): resultA, b.Key(): resultB}, jobs.SessionOutcome{})
	})

	assertCasesPrecedeTaskEnd(t, stream, a.Key(), "TestA")
	assertCasesPrecedeTaskEnd(t, stream, b.Key(), "TestB")
	assertNoTaskEventCarriesTestCases(t, stream)
	if !bytes.Contains(stream.artifact, []byte(`"batchResults"`)) {
		t.Fatal("the -batch result event is missing from the artifact")
	}
}

// TestSessionRecordingEmitsTestCasesBesideAHumanRenderer covers the recorder a
// human renderer runs with: its events.jsonl carries the same records.
func TestSessionRecordingEmitsTestCasesBesideAHumanRenderer(t *testing.T) {
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer := WithSessionRecording(NewTextRenderer(io.Discard, io.Discard, TextRendererConfig{}), session, false)
	if _, recorded := renderer.(*sessionRecordingRenderer); !recorded {
		t.Fatalf("renderer %T is not the session recorder", renderer)
	}
	job := testCaseJob("app")
	event, result := parsedResult(t, failedAndSkippedResult)

	stream := recordTestCaseStream(t, renderer, session, nil, func(renderer jobs.Renderer) {
		renderer.Start([]*jobs.ScheduledJob{job})
		renderer.JobStart(job)
		renderer.JobEvent(job, event)
		renderer.JobComplete(job, result)
		renderer.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
	})

	assertCasesPrecedeTaskEnd(t, stream, job.Key(), "TestFails", "TestSkips")
	assertNoTaskEventCarriesTestCases(t, stream)
	for i, line := range stream.lines {
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, line); len(violations) != 0 &&
			stream.records[i].Record != protocolcli.RecordSessionEnd {
			t.Fatalf("artifact record %d violates the contract: %+v\n%s", i, violations, line)
		}
	}
}
