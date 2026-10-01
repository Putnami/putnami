package output

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.putnami.dev/tooling/cli/internal/iox"
)

// redraw repaints the live zone by diffing a freshly built frame against the
// last one. Must be called with r.mu held.
func (r *LiveRenderer) redraw() {
	r.paint(r.renderFrame(time.Now()))
}

// renderFrame builds the live zone as one string per line: the counts header,
// the global progress bar, the project rows (overflow-trimmed), an optional
// overflow footer, and the rolling failure pane. paint then rewrites only the
// lines that changed.
func (r *LiveRenderer) renderFrame(now time.Time) []string {
	r.spinnerFrame = spinnerFrameAt(now.Sub(r.start))

	frame := make([]string, 0, len(r.rows)+4)
	frame = append(frame, r.renderHeaderAt(now))

	rows := r.sortedRows()
	rows, hidden := r.liveRowsAt(rows, r.liveRowBudget(), now)
	r.markVisibleRows(rows)
	for _, row := range rows {
		frame = append(frame, r.renderRow(row))
	}
	if hidden > 0 {
		frame = append(frame, colorize(fmt.Sprintf("  … %d more", hidden), Dim))
	}
	frame = append(frame, r.renderFailurePane()...)
	return frame
}

// liveRowBudget is how many project rows the live zone can show. It reserves
// the header, a possible overflow footer, the failure pane, and a 2-line safety
// margin so the live zone stays shorter than the terminal — which keeps the
// header on screen even when the terminal under-/over-reports its height or has
// scrollback above the zone.
func (r *LiveRenderer) liveRowBudget() int {
	reserved := 1 /*header*/ + 1 /*overflow footer*/ + 2 /*safety margin*/ + r.failurePaneHeight()
	return r.height - reserved
}

// paint writes frame to the terminal in a single batched write, rewriting only
// the lines that differ from the previous frame and overwriting them in place
// rather than clearing the whole zone first. Repainting just the changed lines
// in one write is what removes the flicker the old full-clear redraw produced,
// which is what lets the cadence run fast enough to animate the running spinner.
// Must be called with r.mu held.
func (r *LiveRenderer) paint(frame []string) {
	if framesEqual(frame, r.prevFrame) {
		return
	}

	var b strings.Builder
	if n := len(r.prevFrame); n > 0 {
		fmt.Fprintf(&b, "\033[%dA", n) // move to the top of the live zone
	}
	for i, line := range frame {
		if i < len(r.prevFrame) && r.prevFrame[i] == line {
			b.WriteByte('\n') // unchanged: step past without repainting
			continue
		}
		b.WriteByte('\r')
		b.WriteString(line)
		b.WriteString(ClearToEOL) // drop any leftover tail from a longer prior line
		b.WriteByte('\n')
	}
	// Frame shrank: blank the orphaned trailing lines, then move the cursor back
	// up so it rests just below the new (shorter) zone for the next paint.
	if extra := len(r.prevFrame) - len(frame); extra > 0 {
		for range extra {
			b.WriteString("\r" + ClearToEOL + "\n")
		}
		fmt.Fprintf(&b, "\033[%dA", extra)
	}

	r.prevFrame = frame
	iox.Fprint(r.errOut, b.String())
}

func framesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// liveRows chooses the project rows to show inside a constrained terminal.
// Final output still prints every row in stable order; the live view gives
// scarce vertical space to unfinished work and recently completed visible rows
// so late-run progress does not disappear behind a long list of completed
// projects. The hidden count only reports unfinished, failed, or retained
// completed projects that are outside the selected rows.
func (r *LiveRenderer) liveRowsAt(rows []*projectRow, maxRows int, now time.Time) ([]*projectRow, int) {
	if maxRows < 1 || len(rows) <= maxRows {
		return rows, 0
	}

	selected := make([]*projectRow, len(rows))
	copy(selected, rows)
	sort.SliceStable(selected, func(i, j int) bool {
		pi := r.liveRowPriorityAt(selected[i], now)
		pj := r.liveRowPriorityAt(selected[j], now)
		if pi != pj {
			return pi < pj
		}
		return selected[i].order < selected[j].order
	})
	selected = selected[:maxRows]
	visible := make(map[*projectRow]struct{}, len(selected))
	for _, row := range selected {
		visible[row] = struct{}{}
	}
	hidden := 0
	for _, row := range rows {
		if _, ok := visible[row]; ok {
			continue
		}
		if r.liveOverflowStatusAt(row, now) {
			hidden++
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		return selected[i].order < selected[j].order
	})
	return selected, hidden
}

func (r *LiveRenderer) liveRowPriorityAt(row *projectRow, now time.Time) int {
	status := r.projectStatus(row)
	switch {
	case status == statusFailed:
		return 0
	case status == statusDone && row.doneVisibleUntil.After(now):
		return 1
	case status == statusRunning:
		return 2
	case status == statusBlocked:
		return 3
	case status == statusQueued:
		return 4
	default:
		return 5
	}
}

func (r *LiveRenderer) liveOverflowStatusAt(row *projectRow, now time.Time) bool {
	status := r.projectStatus(row)
	switch status {
	case statusDone:
		return row.doneVisibleUntil.After(now)
	case statusSkipped, statusAborted:
		return false
	default:
		return true
	}
}

func (r *LiveRenderer) markVisibleRows(rows []*projectRow) {
	visible := make(map[*projectRow]struct{}, len(rows))
	for _, row := range rows {
		visible[row] = struct{}{}
	}
	for _, row := range r.rows {
		_, row.visible = visible[row]
	}
}

// clearLiveZone erases the live zone, leaving the cursor at the top of where
// the zone was and resetting the frame so the next paint repaints fresh. Used
// by serve-mode scrollback and by Finish. Must be called with r.mu held.
func (r *LiveRenderer) clearLiveZone() {
	for range r.prevFrame {
		iox.Fprint(r.errOut, MoveUp+ClearLine)
	}
	r.prevFrame = nil
}

// renderHeader builds the one-line session counter:
// "impacted: N projects · elapsed: MM:SS · running: R · done: D · blocked: B · failed: F".
func (r *LiveRenderer) renderHeaderAt(now time.Time) string {
	var impacted, running, done, blocked, failed, queued, skipped int
	for _, row := range r.rows {
		impacted++
		switch r.projectStatus(row) {
		case statusRunning:
			running++
		case statusDone:
			done++
		case statusBlocked:
			blocked++
		case statusFailed:
			failed++
		case statusQueued:
			queued++
		case statusSkipped:
			skipped++
		}
	}

	parts := []string{
		fmt.Sprintf("impacted: %d projects", impacted),
		fmt.Sprintf("elapsed: %s", r.sessionElapsed(now)),
		fmt.Sprintf("running: %d", running),
		fmt.Sprintf("done: %d", done),
		fmt.Sprintf("blocked: %d", blocked),
		fmt.Sprintf("failed: %d", failed),
	}
	if queued > 0 {
		parts = append(parts, fmt.Sprintf("queued: %d", queued))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("skipped: %d", skipped))
	}
	line := strings.Join(parts, " · ")
	plainW := utf8.RuneCountInString(line)

	// On a narrow terminal, keep the counts and drop the bar before wrapping.
	if plainW >= r.width {
		return colorize(truncate(line, r.width), Dim)
	}
	header := colorize(line, Dim)
	if mini := r.renderMiniProgress(now, r.width-plainW-2); mini != "" {
		header += "  " + mini
	}
	return header
}

func (r *LiveRenderer) sessionElapsed(now time.Time) string {
	if r.start.IsZero() {
		return "--:--"
	}
	return formatClock(now.Sub(r.start))
}

// renderRow builds one project line with aligned columns:
// "{icon} {name} {status} {elapsed}  {summary cells}".
func (r *LiveRenderer) renderRow(row *projectRow) string {
	status := r.projectStatus(row)

	name := seg{truncate(row.name, r.nameWidth), row.color}
	label := r.statusLabel(row, status)
	label.text = truncate(label.text, r.statusWidth)

	var b strings.Builder
	b.WriteString(r.padCol(1, r.statusIcon(status, row.color)))
	b.WriteByte(' ')
	b.WriteString(r.padCol(r.nameWidth, name))
	b.WriteByte(' ')
	b.WriteString(r.padCol(r.statusWidth, label))
	b.WriteByte(' ')
	b.WriteString(r.padCol(5, seg{r.elapsed(row), Dim}))
	b.WriteString("  ")
	b.WriteString(r.renderSummary(row, status))
	return strings.TrimRight(b.String(), " ")
}

// liveSpinnerFrames is the braille spinner shown on running rows — ten frames of
// real motion, which reads as smooth rotation where a three-level brightness
// pulse only flickered.
var liveSpinnerFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

const spinnerInterval = 80 * time.Millisecond

// spinnerFrameAt selects the spinner frame from elapsed run time, so the spin
// rate stays the same regardless of the redraw interval.
func spinnerFrameAt(elapsed time.Duration) int {
	if elapsed <= 0 {
		return 0
	}
	return int(elapsed/spinnerInterval) % len(liveSpinnerFrames)
}

// spinnerGlyph returns the current running-row spinner frame.
func (r *LiveRenderer) spinnerGlyph() string {
	return string(liveSpinnerFrames[r.spinnerFrame%len(liveSpinnerFrames)])
}

// statusIcon returns the leading glyph for a project status.
func (r *LiveRenderer) statusIcon(status, projColor string) seg {
	switch status {
	case statusRunning:
		return seg{r.spinnerGlyph(), projColor}
	case statusDone:
		return seg{"✓", Green}
	case statusFailed:
		return seg{"✗", Red}
	case statusAborted:
		return seg{"⊘", Yellow}
	case statusSkipped:
		return seg{"·", Dim}
	default: // queued, blocked
		return seg{"○", Dim}
	}
}

// statusLabel returns the status/phase text: the running command name while
// active, otherwise the status word.
func (r *LiveRenderer) statusLabel(row *projectRow, status string) seg {
	if status == statusRunning {
		if cmd := r.activeCommand(row); cmd != "" {
			return seg{cmd, ""}
		}
		return seg{statusRunning, ""}
	}
	color := Dim
	switch status {
	case statusFailed:
		color = Red
	case statusAborted:
		// Not dim: aborted work is unresolved, not quietly settled.
		color = Yellow
	}
	return seg{status, color}
}

// activeCommand returns the lowest-rank running command name for a project.
func (r *LiveRenderer) activeCommand(row *projectRow) string {
	best := ""
	bestRank := 1 << 30
	for name, cmd := range row.commands {
		if cmd.status == statusRunning {
			if rk := commandRank(name); rk < bestRank {
				bestRank, best = rk, name
			}
		}
	}
	return best
}

// elapsed renders the project's mm:ss clock, or the not-started placeholder.
func (r *LiveRenderer) elapsed(row *projectRow) string {
	if row.workDuration <= 0 && row.activeJobs == 0 {
		return "--:--"
	}
	return formatClock(r.projectElapsed(row, time.Now()))
}

func (r *LiveRenderer) projectElapsed(row *projectRow, now time.Time) time.Duration {
	elapsed := row.workDuration
	if row.activeJobs > 0 && !row.activeStart.IsZero() && now.After(row.activeStart) {
		elapsed += now.Sub(row.activeStart)
	}
	return elapsed
}

// renderSummary builds the per-command summary cells, the blocked-by hint, or
// the captured server URL in serve mode.
func (r *LiveRenderer) renderSummary(row *projectRow, status string) string {
	if r.serveMode && row.serveStatus != "" {
		return colorize("on "+row.serveStatus, Dim)
	}

	if status == statusBlocked {
		if by := r.blockedBy(row); by != "" {
			return colorize("blocked by "+by, Dim)
		}
	}

	cols := r.visibleColumns()
	cells := make([]string, 0, len(cols.names))
	for _, name := range cols.names {
		cells = append(cells, r.summaryCell(row, name))
	}
	out := joinCells(cells)
	if cols.hidden > 0 {
		out += colorize(fmt.Sprintf(" +%d", cols.hidden), Dim)
	}
	return out
}

// joinCells concatenates fixed-width summary cells, using the " · " separator
// only between two filled cells. A blank cell (a column the project doesn't use,
// e.g. coverage) gets plain spacing so no orphan separator appears, while the
// fixed widths keep every column aligned across rows.
func joinCells(cells []string) string {
	var b strings.Builder
	for i, c := range cells {
		if i > 0 {
			if strings.TrimSpace(cells[i-1]) == "" || strings.TrimSpace(c) == "" {
				b.WriteString("   ")
			} else {
				b.WriteString(" · ")
			}
		}
		b.WriteString(c)
	}
	return b.String()
}

type columnFit struct {
	names  []string
	hidden int
}

// visibleColumns selects which summary columns fit the terminal width. It hides
// lower-value cells first while preserving canonical display order for the cells
// that remain.
//
// The fit depends only on run-fixed layout state, so it is identical for every
// row in every frame. renderSummary calls this once per visible row on each
// redraw, so the result is memoized and recomputed only when the width changes
// (a terminal resize). Callers run under r.mu (the redraw loop) or after the
// redraw goroutine has stopped (Finish), so the cache needs no extra locking.
func (r *LiveRenderer) visibleColumns() columnFit {
	if r.summaryFit != nil && r.summaryFitWidth == r.width {
		return *r.summaryFit
	}
	fit, _ := r.visibleColumnsForBudget(r.width - r.leadingWidth())
	r.summaryFit = &fit
	r.summaryFitWidth = r.width
	return fit
}

func (r *LiveRenderer) visibleColumnsForBudget(budget int) (columnFit, int) {
	candidates := make([]string, len(r.colOrder))
	copy(candidates, r.colOrder)

	for len(candidates) > 0 {
		hidden := len(r.colOrder) - len(candidates)
		used := r.columnsWidth(candidates)
		if hidden > 0 {
			used += len(fmt.Sprintf(" +%d", hidden))
		}
		if used <= budget {
			return columnFit{names: candidates, hidden: hidden}, used
		}
		candidates = dropNextSummaryColumn(candidates)
	}

	hidden := len(r.colOrder)
	used := 0
	if hidden > 0 {
		used = len(fmt.Sprintf(" +%d", hidden))
	}
	return columnFit{hidden: hidden}, used
}

func (r *LiveRenderer) columnsWidth(cols []string) int {
	total := 0
	for i, name := range cols {
		total += r.colWidth[name]
		if i > 0 {
			total += 3
		}
	}
	return total
}

func dropNextSummaryColumn(cols []string) []string {
	if len(cols) == 0 {
		return cols
	}
	dropIdx := 0
	dropRank := summaryColumnHideRank(cols[0])
	for i := 1; i < len(cols); i++ {
		if rank := summaryColumnHideRank(cols[i]); rank < dropRank {
			dropIdx, dropRank = i, rank
		}
	}
	next := make([]string, 0, len(cols)-1)
	next = append(next, cols[:dropIdx]...)
	next = append(next, cols[dropIdx+1:]...)
	return next
}

// leadingWidth is the visible width of the fixed columns before the summary.
func (r *LiveRenderer) leadingWidth() int {
	return r.leadingWidthForName(r.nameWidth)
}

func (r *LiveRenderer) leadingWidthForName(nameWidth int) int {
	// icon + space + name + space + status + space + elapsed(5) + two spaces
	return nameWidth + r.fixedLeadingWidthWithoutName()
}

func (r *LiveRenderer) fixedLeadingWidthWithoutName() int {
	// icon + space + space-after-name + status + space + elapsed(5) + two spaces
	return 1 + 1 + 1 + r.statusWidth + 1 + 5 + 2
}

// summaryCell renders a single command's compact cell, padded to its column.
func (r *LiveRenderer) summaryCell(row *projectRow, col string) string {
	width := r.colWidth[col]

	cmd := row.commands[col]
	if cmd == nil {
		return strings.Repeat(" ", width)
	}

	label := seg{cmd.name, Dim}
	glyph, value := r.cellValue(cmd)

	segs := []seg{label, {" ", ""}}
	if glyph.text != "" {
		segs = append(segs, glyph, seg{" ", ""})
	}
	// Truncate the value to whatever space remains in the reserved column.
	avail := width - segWidth(segs)
	value.text = truncate(value.text, avail)
	segs = append(segs, value)
	return r.padCol(width, segs...)
}

// cellValue returns the status glyph and value text for a command cell.
func (r *LiveRenderer) cellValue(cmd *commandState) (glyph, value seg) {
	if cmd.displayCached {
		value := r.cachedValue(cmd)
		if cmd.alwaysRunNote != "" {
			value += " " + cmd.alwaysRunNote
		}
		return seg{"↻", Dim}, seg{value, Dim}
	}
	switch cmd.status {
	case "":
		return seg{}, seg{"queued", Dim}
	case statusRunning:
		return seg{}, seg{r.runningValue(cmd), Dim}
	case statusSkipped:
		if cmd.summary != "" {
			return seg{}, seg{strings.SplitN(cmd.summary, "\n", 2)[0], Dim}
		}
		return seg{}, seg{"–", Dim}
	case statusAborted:
		return seg{"⊘", Yellow}, seg{"aborted", Yellow}
	case statusCoalesced:
		// Coalescing is the outcome, independent of any metrics or summary replayed
		// from the published result. Those events still feed final aggregation, but
		// must not replace the distinct live-cell marker.
		return seg{}, seg{statusCoalesced, Dim}
	case statusFailed:
		return seg{"✗", Red}, seg{r.failedValue(cmd), ""}
	default: // success
		return seg{"✓", Green}, seg{r.successValue(cmd), ""}
	}
}

// runningValue summarizes a command still in flight.
func (r *LiveRenderer) runningValue(cmd *commandState) string {
	if cmd.progress != nil && cmd.progress.total > 0 {
		return fmt.Sprintf("%.0f/%.0f", cmd.progress.current, cmd.progress.total)
	}
	if cmd.progress != nil && cmd.progress.label != "" {
		return cmd.progress.label
	}
	if cmd.phase != "" {
		return cmd.phase
	}
	return "…"
}

// cachedValue shows the restored prior result when cached events carried one,
// falling back to the cache marker for status-only entries.
func (r *LiveRenderer) cachedValue(cmd *commandState) string {
	if value := r.successValue(cmd); value != "ok" {
		return value
	}
	if cmd.summary != "" {
		return strings.SplitN(cmd.summary, "\n", 2)[0]
	}
	return statusCached
}

// successValue builds the compact summary for a completed command from its
// metrics, falling back to a simple marker when no metric is available.
func (r *LiveRenderer) successValue(cmd *commandState) string {
	switch cmd.name {
	case "lint":
		if w := r.warningCount(cmd); w > 0 {
			return fmt.Sprintf("%d warn", w)
		}
		return "ok"
	case "test":
		value := "ok"
		if total := r.metric(cmd, "tests-total"); total > 0 {
			value = fmt.Sprintf("%d/%d", r.metric(cmd, "tests-passed"), total)
		}
		// Coverage rides in the test cell rather than a column of its own so it can
		// never be dropped independently of the counts it qualifies — a percentage
		// that disappears on a narrow terminal is how a drop goes unnoticed.
		if pct, ok := coverageOfCommand(cmd); ok {
			value += fmt.Sprintf(" %.1f%%", pct)
		}
		return value
	case "build":
		if files := r.filesBuilt(cmd); files > 0 {
			return fmt.Sprintf("%d files", files)
		}
		return "ok"
	default:
		return "ok"
	}
}

// failedValue builds the compact summary for a failed command.
func (r *LiveRenderer) failedValue(cmd *commandState) string {
	switch cmd.name {
	case "lint":
		if e := r.errorCount(cmd); e > 0 {
			return fmt.Sprintf("%d err", e)
		}
		return "failed"
	case "test":
		if f := r.metric(cmd, "tests-failed"); f > 0 {
			return fmt.Sprintf("%d failed", f)
		}
		return "failed"
	default:
		return "failed"
	}
}

// projectStatus derives a project's display status from its commands and deps.
func (r *LiveRenderer) projectStatus(row *projectRow) string {
	if len(row.commands) == 0 {
		return statusQueued
	}
	anyRunning, anyFailed, anyStarted, allSettled := false, false, false, true
	for _, cmd := range row.commands {
		if cmd.status == statusRunning {
			anyRunning = true
		}
		if cmd.status != "" {
			anyStarted = true
		}
		if !cmd.settled() {
			allSettled = false
		}
		if cmd.status == statusFailed {
			anyFailed = true
		}
	}

	switch {
	case anyRunning:
		return statusRunning
	case !anyStarted:
		if r.blockedBy(row) != "" {
			return statusBlocked
		}
		return statusQueued
	case anyFailed:
		// Surface a failure as soon as nothing is actively running, even if
		// later tasks remain (they will settle as skipped/canceled).
		return statusFailed
	case !allSettled:
		// Some commands done, more pending: the project is actively progressing.
		return statusRunning
	}

	// All settled with no failures. Any cut-off command makes the project
	// aborted no matter how its siblings fared — the project did not finish.
	// Skipped stands only when every command was skipped.
	allSkipped := true
	for _, cmd := range row.commands {
		if cmd.status == statusAborted {
			return statusAborted
		}
		if cmd.status != statusSkipped {
			allSkipped = false
		}
	}
	if allSkipped {
		return statusSkipped
	}
	return statusDone
}

// blockedBy returns the name of the first not-yet-finished upstream project, or
// "" when nothing is blocking.
func (r *LiveRenderer) blockedBy(row *projectRow) string {
	deps := make([]*projectRow, 0, len(row.deps))
	for id := range row.deps {
		if dep := r.rowByID[id]; dep != nil {
			deps = append(deps, dep)
		}
	}
	sort.SliceStable(deps, func(i, j int) bool { return deps[i].order < deps[j].order })
	for _, dep := range deps {
		if !r.projectSettled(dep) {
			return dep.name
		}
	}
	return ""
}
