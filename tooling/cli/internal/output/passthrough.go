package output

import (
	"go.putnami.dev/cli/model/jobs"
)

// PassthroughRenderer is a no-op job renderer used by interactive extension
// commands. The subprocess inherits stdin/stdout/stderr directly, so all
// renderer chrome (target headers, spinner, JSONL parsing, "X succeeded"
// summary) must be suppressed.
type PassthroughRenderer struct{}

// NewPassthroughRenderer creates a renderer that emits no output.
func NewPassthroughRenderer() *PassthroughRenderer {
	return &PassthroughRenderer{}
}

// Start does nothing.
func (PassthroughRenderer) Start([]*jobs.ScheduledJob) {}

// JobStart does nothing.
func (PassthroughRenderer) JobStart(*jobs.ScheduledJob) {}

// JobEvent does nothing.
func (PassthroughRenderer) JobEvent(*jobs.ScheduledJob, jobs.RawJobEvent) {}

// JobComplete does nothing.
func (PassthroughRenderer) JobComplete(*jobs.ScheduledJob, *jobs.JobResult) {}

// Finish does nothing.
func (PassthroughRenderer) Finish(map[string]*jobs.JobResult, jobs.SessionOutcome) {}
