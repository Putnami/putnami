package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// `putnami report` reads back a finished run's report. The
// properties pinned here are the ones a consumer binds to: the structured form
// is the recorded document itself, the human form states the verdict, and a
// workspace with no report fails as a FAILURE rather than a usage error.

// writeTestReport records one report and returns its session id.
func writeTestReport(t *testing.T, wsRoot, sessionID, origin string, mutate func(*protocolcli.ReportFile)) string {
	t.Helper()
	dirty := true
	report := &protocolcli.ReportFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		SessionID:       sessionID,
		StartTime:       "2026-08-07T09:30:00+02:00",
		EndTime:         "2026-08-07T09:30:02+02:00",
		Origin:          origin,
		EnforceCoverage: true,
		Git: &protocolcli.ReportGit{
			Branch: "epic-2990",
			Sha:    "abcdef1234567890abcdef1234567890abcdef12",
			Dirty:  &dirty,
		},
		Run: protocolcli.ReportRun{
			Outcome:    protocolcli.RunOutcomeFailure,
			ExitCode:   protocolcli.ExitFailure,
			Counts:     protocolcli.RunCounts{Total: 3, Succeeded: 2, Failed: 1},
			Reuse:      protocolcli.RunReuse{LocalCache: 1},
			DurationMs: 2000,
		},
		Commands: []protocolcli.ReportCommand{{
			Command:     "test",
			Counts:      protocolcli.RunCounts{Total: 3, Succeeded: 2, Failed: 1},
			Reuse:       protocolcli.RunReuse{LocalCache: 1},
			FreshWallMs: 1500,
			Tests:       &protocolcli.ReportTests{Total: 12, Passed: 11, Failed: 1},
			Coverage:    &protocolcli.ReportCoverage{Percentage: 83.9, Granularity: protocolcli.CoverageStatements, Enforced: true},
			Errors:      1,
		}},
		Jobs: []protocolcli.ReportJob{{
			Key: "/tooling/cli:test~test", Project: "/tooling/cli", Task: "test~test",
			Command: "test", Outcome: protocolcli.TaskStatusFailed, Reuse: protocolcli.TaskReuseNone,
			DurationMs: 1500,
			Diagnostics: []protocolcli.Diagnostic{{
				Severity: "error", Message: "report_test.go:1: it drifted\nsecond line", File: "tooling/cli/report_test.go", Line: 1,
			}},
		}},
		ElidedJobs: 2,
	}
	if mutate != nil {
		mutate(report)
	}
	if err := workspace_state.NewReportStore(wsRoot).Write(report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	return sessionID
}

// TestReportShow_PrintsTheRecordedDocumentVerbatim: the structured form is the
// contract itself, not an envelope around it — and it is the RECORDED bytes, so
// a member this binary's struct does not know about still reaches the consumer
// that asked for the document.
func TestReportShow_PrintsTheRecordedDocumentVerbatim(t *testing.T) {
	wsRoot := t.TempDir()
	id := writeTestReport(t, wsRoot, "20260807-093000-aaaaaa", protocolcli.ReportOriginCLI, nil)

	// A future producer's member, added to the recorded file after the write.
	path := workspace_state.NewReportStore(wsRoot).Path(id)
	recorded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	widened := strings.Replace(string(recorded), "{\n", "{\n  \"futureMember\": 7,\n", 1)
	if err := os.WriteFile(path, []byte(widened), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return ReportShow(wsRoot, "", "json") })
	if err != nil {
		t.Fatalf("ReportShow: %v", err)
	}
	if out != widened {
		t.Errorf("structured output is not the recorded document:\ngot:\n%s\nwant:\n%s", out, widened)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("output is not one JSON document: %v", err)
	}
	if _, wrapped := document["status"]; wrapped {
		t.Error("the report was wrapped in a result envelope; it IS the contract document")
	}
	if string(document["sessionId"]) != `"`+id+`"` {
		t.Errorf("sessionId = %s, want the requested report's", document["sessionId"])
	}
}

// TestReportShow_RendersTheRunsVerdict: the human form answers what the run did
// without the reader opening the session — outcome, per-command synthesis, and
// the head of the report's own prioritized job list with the failure's message.
func TestReportShow_RendersTheRunsVerdict(t *testing.T) {
	wsRoot := t.TempDir()
	id := writeTestReport(t, wsRoot, "20260807-093000-aaaaaa", protocolcli.ReportOriginCLI, nil)

	out, err := sharedtest.CaptureStdout(t, func() error { return ReportShow(wsRoot, "", "") })
	if err != nil {
		t.Fatalf("ReportShow: %v", err)
	}
	for _, want := range []string{
		id,
		"failure (exit 1)",
		"abcdef123456 on epic-2990 (dirty)",
		"3 total, 2 succeeded, 1 failed",
		"1/12 failed",
		"83.9% statements",
		"/tooling/cli:test~test",
		"Jobs (1 recorded, 2 elided)",
		"error: tooling/cli/report_test.go:1: report_test.go:1: it drifted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report is missing %q:\n%s", want, out)
		}
	}
	// A diagnostic's message is folded to its first line: the untruncated text
	// stays in the session, and a multi-line message must not break the list.
	if strings.Contains(out, "second line") {
		t.Errorf("a diagnostic's continuation lines leaked into the job list:\n%s", out)
	}
}

// TestReportShow_ReadsAnExactSession: --session names a report directly, with no
// origin filter — an agent's own run is readable by anyone who asks for it by
// name, while the newest lookup passes over it.
func TestReportShow_ReadsAnExactSession(t *testing.T) {
	wsRoot := t.TempDir()
	writeTestReport(t, wsRoot, "20260807-090000-aaaaaa", protocolcli.ReportOriginCLI, nil)
	agentRun := writeTestReport(t, wsRoot, "20260807-094000-bbbbbb", protocolcli.ReportOriginMCP, nil)

	out, err := sharedtest.CaptureStdout(t, func() error { return ReportShow(wsRoot, agentRun, "") })
	if err != nil {
		t.Fatalf("ReportShow --session: %v", err)
	}
	if !strings.Contains(out, agentRun) || !strings.Contains(out, "Origin:   mcp") {
		t.Errorf("--session did not read the named report:\n%s", out)
	}

	newest, err := sharedtest.CaptureStdout(t, func() error { return ReportShow(wsRoot, "", "") })
	if err != nil {
		t.Fatalf("ReportShow: %v", err)
	}
	if !strings.Contains(newest, "20260807-090000-aaaaaa") {
		t.Errorf("the newest lookup did not withhold the agent's run:\n%s", newest)
	}
}

// TestReportShow_MissingReportIsAFailure: an interrupted run records no report,
// so "there is none" is an answer about the workspace — exit 1, not the usage
// exit code a malformed command line earns.
func TestReportShow_MissingReportIsAFailure(t *testing.T) {
	wsRoot := t.TempDir()

	err := ReportShow(wsRoot, "", "")
	if err == nil {
		t.Fatal("an empty store reported success")
	}
	if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitFailure {
		t.Errorf("exit code for a missing report = %d, want %d", code, protocolcli.ExitFailure)
	}
	if !strings.Contains(err.Error(), filepath.Join(".putnami", "reports")) {
		t.Errorf("the failure does not say where reports are recorded: %v", err)
	}

	writeTestReport(t, wsRoot, "20260807-090000-aaaaaa", protocolcli.ReportOriginCLI, nil)
	err = ReportShow(wsRoot, "20260807-999999-zzzzzz", "")
	if err == nil {
		t.Fatal("an unrecorded session reported success")
	}
	if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitFailure {
		t.Errorf("exit code for an unrecorded session = %d, want %d", code, protocolcli.ExitFailure)
	}
}

// TestReportShow_JSONLIsOneLine: jsonl is a line-framed mode — this command's
// failures already emit the one-line envelope there, so its success must be one
// line too, or a `--output=jsonl | while read line` consumer breaks only on the
// runs that worked. Compaction re-frames the recorded bytes without decoding
// them, so an unknown member still survives.
func TestReportShow_JSONLIsOneLine(t *testing.T) {
	wsRoot := t.TempDir()
	id := writeTestReport(t, wsRoot, "20260807-093000-aaaaaa", protocolcli.ReportOriginCLI, nil)

	out, err := sharedtest.CaptureStdout(t, func() error { return ReportShow(wsRoot, "", "jsonl") })
	if err != nil {
		t.Fatalf("ReportShow: %v", err)
	}
	if lines := strings.Count(strings.TrimRight(out, "\n"), "\n"); lines != 0 {
		t.Fatalf("jsonl output spans %d lines, want one:\n%s", lines+1, out)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("jsonl line is not a JSON document: %v", err)
	}
	if string(document["sessionId"]) != `"`+id+`"` {
		t.Errorf("sessionId = %s, want the recorded report's", document["sessionId"])
	}
}
