package output

import (
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

func TestFailurePaneRollingLastN(t *testing.T) {
	names := []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7"}
	planned := make([]*jobs.ScheduledJob, 0, len(names))
	for _, name := range names {
		planned = append(planned, makeLiveTestJob(name, "test"))
	}
	r, _ := newTestRenderer(t, 120, planned...)

	for _, j := range planned {
		r.JobStart(j)
		r.JobComplete(j, &jobs.JobResult{Status: "failed"})
	}

	pane := r.renderFailurePane()
	if len(pane) != maxFailurePaneRows+1 {
		t.Fatalf("pane lines = %d, want %d (header + last %d)", len(pane), maxFailurePaneRows+1, maxFailurePaneRows)
	}
	if !strings.Contains(pane[0], "7 total") {
		t.Errorf("header should show the total count: %q", pane[0])
	}

	joined := strings.Join(pane, "\n")
	// Rolling: the two oldest failures scrolled out, the newest stays visible.
	if strings.Contains(joined, "p1") || strings.Contains(joined, "p2") {
		t.Errorf("oldest failures should have scrolled out of the pane: %q", joined)
	}
	if !strings.Contains(joined, "p7") {
		t.Errorf("most-recent failure should be visible: %q", joined)
	}
}

func TestFailurePaneEmptyWithoutFailures(t *testing.T) {
	ok := makeLiveTestJob("app", "test")
	r, _ := newTestRenderer(t, 120, ok)
	r.JobStart(ok)
	r.JobComplete(ok, &jobs.JobResult{Status: "success"})

	if pane := r.renderFailurePane(); pane != nil {
		t.Errorf("clean run should render no failure pane, got %v", pane)
	}
	if h := r.failurePaneHeight(); h != 0 {
		t.Errorf("failurePaneHeight = %d, want 0", h)
	}
}

func TestFailurePanePrefersDiagnostic(t *testing.T) {
	job := makeLiveTestJob("sdk", "build")
	r, _ := newTestRenderer(t, 200, job)
	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{Status: "failed", Events: []jobs.RawJobEvent{
		{Type: jobs.EventTypeDiagnostic, Data: map[string]any{
			"severity": "error", "message": "type mismatch",
			"file": "src/index.ts", "line": float64(5), "code": "TS2345",
		}},
	}})

	joined := strings.Join(r.renderFailurePane(), "\n")
	if !strings.Contains(joined, "src/index.ts:5") || !strings.Contains(joined, "TS2345") {
		t.Errorf("pane should surface the first diagnostic location: %q", joined)
	}
}

func TestFirstDiagnosticLine(t *testing.T) {
	result := &jobs.JobResult{Events: []jobs.RawJobEvent{
		{Type: jobs.EventTypeDiagnostic, Data: map[string]any{"severity": "warning", "message": "ignore me"}},
		{Type: jobs.EventTypeDiagnostic, Data: map[string]any{
			"severity": "error", "message": "bad type",
			"file": "src/x.ts", "line": float64(5), "code": "TS2345",
		}},
	}}
	if got, want := firstDiagnosticLine(result), "src/x.ts:5 TS2345 bad type"; got != want {
		t.Errorf("firstDiagnosticLine = %q, want %q", got, want)
	}

	if got := firstDiagnosticLine(&jobs.JobResult{}); got != "" {
		t.Errorf("no diagnostics should yield empty string, got %q", got)
	}
}
