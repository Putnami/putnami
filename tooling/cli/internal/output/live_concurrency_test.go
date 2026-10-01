package output

import (
	"io"
	"sync"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestLiveRenderer_ConcurrentJobReports stresses the renderer's worker-side
// callbacks (JobStart / JobEvent / JobComplete) against its background
// redrawLoop goroutine. The scheduler calls JobStart/Complete from any of
// MaxParallel worker goroutines, and the redraw timer fires concurrently,
// so the renderer's mutex must protect every shared field touched in
// either path. Run under `-race` to catch any escape.
//
// The test does not assert visible output (live drawing depends on TTY
// state); it discards writes and checks that no panic or race fires.
func TestLiveRenderer_ConcurrentJobReports(t *testing.T) {
	const projects = 6
	const cmds = 4
	planned := make([]*jobs.ScheduledJob, 0, projects*cmds)
	for p := range projects {
		for c := range cmds {
			projName := "p" + string(rune('a'+p))
			cmdName := []string{"build", "lint", "test", "package"}[c]
			planned = append(planned, &jobs.ScheduledJob{
				Project: &workspace.Project{
					ID:   "/" + projName,
					Name: projName,
					Path: projName,
				},
				Extension: &extension.ExtensionDescription{
					Name: "@putnami/test",
					Path: "/ext/test",
				},
				JobDef: &extension.JobDefinition{
					Name:          cmdName,
					ExtensionName: "@putnami/test",
				},
			})
		}
	}

	r := NewLiveRenderer(io.Discard, io.Discard)
	r.Start(planned)

	var wg sync.WaitGroup
	// Spawn one worker per planned job; each reports its own lifecycle.
	for _, job := range planned {
		wg.Add(1)
		go func(j *jobs.ScheduledJob) {
			defer wg.Done()
			r.JobStart(j)
			r.JobEvent(j, jobs.RawJobEvent{
				Type: jobs.EventTypePhase,
				Data: map[string]any{"action": "start", "name": "phase1"},
			})
			r.JobEvent(j, jobs.RawJobEvent{
				Type: jobs.EventTypeSummary,
				Data: map[string]any{"message": "done"},
			})
			r.JobComplete(j, &jobs.JobResult{Status: "success"})
		}(job)
	}
	wg.Wait()

	results := make(map[string]*jobs.JobResult, len(planned))
	for _, j := range planned {
		results[j.Key()] = &jobs.JobResult{Status: "success"}
	}
	r.Finish(results, jobs.SessionOutcome{})
}

// TestLiveRenderer_FinishDuringRedraw exercises the Start → many redraws
// → Finish lifecycle: Finish must stop the redraw goroutine cleanly even
// if it is mid-draw. Re-entrant Finish calls (e.g. on retry) must also
// be safe. The mu / stopCh / doneCh dance in stopRedraw is the path under
// test.
func TestLiveRenderer_FinishDuringRedraw(t *testing.T) {
	planned := []*jobs.ScheduledJob{
		{
			Project:   &workspace.Project{ID: "/x", Name: "x", Path: "x"},
			Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: "/ext/test"},
			JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "@putnami/test"},
		},
	}
	r := NewLiveRenderer(io.Discard, io.Discard)
	r.Start(planned)

	// Drive a burst of updates so the redrawLoop has work to do while we
	// race it with Finish.
	for range 50 {
		r.JobStart(planned[0])
		r.JobEvent(planned[0], jobs.RawJobEvent{
			Type: jobs.EventTypeProgress,
			Data: map[string]any{"current": float64(1), "total": float64(2), "message": "tick"},
		})
	}

	r.Finish(map[string]*jobs.JobResult{"/x:build": {Status: "success"}}, jobs.SessionOutcome{})
	// Idempotent: calling Finish again must not panic or hang.
	r.Finish(map[string]*jobs.JobResult{"/x:build": {Status: "success"}}, jobs.SessionOutcome{})
}
