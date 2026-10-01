package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/parse"
)

func TestFormatTestSummaryOutcomes(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bounded-test-output", "one-deterministic-structured-outcome-summary-by-default")
	t.Parallel()
	for _, test := range []struct {
		name    string
		summary *parse.TestSummary
		failed  bool
		want    string
	}{
		{name: "all pass", summary: &parse.TestSummary{Passed: 3, Total: 3}, want: "3/3 passed"},
		{name: "skipped", summary: &parse.TestSummary{Passed: 2, Skipped: 1, Total: 3}, want: "2/3 passed, 1 skipped"},
		{name: "failing test", summary: &parse.TestSummary{Passed: 1, Failed: 1, Total: 2}, failed: true, want: "1/2 passed, 1 failed"},
		{name: "module failure with no parsed tests", summary: &parse.TestSummary{}, failed: true, want: "test run failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := formatTestSummary(test.summary, test.failed, nil, 0); got != test.want {
				t.Fatalf("summary = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRunTestZeroCountFailureKeepsRecapAndDebugTranscript(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bounded-test-output", "the-complete-transcript-is-retained-as-session-detail")
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, "test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "test", "module.test.ts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		junit := `<?xml version="1.0" encoding="UTF-8"?><testsuites tests="0" failures="0"></testsuites>`
		if err := os.MkdirAll(ctx.OutputPath, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(ctx.OutputPath, "results.junit.xml"), []byte(junit), 0o644); err != nil {
			return nil, err
		}
		return &exec.Result{
			Success:  false,
			ExitCode: 1,
			Stderr:   "module initialization failed\ncaused by missing binding\n",
		}, nil
	})

	var status string
	var runErr error
	events := captureEvents(t, func() {
		status, _, runErr = runTest(ctx, jsonl.New(), nil)
	})
	if runErr != nil || status != "FAILED" {
		t.Fatalf("runTest status=%q err=%v, want FAILED", status, runErr)
	}
	var summary string
	var transcript []string
	for _, event := range events {
		switch event["type"] {
		case "summary":
			summary, _ = event["message"].(string)
		case "log":
			if event["level"] == "debug" {
				transcript = append(transcript, str(event["message"]))
			}
		}
	}
	if summary != "test run failed" {
		t.Fatalf("summary = %q, want deterministic zero-count failure recap", summary)
	}
	if got := strings.Join(transcript, "\n"); got != "module initialization failed\ncaused by missing binding" {
		t.Fatalf("debug transcript = %q, want every producer line", got)
	}
}

func TestEmitTestTranscriptVerboseChangesVisibilityOnly(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bounded-test-output", "test-verbose-changes-visibility-only")
	output := "bun test v1\n✓ first test\n✓ second test\n"
	emit := func(verbose bool) []map[string]any {
		return captureEvents(t, func() {
			emitTestTranscript(jsonl.New(), output, verbose, "")
		})
	}
	quietEvents := emit(false)
	verboseEvents := emit(true)
	if len(quietEvents) != 3 || len(verboseEvents) != 3 {
		t.Fatalf("log counts quiet=%d verbose=%d, want 3", len(quietEvents), len(verboseEvents))
	}
	for i := range quietEvents {
		if quietEvents[i]["type"] != "log" || quietEvents[i]["level"] != "debug" {
			t.Fatalf("quiet event %d = %+v, want debug log", i, quietEvents[i])
		}
		if verboseEvents[i]["level"] != "info" {
			t.Fatalf("verbose event %d = %+v, want info log", i, verboseEvents[i])
		}
		if quietEvents[i]["message"] != verboseEvents[i]["message"] {
			t.Fatalf("message %d changed with verbosity: quiet=%q verbose=%q", i, quietEvents[i]["message"], verboseEvents[i]["message"])
		}
	}
	routedEvents := captureEvents(t, func() {
		emitTestTranscript(jsonl.New(), output, false, "/a")
	})
	contextData, ok := routedEvents[0]["context"].(map[string]any)
	if !ok || len(contextData) != 1 || contextData[protocolcli.BatchProjectLogContextKey] != "/a" {
		t.Fatalf("routed context = %#v, want exact %s=/a hint", routedEvents[0]["context"], protocolcli.BatchProjectLogContextKey)
	}
}

func TestBoundTestFailureDiagnosticsKeepsRuneSafeAccounting(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bounded-test-output", "failure-diagnostics-are-bounded-with-omission-accounting")
	t.Parallel()
	const tailSentinel = "\nTAIL_VERDICT_SENTINEL"
	diagnostics := make([]testBatchDiagnostic, 18)
	for i := range diagnostics {
		diagnostics[i] = testBatchDiagnostic{
			Severity:    "error",
			Category:    errs.CodeTestFailed.String(),
			Description: strings.Repeat("é", protocolcli.ReportMaxMessageBytes) + tailSentinel,
		}
	}

	bounded, omitted := boundTestFailureDiagnostics(diagnostics)
	if len(bounded) != protocolcli.ReportMaxJobDiagnostics {
		t.Fatalf("diagnostics = %d, want %d", len(bounded), protocolcli.ReportMaxJobDiagnostics)
	}
	if omitted != 3 {
		t.Fatalf("omitted = %d, want 3", omitted)
	}
	for i, diagnostic := range bounded {
		if len(diagnostic.Description) > protocolcli.ReportMaxMessageBytes {
			t.Fatalf("diagnostic %d is %d bytes, limit %d", i, len(diagnostic.Description), protocolcli.ReportMaxMessageBytes)
		}
		if !utf8.ValidString(diagnostic.Description) {
			t.Fatalf("diagnostic %d is not valid UTF-8", i)
		}
	}
	if !strings.Contains(bounded[0].Description, "… [truncated; showing test name and final output;") ||
		!strings.Contains(bounded[0].Description, "was emitted as transcript log events") ||
		!strings.HasSuffix(bounded[0].Description, tailSentinel) {
		t.Fatalf("bounded message did not retain a marked tail: %q", bounded[0].Description)
	}
	if diagnostics[0].Description == bounded[0].Description {
		t.Fatal("boundTestFailureDiagnostics mutated its caller or failed to bound the message")
	}
	if marker := bounded[len(bounded)-1]; marker.Category != protocolcli.TestFailureDetailsTruncatedCode ||
		marker.Severity != "info" ||
		!strings.Contains(marker.Description, "was emitted as transcript log events") {
		t.Fatalf("last diagnostic = %+v, want execution-scoped omission marker", marker)
	}

	data := map[string]any{"testSummary": map[string]any{"total": 18}}
	recordFailureDetailsTruncated(data, omitted)
	summary := data["testSummary"].(map[string]any)
	if summary["failureDetailsTruncated"] != 3 {
		t.Fatalf("truncation accounting = %#v", summary["failureDetailsTruncated"])
	}
}

func TestBoundTestFailureMessageKeepsTestNameAndTail(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bounded-test-output", "a-bounded-failure-message-keeps-the-test-name-and-tail")
	const testName = "Test failed: object diff preserves its name"
	const tail = "FINAL_DIFF_SENTINEL"
	message := testName + "\n" + strings.Repeat("é", protocolcli.ReportMaxMessageBytes) + tail

	bounded := boundTestFailureMessage(message)
	if len(bounded) > protocolcli.ReportMaxMessageBytes {
		t.Fatalf("message = %d bytes, want at most %d", len(bounded), protocolcli.ReportMaxMessageBytes)
	}
	if !utf8.ValidString(bounded) {
		t.Fatalf("message is not valid UTF-8: %q", bounded)
	}
	if !strings.HasPrefix(bounded, testName+"\n") || !strings.HasSuffix(bounded, tail) {
		t.Fatalf("bounded message lost its test name or final diff: %q", bounded)
	}
}
