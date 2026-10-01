package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/parse"
)

// casesJUnitXML is a bun report of one test file with a nested describe, a
// failing case, a skipped case and a passing one.
func casesJUnitXML(file string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="bun test" tests="3" assertions="2" failures="1" skipped="1" time="0.3">
  <testsuite name="` + file + `" file="` + file + `" tests="3" assertions="2" failures="1" skipped="1" time="0">
    <testsuite name="math" file="` + file + `" line="3" tests="3" assertions="2" failures="1" skipped="1" time="0.3">
      <testcase name="adds" classname="math" time="0.25" file="` + file + `" line="4" assertions="1" />
      <testcase name="divides" classname="math" time="0.05" file="` + file + `" line="8" assertions="1">
        <failure type="AssertionError" message="Expected: 2">AssertionError: Expected: 2&#10;      at ` + file + `:9:5</failure>
      </testcase>
      <testcase name="rounds" classname="math" time="0" file="` + file + `" line="12" assertions="0">
        <skipped message="later" />
      </testcase>
    </testsuite>
  </testsuite>
</testsuites>`
}

// wantCases is the protocol form of casesJUnitXML for a test file at the
// workspace-relative path file.
func wantCases(file, reported string) []protocolcli.TestCase {
	return []protocolcli.TestCase{
		{Name: "math > adds", Suite: file, Status: protocolcli.TestCaseStatusPassed, DurationMs: 250, File: file, Line: 4},
		{
			Name:       "math > divides",
			Suite:      file,
			Status:     protocolcli.TestCaseStatusFailed,
			DurationMs: 50,
			Output:     "AssertionError: Expected: 2\n      at " + reported + ":9:5",
			File:       file,
			Line:       8,
		},
		{Name: "math > rounds", Suite: file, Status: protocolcli.TestCaseStatusSkipped, Output: "later", File: file, Line: 12},
	}
}

func TestRunTestRecordsTestCases(t *testing.T) {
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	testDir := filepath.Join(dir, "project", "test")
	if err := os.MkdirAll(testDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "math.test.ts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		if err := os.MkdirAll(ctx.OutputPath, 0o755); err != nil {
			return nil, err
		}
		junit := casesJUnitXML("test/math.test.ts")
		if err := os.WriteFile(filepath.Join(ctx.OutputPath, "results.junit.xml"), []byte(junit), 0o644); err != nil {
			return nil, err
		}
		return &exec.Result{Success: false, ExitCode: 1}, nil
	})

	status, data, err := runTest(ctx, jsonl.New(), nil)
	if err != nil || status != "FAILED" {
		t.Fatalf("runTest status=%q err=%v, want FAILED", status, err)
	}
	if got, want := data["testCases"], wantCases("project/test/math.test.ts", "test/math.test.ts"); !reflect.DeepEqual(got, want) {
		t.Fatalf("testCases =\n  %#v\nwant\n  %#v", got, want)
	}
	if _, ok := data["testCasesDropped"]; ok {
		t.Fatalf("data = %v, want no testCasesDropped", data)
	}
	summary := data["testSummary"].(map[string]any)
	if summary["total"] != 3 || summary["failed"] != 1 || summary["skipped"] != 1 {
		t.Fatalf("testSummary = %v, want it unchanged by the cases", summary)
	}
}

func TestRunTestBatchRecordsEachProjectsOwnTestCases(t *testing.T) {
	mockBunResolution(t)
	ctx, _ := makeTestBatchContext(t)
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		outDir := reporterOutDir(args)
		name := "a"
		if strings.Contains(outDir, filepath.Join("packages", "b")+string(filepath.Separator)) {
			name = "b"
		}
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return nil, err
		}
		junit := casesJUnitXML("test/" + name + ".test.ts")
		if err := os.WriteFile(filepath.Join(outDir, "results.junit.xml"), []byte(junit), 0o644); err != nil {
			return nil, err
		}
		return &exec.Result{Success: false, ExitCode: 1}, nil
	})

	_, data, err := runTestBatch(ctx, jsonl.New())
	if err != nil {
		t.Fatalf("runTestBatch: %v", err)
	}
	results := data["batchResults"].([]testBatchProjectResult)
	if len(results) != 2 {
		t.Fatalf("batchResults = %#v, want two", results)
	}
	for _, result := range results {
		name := filepath.Base(result.ProjectID)
		want := wantCases("packages/"+name+"/test/"+name+".test.ts", "test/"+name+".test.ts")
		if got := result.Data["testCases"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s testCases =\n  %#v\nwant\n  %#v", result.ProjectID, got, want)
		}
	}
}

func TestRecordTestCasesKeepsTheBoundAndCountsTheRest(t *testing.T) {
	summary := &parse.TestSummary{}
	for i := range protocolcli.TestCaseMaxPerTask + 1 {
		summary.Cases = append(summary.Cases, parse.TestCase{
			Name: fmt.Sprintf("case %04d", i), SuiteFile: "test/a.test.ts", Status: parse.TestCasePassed,
		})
	}
	// A case whose suite file lies outside the workspace has no suite, so the
	// contract drops it too.
	summary.Cases = append(summary.Cases, parse.TestCase{
		Name: "escapes", SuiteFile: "../../outside.test.ts", Status: parse.TestCaseFailed,
	})

	data := map[string]any{}
	recordTestCases(data, "/ws", "packages/a", summary, protocolcli.TestCaseMaxBytesPerTask)
	kept, _ := data["testCases"].([]protocolcli.TestCase)
	if len(kept) != protocolcli.TestCaseMaxPerTask {
		t.Fatalf("testCases = %d cases, want %d", len(kept), protocolcli.TestCaseMaxPerTask)
	}
	if kept[0].Suite != "packages/a/test/a.test.ts" {
		t.Fatalf("suite = %q, want the workspace-relative test file", kept[0].Suite)
	}
	if data["testCasesDropped"] != 2 {
		t.Fatalf("testCasesDropped = %v, want 2", data["testCasesDropped"])
	}

	empty := map[string]any{}
	recordTestCases(empty, "/ws", "packages/a", &parse.TestSummary{}, protocolcli.TestCaseMaxBytesPerTask)
	if len(empty) != 0 {
		t.Fatalf("data = %v, want no test-case keys for a report without cases", empty)
	}
}

func TestWorkspaceTestPath(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "ws")
	for _, test := range []struct {
		project, file, want string
	}{
		{project: "packages/a", file: "test/a.test.ts", want: "packages/a/test/a.test.ts"},
		{project: "packages/a", file: "../shared/helper.ts", want: "packages/shared/helper.ts"},
		{project: "packages/a", file: filepath.Join(root, "packages", "a", "x.test.ts"), want: "packages/a/x.test.ts"},
		{project: filepath.Join(root, "packages", "a"), file: "test/a.test.ts", want: "packages/a/test/a.test.ts"},
		{project: "packages/a", file: "../../../outside.test.ts"},
		{project: "packages/a", file: filepath.Join(string(filepath.Separator), "elsewhere", "x.test.ts")},
		{project: "packages/a", file: "../.."},
		{project: "packages/a", file: ""},
	} {
		if got := workspaceTestPath(root, test.project, test.file); got != test.want {
			t.Errorf("workspaceTestPath(%q, %q) = %q, want %q", test.project, test.file, got, test.want)
		}
	}
}
