package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

func TestCloudLoggingRenderer_JobStart(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	job := makeTestJob("my-app", "build")
	r.JobStart(job)

	events := parseJSONLines(t, buf.String())
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0]["severity"] != "INFO" {
		t.Errorf("severity = %v, want INFO", events[0]["severity"])
	}
	if events[0]["message"] == nil {
		t.Error("message should not be nil")
	}
}

func TestCloudLoggingRenderer_JobComplete_Success(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	job := makeTestJob("my-app", "build")
	result := &jobs.JobResult{
		Status:   "success",
		Duration: 3 * time.Second,
	}
	r.JobComplete(job, result)

	events := parseJSONLines(t, buf.String())
	if events[0]["severity"] != "INFO" {
		t.Errorf("severity = %v, want INFO", events[0]["severity"])
	}
}

func TestCloudLoggingRenderer_JobComplete_Failed(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	job := makeTestJob("my-app", "build")
	result := &jobs.JobResult{
		Status:   "failed",
		Duration: 1 * time.Second,
		Error:    &jobs.JobError{Message: "build failed"},
	}
	r.JobComplete(job, result)

	events := parseJSONLines(t, buf.String())
	if events[0]["severity"] != "ERROR" {
		t.Errorf("severity = %v, want ERROR", events[0]["severity"])
	}
}

func TestCloudLoggingRenderer_JobComplete_Cached(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	job := makeTestJob("my-app", "build")
	result := &jobs.JobResult{
		Status:   "success",
		Duration: 0,
		CacheHit: true,
	}
	r.JobComplete(job, result)

	events := parseJSONLines(t, buf.String())
	labels := events[0]["logging.googleapis.com/labels"].(map[string]any)
	if labels["status"] != "cached" {
		t.Errorf("status label = %v, want cached", labels["status"])
	}
}

func TestCloudLoggingRenderer_JobComplete_Coalesced(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)
	r.JobComplete(makeTestJob("my-app", "build"), &jobs.JobResult{Status: "success", Coalesced: true})

	events := parseJSONLines(t, buf.String())
	labels := events[0]["logging.googleapis.com/labels"].(map[string]any)
	if labels["status"] != "coalesced" || labels["coalesced"] != true || labels["cache"] != false {
		t.Errorf("coalesced labels = %v", labels)
	}
}

func TestCloudLoggingRenderer_JobEvent_Diagnostic(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	job := makeTestJob("my-app", "lint")
	event := jobs.RawJobEvent{
		Type:    jobs.EventTypeDiagnostic,
		Message: "unused var",
		Data:    map[string]any{"severity": "error"},
	}
	r.JobEvent(job, event)

	events := parseJSONLines(t, buf.String())
	if events[0]["severity"] != "ERROR" {
		t.Errorf("severity = %v, want ERROR", events[0]["severity"])
	}
}

func TestCloudLoggingRenderer_JobEvent_Warning(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	job := makeTestJob("my-app", "lint")
	event := jobs.RawJobEvent{
		Type:    jobs.EventTypeDiagnostic,
		Message: "deprecated function",
		Data:    map[string]any{"severity": "warning"},
	}
	r.JobEvent(job, event)

	events := parseJSONLines(t, buf.String())
	if events[0]["severity"] != "WARNING" {
		t.Errorf("severity = %v, want WARNING", events[0]["severity"])
	}
}

func TestCloudLoggingRenderer_JobEvent_Log(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	job := makeTestJob("my-app", "build")
	event := jobs.RawJobEvent{
		Type:    jobs.EventTypeLog,
		Message: "compiling...",
		Data:    map[string]any{"level": "debug"},
	}
	r.JobEvent(job, event)

	events := parseJSONLines(t, buf.String())
	if events[0]["severity"] != "DEBUG" {
		t.Errorf("severity = %v, want DEBUG", events[0]["severity"])
	}
}

func TestCloudLoggingRenderer_Finish_Success(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	results := map[string]*jobs.JobResult{
		"app:build": {Status: "success"},
	}
	r.Finish(results, jobs.SessionOutcome{})

	events := parseJSONLines(t, buf.String())
	if events[0]["severity"] != "INFO" {
		t.Errorf("severity = %v, want INFO", events[0]["severity"])
	}
}

func TestCloudLoggingRenderer_Finish_WithFailures(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	results := map[string]*jobs.JobResult{
		"app:build": {Status: "success"},
		"lib:build": {Status: "failed"},
	}
	r.Finish(results, jobs.SessionOutcome{})

	events := parseJSONLines(t, buf.String())
	if events[0]["severity"] != "ERROR" {
		t.Errorf("severity = %v, want ERROR", events[0]["severity"])
	}
}

func TestEventSeverity(t *testing.T) {
	tests := []struct {
		event jobs.RawJobEvent
		want  string
	}{
		{
			event: jobs.RawJobEvent{Type: jobs.EventTypeDiagnostic, Data: map[string]any{"severity": "error"}},
			want:  "ERROR",
		},
		{
			event: jobs.RawJobEvent{Type: jobs.EventTypeDiagnostic, Data: map[string]any{"severity": "warning"}},
			want:  "WARNING",
		},
		{
			event: jobs.RawJobEvent{Type: jobs.EventTypeDiagnostic, Data: map[string]any{"severity": "info"}},
			want:  "INFO",
		},
		{
			event: jobs.RawJobEvent{Type: jobs.EventTypeLog, Data: map[string]any{"level": "error"}},
			want:  "ERROR",
		},
		{
			event: jobs.RawJobEvent{Type: jobs.EventTypeLog, Data: map[string]any{"level": "warn"}},
			want:  "WARNING",
		},
		{
			event: jobs.RawJobEvent{Type: jobs.EventTypeLog, Data: map[string]any{"level": "debug"}},
			want:  "DEBUG",
		},
		{
			event: jobs.RawJobEvent{Type: jobs.EventTypeLog, Data: map[string]any{"level": "info"}},
			want:  "INFO",
		},
		{
			event: jobs.RawJobEvent{Type: "progress"},
			want:  "INFO",
		},
	}

	for _, tt := range tests {
		got := eventSeverity(tt.event)
		if got != tt.want {
			t.Errorf("eventSeverity(%s/%v) = %q, want %q", tt.event.Type, tt.event.Data, got, tt.want)
		}
	}
}

// TestCloudLoggingRenderer_ConcurrentJobEvents drives JobStart/JobEvent/
// JobComplete from many goroutines into one shared renderer, mirroring the
// scheduler's parallel worker callbacks. emit serializes each structured log
// line into the shared buffer under the renderer's mutex; run under `-race` to
// catch any unsynchronized access. Every emitted line must remain a single,
// well-formed JSON object (a lost lock would interleave or corrupt lines).
func TestCloudLoggingRenderer_ConcurrentJobEvents(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)
	r.Start(nil)

	const goroutines = 16
	const eventsPerGoroutine = 25

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			job := makeTestJob(fmt.Sprintf("project-%d", id), "build")
			r.JobStart(job)
			for j := 0; j < eventsPerGoroutine; j++ {
				r.JobEvent(job, jobs.RawJobEvent{
					Type:    "progress",
					Message: fmt.Sprintf("step %d", j),
					Data:    map[string]any{"current": j, "total": eventsPerGoroutine},
				})
			}
			r.JobComplete(job, &jobs.JobResult{
				Status:   "success",
				Duration: time.Duration(id) * time.Millisecond,
			})
		}(i)
	}
	wg.Wait()

	// Each goroutine emits 1 start + k events + 1 complete; all must be present
	// and each line must be a single valid JSON object (no interleaving).
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	wantLines := goroutines * (eventsPerGoroutine + 2)
	if len(lines) != wantLines {
		t.Errorf("expected %d lines, got %d", wantLines, len(lines))
	}
	for i, line := range lines {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("line %d is not valid JSON: %v (line: %q)", i, err, line)
		}
	}
}
