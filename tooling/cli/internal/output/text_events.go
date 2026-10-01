package output

import (
	"fmt"
	"strings"
	"time"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// renderVerboseEvent renders a single event in verbose mode. Must be called
// with r.mu held.
func (r *TextRenderer) renderVerboseEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	noColor := !ColorsEnabled()

	switch event.Type {
	case jobs.EventTypePhase:
		action, _ := event.Data["action"].(string)
		name, _ := event.Data["name"].(string)
		if action == "start" {
			iox.Fprint(r.errOut, r.verboseLine(job, verboseRunningIcon(), "--:--",
				seg{"[" + name + "]", Dim}, seg{" start", ""},
			)+"\n")
		}
	case jobs.EventTypeLog:
		line := FormatLogEvent(event, noColor, r.verbosePrefix(job, verboseRunningIcon(), "--:--", noColor))
		if line != "" {
			iox.Fprint(r.errOut, line+"\n")
		}
	case jobs.EventTypeDiagnostic:
		r.renderDiagnosticPrefixed(event, verboseDetailPrefix(r.nameWidth, r.statusWidth))
	}
}

// renderDebugEvent renders a single event in debug mode (all events).
// Must be called with r.mu held.
func (r *TextRenderer) renderDebugEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	prefix := r.eventPrefix(job) + fmt.Sprintf("[%s] ", event.Type)

	switch event.Type {
	case jobs.EventTypePhase:
		action, _ := event.Data["action"].(string)
		name, _ := event.Data["name"].(string)
		if action == "end" {
			status, _ := event.Data["status"].(string)
			dur, _ := event.Data["duration"].(float64)
			iox.Fprintf(r.errOut, "%s%s %s %s\n", prefix, name, status, formatDurationMs(dur))
		} else {
			iox.Fprintf(r.errOut, "%s%s %s\n", prefix, name, action)
		}

	case jobs.EventTypeLog:
		level, _ := event.Data["level"].(string)
		msg, _ := event.Data["message"].(string)
		iox.Fprintf(r.errOut, "%s%s: %s\n", prefix, level, msg)

	case jobs.EventTypeProgress:
		current, _ := event.Data["current"].(float64)
		total, _ := event.Data["total"].(float64)
		label, _ := event.Data["message"].(string)
		if total > 0 {
			iox.Fprintf(r.errOut, "%s%.0f/%.0f %s\n", prefix, current, total, label)
		} else {
			iox.Fprintf(r.errOut, "%s%.0f %s\n", prefix, current, label)
		}

	case jobs.EventTypeMetric:
		name, _ := event.Data["name"].(string)
		value, _ := event.Data["value"].(float64)
		unit, _ := event.Data["unit"].(string)
		iox.Fprintf(r.errOut, "%s%s=%.2f%s\n", prefix, name, value, unit)

	case jobs.EventTypeDiagnostic:
		r.renderDiagnosticPrefixed(event, prefix)

	case jobs.EventTypeArtifact:
		name, _ := event.Data["name"].(string)
		kind, _ := event.Data["kind"].(string)
		iox.Fprintf(r.errOut, "%s%s (%s)\n", prefix, name, kind)

	case jobs.EventTypeSummary:
		msg, _ := event.Data["message"].(string)
		iox.Fprintf(r.errOut, "%s%s\n", prefix, msg)
	}
}

// eventPrefix returns the indentation prefix for event output.
// In multi-job mode, it includes the job label for disambiguation.
func (r *TextRenderer) eventPrefix(job *jobs.ScheduledJob) string {
	if r.multiJob {
		return fmt.Sprintf("    [%s] ", jobLabel(job))
	}
	return "    "
}

// renderDiagnostic renders a diagnostic event with the default prefix.
func (r *TextRenderer) renderDiagnostic(event jobs.RawJobEvent) {
	r.renderDiagnosticPrefixed(event, "    ")
}

// renderDiagnosticPrefixed renders a diagnostic event with a given prefix.
func (r *TextRenderer) renderDiagnosticPrefixed(event jobs.RawJobEvent, prefix string) {
	severity, _ := event.Data["severity"].(string)
	msg, _ := event.Data["message"].(string)
	loc, _ := event.Data["location"].(map[string]any)

	if loc != nil {
		file, _ := loc["file"].(string)
		line, _ := loc["line"].(float64)
		if file != "" {
			if line > 0 {
				iox.Fprintf(r.errOut, "%s%s:%d: %s: %s\n", prefix, file, int(line), severity, msg)
			} else {
				iox.Fprintf(r.errOut, "%s%s: %s: %s\n", prefix, file, severity, msg)
			}
			return
		}
	}
	iox.Fprintf(r.errOut, "%s%s: %s\n", prefix, severity, msg)
}

func verboseDetailPrefix(nameWidth, statusWidth int) string {
	// leading indent + icon + spaces/name/status/duration columns + detail gap
	width := 2 + 1 + 1 + nameWidth + 1 + statusWidth + 1 + verboseDurationWidth + 2
	return strings.Repeat(" ", width)
}

// jobLabel returns a display label for a job: "projectName commandName".
func jobLabel(job *jobs.ScheduledJob) string {
	return job.Project.Name + " " + job.JobDef.Name
}

// statusIcon returns a text icon for a job status.
func statusIcon(status string) string {
	switch status {
	case "success":
		return "done"
	case "failed":
		return "FAIL"
	case "canceled":
		return "cancel"
	case "skipped":
		return "skip"
	case "cached":
		return "cached"
	case "coalesced":
		return "coalesced"
	case "running":
		return "..."
	default:
		return status
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

// formatDurationMs formats milliseconds for display.
func formatDurationMs(ms float64) string {
	if ms <= 0 {
		return ""
	}
	return formatDuration(time.Duration(ms) * time.Millisecond)
}
