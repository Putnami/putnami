package test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"go.putnami.dev/go/extension/internal/parse"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

func TestFormatTestSummaryOutcomes(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "bounded-test-output", "the-default-outcome-summary-is-deterministic")
	t.Parallel()
	for _, test := range []struct {
		name       string
		counts     parse.TestCounts
		failed     bool
		unreadable bool
		want       string
	}{
		{name: "all pass", counts: parse.TestCounts{Passed: 3}, want: "3/3 passed"},
		{name: "skipped", counts: parse.TestCounts{Passed: 2, Skipped: 1}, want: "2/3 passed, 1 skipped"},
		{name: "failing test", counts: parse.TestCounts{Passed: 1, Failed: 1}, failed: true, want: "1/2 passed, 1 failed"},
		{name: "package failure", counts: parse.TestCounts{Passed: 1, PackagesFailed: 1}, failed: true, want: "1/1 passed, 1 package(s) failed"},
		{name: "unreadable coverage profile", counts: parse.TestCounts{Passed: 3}, unreadable: true, want: "3/3 passed, coverage profile unreadable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := formatTestSummary(test.counts, test.failed, false, 0, test.unreadable, 0); got != test.want {
				t.Fatalf("summary = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEmitTestTranscriptVerboseChangesVisibilityOnly(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "bounded-test-output", "test-verbose-changes-visibility-without-changing-result-data")
	output := strings.Join([]string{
		`{"Action":"output","Package":"example","Test":"TestA","Output":"=== RUN   TestA\n"}`,
		`{"Action":"output","Package":"example","Test":"TestA","Output":"--- PASS: TestA (0.00s)\n"}`,
		`{"Action":"pass","Package":"example","Test":"TestA"}`,
	}, "\n")

	emit := func(verbose bool) []map[string]any {
		return captureJobEvents(t, func(emitter *jsonl.Emitter) {
			emitTestTranscript(emitter, output, true, verbose, "")
		})
	}
	quietEvents := emit(false)
	verboseEvents := emit(true)
	if len(quietEvents) != 2 || len(verboseEvents) != 2 {
		t.Fatalf("log counts quiet=%d verbose=%d, want 2", len(quietEvents), len(verboseEvents))
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
	routedEvents := captureJobEvents(t, func(emitter *jsonl.Emitter) {
		emitTestTranscript(emitter, output, true, false, "/a")
	})
	contextData, ok := routedEvents[0]["context"].(map[string]any)
	if !ok || len(contextData) != 1 || contextData[protocolcli.BatchProjectLogContextKey] != "/a" {
		t.Fatalf("routed context = %#v, want exact %s=/a hint", routedEvents[0]["context"], protocolcli.BatchProjectLogContextKey)
	}
}

func TestBoundFailureDiagnosticsKeepsRuneSafeAccounting(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "bounded-test-output", "failure-diagnostics-are-bounded-with-explicit-omission-accounting")
	t.Parallel()
	const tailSentinel = "\nTAIL_VERDICT_SENTINEL"
	diagnostics := make([]parse.ToolDiagnostic, 18)
	for i := range diagnostics {
		diagnostics[i] = parse.ToolDiagnostic{
			Severity:    "error",
			Category:    "TEST_FAILED",
			Description: strings.Repeat("é", protocolcli.ReportMaxMessageBytes) + tailSentinel,
		}
	}

	bounded, omitted := boundFailureDiagnostics(diagnostics)
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
	if !strings.HasPrefix(bounded[0].Description, "… [truncated;") ||
		!strings.Contains(bounded[0].Description, "was emitted as transcript log events") ||
		!strings.HasSuffix(bounded[0].Description, tailSentinel) {
		t.Fatalf("bounded message did not retain a marked tail: %q", bounded[0].Description)
	}
	if diagnostics[0].Description == bounded[0].Description {
		t.Fatal("boundFailureDiagnostics mutated its caller or failed to bound the message")
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

func TestTestTranscriptLinesKeepsBuildOutput(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "bounded-test-output", "the-complete-transcript-is-retained-as-session-detail")
	output := strings.Join([]string{
		`{"Action":"build-output","Package":"example","Output":"# example [example.test]\n"}`,
		`{"Action":"build-output","Package":"example","Output":"example_test.go:3:39: cannot use string as int\n"}`,
		`{"Action":"output","Package":"example","Output":"FAIL\texample [build failed]\n"}`,
	}, "\n")

	got := strings.Join(testTranscriptLines(output, true), "\n")
	for _, want := range []string{"# example [example.test]", "cannot use string as int", "FAIL\texample [build failed]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transcript = %q, want compile evidence %q", got, want)
		}
	}
}

func TestBoundFailureDiagnosticsKeepsSelectedEarlyFailureBlock(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "bounded-test-output", "bounding-keeps-the-selected-early-failure-block")
	message := "panic: early failure\n" + strings.Repeat("stack frame\n", 200) + strings.Repeat("ok\tlate/package\n", 200)
	diagnostics := []parse.ToolDiagnostic{{
		Severity:    "error",
		Category:    "GO_TEST_FAILURE",
		Description: parse.TestFailureText(message),
	}}

	bounded, omitted := boundFailureDiagnostics(diagnostics)
	if omitted != 0 || len(bounded) != 1 {
		t.Fatalf("bounded = %+v, omitted = %d", bounded, omitted)
	}
	if len(bounded[0].Description) > protocolcli.ReportMaxMessageBytes {
		t.Fatalf("message = %d bytes, want at most %d", len(bounded[0].Description), protocolcli.ReportMaxMessageBytes)
	}
	if !strings.Contains(bounded[0].Description, "panic: early failure") {
		t.Fatalf("bounded diagnostic discarded the selected panic block: %q", bounded[0].Description)
	}
}
