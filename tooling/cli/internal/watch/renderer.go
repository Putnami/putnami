package watch

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// WatchRenderer wraps a base renderer with watch-specific behavior:
// terminal clearing between iterations and displaying changed files.
type WatchRenderer struct {
	base      jobs.Renderer
	out       io.Writer
	serveMode bool
	mu        sync.Mutex
	// ready is the typed serve-readiness adapter (serve_ready.go). It is wired
	// here because JobEvent is the only place the serve job's event stream passes
	// through; what counts as readiness lives in exactly one file.
	ready serveReadySignal
}

// NewWatchRenderer creates a watch-aware renderer that delegates to the
// given base renderer for actual job output.
func NewWatchRenderer(base jobs.Renderer, out io.Writer, serveMode bool) *WatchRenderer {
	return &WatchRenderer{
		base:      base,
		out:       out,
		serveMode: serveMode,
	}
}

// IterationStart is called at the beginning of each watch iteration.
// It clears the terminal (unless in serve mode) and prints changed files.
func (r *WatchRenderer) IterationStart(iteration int, changedFiles []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if iteration > 0 && !r.serveMode {
		// Clear terminal for non-serve modes
		iox.Fprint(r.out, "\033[2J\033[H")
	}

	if iteration == 0 {
		iox.Fprintf(r.out, "\n  [watch] starting...\n")
	} else {
		iox.Fprintf(r.out, "\n  [watch] change detected — re-running\n")
		if len(changedFiles) > 0 {
			maxFiles := 5
			shown := changedFiles
			if len(shown) > maxFiles {
				shown = shown[:maxFiles]
			}
			for _, f := range shown {
				iox.Fprintf(r.out, "    %s\n", f)
			}
			if len(changedFiles) > maxFiles {
				iox.Fprintf(r.out, "    ... and %d more\n", len(changedFiles)-maxFiles)
			}
		}
	}
	iox.Fprintln(r.out)
}

// IterationEnd is called after each watch iteration completes.
func (r *WatchRenderer) IterationEnd(iteration int, success bool, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if success {
		iox.Fprintf(r.out, "\n  [watch] done in %s — waiting for changes...\n", formatDuration(duration))
	} else {
		iox.Fprintf(r.out, "\n  [watch] failed in %s — waiting for changes...\n", formatDuration(duration))
	}
}

// IterationInterrupted reports an iteration a signal cut short. It is neither
// "done" nor "failed": the jobs were killed before they could be either, and
// the session is stopping rather than waiting for the next change.
func (r *WatchRenderer) IterationInterrupted(duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	iox.Fprintf(r.out, "\n  [watch] interrupted after %s\n", formatDuration(duration))
}

// ServeRestarting is called when the server is about to restart.
func (r *WatchRenderer) ServeRestarting(changedFiles []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	iox.Fprintf(r.out, "\n  [watch] restarting server...\n")
	if len(changedFiles) > 0 {
		summary := changedFiles[0]
		if len(changedFiles) > 1 {
			summary += fmt.Sprintf(" (+%d more)", len(changedFiles)-1)
		}
		iox.Fprintf(r.out, "    changed: %s\n", summary)
	}
}

// Shutdown is called when watch mode exits.
func (r *WatchRenderer) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	iox.Fprintf(r.out, "\n  [watch] stopped\n\n")
}

// AffectedProjects displays which projects will be re-run.
func (r *WatchRenderer) AffectedProjects(projects []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(projects) > 0 {
		iox.Fprintf(r.out, "  [watch] affected: %s\n", strings.Join(projects, ", "))
	}
}

// ResetServeReadySignal creates a new readiness signal channel for the current
// serve iteration. The channel is closed on the iteration's first typed `ready`
// event from the serve job (serve_ready.go).
func (r *WatchRenderer) ResetServeReadySignal() <-chan struct{} {
	return r.ready.reset()
}

// --- Delegate to base renderer ---

func (r *WatchRenderer) Start(planned []*jobs.ScheduledJob) {
	r.base.Start(planned)
}

func (r *WatchRenderer) JobStart(job *jobs.ScheduledJob) {
	r.base.JobStart(job)
}

func (r *WatchRenderer) JobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	if r.serveMode {
		r.ready.observe(event)
	}
	r.base.JobEvent(job, event)
}

// BatchJobEvent forwards the physical batch stream to a session-aware base.
func (r *WatchRenderer) BatchJobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	if recorder, ok := r.base.(interface {
		BatchJobEvent(*jobs.ScheduledJob, jobs.RawJobEvent)
	}); ok {
		recorder.BatchJobEvent(job, event)
	}
}

// BatchMemberJobEvent preserves human member projection while allowing a
// machine base to suppress the duplicate through its optional capability.
func (r *WatchRenderer) BatchMemberJobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	if renderer, ok := r.base.(interface {
		BatchMemberJobEvent(*jobs.ScheduledJob, jobs.RawJobEvent)
	}); ok {
		renderer.BatchMemberJobEvent(job, event)
		return
	}
	r.base.JobEvent(job, event)
}

func (r *WatchRenderer) JobComplete(job *jobs.ScheduledJob, result *jobs.JobResult) {
	r.base.JobComplete(job, result)
}

func (r *WatchRenderer) Finish(results map[string]*jobs.JobResult, outcome jobs.SessionOutcome) {
	r.base.Finish(results, outcome)
}

// SessionRecordingRenderer exposes the delegated renderer so the engine can
// attach a child session directly to a machine renderer without inserting a
// second recorder between watch and its live output.
func (r *WatchRenderer) SessionRecordingRenderer() jobs.Renderer {
	return r.base
}

// RecordSessionEvent forwards scheduler/recovery audit signals when the base
// renderer records the complete v2 session stream.
func (r *WatchRenderer) RecordSessionEvent(record jobs.SessionRecord) {
	if recorder, ok := r.base.(interface {
		RecordSessionEvent(jobs.SessionRecord)
	}); ok {
		recorder.RecordSessionEvent(record)
	}
}

// formatDuration formats a duration for display.
func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return "<1ms"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}
