package output

import (
	"io"
	"sync"
	"time"

	"go.putnami.dev/cli/model/jobs"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// TextRendererConfig configures the text output renderer.
type TextRendererConfig struct {
	Verbose   bool
	Debug     bool
	Quiet     bool
	ServeMode bool
	// Lifecycle suppresses successful task chrome and the generic session recap.
	// The lifecycle caller emits one composed action list after all phases finish.
	Lifecycle bool
}

// TextRenderer renders job execution output as human-readable text.
type TextRenderer struct {
	out    io.Writer
	errOut io.Writer
	cfg    TextRendererConfig
	start  time.Time
	// planned is the run's plan, kept so Finish can hand it to jobs.ReduceRun.
	// Holding the slice costs one header; walking the result map without it
	// would cost a sort.
	planned     []*jobs.ScheduledJob
	multiJob    bool // true when more than one job is planned
	nameWidth   int
	statusWidth int
	mu          sync.Mutex
}

// NewTextRenderer creates a text renderer writing to the given writers.
func NewTextRenderer(out, errOut io.Writer, cfg TextRendererConfig) *TextRenderer {
	return &TextRenderer{out: out, errOut: errOut, cfg: cfg}
}

// Start is called when job execution begins.
func (r *TextRenderer) Start(planned []*jobs.ScheduledJob) {
	r.start = time.Now()
	r.planned = planned
	r.multiJob = len(planned) > 1
	r.computeVerboseLayout(planned)
	if r.cfg.Quiet || r.cfg.Lifecycle {
		return
	}
	iox.Fprintf(r.errOut, "\n")
}

// JobStart is called when a job begins execution.
func (r *TextRenderer) JobStart(job *jobs.ScheduledJob) {
	if r.cfg.Quiet {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg.Verbose {
		iox.Fprint(r.errOut, r.verboseLine(job, verboseRunningIcon(), "--:--",
			seg{verboseStepName(job), Dim}, seg{" starting", ""},
		)+"\n")
	} else if r.cfg.Debug {
		iox.Fprintf(r.errOut, "  %s %s ...\n", jobLabel(job), statusIcon("running"))
	}
}

// JobEvent is called for each JSONL event from a running job.
func (r *TextRenderer) JobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg.Lifecycle && event.Type == jobs.EventTypeDiagnostic {
		severity, _ := event.Data["severity"].(string)
		switch severity {
		case string(runtimeproto.SeverityWarning), "warn", string(runtimeproto.SeverityError), "fatal":
			r.renderDiagnostic(event)
		}
		return
	}
	if r.cfg.Debug {
		r.renderDebugEvent(job, event)
		return
	}
	// Serve mode: show log events with human-readable formatting
	if r.cfg.ServeMode && event.Type == jobs.EventTypeLog {
		noColor := !ColorsEnabled()
		label := serveLogLabel(stripStepSuffix(job.JobDef.Name), job.Project.Name, noColor, Dim)
		line := FormatLogEvent(event, noColor, label)
		if line != "" {
			iox.Fprint(r.errOut, line+"\n")
		}
		return
	}
	if r.cfg.Verbose {
		r.renderVerboseEvent(job, event)
	}
}

// JobComplete is called when a job finishes execution.
func (r *TextRenderer) JobComplete(job *jobs.ScheduledJob, result *jobs.JobResult) {
	if r.cfg.Quiet {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfg.Lifecycle {
		if result.Status == string(jobs.TaskStatusFailed) {
			iox.Fprintf(r.errOut, "  %s %s  %s%s\n", jobLabel(job), statusIcon(result.Status),
				formatDuration(result.Duration), replayedFailureSuffix(result))
			r.renderFailureDetails(result)
		}
		return
	}

	if r.cfg.Verbose {
		r.jobCompleteVerbose(job, result)
		return
	}

	label := jobLabel(job)

	// Reused results are always visible regardless of verbose — and a reused
	// result that FAILED is a failure first: a cache hit whose declared output
	// drifted from this checkout must render as one, with its diagnostics,
	// rather than as the "cached" its provenance alone would say.
	if outcome := result.Outcome(); result.Status != "failed" &&
		(outcome == jobs.JobOutcomeCached || outcome == jobs.JobOutcomeCoalesced) {
		iox.Fprintf(r.errOut, "  %s %s\n", label, statusIcon(outcome))
		return
	}

	icon := statusIcon(result.Status)
	dur := formatDuration(result.Duration)

	if result.Status == "failed" {
		iox.Fprintf(r.errOut, "  %s %s  %s%s%s\n", label, icon, dur, reusedFailureSuffix(result), replayedFailureSuffix(result))
		r.renderFailureDetails(result)
	} else {
		// In multi-job mode or debug, always show completed jobs for visibility
		if r.multiJob || r.cfg.Debug {
			iox.Fprintf(r.errOut, "  %s %s  %s\n", label, icon, dur)
		}
		if r.cfg.Debug {
			for _, ev := range result.Events {
				if ev.Type == jobs.EventTypeSummary {
					if msg, ok := ev.Data["message"].(string); ok {
						iox.Fprintf(r.errOut, "    %s\n", msg)
					}
				}
			}
		}
	}
}

// jobCompleteVerbose renders job completion in verbose mode with live-style rows.
// Must be called with r.mu held.
func (r *TextRenderer) jobCompleteVerbose(job *jobs.ScheduledJob, result *jobs.JobResult) {
	step := verboseStepName(job)
	dur := formatDuration(result.Duration)

	if outcome := result.Outcome(); result.Status != "failed" &&
		(outcome == jobs.JobOutcomeCached || outcome == jobs.JobOutcomeCoalesced) {
		iox.Fprint(r.errOut, r.verboseLine(job, verboseCachedIcon(), dur,
			seg{step, Dim}, seg{" " + outcome, ""},
		)+"\n")
		return
	}

	if result.Status == "failed" {
		iox.Fprint(r.errOut, r.verboseLine(job, verboseFailedIcon(), dur,
			seg{step, Dim}, seg{" failed" + reusedFailureSuffix(result) + replayedFailureSuffix(result), ""},
		)+"\n")
		r.renderFailureDetails(result)
	} else {
		iox.Fprint(r.errOut, r.verboseLine(job, verboseSuccessIcon(), dur,
			seg{step, Dim}, seg{" done", ""},
		)+"\n")
		for _, ev := range result.Events {
			if ev.Type == jobs.EventTypeSummary {
				if msg, ok := ev.Data["message"].(string); ok {
					iox.Fprint(r.errOut, r.verboseLine(job, verboseRunningIcon(), "",
						seg{msg, ""},
					)+"\n")
				}
			}
		}
	}
}

// renderFailureDetails prints error messages, diagnostics, and fallback log events for a failed job.
// Must be called with r.mu held.
func (r *TextRenderer) renderFailureDetails(result *jobs.JobResult) {
	hasDetails := false
	for _, ev := range result.Events {
		if ev.Type == jobs.EventTypeDiagnostic {
			hasDetails = true
			r.renderDiagnostic(ev)
		}
	}
	for _, line := range failureLogLines(result.Events) {
		hasDetails = true
		iox.Fprintf(r.errOut, "    %s\n", line)
	}
	if result.Error != nil && result.Error.Message != "" {
		details, generic := splitErrorDetailLines(result.Error.Message)
		for _, line := range details {
			hasDetails = true
			iox.Fprintf(r.errOut, "    %s\n", line)
		}
		if !hasDetails {
			if summary := genericExitSummary(generic); summary != "" {
				hasDetails = true
				iox.Fprintf(r.errOut, "    %s\n", summary)
			}
		}
	}
	if !hasDetails {
		iox.Fprintf(r.errOut, "    (no details emitted)\n")
	}
	// A replayed failure prints the ORIGINAL failure output above, verbatim, so
	// it is never quieter than a fresh one. This is the only line that differs:
	// it says why the verdict came back instantly and how to force the work.
	if result.ReplayedFailure != nil {
		iox.Fprintf(r.errOut, "    %s\n", replayedFailureHint)
	}
}
