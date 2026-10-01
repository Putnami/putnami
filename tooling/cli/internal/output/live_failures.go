package output

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"go.putnami.dev/cli/model/jobs"
)

// maxFailurePaneRows caps the rolling failure pane; older failures scroll out of
// the pane (the end-of-run Failures block still lists every one).
const maxFailurePaneRows = 5

// liveFailure is one settled command failure captured for the rolling pane at
// the bottom of the live zone, so --continue-on-error runs surface what broke
// as it happens instead of only in the end-of-run recap.
type liveFailure struct {
	project string
	color   string
	cmd     string
	cause   string
}

// recordFailure appends a compact entry for a command that just settled failed.
// The full diagnostics still print in the final Failures: block.
func (r *LiveRenderer) recordFailure(row *projectRow, cmd *commandState) {
	cause := cmd.firstDiag
	if cause == "" {
		cause = r.failureCause(cmd)
	}
	r.failures = append(r.failures, liveFailure{
		project: row.name,
		color:   row.color,
		cmd:     cmd.name,
		cause:   cause,
	})
}

// firstDiagnosticLine extracts a one-line "file:line code message" summary from
// the first error diagnostic in a failed job's events, or "" when none was
// emitted.
func firstDiagnosticLine(result *jobs.JobResult) string {
	if result == nil {
		return ""
	}
	for _, ev := range result.Events {
		if ev.Type != jobs.EventTypeDiagnostic {
			continue
		}
		if sev, _ := ev.Data["severity"].(string); sev != "error" {
			continue
		}
		msg, _ := ev.Data["message"].(string)
		code, _ := ev.Data["code"].(string)
		loc := extractLocation(ev.Data)

		var b strings.Builder
		if loc.file != "" {
			b.WriteString(loc.file)
			if loc.line > 0 {
				fmt.Fprintf(&b, ":%d", loc.line)
			}
			b.WriteByte(' ')
		}
		if code != "" {
			b.WriteString(code)
			b.WriteByte(' ')
		}
		b.WriteString(msg)
		return strings.TrimSpace(b.String())
	}
	return ""
}

// failurePaneHeight is the number of live-zone lines the failure pane occupies
// (a header rule plus one line per shown failure), used to budget project rows.
func (r *LiveRenderer) failurePaneHeight() int {
	if len(r.failures) == 0 {
		return 0
	}
	return min(len(r.failures), maxFailurePaneRows) + 1 // shown rows + header rule
}

// renderFailurePane builds the rolling failure pane pinned at the bottom of the
// live zone, showing the most-recent failures (older ones scroll out) so a
// --continue-on-error run shows what just broke.
func (r *LiveRenderer) renderFailurePane() []string {
	total := len(r.failures)
	if total == 0 {
		return nil
	}
	shown := r.failures
	if total > maxFailurePaneRows {
		shown = r.failures[total-maxFailurePaneRows:]
	}

	head := "failures"
	if total > len(shown) {
		head = fmt.Sprintf("failures (%d total)", total)
	}
	lines := []string{r.failurePaneHeader(head)}

	// "  ✗ name cmd " — leading width before the cause text.
	leadWidth := 2 + 1 + 1 + r.nameWidth + 1 + r.statusWidth + 1
	for _, f := range shown {
		name := r.padCol(r.nameWidth, seg{truncate(f.project, r.nameWidth), f.color})
		cmd := r.padCol(r.statusWidth, seg{truncate(f.cmd, r.statusWidth), Dim})
		cause := truncate(f.cause, max(0, r.width-leadWidth))
		lines = append(lines, "  "+colorize("✗", Red)+" "+name+" "+cmd+" "+cause)
	}
	return lines
}

// failurePaneHeader draws the dim rule that separates the pane from the table.
func (r *LiveRenderer) failurePaneHeader(head string) string {
	prefix := "  ── " + head + " "
	rule := max(0, min(r.width-utf8.RuneCountInString(prefix), 40))
	return colorize(prefix+strings.Repeat("─", rule), Dim)
}
