package deliverycli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verboseSessionCapture is the real capture whose stdout and events.jsonl were
// byte-identical (internal/reportstream/testdata/README.md).
const (
	verboseSessionCapture = "internal/reportstream/testdata/verbose-session.jsonl"
	verboseSessionID      = "20261027-231809-0e209a"
)

func readVerboseSessionCapture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(verboseSessionCapture)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	return data
}

// withoutTaskEvents drops every task:event and test:case record, as a bounded
// stdout that ran out of budget does, and keeps the task terminals and the
// final session:end.
func withoutTaskEvents(stream []byte) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(string(stream), "\n") {
		if strings.Contains(line, `"record":"test:case"`) || strings.Contains(line, `"record":"task:event"`) {
			continue
		}
		out.WriteString(line)
	}
	return out.String()
}

func writeSessionArtifact(t *testing.T, workspaceRoot string, artifact []byte) {
	t.Helper()
	dir := filepath.Join(workspaceRoot, ".putnami", "sessions", verboseSessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), artifact, 0o644); err != nil {
		t.Fatal(err)
	}
}

func batchProjects(t *testing.T, body map[string]any, kind string) []any {
	t.Helper()
	report, _ := body[kind].(map[string]any)
	projects, _ := report["projects"].([]any)
	return projects
}

// The wrapper posts the rows its stdout elided: they come from the session's
// complete events.jsonl, and the stream reports no elision. No note is printed.
func TestReportWrappedRunPostsTheSessionRecordBatch(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	var stderr []string
	ioctx.Stderr = func(line string) { stderr = append(stderr, line) }
	session := readVerboseSessionCapture(t)
	writeSessionArtifact(t, root, session)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})
	stubReportWrappedJSONL(t, 0, nil, withoutTaskEvents(session))

	if err := runReport(t, ioctx, root, []string{"--", "putnami", "test", "--output=jsonl"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(fake.reportRequests) != 1 {
		t.Fatalf("report POSTs = %d, want 1", len(fake.reportRequests))
	}
	body := fake.reportRequests[0].Body
	tests := batchProjects(t, body, "tests")
	coverage := batchProjects(t, body, "coverage")
	if len(tests) != 1 || len(coverage) != 1 {
		t.Fatalf("tests = %v, coverage = %v, want the session's one project each", body["tests"], body["coverage"])
	}
	if project, _ := coverage[0].(map[string]any); project["project"] != "go.putnami.dev/protocol/gomod" || project["percentage"] != 95.89 {
		t.Fatalf("coverage project = %v", project)
	}
	if project, _ := tests[0].(map[string]any); project["passed"] != float64(20) || project["total"] != float64(20) {
		t.Fatalf("tests project = %v, want the session's 20 passing tests", project)
	}
	stream, _ := body["stream"].(map[string]any)
	if _, elided := stream["elidedLines"]; elided {
		t.Fatalf("stream = %v, want no elision for the complete session record", stream)
	}
	if len(stderr) != 0 {
		t.Fatalf("stderr = %q, want no note", stderr)
	}
}

// Without a session record to read, the wrapper posts the stdout fold it
// always posted, reports the elision the stream declares, and prints one line
// that says so.
func TestReportWrappedRunFallsBackToStdoutAndSaysSo(t *testing.T) {
	fake := &reportFakeServer{}
	ioctx, root := newReportTestIO(t, fake)
	var stderr []string
	ioctx.Stderr = func(line string) { stderr = append(stderr, line) }
	session := readVerboseSessionCapture(t)
	elided := bytes.Replace(session,
		[]byte(`"elided":{"ordinary":{"records":0,"bytes":0},"failure":{"records":0,"bytes":0}}`),
		[]byte(`"elided":{"ordinary":{"records":7,"bytes":700},"failure":{"records":0,"bytes":0}}`), 1)
	stubReportGit(t, &fakeGit{t: t, outs: cleanGitOutputs()})
	stubReportWrappedJSONL(t, 0, nil, string(elided))

	if err := runReport(t, ioctx, root, []string{"--", "putnami", "test", "--output=jsonl"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(fake.reportRequests) != 1 {
		t.Fatalf("report POSTs = %d, want 1", len(fake.reportRequests))
	}
	body := fake.reportRequests[0].Body
	if len(batchProjects(t, body, "tests")) != 1 || len(batchProjects(t, body, "coverage")) != 1 {
		t.Fatalf("tests = %v, coverage = %v, want the stdout fold", body["tests"], body["coverage"])
	}
	stream, _ := body["stream"].(map[string]any)
	if stream["elidedLines"] != float64(7) {
		t.Fatalf("stream = %v, want elidedLines 7", stream)
	}
	if len(stderr) != 1 || !strings.HasPrefix(stderr[0], "note: delivery reports come from the command's stdout, not its session record: ") {
		t.Fatalf("stderr = %q, want one note line", stderr)
	}
}
