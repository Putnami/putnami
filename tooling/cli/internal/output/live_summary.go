package output

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/cli/model/jobs"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// Finish stops the live loop, freezes the final project table to scrollback,
// and prints the global session summary.
func (r *LiveRenderer) Finish(results map[string]*jobs.JobResult, outcome jobs.SessionOutcome) {
	// Join the redraw goroutine first; afterwards no other goroutine touches
	// renderer state (the scheduler has drained), so the rest runs lock-free.
	r.stopRedraw()

	r.settleOpenCommands(outcome.Aborted)
	if r.lifecycleMode {
		r.clearLiveZone()
		iox.Fprint(r.errOut, ShowCursor)
		r.printLifecycleDiagnostics(results)
		if outcome.Aborted || hasFailedResult(results) {
			r.printFailuresBlock(results)
		}
		return
	}

	// Replace the ephemeral live zone with a permanent project table.
	r.clearLiveZone()
	for _, row := range r.sortedRows() {
		iox.Fprint(r.errOut, r.renderRow(row)+"\n")
	}

	iox.Fprint(r.errOut, ShowCursor)

	r.printFinalSummary(results, outcome)
}

func (r *LiveRenderer) printLifecycleDiagnostics(results map[string]*jobs.JobResult) {
	keys := make([]string, 0, len(results))
	for key := range results {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	text := NewTextRenderer(r.out, r.errOut, TextRendererConfig{})
	for _, key := range keys {
		result := results[key]
		if result == nil || result.Status == string(jobs.TaskStatusFailed) {
			continue
		}
		for _, event := range result.Events {
			if event.Type != jobs.EventTypeDiagnostic {
				continue
			}
			severity, _ := event.Data["severity"].(string)
			switch severity {
			case string(runtimeproto.SeverityWarning), "warn", string(runtimeproto.SeverityError), "fatal":
				text.renderDiagnostic(event)
			}
		}
	}
}

func hasFailedResult(results map[string]*jobs.JobResult) bool {
	for _, result := range results {
		if result != nil && result.Status == string(jobs.TaskStatusFailed) {
			return true
		}
	}
	return false
}

// settleOpenCommands gives every command still pending or running a terminal
// status; the run is over, so unfinished work didn't complete. An aborted
// session settles them as aborted rather than skipped — they were cut off, not
// passed over. It also closes active timing intervals before rows are printed.
func (r *LiveRenderer) settleOpenCommands(aborted bool) {
	unfinished := statusSkipped
	if aborted {
		unfinished = statusAborted
	}
	now := time.Now()
	for _, row := range r.rows {
		for _, cmd := range row.commands {
			if !cmd.settled() {
				if cmd.stepWorst == statusFailed {
					cmd.status = statusFailed
				} else {
					cmd.status = unfinished
				}
				if cmd.endTime.IsZero() {
					cmd.endTime = now
				}
			}
		}
		r.finishAllProjectWork(row, now)
	}
}

// printFinalSummary prints the Session/Projects/Tasks/Failures blocks.
func (r *LiveRenderer) printFinalSummary(results map[string]*jobs.JobResult, outcome jobs.SessionOutcome) {
	total := time.Since(r.start)

	iox.Fprintf(r.errOut, "\n")
	if outcome.Aborted {
		// Say plainly that the plan never ran out. "completed" here would read
		// as a verdict on work that was killed before it could produce one.
		r.line("%s", colorize(sessionAbortHeadline(outcome, total), Yellow))
	} else {
		r.line("Session completed in %s", formatSessionDuration(total))
	}

	r.printProjectsBlock()
	r.printTasksBlock()
	r.printFailuresBlock(results)
	r.printSkippedBlock(results)
	renderVisibleSummaries(r.errOut, results)
	r.printPublishedBlock(results)
	iox.Fprintf(r.errOut, "\n")
}

// sessionAbortHeadline replaces the "completed" headline for a run that was cut
// short, saying who stopped it and how far it got.
func sessionAbortHeadline(outcome jobs.SessionOutcome, total time.Duration) string {
	return fmt.Sprintf("Session %s after %s",
		abortDescription(outcome), formatSessionDuration(total))
}

// printPublishedBlock summarizes the published artifacts without listing every
// registry coordinate, keeping the shared publish version easy to copy.
func (r *LiveRenderer) printPublishedBlock(results map[string]*jobs.JobResult) {
	published := collectPublished(results)
	if len(published) == 0 {
		return
	}
	lines := formatPublishedSummaryLines(summarizePublished(published))

	iox.Fprintf(r.errOut, "\n")
	r.heading("Published:")
	for _, line := range lines {
		r.line("%s", line)
	}
}

// printProjectsBlock prints the by-project status counts.
func (r *LiveRenderer) printProjectsBlock() {
	var tally liveTally
	for _, row := range r.rows {
		tally.add(r.projectStatus(row))
	}

	parts := []string{
		fmt.Sprintf("%d impacted", len(r.rows)),
		fmt.Sprintf("%d succeeded", tally.passed),
		fmt.Sprintf("%d failed", tally.failed),
		fmt.Sprintf("%d skipped", tally.skipped),
	}
	if tally.aborted > 0 {
		parts = append(parts, fmt.Sprintf("%d aborted", tally.aborted))
	}
	if tally.blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", tally.blocked))
	}

	iox.Fprintf(r.errOut, "\n")
	r.heading("Projects:")
	r.line("%s", strings.Join(parts, " · "))
}

// printTasksBlock prints one aggregated line per requested task type.
func (r *LiveRenderer) printTasksBlock() {
	cmds := r.taskCommandOrder()
	if len(cmds) == 0 {
		return
	}
	aggs := r.aggregateTasks()

	labelW := 6
	for _, c := range cmds {
		if len(c) > labelW {
			labelW = len(c)
		}
	}

	iox.Fprintf(r.errOut, "\n")
	r.heading("Tasks:")
	for _, c := range cmds {
		label := c + strings.Repeat(" ", labelW-len(c))
		detail := r.taskLine(c, aggs[c])
		iox.Fprintf(r.errOut, "%s  %s\n", colorize(label, Bold), detail)
	}
}

// taskLine formats the aggregated detail for one task type.
func (r *LiveRenderer) taskLine(cmd string, a *taskAgg) string {
	if a == nil || (a.passed == 0 && a.failed == 0) {
		// A task with nothing to report is only "skipped" if it was never cut
		// off. Saying skipped for a task the user killed would suggest it was
		// never going to run at all.
		if a != nil && a.aborted > 0 {
			return colorize("aborted", Yellow)
		}
		return colorize("skipped", Dim)
	}

	parts := []string{
		fmt.Sprintf("%d passed", a.passed),
		fmt.Sprintf("%d failed", a.failed),
	}
	if a.aborted > 0 {
		parts = append(parts, colorize(fmt.Sprintf("%d aborted", a.aborted), Yellow))
	}
	switch cmd {
	case "lint":
		parts = append(parts,
			fmt.Sprintf("%d errors", a.lintErrors),
			fmt.Sprintf("%d warnings", a.lintWarnings),
		)
	case "test":
		if a.testsTotal > 0 {
			parts = append(parts, fmt.Sprintf("%s tests", formatThousands(a.testsTotal)))
		}
		if a.testsFailed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", a.testsFailed))
		}
		if pct, annotation, ok := a.coverage(); ok {
			parts = append(parts, fmt.Sprintf("coverage %.1f%%%s", pct, annotation))
		}
	case "build":
		if a.filesBuilt > 0 {
			parts = append(parts, fmt.Sprintf("%s files built", formatThousands(a.filesBuilt)))
		}
		if a.artifacts > 0 {
			parts = append(parts, fmt.Sprintf("%d artifacts", a.artifacts))
		}
	default:
		// deploy/publish/etc. carry no metrics; surface skipped work instead.
		if a.skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", a.skipped))
		}
	}
	return strings.Join(parts, " · ")
}

// printFailuresBlock prints a short failure list with summarized cause and
// detail lines pulled from job results.
func (r *LiveRenderer) printFailuresBlock(results map[string]*jobs.JobResult) {
	type failure struct {
		row *projectRow
		cmd *commandState
	}
	var failures []failure
	for _, row := range r.sortedRows() {
		for _, name := range row.commandOrder {
			if cmd := row.commands[name]; cmd != nil && cmd.status == statusFailed {
				failures = append(failures, failure{row, cmd})
			}
		}
	}
	if len(failures) == 0 {
		return
	}

	iox.Fprintf(r.errOut, "\n")
	r.heading("Failures:")
	for _, f := range failures {
		cause := r.failureCause(f.cmd)
		if note := commandReplayNote(results, f.row.id, f.cmd.name); note != "" {
			cause += "  (" + note + ")"
		}
		iox.Fprintf(r.errOut, "%s %s %s %s %s\n",
			colorize(f.row.name, f.row.color),
			colorize("·", Dim), f.cmd.name,
			colorize("·", Dim), cause,
		)
		r.printFailureDetails(results, f.row.id, f.cmd.name)
	}
	r.printFailureFollowupHint(results)
}

// commandReplayNote returns the replay annotation of the first replayed failed
// task of one project command, or "" when none of them was replayed.
func commandReplayNote(results map[string]*jobs.JobResult, projID, cmd string) string {
	keys := make([]string, 0, len(results))
	for key := range results {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result := results[key]
		if result == nil || result.Status != statusFailed || result.ReplayedFailure == nil {
			continue
		}
		if jp, jc := splitJobKey(key); jp == projID && jc == cmd {
			return replayedFailureNote(result)
		}
	}
	return ""
}

func (r *LiveRenderer) printSkippedBlock(results map[string]*jobs.JobResult) {
	type skipped struct {
		project string
		cmd     string
		reason  string
	}

	keys := make([]string, 0, len(results))
	for key := range results {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var skippedJobs []skipped
	for _, key := range keys {
		result := results[key]
		if result.Status != statusSkipped {
			continue
		}
		reason := skippedCause(result)
		if reason == "" {
			continue
		}
		projID, cmd := splitJobKey(key)
		project := projID
		if row := r.rowByID[projID]; row != nil {
			project = row.name
		}
		skippedJobs = append(skippedJobs, skipped{project: project, cmd: cmd, reason: reason})
	}
	if len(skippedJobs) == 0 {
		return
	}

	const maxSkipped = 10
	iox.Fprintf(r.errOut, "\n")
	r.heading("Skipped:")
	for i, s := range skippedJobs {
		if i >= maxSkipped {
			r.line("... and %d more skipped jobs with reasons", len(skippedJobs)-maxSkipped)
			break
		}
		r.line("%s · %s · %s", s.project, s.cmd, s.reason)
	}
}

func skippedCause(result *jobs.JobResult) string {
	if result.Error != nil && result.Error.Message != "" {
		return firstLine(result.Error.Message)
	}
	for _, ev := range result.Events {
		if ev.Type != jobs.EventTypeSummary {
			continue
		}
		if msg, _ := ev.Data["message"].(string); msg != "" {
			return firstLine(msg)
		}
	}
	return ""
}

func firstLine(s string) string {
	return strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
}

// failureCause summarizes why a command failed, preferring structured metrics.
func (r *LiveRenderer) failureCause(cmd *commandState) string {
	switch cmd.name {
	case "test":
		if f := r.metric(cmd, "tests-failed"); f > 0 {
			return fmt.Sprintf("%d failing tests", f)
		}
	case "lint":
		if e := r.errorCount(cmd); e > 0 {
			return fmt.Sprintf("%d errors", e)
		}
	}
	if cmd.failedStep != "" {
		return cmd.failedStep + " failed"
	}
	if cmd.summary != "" {
		return strings.SplitN(cmd.summary, "\n", 2)[0]
	}
	return "failed"
}

// printFailureDetails prints capped error/diagnostic output for a failed
// command, so failures remain inspectable without verbose mode.
func (r *LiveRenderer) printFailureDetails(results map[string]*jobs.JobResult, projID, cmd string) {
	const maxLines = 10
	printed := 0
	truncated := false

	emit := func(line string) bool {
		if printed >= maxLines {
			return false
		}
		iox.Fprintf(r.errOut, "    %s\n", line)
		printed++
		return true
	}

	keys := make([]string, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		result := results[key]
		if result.Status != statusFailed {
			continue
		}
		jp, jc := splitJobKey(key)
		if jp != projID || jc != cmd {
			continue
		}

		for _, ev := range result.Events {
			if ev.Type != jobs.EventTypeDiagnostic {
				continue
			}
			if printed >= maxLines {
				truncated = true
				break
			}
			RenderDiagnosticEvent(r.errOut, ev, "    ", true)
			printed++
		}
		for _, l := range failureLogLines(result.Events) {
			if !emit(l) {
				truncated = true
				break
			}
		}
		if result.Error != nil && result.Error.Message != "" {
			details, generic := splitErrorDetailLines(result.Error.Message)
			for _, l := range details {
				if !emit(l) {
					truncated = true
					break
				}
			}
			if printed == 0 {
				if summary := genericExitSummary(generic); summary != "" {
					emit(summary)
				}
			}
		}
	}

	if printed == 0 {
		iox.Fprintf(r.errOut, "    %s\n", colorize("(no details emitted)", Dim))
		return
	}

	if truncated {
		iox.Fprintf(r.errOut, "    %s\n", colorize("(output truncated)", Dim))
	}
}

func (r *LiveRenderer) printFailureFollowupHint(results map[string]*jobs.JobResult) {
	// A replayed failure re-executed nothing, so the first thing to say is why
	// it came back instantly and how to force the work anyway.
	if anyReplayedFailure(results) {
		iox.Fprintf(r.errOut, "%s\n", colorize(replayedFailureHint, Dim))
	}
	hint := "Hint: re-run with --output=jsonl for structured diagnostics and a complete session artifact."
	iox.Fprintf(r.errOut, "%s\n", colorize(hint, Dim))
}

// splitJobKey splits "projID:command~step" into project ID and command name.
func splitJobKey(key string) (projID, cmd string) {
	idx := strings.IndexByte(key, ':')
	if idx < 0 {
		return key, ""
	}
	return key[:idx], stripStepSuffix(key[idx+1:])
}

// --- task aggregation ---

type taskAgg struct {
	// liveTally supplies passed/failed/skipped/aborted and the one bucketing
	// rule the Projects block also uses.
	liveTally
	lintErrors, lintWarnings int
	testsTotal, testsFailed  int
	coverageSamples          []coverageSample
	coverageLive             bool
	coverageReplaySources    map[string]struct{}
	filesBuilt, artifacts    int
}

type coverageSample struct {
	percentage  float64
	granularity string
	covered     int
	total       int
}

// coverage returns the aggregated coverage percentage. Counts are weighted
// only when every project reports counts at the same granularity; mixing Go
// statements with TypeScript lines falls back to a per-project percentage mean
// instead of silently dropping one language from the aggregate.
func (a *taskAgg) coverage() (float64, string, bool) {
	if len(a.coverageSamples) == 0 {
		return 0, "", false
	}

	granularity := a.coverageSamples[0].granularity
	weighted := granularity != ""
	covered, total := 0, 0
	percentage := 0.0
	for _, sample := range a.coverageSamples {
		percentage += sample.percentage
		if sample.granularity != granularity || sample.total <= 0 {
			weighted = false
		}
		covered += sample.covered
		total += sample.total
	}
	pct := percentage / float64(len(a.coverageSamples))
	if weighted && total > 0 {
		pct = 100 * float64(covered) / float64(total)
	}
	if len(a.coverageReplaySources) == 0 {
		return pct, "", true
	}
	sources := make([]string, 0, len(a.coverageReplaySources))
	for source := range a.coverageReplaySources {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	if a.coverageLive {
		return pct, " (live + replayed: validation @ " + strings.Join(sources, ", ") + ")", true
	}
	return pct, " (replayed: validation @ " + strings.Join(sources, ", ") + ")", true
}

// aggregateTasks rolls up per-command metrics across all projects.
func (r *LiveRenderer) aggregateTasks() map[string]*taskAgg {
	aggs := make(map[string]*taskAgg)
	addCoverage := func(a *taskAgg, summary runtimeproto.CoverageSummary, replay *CoverageReplay) {
		a.coverageSamples = append(a.coverageSamples, coverageSample{
			percentage:  summary.Percentage,
			granularity: summary.Granularity,
			covered:     summary.Covered,
			total:       summary.Total,
		})
		if replay == nil {
			a.coverageLive = true
			return
		}
		if a.coverageReplaySources == nil {
			a.coverageReplaySources = make(map[string]struct{})
		}
		source := strings.TrimSpace(replay.Source.Revision)
		if source == "" {
			source = "session " + replay.SessionID
		}
		a.coverageReplaySources[source] = struct{}{}
	}
	for _, row := range r.rows {
		for name, cmd := range row.commands {
			a := aggs[name]
			if a == nil {
				a = &taskAgg{}
				aggs[name] = a
			}
			a.add(cmd.status)

			a.lintErrors += r.errorCount0(cmd)
			a.lintWarnings += r.warnings0(cmd)
			// Prefer the typed testSummary payload (protocol/runtime);
			// fall back to metrics for jobs not yet emitting it.
			if cmd.testSummary != nil {
				a.testsTotal += cmd.testSummary.Total
				a.testsFailed += cmd.testSummary.Failed
			} else {
				a.testsTotal += r.metric(cmd, "tests-total")
				a.testsFailed += r.metric(cmd, "tests-failed")
			}
			a.filesBuilt += r.filesBuilt(cmd)
			a.artifacts += cmd.artifacts

			if cs := cmd.coverageSummary; cs != nil {
				addCoverage(a, *cs, nil)
			} else if ct := r.metric(cmd, "coverage-statements-total"); ct > 0 {
				covered := r.metric(cmd, "coverage-statements-covered")
				pct := 100 * float64(covered) / float64(ct)
				if reported, _, ok := r.coverageOf(cmd); ok {
					pct = reported
				}
				addCoverage(a, runtimeproto.CoverageSummary{
					Percentage:  pct,
					Granularity: runtimeproto.CoverageStatements,
					Covered:     covered,
					Total:       ct,
				}, nil)
			} else if pct, granularity, ok := r.coverageOf(cmd); ok {
				addCoverage(a, runtimeproto.CoverageSummary{Percentage: pct, Granularity: granularity}, nil)
			} else if replay := cmd.replayedCoverage; replay != nil {
				addCoverage(a, replay.Entry.Summary, replay)
			}
		}
	}
	return aggs
}

// errorCount0/warnings0 return lint counts only for lint commands (avoids
// counting unrelated diagnostics from other commands).
func (r *LiveRenderer) errorCount0(cmd *commandState) int {
	if cmd.name != "lint" {
		return 0
	}
	return r.errorCount(cmd)
}

func (r *LiveRenderer) warnings0(cmd *commandState) int {
	if cmd.name != "lint" {
		return 0
	}
	return r.warningCount(cmd)
}

// coverageOf returns a single command's coverage percentage and granularity if
// available. It shares coverageOfCommand's metric precedence with the per-row
// test cell, and only adds which unit the number is expressed in.
func (r *LiveRenderer) coverageOf(cmd *commandState) (float64, string, bool) {
	pct, ok := coverageOfCommand(cmd)
	if !ok {
		return 0, "", false
	}
	if _, statements := getMetric(cmd, "coverage"); statements {
		return pct, runtimeproto.CoverageStatements, true
	}
	return pct, runtimeproto.CoverageLines, true
}

// taskCommandOrder returns the distinct command names across all projects in
// canonical order.
func (r *LiveRenderer) taskCommandOrder() []string {
	seen := make(map[string]bool)
	var cmds []string
	for _, row := range r.rows {
		for name := range row.commands {
			if !seen[name] {
				seen[name] = true
				cmds = append(cmds, name)
			}
		}
	}
	sort.SliceStable(cmds, func(i, j int) bool {
		ri, rj := commandRank(cmds[i]), commandRank(cmds[j])
		if ri != rj {
			return ri < rj
		}
		return cmds[i] < cmds[j]
	})
	return cmds
}

// --- small output helpers ---

func (r *LiveRenderer) heading(s string) {
	iox.Fprintf(r.errOut, "%s\n", colorize(s, Bold))
}

func (r *LiveRenderer) line(format string, args ...any) {
	iox.Fprintf(r.errOut, format+"\n", args...)
}
