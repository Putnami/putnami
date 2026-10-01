package watch

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

// nullRenderer is a no-op renderer for testing.
type nullRenderer struct{}

func (n *nullRenderer) Start([]*jobs.ScheduledJob)                             {}
func (n *nullRenderer) JobStart(*jobs.ScheduledJob)                            {}
func (n *nullRenderer) JobEvent(*jobs.ScheduledJob, jobs.RawJobEvent)          {}
func (n *nullRenderer) JobComplete(*jobs.ScheduledJob, *jobs.JobResult)        {}
func (n *nullRenderer) Finish(map[string]*jobs.JobResult, jobs.SessionOutcome) {}

func TestWatchRenderer_IterationStart_Initial(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, false)

	r.IterationStart(0, nil)

	output := buf.String()
	if !strings.Contains(output, "[watch] starting") {
		t.Errorf("expected 'starting' message, got: %s", output)
	}
}

func TestWatchRenderer_IterationStart_Subsequent(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, false)

	r.IterationStart(1, []string{"src/main.go", "src/lib.go"})

	output := buf.String()
	if !strings.Contains(output, "change detected") {
		t.Errorf("expected 'change detected' message, got: %s", output)
	}
	if !strings.Contains(output, "src/main.go") {
		t.Errorf("expected changed file listed, got: %s", output)
	}
}

func TestWatchRenderer_IterationStart_TruncatesFiles(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, false)

	files := make([]string, 10)
	for i := range files {
		files[i] = "file" + string(rune('0'+i)) + ".go"
	}

	r.IterationStart(1, files)

	output := buf.String()
	if !strings.Contains(output, "and 5 more") {
		t.Errorf("expected truncation message, got: %s", output)
	}
}

func TestWatchRenderer_IterationEnd_Success(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, false)

	r.IterationEnd(1, true, 2*time.Second)

	output := buf.String()
	if !strings.Contains(output, "waiting for changes") {
		t.Errorf("expected 'waiting' message, got: %s", output)
	}
	if strings.Contains(output, "failed") {
		t.Error("unexpected 'failed' in success output")
	}
}

func TestWatchRenderer_IterationEnd_Failure(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, false)

	r.IterationEnd(1, false, 500*time.Millisecond)

	output := buf.String()
	if !strings.Contains(output, "failed") {
		t.Errorf("expected 'failed' message, got: %s", output)
	}
}

func TestWatchRenderer_ServeRestarting(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, true)

	r.ServeRestarting([]string{"src/main.go", "src/routes.go"})

	output := buf.String()
	if !strings.Contains(output, "restarting server") {
		t.Errorf("expected 'restarting' message, got: %s", output)
	}
	if !strings.Contains(output, "src/main.go") {
		t.Errorf("expected changed file in restart message, got: %s", output)
	}
}

func TestWatchRenderer_Shutdown(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, false)

	r.Shutdown()

	output := buf.String()
	if !strings.Contains(output, "stopped") {
		t.Errorf("expected 'stopped' message, got: %s", output)
	}
}

func TestWatchRenderer_AffectedProjects(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, false)

	r.AffectedProjects([]string{"lib", "app"})

	output := buf.String()
	if !strings.Contains(output, "lib, app") {
		t.Errorf("expected project names, got: %s", output)
	}
}

func TestWatchRenderer_ServeModeNoClears(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, true)

	r.IterationStart(1, []string{"main.go"})

	output := buf.String()
	// In serve mode, should NOT contain clear escape sequence
	if strings.Contains(output, "\033[2J") {
		t.Error("serve mode should not clear terminal")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Microsecond, "<1ms"},
		{100 * time.Millisecond, "100ms"},
		{2500 * time.Millisecond, "2.5s"},
		{90 * time.Second, "1.5m"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// A Ctrl-C'd iteration is neither "done" nor "failed", and the session is
// stopping rather than waiting for the next change. It reported "done" before
// the abort plumbing existed and "failed" immediately after — both misread a
// user's own interrupt as a verdict on their code.
func TestWatchRenderer_IterationInterrupted(t *testing.T) {
	var buf bytes.Buffer
	r := NewWatchRenderer(&nullRenderer{}, &buf, false)

	r.IterationInterrupted(1500 * time.Millisecond)

	out := buf.String()
	if !strings.Contains(out, "interrupted") {
		t.Errorf("expected an interrupted line, got: %q", out)
	}
	if strings.Contains(out, "failed") || strings.Contains(out, "done") {
		t.Errorf("interrupted iteration must not read as done/failed: %q", out)
	}
	if strings.Contains(out, "waiting for changes") {
		t.Errorf("interrupted session is stopping, not waiting: %q", out)
	}
}

// The renderer is where the serve job's event stream passes through, so it is
// where the serve-readiness adapter is wired (serve_ready.go owns what counts as
// ready).
func TestWatchRenderer_WiresServeReadinessThroughJobEvents(t *testing.T) {
	r := NewWatchRenderer(&nullRenderer{}, &bytes.Buffer{}, true)
	ready := r.ResetServeReadySignal()

	r.JobEvent(nil, serverReadyEvent(t))

	select {
	case <-ready:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("serve ready signal was not closed")
	}
}

// Outside serve mode the readiness adapter is inert: a plain `putnami build
// --watch` has no server to wait for, so a job's readiness event must never
// reach its loop as a lifecycle signal.
func TestWatchRenderer_NonServeModeIgnoresReadiness(t *testing.T) {
	r := NewWatchRenderer(&nullRenderer{}, &bytes.Buffer{}, false)
	ready := r.ResetServeReadySignal()

	r.JobEvent(nil, serverReadyEvent(t))

	select {
	case <-ready:
		t.Fatal("non-serve watch closed the readiness signal")
	case <-time.After(30 * time.Millisecond):
	}
}
