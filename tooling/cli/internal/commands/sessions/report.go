package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// reportJobsShown bounds the job lines the human render prints. The document
// itself is bounded at protocolcli.ReportMaxJobs; a terminal reader wants the
// head of that already-prioritized list (failures first), not all of it, and
// the count of what was left out is printed rather than dropped.
const reportJobsShown = 12

// reportJobDiagnosticsShown bounds the diagnostics printed under one job line.
const reportJobDiagnosticsShown = 3

// ReportShow prints the report a finished run recorded, either the newest one
// (sessionID == "") or the one belonging to an exact session id.
//
// This is the contracted handoff the report exists for: before it,
// a consumer wanting a run's bilan had to guess which session directory was new
// and scrape it. Two facts about that guess survive here and are worth stating
// to whoever reads this output:
//
//   - A run that never reached finalize — a crash, a SIGKILL — writes no
//     report (a gracefully interrupted run does, with outcome "aborted").
//     "Newest" can therefore belong to an EARLIER run, which is why a consumer
//     binding a report to a commit verifies git.sha rather than trusting
//     recency.
//   - The newest lookup withholds MCP-origin reports (see ReportStore.Latest):
//     an agent's background run must not answer "what did my last run do".
func ReportShow(wsRoot, sessionID, outputFormat string) error {
	store := workspace_state.NewReportStore(wsRoot)

	recorded, err := loadReport(store, sessionID)
	if err != nil {
		return err
	}

	if protocolcli.OutputMode(outputFormat).IsStructured() {
		// DELIBERATE divergence from the structured-command norm: this command
		// writes the recorded document RAW instead of wrapping it in a result
		// envelope, and the dispatcher registers it as such (registerRawCommand).
		// Every other structured command's payload exists only inside the
		// envelope; a report is itself a versioned, validated contract document,
		// and a consumer that POSTs it — cloud's CI runner — must post exactly the
		// bytes the schema describes. Wrapping it would make every consumer unwrap
		// it first, and re-serializing it would drop members a newer producer
		// added.
		raw := recorded.Raw
		// jsonl is a line-framed mode: every structured command emits exactly one
		// compact line there, and this command's own FAILURES do too (the shared
		// envelope), so its success must not be the one multi-line document in a
		// stream a consumer reads line by line. Compaction only re-frames the
		// recorded bytes — no decode through this binary's structs, so members a
		// newer producer added survive.
		if protocolcli.OutputMode(outputFormat) == protocolcli.OutputJSONL {
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err == nil {
				raw = compact.Bytes()
			}
		}
		if _, err := iox.Stdout().Write(raw); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			iox.Fprintln(os.Stdout)
		}
		return nil
	}

	renderReportText(recorded.Report)
	return nil
}

// loadReport resolves the requested report. An absent one is an ordinary
// FAILURE (exit 1), not a usage error: asking for a run's report is a
// well-formed request, and "the run never finished, so it recorded nothing" is
// an answer about the workspace rather than about the command line.
func loadReport(store *workspace_state.ReportStore, sessionID string) (*workspace_state.RecordedReport, error) {
	if sessionID == "" {
		recorded := store.Latest()
		if recorded == nil {
			return nil, fmt.Errorf("no run report recorded in %s\n  A run records its report when it finishes, so an interrupted run leaves none", store.Root())
		}
		return recorded, nil
	}
	recorded, err := store.Read(sessionID)
	if err != nil {
		return nil, fmt.Errorf("no report for session %s: %w", sessionID, err)
	}
	return recorded, nil
}

// renderReportText renders the report for a human: the run's verdict, the
// per-command synthesis, and the head of the report's own prioritized job list.
func renderReportText(report *protocolcli.ReportFile) {
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  Report:   %s\n", report.SessionID)
	iox.Fprintf(os.Stdout, "  Started:  %s\n", formatTimestamp(report.StartTime))
	iox.Fprintf(os.Stdout, "  Duration: %s\n", formatDurationMs64(report.Run.DurationMs))
	iox.Fprintf(os.Stdout, "  Outcome:  %s (exit %d)\n", report.Run.Outcome, report.Run.ExitCode)
	iox.Fprintf(os.Stdout, "  Origin:   %s%s\n", report.Origin, enforceCoverageNote(report.EnforceCoverage))
	if report.Git != nil {
		iox.Fprintf(os.Stdout, "  Commit:   %s\n", reportCommitLine(report.Git))
	}

	counts := report.Run.Counts
	reuse := report.Run.Reuse
	iox.Fprintf(os.Stdout, "  Tasks:    %d total, %d succeeded, %d failed, %d skipped, %d reused\n",
		counts.Total, counts.Succeeded, counts.Failed, counts.Skipped,
		reuse.LocalCache+reuse.RemoteCache+reuse.Coalesced)

	renderReportCommands(report.Commands)
	renderReportJobs(report)
}

// renderReportCommands prints one row per root command.
func renderReportCommands(commands []protocolcli.ReportCommand) {
	if len(commands) == 0 {
		return
	}
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  %-10s %-7s %-7s %-7s %-11s %-16s %s\n",
		"COMMAND", "TASKS", "FAILED", "REUSED", "FRESH WALL", "TESTS", "COVERAGE")
	for _, cmd := range commands {
		reused := cmd.Reuse.LocalCache + cmd.Reuse.RemoteCache + cmd.Reuse.Coalesced
		iox.Fprintf(os.Stdout, "  %-10s %-7d %-7d %-7d %-11s %-16s %s\n",
			cmd.Command, cmd.Counts.Total, cmd.Counts.Failed, reused,
			formatDurationMs64(cmd.FreshWallMs), reportTests(cmd.Tests), reportCoverage(cmd.Coverage))
		if cmd.Errors > 0 || cmd.Warnings > 0 {
			iox.Fprintf(os.Stdout, "  %-10s %d error(s), %d warning(s)\n", "", cmd.Errors, cmd.Warnings)
		}
	}
}

// renderReportJobs prints the head of the report's job list. The list arrives
// already prioritized by the producer (failed first, then diagnostic-bearing,
// then coverage-bearing, then the longest wall), so printing its head shows the
// most informative jobs without this renderer re-deciding what matters.
func renderReportJobs(report *protocolcli.ReportFile) {
	if len(report.Jobs) == 0 {
		return
	}
	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  Jobs (%d recorded, %d elided):\n", len(report.Jobs), report.ElidedJobs)
	for i, job := range report.Jobs {
		if i == reportJobsShown {
			iox.Fprintf(os.Stdout, "    … %d more recorded\n", len(report.Jobs)-reportJobsShown)
			break
		}
		line := fmt.Sprintf("    %s %-45s %-8s %s",
			reportJobMark(job.Outcome), job.Key, formatDurationMs64(job.DurationMs), reportJobNote(job))
		iox.Fprintln(os.Stdout, strings.TrimRight(line, " "))
		renderReportJobDiagnostics(job)
	}
}

// renderReportJobDiagnostics prints a failed job's diagnostics. They are
// printed for failures only: a succeeding job's warnings are counted in its
// command's row, and reprinting them here would bury the failure the reader
// opened the report for.
func renderReportJobDiagnostics(job protocolcli.ReportJob) {
	if job.Outcome != protocolcli.TaskStatusFailed {
		return
	}
	for i, diag := range job.Diagnostics {
		if i == reportJobDiagnosticsShown {
			iox.Fprintf(os.Stdout, "        … %d more\n", len(job.Diagnostics)-reportJobDiagnosticsShown)
			break
		}
		location := diag.File
		if location != "" && diag.Line > 0 {
			location = fmt.Sprintf("%s:%d", location, diag.Line)
		}
		if location != "" {
			location += ": "
		}
		iox.Fprintf(os.Stdout, "        %s: %s%s\n", diag.Severity, location, firstLine(diag.Message))
	}
}

// reportJobNote is the trailing annotation on a job line: how the result was
// obtained, what it covered, and how many diagnostics it carried.
func reportJobNote(job protocolcli.ReportJob) string {
	var parts []string
	if job.Reuse != "" && job.Reuse != protocolcli.TaskReuseNone {
		parts = append(parts, "["+job.Reuse+"]")
	}
	if job.Coverage != nil {
		parts = append(parts, reportCoverage(job.Coverage))
	}
	total := len(job.Diagnostics) + job.TruncatedCount
	if total > 0 {
		parts = append(parts, fmt.Sprintf("%d diagnostic(s)", total))
	}
	return strings.Join(parts, "  ")
}

func reportJobMark(outcome string) string {
	switch outcome {
	case protocolcli.TaskStatusSuccess:
		return "✓"
	case protocolcli.TaskStatusFailed:
		return "✗"
	default:
		return "-"
	}
}

// reportTests renders a command's test counters, or "-" when it ran none.
func reportTests(tests *protocolcli.ReportTests) string {
	if tests == nil {
		return "-"
	}
	if tests.Failed > 0 {
		return fmt.Sprintf("%d/%d failed", tests.Failed, tests.Total)
	}
	return fmt.Sprintf("%d passed", tests.Passed)
}

// reportCoverage renders a coverage measurement, or "-" when none was
// collected. Absent is not zero: a command that measured nothing must not read
// as 0%.
func reportCoverage(coverage *protocolcli.ReportCoverage) string {
	if coverage == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%% %s", coverage.Percentage, coverage.Granularity)
}

// enforceCoverageNote annotates the origin line only when the coverage gate was
// OFF. Enforcement is the default, so flagging it would mark every report; the
// exception is what a reader needs to see, because it is the one case where a
// passing run does not mean the thresholds held.
func enforceCoverageNote(enforced bool) string {
	if enforced {
		return ""
	}
	return " (coverage gate off)"
}

// reportCommitLine renders the repository state the run was produced against —
// the join key a consumer files the report under.
func reportCommitLine(git *protocolcli.ReportGit) string {
	line := shared.ShortSHA(git.Sha)
	if git.Branch != "" {
		line += " on " + git.Branch
	}
	if git.Dirty != nil && *git.Dirty {
		line += " (dirty)"
	}
	if git.Baseline != "" {
		line += " vs " + git.Baseline
	}
	return line
}

func firstLine(message string) string {
	if idx := strings.IndexByte(message, '\n'); idx >= 0 {
		return message[:idx]
	}
	return message
}
