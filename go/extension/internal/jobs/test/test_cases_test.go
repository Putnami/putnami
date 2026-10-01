package test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/parse"
	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// timedTestEvent is testEvent with the Elapsed seconds of a result event.
func timedTestEvent(action, pkg, testName string, elapsed float64) string {
	data, _ := json.Marshal(parse.TestEvent{Action: action, Package: pkg, Test: testName, Elapsed: elapsed})
	return string(data)
}

func TestRecordTestCasesKeepsTheBoundAndCountsTheRest(t *testing.T) {
	cases := make([]protocolcli.TestCase, 0, protocolcli.TestCaseMaxPerTask+2)
	for i := range protocolcli.TestCaseMaxPerTask {
		cases = append(cases, protocolcli.TestCase{
			Name: fmt.Sprintf("TestPass%04d", i), Suite: "example.com/a", Status: protocolcli.TestCaseStatusPassed,
		})
	}
	cases = append(cases,
		protocolcli.TestCase{Name: "TestFailA", Suite: "example.com/a", Status: protocolcli.TestCaseStatusFailed},
		protocolcli.TestCase{Name: "TestFailB", Suite: "example.com/a", Status: protocolcli.TestCaseStatusFailed},
	)

	data := map[string]any{}
	recordTestCases(data, cases, protocolcli.TestCaseMaxBytesPerTask)
	kept, ok := data["testCases"].([]protocolcli.TestCase)
	if !ok || len(kept) != protocolcli.TestCaseMaxPerTask {
		t.Fatalf("testCases = %d cases, want %d", len(kept), protocolcli.TestCaseMaxPerTask)
	}
	failed := 0
	for _, testCase := range kept {
		if testCase.Status == protocolcli.TestCaseStatusFailed {
			failed++
		}
	}
	if failed != 2 {
		t.Errorf("kept %d failed cases, want both", failed)
	}
	if data["testCasesDropped"] != 2 {
		t.Errorf("testCasesDropped = %v, want 2", data["testCasesDropped"])
	}
}

func TestRecordTestCasesWritesNothingForNoCases(t *testing.T) {
	data := map[string]any{}
	recordTestCases(data, nil, protocolcli.TestCaseMaxBytesPerTask)
	if len(data) != 0 {
		t.Fatalf("data = %v, want no test-case keys", data)
	}
	// A case without a suite breaks the contract: it is dropped and counted.
	recordTestCases(data, []protocolcli.TestCase{{Name: "TestA", Status: protocolcli.TestCaseStatusPassed}}, protocolcli.TestCaseMaxBytesPerTask)
	if _, ok := data["testCases"]; ok || data["testCasesDropped"] != 1 {
		t.Fatalf("data = %v, want only testCasesDropped 1", data)
	}
}

func TestRecordTestCasesKeepsWithinItsByteBudget(t *testing.T) {
	failed := protocolcli.TestCase{
		Name: "TestFail", Suite: "example.com/a", Status: protocolcli.TestCaseStatusFailed, Output: strings.Repeat("x", 4000),
	}
	passed := protocolcli.TestCase{Name: "TestPass", Suite: "example.com/a", Status: protocolcli.TestCaseStatusPassed}

	data := map[string]any{}
	// The budget of a member of a 2048-project batch is 4 KiB: the failed case
	// alone fills it, so the passed case is dropped and counted.
	recordTestCases(data, []protocolcli.TestCase{passed, failed}, protocolcli.TestCaseBatchMemberBytes(2048))
	kept, _ := data["testCases"].([]protocolcli.TestCase)
	if len(kept) != 1 || kept[0].Name != "TestFail" || data["testCasesDropped"] != 1 {
		t.Fatalf("data = %v, want the failed case kept and 1 dropped", data)
	}
}

func TestRunBatchRecordsEachMembersOwnTestCases(t *testing.T) {
	ctx := makeBatchTestContext(t, false)
	t.Setenv("GOWORK", "off")
	mustWrite(t, filepath.Join(ctx.WorkspaceRoot, "b", "b_test.go"), "package b\n")
	output := strings.Join([]string{
		testEvent("run", "example.com/a", "TestA", ""),
		timedTestEvent("pass", "example.com/a", "TestA", 0.12),
		testEvent("run", "example.com/b", "TestB", ""),
		testEvent("output", "example.com/b", "TestB", "=== RUN   TestB\n"),
		testEvent("output", "example.com/b", "TestB", "    b_test.go:9: boom\n"),
		testEvent("output", "example.com/b", "TestB", "--- FAIL: TestB (0.03s)\n"),
		timedTestEvent("fail", "example.com/b", "TestB", 0.03),
		testEvent("run", "example.com/a", "TestA2", ""),
		timedTestEvent("skip", "example.com/a", "TestA2", 0),
		testEvent("pass", "example.com/a", "", ""),
		testEvent("fail", "example.com/b", "", ""),
	}, "\n") + "\n"
	mockGoCommand(t, func(_ string, _ []string, _ string, _ []string) ([]byte, error) {
		return []byte(output), errors.New("exit status 1")
	})

	_, data, err := runBatch(ctx, nil)
	if err != nil {
		t.Fatalf("runBatch: %v", err)
	}
	results := data["batchResults"].([]batchProjectResult)
	want := map[string][]protocolcli.TestCase{
		"/a": {
			{Name: "TestA", Suite: "example.com/a", Status: protocolcli.TestCaseStatusPassed, DurationMs: 120},
			{Name: "TestA2", Suite: "example.com/a", Status: protocolcli.TestCaseStatusSkipped},
		},
		"/b": {{
			Name:       "TestB",
			Suite:      "example.com/b",
			Status:     protocolcli.TestCaseStatusFailed,
			DurationMs: 30,
			Output:     "b_test.go:9: boom",
			File:       "b/b_test.go",
			Line:       9,
		}},
	}
	for _, result := range results {
		got := result.Data["testCases"]
		if !reflect.DeepEqual(got, want[result.ProjectID]) {
			t.Errorf("%s testCases = %#v, want %#v", result.ProjectID, got, want[result.ProjectID])
		}
		if _, ok := result.Data["testCasesDropped"]; ok {
			t.Errorf("%s data = %v, want no testCasesDropped", result.ProjectID, result.Data)
		}
	}
}

// writeSoloTestModule lays out module example.com/solo at <workspace>/solo
// with one test file.
func writeSoloTestModule(t *testing.T) *pctx.Context {
	t.Helper()
	workspace := t.TempDir()
	moduleRoot := filepath.Join(workspace, "solo")
	if err := os.MkdirAll(moduleRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(moduleRoot, "go.mod"), "module example.com/solo\n\ngo 1.25.7\n")
	mustWrite(t, filepath.Join(moduleRoot, "solo_test.go"), "package solo\n")
	mockGoCommand(t, func(_ string, _ []string, _ string, _ []string) ([]byte, error) {
		return []byte(strings.Join([]string{
			testEvent("run", "example.com/solo", "TestOK", ""),
			timedTestEvent("pass", "example.com/solo", "TestOK", 0.5),
			testEvent("run", "example.com/solo", "TestBad", ""),
			testEvent("output", "example.com/solo", "TestBad", "    solo_test.go:5: bad\n"),
			timedTestEvent("fail", "example.com/solo", "TestBad", 0.004),
			testEvent("fail", "example.com/solo", "", ""),
		}, "\n") + "\n"), errors.New("exit status 1")
	})
	return &pctx.Context{
		WorkspaceRoot: workspace,
		OutputPath:    t.TempDir(),
		Project:       pctx.Project{Name: "solo", FullPath: moduleRoot},
		Params:        pctx.Params{"coverage": json.RawMessage(`false`)},
	}
}

func TestRunRecordsTestCases(t *testing.T) {
	ctx := writeSoloTestModule(t)
	status, data, err := Run(ctx, jsonl.New(), nil)
	if err != nil || status != "FAILED" {
		t.Fatalf("Run status=%q err=%v, want FAILED", status, err)
	}
	want := []protocolcli.TestCase{
		{Name: "TestOK", Suite: "example.com/solo", Status: protocolcli.TestCaseStatusPassed, DurationMs: 500},
		{
			Name:       "TestBad",
			Suite:      "example.com/solo",
			Status:     protocolcli.TestCaseStatusFailed,
			DurationMs: 4,
			Output:     "solo_test.go:5: bad",
			File:       "solo/solo_test.go",
			Line:       5,
		},
	}
	if got := data["testCases"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("testCases = %#v, want %#v", got, want)
	}
	summary := data["testSummary"].(map[string]any)
	if summary["passed"] != 1 || summary["failed"] != 1 || summary["total"] != 2 {
		t.Fatalf("testSummary = %v, want it unchanged by the cases", summary)
	}
}

func TestRunWithoutJSONRecordsNoTestCases(t *testing.T) {
	ctx := writeSoloTestModule(t)
	ctx.Params["test-json"] = json.RawMessage(`false`)
	_, data, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, key := range []string{"testCases", "testCasesDropped"} {
		if _, ok := data[key]; ok {
			t.Errorf("data has %s = %v, want none without -json", key, data[key])
		}
	}
}
