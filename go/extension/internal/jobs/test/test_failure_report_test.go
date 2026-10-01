package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

// captureJobEvents runs fn with os.Stdout redirected and returns the JSONL
// events it emitted. The reader drains concurrently so a chatty job cannot
// deadlock on the pipe buffer.
func captureJobEvents(t *testing.T, fn func(emit *jsonl.Emitter)) []map[string]any {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		drained <- buf.Bytes()
	}()

	original := os.Stdout
	os.Stdout = w
	fn(jsonl.New())
	os.Stdout = original
	_ = w.Close()
	raw := <-drained
	_ = r.Close()

	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil {
			events = append(events, event)
		}
	}
	return events
}

func eventsOfType(events []map[string]any, eventType string) []map[string]any {
	var matched []map[string]any
	for _, event := range events {
		if event["type"] == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}

// writePackageFailureModule writes a module whose every test passes while the
// package still exits non-zero — the shape of a data race detected after the
// last test, a post-test panic, or a TestMain exit code. `go test -json` reports
// it as a package-level FAIL with no test-level fail event.
func writePackageFailureModule(t *testing.T, dir string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module failmod\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "failmod.go"), "package failmod\n\nfunc Value() int { return 1 }\n")
	mustWrite(t, filepath.Join(dir, "failmod_test.go"), strings.Join([]string{
		"package failmod",
		"",
		"import (",
		"\t\"os\"",
		"\t\"testing\"",
		")",
		"",
		"func TestMain(m *testing.M) {",
		"\tm.Run()",
		"\tos.Exit(1)",
		"}",
		"",
		"func TestValue(t *testing.T) {",
		"\tif Value() != 1 {",
		"\t\tt.Fatal(\"unexpected\")",
		"\t}",
		"}",
		"",
	}, "\n"))
}

// TestRun_PackageLevelFailureIsDiagnosed is the end-to-end guard for unparseable Go test output: a
// real `go test` run whose tests all pass but whose package fails must emit a
// diagnostic and must not summarize as a clean "N/N passed". Before the fix
// this produced FAILED with "(no details emitted)" and "1/1 passed".
func TestRun_PackageLevelFailureIsDiagnosed(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-failure-attribution", "a-package-level-failure-is-diagnosed")
	dir := t.TempDir()
	outDir := t.TempDir()
	writePackageFailureModule(t, dir)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "failmod", FullPath: dir},
		Params:        pctx.Params{},
	}

	var status string
	var runErr error
	events := captureJobEvents(t, func(emit *jsonl.Emitter) {
		status, _, runErr = Run(ctx, emit, nil)
	})
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED (non-zero go test exit)", status)
	}

	diagnostics := eventsOfType(events, "diagnostic")
	if len(diagnostics) == 0 {
		t.Fatalf("failed run emitted no diagnostic; events = %+v", events)
	}
	found := false
	for _, diagnostic := range diagnostics {
		message, _ := diagnostic["message"].(string)
		if diagnostic["severity"] == "error" && strings.TrimSpace(message) != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no non-empty error diagnostic; diagnostics = %+v", diagnostics)
	}

	summaries := eventsOfType(events, "summary")
	if len(summaries) == 0 {
		t.Fatalf("failed run emitted no summary; events = %+v", events)
	}
	summary, _ := summaries[0]["message"].(string)
	if !strings.Contains(summary, "package(s) failed") {
		t.Errorf("summary = %q, want it to name the package failure", summary)
	}

	var transcript []string
	for _, event := range eventsOfType(events, "log") {
		if event["level"] == "debug" {
			message, _ := event["message"].(string)
			transcript = append(transcript, message)
		}
	}
	joinedTranscript := strings.Join(transcript, "\n")
	for _, want := range []string{"=== RUN   TestValue", "--- PASS: TestValue", "FAIL"} {
		if !strings.Contains(joinedTranscript, want) {
			t.Errorf("debug transcript missing %q: %q", want, joinedTranscript)
		}
	}
}

// TestRun_FailureWithoutJSONStillDiagnoses covers the `--test-json=false`
// cadence, where there are no events to parse. The diagnostics used to be gated
// behind useJSON, so such a run failed with nothing at all.
func TestRun_FailureWithoutJSONStillDiagnoses(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-failure-attribution", "a-failure-with-no-parseable-per-test-output-is-still-a-failure")
	dir := t.TempDir()
	outDir := t.TempDir()
	writePackageFailureModule(t, dir)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: "failmod", FullPath: dir},
		Params:        pctx.Params{"test-json": json.RawMessage(`false`)},
	}

	var status string
	var runErr error
	events := captureJobEvents(t, func(emit *jsonl.Emitter) {
		status, _, runErr = Run(ctx, emit, nil)
	})
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}

	diagnostics := eventsOfType(events, "diagnostic")
	if len(diagnostics) == 0 {
		t.Fatalf("failed run with --test-json=false emitted no diagnostic; events = %+v", events)
	}
	message, _ := diagnostics[0]["message"].(string)
	if !strings.Contains(message, "FAIL") {
		t.Errorf("diagnostic = %q, want the raw go test output", message)
	}
}

// TestPackageFailureSummary pins the summary wording for a run that failed with
// no failing test, in both shapes: with and without a package-level fail event.
func TestPackageFailureSummary(t *testing.T) {
	if got := packageFailureSummary(2); got != "2 package(s) failed" {
		t.Errorf("packageFailureSummary(2) = %q", got)
	}
	if got := packageFailureSummary(0); got != "go test failed" {
		t.Errorf("packageFailureSummary(0) = %q", got)
	}
}

// TestRunBatchSingleProjectRaceReportIsDiagnosed covers the batch twin: every
// test passes, the package FAILs on a data race, and nothing parses into a
// located diagnostic. The raw output must reach the result.
func TestRunBatchSingleProjectRaceReportIsDiagnosed(t *testing.T) {
	ctx := makeBatchTestContext(t, false)
	ctx.SelectedProjects = ctx.SelectedProjects[:1]
	t.Setenv("GOWORK", "off")

	output := strings.Join([]string{
		testEvent("run", "example.com/a", "TestA", ""),
		testEvent("pass", "example.com/a", "TestA", ""),
		testEvent("output", "example.com/a", "", "WARNING: DATA RACE\n"),
		testEvent("output", "example.com/a", "", "Write at 0x00c000123456 by goroutine 12:\n"),
		testEvent("output", "example.com/a", "", "Found 1 data race(s)\n"),
		testEvent("fail", "example.com/a", "", ""),
	}, "\n") + "\n"

	mockGoCommand(t, func(_ string, _ []string, _ string, _ []string) ([]byte, error) {
		return []byte(output), errors.New("exit status 1")
	})

	_, data, err := runBatch(ctx, nil)
	if err != nil {
		t.Fatalf("runBatch: %v", err)
	}
	results := data["batchResults"].([]batchProjectResult)
	if len(results) != 1 || results[0].Status != "FAILED" {
		t.Fatalf("results = %+v, want one FAILED project", results)
	}
	if len(results[0].Diagnostics) == 0 {
		t.Fatalf("failed project carries no diagnostic: %+v", results[0])
	}
	if results[0].Summary.Errors == 0 {
		t.Errorf("summary = %+v, want at least one error", results[0].Summary)
	}
	joined := ""
	for _, diagnostic := range results[0].Diagnostics {
		joined += diagnostic.Description + "\n"
	}
	if !strings.Contains(joined, "WARNING: DATA RACE") {
		t.Errorf("diagnostics = %q, want the race report", joined)
	}
}

// TestRunBatchSingleProjectUnattributedFailureIsReported pins the one-project
// safety net: a non-zero exit whose output carries no fail event at all (a
// module resolution error, a build failure that never reached the JSON writer)
// used to be swallowed because unattributedFailure was hardcoded to "".
func TestRunBatchSingleProjectUnattributedFailureIsReported(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-failure-attribution", "a-single-project-batch-failure-that-cannot-be-attributed-is-still-reported")
	ctx := makeBatchTestContext(t, false)
	ctx.SelectedProjects = ctx.SelectedProjects[:1]
	t.Setenv("GOWORK", "off")

	mockGoCommand(t, func(_ string, _ []string, _ string, _ []string) ([]byte, error) {
		return []byte("go: updates to go.mod needed; to update it:\n\tgo mod tidy\n"),
			errors.New("exit status 1")
	})

	_, data, err := runBatch(ctx, nil)
	if err != nil {
		t.Fatalf("runBatch: %v", err)
	}
	results := data["batchResults"].([]batchProjectResult)
	if len(results) != 1 || results[0].Status != "FAILED" {
		t.Fatalf("results = %+v, want one FAILED project", results)
	}
	if len(results[0].Diagnostics) == 0 {
		t.Fatalf("failed project carries no diagnostic: %+v", results[0])
	}
	joined := ""
	for _, diagnostic := range results[0].Diagnostics {
		joined += diagnostic.Description + "\n"
	}
	if !strings.Contains(joined, "go mod tidy") {
		t.Errorf("diagnostics = %q, want the raw go output", joined)
	}
}

// TestSplitBatchTestOutputSingleProjectAttributedFailure guards against
// double-reporting: when the output does attribute the failure, the one-project
// branch must not also raise an unattributed-failure message.
func TestSplitBatchTestOutputSingleProjectAttributedFailure(t *testing.T) {
	projects := []*batchProject{{ref: pctx.ProjectRef{ID: "/a"}, modulePath: "example.com/a", packages: []string{"example.com/a"}}}
	output := testEvent("fail", "example.com/a", "TestA", "") + "\n" +
		testEvent("fail", "example.com/a", "", "") + "\n"

	outputs, failed, unattributed := splitBatchTestOutput(output, projects, true)
	if outputs["/a"] != output {
		t.Errorf("output not routed to the lone project: %q", outputs["/a"])
	}
	if !failed["/a"] {
		t.Error("lone project not marked failed")
	}
	if unattributed != "" {
		t.Errorf("unattributedFailure = %q, want empty when the output attributes the failure", unattributed)
	}
}

func TestSplitBatchTestOutputKeepsLargeSuccessfulTranscriptLine(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "bounded-test-output", "a-large-successful-transcript-line-survives-the-split")
	projects := []*batchProject{
		{ref: pctx.ProjectRef{ID: "/a"}, modulePath: "example.com/a", packages: []string{"example.com/a"}},
		{ref: pctx.ProjectRef{ID: "/b"}, modulePath: "example.com/b", packages: []string{"example.com/b"}},
	}
	large := strings.Repeat("x", 70*1024)
	output := testEvent("output", "example.com/a", "TestA", large+"\n") + "\n" +
		testEvent("pass", "example.com/a", "TestA", "") + "\n"

	outputs, failed, unattributed := splitBatchTestOutput(output, projects, false)
	if !strings.Contains(outputs["/a"], large) {
		t.Fatalf("large successful transcript line was dropped; routed bytes = %d", len(outputs["/a"]))
	}
	if outputs["/b"] != "" || failed["/a"] || failed["/b"] || unattributed != "" {
		t.Fatalf("unexpected attribution: outputs[b]=%q failed=%v unattributed=%q", outputs["/b"], failed, unattributed)
	}
}
