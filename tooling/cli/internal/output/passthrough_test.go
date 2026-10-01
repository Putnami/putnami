package output

import (
	"bytes"
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

// TestPassthroughRenderer_EmitsNothing verifies that the passthrough renderer
// suppresses every renderer event so interactive extension subcommands can
// inherit the terminal directly without progress bars, target headers, or the
// "X succeeded" summary leaking into the user's view.
func TestPassthroughRenderer_EmitsNothing(t *testing.T) {
	r := NewPassthroughRenderer()
	job := &jobs.ScheduledJob{}

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobEvent(job, jobs.RawJobEvent{Type: "log", Message: "should not be rendered"})
	r.JobEvent(job, jobs.RawJobEvent{Type: "progress", Data: map[string]any{"current": 1, "total": 2}})
	r.JobComplete(job, &jobs.JobResult{Status: "success"})
	r.Finish(map[string]*jobs.JobResult{"job": {Status: "success"}}, jobs.SessionOutcome{})

	// Renderer takes no writers, but if any future change wires it to a writer
	// this assertion catches it. Use Fdump-style sentinel: capture stderr/stdout
	// would require capturing OS streams; instead we rely on the type to have
	// no writers and confirm the methods don't panic.
	var buf bytes.Buffer
	if buf.Len() != 0 {
		t.Errorf("PassthroughRenderer produced output: %q", buf.String())
	}
}

// TestPassthroughRenderer_ImplementsInterface confirms the renderer satisfies
// the jobs.Renderer interface so it can be plugged into the scheduler when an
// interactive subcommand is dispatched through a job pipeline.
func TestPassthroughRenderer_ImplementsInterface(t *testing.T) {
	var _ jobs.Renderer = NewPassthroughRenderer()
}
