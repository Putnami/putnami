package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestParseEvent_LogEvent(t *testing.T) {
	data := `{"v":1,"type":"log","level":"info","message":"hello"}`
	e, err := ParseEvent([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if e.V != 1 {
		t.Errorf("V = %d, want 1", e.V)
	}
	if e.Type != EventLog {
		t.Errorf("Type = %q, want log", e.Type)
	}
	if e.Level != "info" {
		t.Errorf("Level = %q, want info", e.Level)
	}
	if e.Message != "hello" {
		t.Errorf("Message = %q, want hello", e.Message)
	}
}

func TestParseEvent_ProgressEvent(t *testing.T) {
	data := `{"v":1,"type":"progress","current":3,"total":10,"message":"building"}`
	e, err := ParseEvent([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != EventProgress {
		t.Errorf("Type = %q, want progress", e.Type)
	}
	if e.Current == nil || *e.Current != 3 {
		t.Errorf("Current = %v, want 3", e.Current)
	}
	if e.Total == nil || *e.Total != 10 {
		t.Errorf("Total = %v, want 10", e.Total)
	}
}

func TestParseEvent_MetricEvent(t *testing.T) {
	data := `{"v":1,"type":"metric","name":"binary-size","value":4096,"unit":"bytes"}`
	e, err := ParseEvent([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "binary-size" {
		t.Errorf("Name = %q", e.Name)
	}
	if e.Value == nil || *e.Value != 4096 {
		t.Errorf("Value = %v, want 4096", e.Value)
	}
	if e.Unit != "bytes" {
		t.Errorf("Unit = %q", e.Unit)
	}
}

func TestParseEvent_PhaseEvent(t *testing.T) {
	data := `{"v":1,"type":"phase","name":"compile","action":"start"}`
	e, err := ParseEvent([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "compile" {
		t.Errorf("Name = %q", e.Name)
	}
	if e.Action == nil || *e.Action != PhaseStart {
		t.Errorf("Action = %v, want start", e.Action)
	}
}

func TestParseEvent_DiagnosticEvent(t *testing.T) {
	data := `{"v":1,"type":"diagnostic","severity":"error","message":"type error","code":"TS2304","location":{"file":"main.ts","line":5,"column":10}}`
	e, err := ParseEvent([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if e.Severity == nil || *e.Severity != SeverityError {
		t.Errorf("Severity = %v, want error", e.Severity)
	}
	if e.Code != "TS2304" {
		t.Errorf("Code = %q", e.Code)
	}
	if e.Location == nil || e.Location.File != "main.ts" || e.Location.Line != 5 {
		t.Errorf("Location = %+v", e.Location)
	}
}

func TestParseEvent_ArtifactEvent(t *testing.T) {
	data := `{"v":1,"type":"artifact","id":"bin","name":"app","kind":"binary","path":"dist/app"}`
	e, err := ParseEvent([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != "bin" {
		t.Errorf("ID = %q", e.ID)
	}
	if e.Kind != "binary" {
		t.Errorf("Kind = %q", e.Kind)
	}
	if e.Path != "dist/app" {
		t.Errorf("Path = %q", e.Path)
	}
}

func TestParseEvent_ResultEvent(t *testing.T) {
	data := `{"v":1,"type":"result","data":{"status":"OK","data":{"key":"val"}}}`
	e, err := ParseEvent([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != EventResult {
		t.Errorf("Type = %q", e.Type)
	}
	var rd ResultData
	if err := json.Unmarshal(e.Data, &rd); err != nil {
		t.Fatal(err)
	}
	if rd.Status != ResultOK {
		t.Errorf("Status = %q", rd.Status)
	}
}

func TestParseEvent_MetaEvent(t *testing.T) {
	data := `{"v":1,"type":"meta","data":{"extension":"@putnami/go","job":"build"}}`
	e, err := ParseEvent([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	var md MetaData
	if err := json.Unmarshal(e.Data, &md); err != nil {
		t.Fatal(err)
	}
	if md.Extension != "@putnami/go" {
		t.Errorf("Extension = %q", md.Extension)
	}
	if md.Job != "build" {
		t.Errorf("Job = %q", md.Job)
	}
}

func TestParseEvent_InvalidJSON(t *testing.T) {
	_, err := ParseEvent([]byte("{invalid"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestDecoder_MultipleEvents(t *testing.T) {
	input := strings.Join([]string{
		`{"v":1,"type":"log","level":"info","message":"start"}`,
		`{"v":1,"type":"log","level":"info","message":"end"}`,
		`{"v":1,"type":"result","data":{"status":"OK"}}`,
	}, "\n")

	events, err := DecodeAll(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	if events[0].Message != "start" {
		t.Errorf("event[0].Message = %q", events[0].Message)
	}
	if events[2].Type != EventResult {
		t.Errorf("event[2].Type = %q", events[2].Type)
	}
}

func TestDecoder_SkipsBlankLines(t *testing.T) {
	input := `{"v":1,"type":"log","level":"info","message":"a"}

{"v":1,"type":"result","data":{"status":"OK"}}
`
	events, err := DecodeAll(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
}

func TestDecoder_Empty(t *testing.T) {
	events, err := DecodeAll(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want 0", len(events))
	}
}

func TestEmitter_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf)

	em.Meta("@putnami/go", "build")
	em.Log(LevelInfo, "compiling")
	em.Phase("compile", PhaseStart, "")
	em.Progress(1, 3, "step 1")
	em.Metric("binary-size", 4096, "bytes")
	em.Diagnostic(SeverityWarning, "unused var", "lint/unused", &SourceLocation{File: "main.go", Line: 5})
	em.Artifact("bin", "app", "binary", "dist/app")
	em.Summary("build complete")
	em.Phase("compile", PhaseEnd, PhaseSuccess)
	em.Result(ResultOK, map[string]any{"key": "val"}, nil)

	events, err := DecodeAll(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 10 {
		t.Fatalf("got %d events, want 10", len(events))
	}

	// Verify types in order.
	expectedTypes := []EventType{
		EventMeta, EventLog, EventPhase, EventProgress, EventMetric,
		EventDiagnostic, EventArtifact, EventSummary, EventPhase, EventResult,
	}
	for i, et := range expectedTypes {
		if events[i].Type != et {
			t.Errorf("event[%d].Type = %q, want %q", i, events[i].Type, et)
		}
	}

	// Verify all events have v=1 and time set.
	for i, e := range events {
		if e.V != 1 {
			t.Errorf("event[%d].V = %d, want 1", i, e.V)
		}
		if e.Time == "" {
			t.Errorf("event[%d].Time is empty", i)
		}
	}
}

func TestEmitter_ResultWithError(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf)
	em.Result(ResultFailed, nil, &JobError{Message: "build failed", Code: "BUILD_ERROR"})

	events, err := DecodeAll(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}

	var rd ResultData
	if err := json.Unmarshal(events[0].Data, &rd); err != nil {
		t.Fatal(err)
	}
	if rd.Status != ResultFailed {
		t.Errorf("Status = %q", rd.Status)
	}
	if rd.Error == nil || rd.Error.Message != "build failed" {
		t.Errorf("Error = %+v", rd.Error)
	}
}

func TestValidateEvent_ValidLog(t *testing.T) {
	e := &Event{V: 1, Type: EventLog, Level: "info", Message: "hello"}
	diags := ValidateEvent(e)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestValidateEvent_Nil(t *testing.T) {
	diags := ValidateEvent(nil)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for nil event")
	}
}

// TestValidateEvent_BadVersion checks versions outside the known set. Version 2
// stopped being "bad" once the v2 event vocabulary was added (event_v2.go); the
// version-dispatch cases live in event_v2_test.go.
func TestValidateEvent_BadVersion(t *testing.T) {
	for _, version := range []int{0, 3, 99} {
		e := &Event{V: version, Type: EventLog, Level: "info", Message: "hello"}
		if diags := ValidateEvent(e); !diag.HasErrors(diags) {
			t.Fatalf("expected error for protocol version %d", version)
		}
	}
}

func TestValidateEvent_UnknownType(t *testing.T) {
	e := &Event{V: 1, Type: "unknown"}
	diags := ValidateEvent(e)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for unknown type")
	}
}

func TestValidateEvent_LogMissingFields(t *testing.T) {
	e := &Event{V: 1, Type: EventLog}
	diags := ValidateEvent(e)
	errors := diag.Errors(diags)
	if len(errors) < 2 {
		t.Fatalf("expected at least 2 errors (level + message), got %d", len(errors))
	}
}

func TestValidateEvent_ProgressMissingFields(t *testing.T) {
	e := &Event{V: 1, Type: EventProgress}
	diags := ValidateEvent(e)
	errors := diag.Errors(diags)
	if len(errors) < 2 {
		t.Fatalf("expected at least 2 errors (current + total), got %d", len(errors))
	}
}

func TestValidateEvent_MetricMissingFields(t *testing.T) {
	e := &Event{V: 1, Type: EventMetric}
	diags := ValidateEvent(e)
	errors := diag.Errors(diags)
	if len(errors) < 2 {
		t.Fatalf("expected at least 2 errors (name + value), got %d", len(errors))
	}
}

func TestValidateEvent_PhaseMissingFields(t *testing.T) {
	e := &Event{V: 1, Type: EventPhase}
	diags := ValidateEvent(e)
	errors := diag.Errors(diags)
	if len(errors) < 2 {
		t.Fatalf("expected at least 2 errors (name + action), got %d", len(errors))
	}
}

func TestValidateEvent_ArtifactMissingFields(t *testing.T) {
	e := &Event{V: 1, Type: EventArtifact}
	diags := ValidateEvent(e)
	errors := diag.Errors(diags)
	if len(errors) < 4 {
		t.Fatalf("expected at least 4 errors (id + name + kind + path), got %d", len(errors))
	}
}

func TestValidateEventStream_Valid(t *testing.T) {
	events := []*Event{
		{V: 1, Type: EventLog, Level: "info", Message: "start"},
		{V: 1, Type: EventResult},
	}
	diags := ValidateEventStream(events)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestValidateEventStream_MissingResult(t *testing.T) {
	events := []*Event{
		{V: 1, Type: EventLog, Level: "info", Message: "hello"},
	}
	diags := ValidateEventStream(events)
	found := false
	for _, d := range diags {
		if d.Code == "missing-result" {
			found = true
		}
	}
	if !found {
		t.Error("expected missing-result diagnostic")
	}
}

func TestValidateEventStream_MultipleResults(t *testing.T) {
	events := []*Event{
		{V: 1, Type: EventResult},
		{V: 1, Type: EventResult},
	}
	diags := ValidateEventStream(events)
	found := false
	for _, d := range diags {
		if d.Code == "multiple-results" {
			found = true
		}
	}
	if !found {
		t.Error("expected multiple-results diagnostic")
	}
}

func TestValidateEventStream_ResultNotLast(t *testing.T) {
	events := []*Event{
		{V: 1, Type: EventResult},
		{V: 1, Type: EventLog, Level: "info", Message: "after result"},
	}
	diags := ValidateEventStream(events)
	found := false
	for _, d := range diags {
		if d.Code == "result-not-last" {
			found = true
		}
	}
	if !found {
		t.Error("expected result-not-last warning")
	}
}

func TestDecoder_EOF(t *testing.T) {
	dec := NewDecoder(strings.NewReader(""))
	_, err := dec.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}
