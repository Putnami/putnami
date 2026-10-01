package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/testjob"
)

// emitTestTranscript retains every Bun output line as ordinary debug detail.
// --test-verbose promotes the same lines to info for human visibility and does
// not affect command arguments, summaries, result data, verdicts or exits.
func emitTestTranscript(emit *jsonl.Emitter, output string, verbose bool, projectID string) {
	if emit == nil {
		return
	}
	level := "debug"
	if verbose {
		level = "info"
	}
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if projectID == "" {
			emit.Log(level, line)
			continue
		}
		emit.LogEvent(level, line, map[string]any{protocolcli.BatchProjectLogContextKey: projectID}, nil)
	}
}

// collectTestFailureDiagnostics projects the complete transcript into bounded,
// causal diagnostics. The unprojected transcript remains available as debug
// log events for the session recorder.
func collectTestFailureDiagnostics(
	proj pctx.ProjectRef,
	testSummary *parse.TestSummary,
	logs string,
) ([]testBatchDiagnostic, int) {
	diagnostics := testFailureDiagnostics(proj, testSummary, logs)
	if len(diagnostics) == 0 {
		for _, unhandled := range testjob.ExtractUnhandledErrors(logs) {
			file := unhandled.File
			if file != "" {
				file = filepath.Join(proj.Path, file)
			}
			code := errs.CodeUnhandledError.String()
			if unhandled.Type != "" {
				code = unhandled.Type
			}
			diagnostics = append(diagnostics, testBatchDiagnostic{
				Severity:    "error",
				Description: unhandled.Message,
				File:        file,
				Line:        unhandled.Line,
				Category:    code,
			})
		}
	}
	diagnostics = append(diagnostics, testBatchDiagnostic{
		Severity:    "error",
		Description: "Tests failed for " + proj.Name,
		Category:    errs.CodeTestsFailed.String(),
	})
	return boundTestFailureDiagnostics(diagnostics)
}

func boundTestFailureDiagnostics(diagnostics []testBatchDiagnostic) ([]testBatchDiagnostic, int) {
	bounded := append([]testBatchDiagnostic(nil), diagnostics...)
	for i := range bounded {
		bounded[i].Description = boundTestFailureMessage(bounded[i].Description)
	}
	if len(bounded) <= protocolcli.ReportMaxJobDiagnostics {
		return bounded, 0
	}
	kept := protocolcli.ReportMaxJobDiagnostics - 1
	omitted := len(bounded) - kept
	out := append([]testBatchDiagnostic(nil), bounded[:kept]...)
	out = append(out, testBatchDiagnostic{
		Severity:    "info",
		Description: fmt.Sprintf("%d additional failure detail(s) omitted; full test output was emitted as transcript log events", omitted),
		Category:    protocolcli.TestFailureDetailsTruncatedCode,
	})
	return out, omitted
}

func boundTestFailureMessage(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len(message) <= protocolcli.ReportMaxMessageBytes {
		return message
	}
	firstLineEnd := strings.IndexByte(message, '\n')
	if firstLineEnd > 0 {
		const marker = "… [truncated; showing test name and final output; full output was emitted as transcript log events]\n"
		const minTailBytes = protocolcli.ReportMaxMessageBytes / 2
		firstBudget := protocolcli.ReportMaxMessageBytes - len(marker) - 1 - minTailBytes
		first := truncateUTF8Head(message[:firstLineEnd], firstBudget)
		tailBudget := protocolcli.ReportMaxMessageBytes - len(first) - 1 - len(marker)
		return first + "\n" + marker + utf8Tail(message[firstLineEnd+1:], tailBudget)
	}
	const marker = "… [truncated; showing final test output; full output was emitted as transcript log events]\n"
	return marker + utf8Tail(message, protocolcli.ReportMaxMessageBytes-len(marker))
}

func truncateUTF8Head(message string, budget int) string {
	if len(message) <= budget {
		return message
	}
	const marker = "…"
	end := max(0, budget-len(marker))
	for end > 0 && !utf8.RuneStart(message[end]) {
		end--
	}
	return message[:end] + marker
}

func utf8Tail(message string, budget int) string {
	if len(message) <= budget {
		return message
	}
	start := len(message) - budget
	for start < len(message) && !utf8.RuneStart(message[start]) {
		start++
	}
	return message[start:]
}

func recordFailureDetailsTruncated(resultData map[string]any, omitted int) {
	if omitted <= 0 || resultData == nil {
		return
	}
	summary, _ := resultData["testSummary"].(map[string]any)
	if summary != nil {
		summary["failureDetailsTruncated"] = omitted
	}
}

func formatTestSummary(
	summary *parse.TestSummary,
	testFailed bool,
	coverage *parse.CoverageSummary,
	omitted int,
) string {
	var parts []string
	if summary != nil && summary.Total > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d passed", summary.Passed, summary.Total))
		if summary.Failed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", summary.Failed))
		}
		if summary.Skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", summary.Skipped))
		}
	}
	if testFailed && (summary == nil || summary.Failed == 0) {
		parts = append(parts, "test run failed")
	}
	if omitted > 0 {
		parts = append(parts, fmt.Sprintf("%d failure detail(s) omitted", omitted))
	}
	if coverage != nil {
		parts = append(parts, fmt.Sprintf("%.1f%% coverage", coverage.LineCoverage))
	}
	return strings.Join(parts, ", ")
}
