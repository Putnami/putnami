package output

import (
	"bytes"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

// logEvent builds a serve log event with the given level/message and optional
// context map.
func logEvent(level, message string) jobs.RawJobEvent {
	return jobs.RawJobEvent{
		Type: jobs.EventTypeLog,
		Data: map[string]any{"level": level, "message": message},
	}
}

func TestRenderServeLog_CapturesURLAndWritesLine(t *testing.T) {
	job := makeLiveTestJob("web", "serve")
	r, errOut := newTestRenderer(t, 120, job)
	r.serveMode = true

	r.mu.Lock()
	r.renderServeLog(job, logEvent("info", "listening on http://localhost:3000"))
	r.mu.Unlock()

	// The server URL should be captured onto the project row.
	row := r.rowByID[job.Project.ID]
	if row == nil {
		t.Fatalf("expected row for project %q", job.Project.ID)
	}
	if row.serveStatus != "http://localhost:3000" {
		t.Errorf("serveStatus = %q, want %q", row.serveStatus, "http://localhost:3000")
	}

	// The formatted log line should be written to the error output.
	out := errOut.String()
	if !strings.Contains(out, "listening on http://localhost:3000") {
		t.Errorf("error output = %q, want it to contain the message", out)
	}
	// The frame is reset so the next redraw repaints the table fresh.
	if len(r.prevFrame) != 0 {
		t.Errorf("prevFrame = %v, want empty after a scrollback write", r.prevFrame)
	}
}

func TestRenderServeLog_TrimsTrailingWhitespaceFromURL(t *testing.T) {
	job := makeLiveTestJob("api", "serve")
	r, _ := newTestRenderer(t, 120, job)
	r.serveMode = true

	r.mu.Lock()
	r.renderServeLog(job, logEvent("info", "ready at http://127.0.0.1:8080  \t\n"))
	r.mu.Unlock()

	row := r.rowByID[job.Project.ID]
	if row.serveStatus != "http://127.0.0.1:8080" {
		t.Errorf("serveStatus = %q, want %q", row.serveStatus, "http://127.0.0.1:8080")
	}
}

func TestRenderServeLog_NoURLLeavesStatusEmpty(t *testing.T) {
	job := makeLiveTestJob("worker", "serve")
	r, errOut := newTestRenderer(t, 120, job)
	r.serveMode = true

	r.mu.Lock()
	r.renderServeLog(job, logEvent("info", "background worker started"))
	r.mu.Unlock()

	row := r.rowByID[job.Project.ID]
	if row.serveStatus != "" {
		t.Errorf("serveStatus = %q, want empty (no URL in message)", row.serveStatus)
	}
	if !strings.Contains(errOut.String(), "background worker started") {
		t.Errorf("error output should still contain the message, got %q", errOut.String())
	}
}

func TestRenderServeLog_DebugEventProducesNoLine(t *testing.T) {
	job := makeLiveTestJob("web", "serve")
	r, errOut := newTestRenderer(t, 120, job)
	r.serveMode = true
	errOut.Reset()

	r.mu.Lock()
	// Debug level events are dropped by FormatLogEvent -> empty line -> no write.
	r.renderServeLog(job, logEvent("debug", "verbose diagnostic"))
	r.mu.Unlock()

	if errOut.String() != "" {
		t.Errorf("error output = %q, want empty for debug event", errOut.String())
	}
}

func TestRenderServeLog_ColorizedLabel(t *testing.T) {
	initial := ColorsEnabled()
	SetColorsEnabled(true)
	t.Cleanup(func() { SetColorsEnabled(initial) })

	job := makeLiveTestJob("web", "serve")
	var out, errOut bytes.Buffer
	r := NewLiveRenderer(&out, &errOut)
	r.Start([]*jobs.ScheduledJob{job})
	r.stopRedraw()
	r.width = 120
	r.serveMode = true
	errOut.Reset()

	r.mu.Lock()
	r.renderServeLog(job, logEvent("info", "serving at http://localhost:5173"))
	r.mu.Unlock()

	row := r.rowByID[job.Project.ID]
	if row == nil || row.serveStatus != "http://localhost:5173" {
		t.Fatalf("serveStatus not captured: %+v", row)
	}
	got := errOut.String()
	if !strings.Contains(got, "serving at http://localhost:5173") {
		t.Errorf("error output = %q, want the message", got)
	}
	// Colorized output should include ANSI escape sequences in the label.
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("error output = %q, want ANSI color codes in colorized mode", got)
	}
}

func TestRenderServeLog_URLWithoutRegisteredRow(t *testing.T) {
	// A serve job whose project was never registered (no row) must not panic
	// when a URL is present; the URL capture is simply skipped.
	registered := makeLiveTestJob("web", "serve")
	r, errOut := newTestRenderer(t, 120, registered)
	r.serveMode = true
	errOut.Reset()

	orphan := makeLiveTestJob("ghost", "serve")
	r.mu.Lock()
	r.renderServeLog(orphan, logEvent("info", "up at http://localhost:9999"))
	r.mu.Unlock()

	if r.rowByID[orphan.Project.ID] != nil {
		t.Fatalf("orphan project should have no row")
	}
	if !strings.Contains(errOut.String(), "up at http://localhost:9999") {
		t.Errorf("error output = %q, want the message line", errOut.String())
	}
}

func TestRenderServeLog_EmptyMessageDoesNotPanicOrWrite(t *testing.T) {
	job := makeLiveTestJob("web", "serve")
	r, errOut := newTestRenderer(t, 120, job)
	r.serveMode = true
	errOut.Reset()

	r.mu.Lock()
	r.renderServeLog(job, jobs.RawJobEvent{Type: jobs.EventTypeLog, Data: map[string]any{"level": "info"}})
	r.mu.Unlock()

	if errOut.String() != "" {
		t.Errorf("error output = %q, want empty for message-less event", errOut.String())
	}
}
