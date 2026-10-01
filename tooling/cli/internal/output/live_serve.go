package output

import (
	"strings"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// renderServeLog tails a log event to the scrollback above the live table and
// captures any server URL for inline display on the project row. It clears the
// live zone, writes the log line, and resets the frame so the next redraw
// repaints the table below it. Must be called with r.mu held.
func (r *LiveRenderer) renderServeLog(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	if msg, _ := event.Data["message"].(string); msg != "" {
		if idx := strings.Index(msg, "http://"); idx >= 0 {
			url := strings.TrimRight(msg[idx:], " \t\n\r")
			if row := r.rowByID[job.Project.ID]; row != nil {
				row.serveStatus = url
			}
		}
	}

	cmd := commandName(job)
	projColor := Dim
	if row := r.rowByID[job.Project.ID]; row != nil {
		projColor = row.color
	}
	label := serveLogLabel(cmd, job.Project.Name, false, projColor)
	line := FormatLogEvent(event, false, label)
	if line == "" {
		return
	}
	r.clearLiveZone()
	iox.Fprint(r.errOut, line+"\n")
}
