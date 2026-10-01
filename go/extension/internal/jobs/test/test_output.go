package test

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.putnami.dev/go/extension/internal/parse"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/jsonl"
)

// emitTestTranscript retains the complete tool transcript as ordinary debug
// events. Normal human output therefore stays summary-first, while the session
// recorder can retain every line. --test-verbose changes visibility only by
// promoting the same lines to info; it never changes the structured summary or
// verdict. projectID is set only for a batched invocation.
func emitTestTranscript(emit *jsonl.Emitter, output string, useJSON, verbose bool, projectID string) {
	var contextData map[string]any
	if projectID != "" {
		contextData = map[string]any{protocolcli.BatchProjectLogContextKey: projectID}
	}
	emitTestTranscriptWithContext(emit, output, useJSON, verbose, contextData)
}

// emitBatchTestTranscript walks a shared invocation once, in source order.
// Package-owned events are tagged for one project; plain text and events whose
// package cannot be attributed are tagged for every member. The CLI can then
// project a correctly ordered transcript onto each task without the producer
// encoding and redacting the aggregate once per project.
func emitBatchTestTranscript(
	emit *jsonl.Emitter,
	output string,
	useJSON, verbose bool,
	projects []*batchProject,
) {
	if emit == nil || len(projects) == 0 {
		return
	}
	if len(projects) == 1 {
		emitTestTranscript(emit, output, useJSON, verbose, projects[0].ref.ID)
		return
	}
	projectIDs := make([]string, 0, len(projects))
	for _, project := range projects {
		projectIDs = append(projectIDs, project.ref.ID)
	}
	sharedContext := map[string]any{protocolcli.BatchProjectLogsContextKey: projectIDs}
	if !useJSON {
		emitTestTranscriptWithContext(emit, output, false, verbose, sharedContext)
		return
	}
	level := "debug"
	if verbose {
		level = "info"
	}
	owners := packageOwners(projects)
	for _, encoded := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		contextData := sharedContext
		var event parse.TestEvent
		var lines []string
		if json.Unmarshal([]byte(encoded), &event) == nil && event.Action != "" {
			if projectID := owners[event.Package]; projectID != "" {
				contextData = map[string]any{protocolcli.BatchProjectLogContextKey: projectID}
			}
			if event.Output != "" {
				lines = strings.Split(strings.TrimRight(event.Output, "\n"), "\n")
			}
		} else {
			lines = []string{encoded}
		}
		for _, line := range lines {
			if strings.TrimSpace(line) != "" {
				emit.LogEvent(level, line, contextData, nil)
			}
		}
	}
}

func emitTestTranscriptWithContext(
	emit *jsonl.Emitter,
	output string,
	useJSON, verbose bool,
	contextData map[string]any,
) {
	if emit == nil {
		return
	}
	level := "debug"
	if verbose {
		level = "info"
	}
	for _, line := range testTranscriptLines(output, useJSON) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(contextData) == 0 {
			emit.Log(level, line)
			continue
		}
		emit.LogEvent(level, line, contextData, nil)
	}
}

// testTranscriptLines unwraps go test -json output without filtering its
// successful RUN/PASS/SKIP scaffolding. Failure diagnostics use a separately
// bounded causal projection; this path is the complete session evidence.
func testTranscriptLines(output string, useJSON bool) []string {
	if !useJSON {
		return strings.Split(strings.TrimRight(output, "\n"), "\n")
	}
	var lines []string
	for _, encoded := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		var event parse.TestEvent
		if json.Unmarshal([]byte(encoded), &event) == nil && event.Action != "" {
			// Build failures use Action "build-output" on current toolchains.
			// Any event carrying Output is part of the complete transcript.
			if event.Output != "" {
				lines = append(lines, strings.Split(strings.TrimRight(event.Output, "\n"), "\n")...)
			}
			continue
		}
		// A toolchain error may be plain text even when -json was requested.
		lines = append(lines, encoded)
	}
	return lines
}

// boundFailureDiagnostics applies the already-public report limits at the
// producer seam. One slot is reserved for honest truncation accounting, and
// every kept message is rune-safe and small enough to fit a failure-priority
// machine-output record. The complete transcript remains in debug log events.
func boundFailureDiagnostics(diagnostics []parse.ToolDiagnostic) ([]parse.ToolDiagnostic, int) {
	bounded := append([]parse.ToolDiagnostic(nil), diagnostics...)
	for i := range bounded {
		bounded[i].Description = boundFailureMessage(bounded[i].Description)
	}
	if len(bounded) <= protocolcli.ReportMaxJobDiagnostics {
		return bounded, 0
	}
	kept := protocolcli.ReportMaxJobDiagnostics - 1
	omitted := len(bounded) - kept
	out := append([]parse.ToolDiagnostic(nil), bounded[:kept]...)
	out = append(out, parse.ToolDiagnostic{
		Category:    protocolcli.TestFailureDetailsTruncatedCode,
		Severity:    "info",
		Description: fmt.Sprintf("%d additional failure detail(s) omitted; full test output was emitted as transcript log events", omitted),
	})
	return out, omitted
}

func boundFailureMessage(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len(message) <= protocolcli.ReportMaxMessageBytes {
		return message
	}
	const prefix = "… [truncated; showing final test output; full output was emitted as transcript log events]\n"
	start := len(message) - (protocolcli.ReportMaxMessageBytes - len(prefix))
	for start < len(message) && !utf8.RuneStart(message[start]) {
		start++
	}
	return prefix + message[start:]
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
	counts parse.TestCounts,
	testFailed bool,
	hasCoverage bool,
	coveragePercentage float64,
	coverageUnreadable bool,
	failureDetailsTruncated int,
) string {
	var parts []string
	if counts.Total() > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d passed", counts.Passed, counts.Total()))
	}
	if counts.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", counts.Failed))
	}
	if counts.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", counts.Skipped))
	}
	if testFailed && counts.Failed == 0 {
		parts = append(parts, packageFailureSummary(counts.PackagesFailed))
	}
	if failureDetailsTruncated > 0 {
		parts = append(parts, fmt.Sprintf("%d failure detail(s) omitted", failureDetailsTruncated))
	}
	if hasCoverage {
		parts = append(parts, fmt.Sprintf("%.1f%% coverage", coveragePercentage))
	} else if coverageUnreadable {
		parts = append(parts, "coverage profile unreadable")
	}
	return strings.Join(parts, ", ")
}
