package output

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.putnami.dev/cli/model/jobs"
)

// stripStepSuffix removes the pipeline step suffix: "build~generate" → "build".
func stripStepSuffix(name string) string {
	if idx := strings.IndexByte(name, '~'); idx >= 0 {
		return name[:idx]
	}
	return name
}

// stepName extracts the step suffix: "build~transpile" → "transpile".
// Returns "" for single-step jobs (no ~ separator).
func stepName(jobName string) string {
	if idx := strings.IndexByte(jobName, '~'); idx >= 0 {
		return jobName[idx+1:]
	}
	return ""
}

// commandName returns the umbrella command for a job: "build~generate" → "build".
func commandName(job *jobs.ScheduledJob) string {
	return stripStepSuffix(job.JobDef.Name)
}

// commandRank gives a canonical ordering for well-known commands so columns
// appear in a stable, intuitive sequence. Unknown commands sort after known
// ones, alphabetically (handled by the caller).
func commandRank(name string) int {
	switch name {
	case "generate":
		return 0
	case "lint":
		return 1
	case "format":
		return 2
	case "test":
		return 3
	case "build":
		return 4
	case "package":
		return 5
	case "publish":
		return 6
	case "deploy":
		return 7
	case "serve":
		return 8
	default:
		return 100
	}
}

// summaryColumnHideRank orders summary cells by how readily they should be
// removed as terminal width tightens. The rendered order stays canonical; this
// priority only chooses which achievements survive on narrower windows.
func summaryColumnHideRank(name string) int {
	switch name {
	case "package":
		return 0
	case "generate", "format", "lint":
		return 2
	case "test":
		return 3
	case "build":
		return 4
	case "publish":
		return 5
	case "deploy", "serve":
		return 6
	default:
		return 2
	}
}

// formatClock renders a duration as mm:ss (or h:mm:ss past an hour), the stable
// fixed-width form used on live project rows. Zero/negative renders the
// not-started placeholder so unstarted rows stay aligned.
func formatClock(d time.Duration) string {
	if d <= 0 {
		return "--:--"
	}
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// formatSessionDuration renders a duration in the compact "4m12s" form used by
// the final summary header.
func formatSessionDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	total := int(d.Round(time.Second).Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// formatThousands renders an integer with thousands separators: 2418 → "2,418".
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// seg is a span of text with an optional ANSI color, used to assemble aligned
// columns whose visible width is independent of the (invisible) color codes.
type seg struct {
	text  string
	color string
}

// segWidth returns the visible rune width of a sequence of segments.
func segWidth(segs []seg) int {
	w := 0
	for _, s := range segs {
		w += utf8.RuneCountInString(s.text)
	}
	return w
}

// padCol renders segments into a left-aligned column of the given visible
// width. Colors wrap only the visible text; padding stays uncolored so columns
// line up regardless of whether color is enabled. Content wider than the column
// is left intact (callers truncate beforehand when a hard cap is required).
func (r *LiveRenderer) padCol(width int, segs ...seg) string {
	var b strings.Builder
	for _, s := range segs {
		if s.color == "" {
			b.WriteString(s.text)
		} else {
			b.WriteString(colorize(s.text, s.color))
		}
	}
	if pad := width - segWidth(segs); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String()
}

// truncate shortens a plain string to at most width runes, appending an ellipsis
// when it had to cut. Used to keep summary cells inside their reserved column.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	runes := []rune(s)
	return string(runes[:width-1]) + "…"
}

// getMetric reads a captured numeric metric for a command.
func getMetric(cmd *commandState, name string) (float64, bool) {
	v, ok := cmd.metrics[name]
	return v, ok
}

// metric returns a captured metric as an int (0 when absent).
func (r *LiveRenderer) metric(cmd *commandState, name string) int {
	return int(cmd.metrics[name])
}

// errorCount returns a lint command's error count, preferring the emitted
// metric and falling back to counted diagnostics.
func (r *LiveRenderer) errorCount(cmd *commandState) int {
	if v, ok := getMetric(cmd, "lint-errors"); ok {
		return int(v)
	}
	return cmd.diagErrors
}

// warningCount returns a lint command's warning count.
func (r *LiveRenderer) warningCount(cmd *commandState) int {
	if v, ok := getMetric(cmd, "lint-warnings"); ok {
		return int(v)
	}
	return cmd.diagWarnings
}

// filesBuilt sums the file-producing build metrics across pipeline steps.
func (r *LiveRenderer) filesBuilt(cmd *commandState) int {
	return r.metric(cmd, "transpiled-files") +
		r.metric(cmd, "compiled-executables") +
		r.metric(cmd, "type-declarations")
}

// coverageOfCommand returns a command's line/statement coverage percentage, if
// it reported one. Both the per-row test cell and the final aggregate read
// coverage through here so they can never disagree about what was measured.
func coverageOfCommand(cmd *commandState) (float64, bool) {
	if cmd == nil {
		return 0, false
	}
	if v, ok := getMetric(cmd, "coverage"); ok {
		return v, true
	}
	if v, ok := getMetric(cmd, "coverage-lines"); ok {
		return v, true
	}
	return 0, false
}

// metricValue reads a numeric metric from a parsed event, tolerating both the
// JSON number form and the string form some emitters use (e.g. byte sizes).
func metricValue(data map[string]any) (float64, bool) {
	switch v := data["value"].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}
