package output

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

const serveLogReadyLine = `{"v":2,"type":"ready","target":"server","name":"api","endpoints":[{"scheme":"http","host":"127.0.0.1","port":43120}],"durationMs":420}`

func serveLogTestJob() *scheduler.ScheduledJob {
	return &scheduler.ScheduledJob{
		Project:   &workspace.Project{ID: "/api", Name: "api", Path: "api"},
		Extension: &extension.ExtensionDescription{Name: "@putnami/go", Path: "/ext"},
		JobDef:    &extension.JobDefinition{Name: "serve~serve", ExtensionName: "@putnami/go"},
	}
}

func serveLogEvent(t *testing.T, line string) scheduler.RawJobEvent {
	t.Helper()
	event, ok := jobmodel.ParseRawEvent(line)
	if !ok {
		t.Fatalf("parse %s", line)
	}
	return event
}

// serveLogRun renders one serve step that logs logs lines and then claims
// readiness, through renderer recorded into a session, and returns the live
// stream and the session artifact.
func serveLogRun(t *testing.T, renderer scheduler.Renderer, live *bytes.Buffer, logs int) ([]byte, []byte) {
	t.Helper()
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer = WithSessionRecording(renderer, session, false)
	job := serveLogTestJob()
	result := &scheduler.JobResult{Status: string(scheduler.TaskStatusSuccess)}
	renderer.Start([]*scheduler.ScheduledJob{job})
	renderer.JobStart(job)
	for i := 0; i < logs; i++ {
		renderer.JobEvent(job, serveLogEvent(t, `{"v":2,"type":"log","level":"info","message":"api request `+strings.Repeat("detail ", 140)+`"}`))
	}
	renderer.JobEvent(job, serveLogEvent(t, `{"v":2,"type":"log","level":"info","message":"listening on 127.0.0.1:43120"}`))
	renderer.JobEvent(job, serveLogEvent(t, serveLogReadyLine))
	renderer.JobComplete(job, result)
	renderer.Finish(map[string]*scheduler.JobResult{job.Key(): result}, scheduler.SessionOutcome{})
	session.Close()
	artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	return live.Bytes(), artifact
}

// recordShapes reduces a stream to what does not vary between two runs: each
// record's kind and, for a task event, the event's type and message.
func recordShapes(t *testing.T, stream []byte) []string {
	t.Helper()
	var shapes []string
	for _, line := range bytes.Split(bytes.TrimSpace(stream), []byte{'\n'}) {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("parse stream record: %v (line: %q)", err, line)
		}
		shape := record.Record
		if record.Event != nil {
			eventType, _ := record.Event["type"].(string)
			message, _ := record.Event["message"].(string)
			shape += " " + eventType + " " + message
		}
		shapes = append(shapes, shape)
	}
	return shapes
}

// A supervised serve run's stdout is the session stream it would be without the
// serve log, recorded into the session the same way, and the serve log carries
// the workload's output rendered as `putnami serve` renders it in text mode.
func TestServeLogRendererKeepsTheStreamAndRendersTheServeLog(t *testing.T) {
	var live, serveLog, plainLive bytes.Buffer
	supervised := NewRenderer(Config{Output: "jsonl", Command: "serve", Out: &live, NoColor: true, ServeMode: true, ServeLog: &serveLog})
	if _, ok := supervised.(*serveLogRenderer); !ok {
		t.Fatalf("a jsonl run with a serve log renders through %T, want the serve log fan-out", supervised)
	}
	stream, artifact := serveLogRun(t, supervised, &live, 0)
	plain, _ := serveLogRun(t, NewRenderer(Config{Output: "jsonl", Command: "serve", Out: &plainLive, NoColor: true, ServeMode: true}), &plainLive, 0)

	if violations := protocolcli.ValidateSessionStream(stream, artifact); len(violations) != 0 {
		t.Fatalf("the supervised stream breaks the session stream contract: %+v", violations)
	}
	got, want := recordShapes(t, stream), recordShapes(t, plain)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the supervised stream differs from the plain one:\n got %q\nwant %q", got, want)
	}
	text := serveLog.String()
	if !strings.Contains(text, "listening on 127.0.0.1:43120") {
		t.Errorf("the serve log does not carry the workload's log line:\n%s", text)
	}
	if strings.Contains(text, `"record"`) || bytes.Contains(stream, []byte("\n  ")) {
		t.Errorf("a stream leaked into the other:\nserve log:\n%s", text)
	}
	if _, fans := NewRenderer(Config{Output: "json", Out: &bytes.Buffer{}, ServeLog: &serveLog}).(*serveLogRenderer); fans {
		t.Error("an aggregated json run fans out to the serve log, whose envelope is written once at the end")
	}
}

// A workload that logged past the stream's ordinary partition still delivers
// its ready claim live: the claim is the control event a supervisor waits on.
func TestServeLogRendererKeepsAReadyClaimLiveAfterTheOrdinaryPartition(t *testing.T) {
	spectest.Proves(t, "cli/supervised-serve-run", "ready-claims-are-never-elided", "a-ready-claim-uses-the-failure-reserve")
	var live, serveLog bytes.Buffer
	renderer := NewRenderer(Config{Output: "jsonl", Command: "serve", Out: &live, NoColor: true, ServeMode: true, ServeLog: &serveLog})
	stream, artifact := serveLogRun(t, renderer, &live, 900)

	if violations := protocolcli.ValidateSessionStream(stream, artifact); len(violations) != 0 {
		t.Fatalf("bounded stream violations: %+v", violations)
	}
	lines := bytes.Split(bytes.TrimSpace(stream), []byte{'\n'})
	var final protocolcli.BoundedSessionEndRecord
	if err := json.Unmarshal(lines[len(lines)-1], &final); err != nil {
		t.Fatal(err)
	}
	if final.MachineOutput.Elided.Ordinary.Records == 0 {
		t.Fatalf("the flood elided nothing, so it did not spend the ordinary partition: %+v", final.MachineOutput.Elided)
	}
	if !bytes.Contains(stream, []byte(`"type":"ready"`)) || !bytes.Contains(stream, []byte(`"port":43120`)) {
		t.Error("the ready claim was elided from the live stream")
	}
	if got := strings.Count(serveLog.String(), "api request"); got != 900 {
		t.Errorf("the serve log carries %d of the 900 log lines, want every one", got)
	}
}
