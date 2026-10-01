package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/testjob"
)

const passingJUnitXML = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="1" failures="0">
  <testsuite name="test" tests="1" failures="0">
    <testcase name="passes" classname="test"/>
  </testsuite>
</testsuites>`

const failingJUnitXML = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="2" failures="1">
  <testsuite name="test" tests="2" failures="1">
    <testcase name="passes" classname="test"/>
    <testcase name="fails" classname="test">
      <failure message="assertion failed" type="AssertionError"/>
    </testcase>
  </testsuite>
</testsuites>`

// reporterOutDir extracts the bun --reporter-outfile output directory so a mock
// can write each project's JUnit report to that project's own output path.
func reporterOutDir(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "--reporter-outfile=") {
			return filepath.Dir(strings.TrimPrefix(a, "--reporter-outfile="))
		}
	}
	return ""
}

// makeTestBatchContext sets up a workspace with two sibling projects that each
// have a test file, and a job context selecting both with their own output dirs.
func makeTestBatchContext(t *testing.T) (*pctx.Context, string) {
	t.Helper()
	ctx, root := makeTestCtx(t)
	ctx.Job = pctx.Job{Name: "test"}
	selected := make([]pctx.ProjectRef, 0, 2)
	for _, name := range []string{"a", "b"} {
		path := filepath.Join("packages", name)
		full := filepath.Join(root, path)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, "foo.test.ts"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		selected = append(selected, pctx.ProjectRef{
			ID:         "/packages/" + name,
			Name:       name,
			Path:       path,
			FullPath:   full,
			OutputPath: filepath.Join(root, ".putnami", "out", path, "test"),
		})
	}
	ctx.SelectedProjects = selected
	return ctx, root
}

// TestRunTestBatchSplitsPerProjectResultsAndOutputDirs asserts one process runs
// both suites, each project's status is independent, and each project's JUnit
// report lands in its OWN output directory.
func TestRunTestBatchSplitsPerProjectResultsAndOutputDirs(t *testing.T) {
	mockBunResolution(t)
	ctx, root := makeTestBatchContext(t)

	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		outDir := reporterOutDir(args)
		fail := strings.Contains(outDir, filepath.Join("packages", "b")+string(filepath.Separator))
		junit := passingJUnitXML
		if fail {
			junit = failingJUnitXML
		}
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(outDir, "results.junit.xml"), []byte(junit), 0o644); err != nil {
			return nil, err
		}
		transcript := "a transcript\n"
		if fail {
			transcript = "b transcript\n"
		}
		return &exec.Result{Success: !fail, ExitCode: 0, Stdout: transcript}, nil
	})

	var status string
	var data map[string]any
	var err error
	events := captureEvents(t, func() {
		status, data, err = runTestBatch(ctx, jsonl.New())
	})
	if err != nil || status != "OK" {
		t.Fatalf("runTestBatch status=%q err=%v, want OK/nil (process-level success)", status, err)
	}
	results, ok := data["batchResults"].([]testBatchProjectResult)
	if !ok || len(results) != 2 {
		t.Fatalf("batchResults = %#v", data["batchResults"])
	}
	if results[0].ProjectID != "/packages/a" || results[0].Status != "OK" {
		t.Fatalf("project a = %+v, want OK", results[0])
	}
	if results[1].ProjectID != "/packages/b" || results[1].Status != "FAILED" {
		t.Fatalf("project b = %+v, want FAILED", results[1])
	}
	// Per-project isolation: a's OK testSummary must reflect its own suite.
	if ts := results[0].Data["testSummary"].(map[string]any); ts["total"].(int) != 1 || ts["failed"].(int) != 0 {
		t.Fatalf("project a testSummary = %v", ts)
	}
	if ts := results[1].Data["testSummary"].(map[string]any); ts["failed"].(int) != 1 {
		t.Fatalf("project b testSummary = %v", ts)
	}
	// Per-project output dirs: each JUnit report is captured under its project.
	for _, name := range []string{"a", "b"} {
		junit := filepath.Join(root, ".putnami", "out", "packages", name, "test", "results.junit.xml")
		if _, err := os.Stat(junit); err != nil {
			t.Fatalf("project %s JUnit not written to its own output dir: %v", name, err)
		}
	}
	wantTranscript := map[string]string{"a transcript": "/packages/a", "b transcript": "/packages/b"}
	for message, projectID := range wantTranscript {
		found := false
		for _, event := range events {
			if event["type"] != "log" || event["message"] != message {
				continue
			}
			contextData, _ := event["context"].(map[string]any)
			if contextData[protocolcli.BatchProjectLogContextKey] != projectID {
				t.Fatalf("%q context = %#v, want %s", message, contextData, projectID)
			}
			found = true
		}
		if !found {
			t.Fatalf("batch producer emitted no %q transcript: %+v", message, events)
		}
	}
}

// TestRunTestBatchEquivalentToSolo pins the batched == solo invariant: for the
// same project the batched result carries the identical status, testSummary,
// and coverageSummary the solo path produces.
func TestRunTestBatchEquivalentToSolo(t *testing.T) {
	// Validation cadence: --enforce-coverage collects the profile so both paths
	// emit a coverageSummary and share the gate result.
	params := pctx.Params{
		"enforce-coverage":   json.RawMessage(`true`),
		"coverage-threshold": json.RawMessage(`40`),
	}

	// Solo run.
	soloCtx := setupCoverageRun(t, params, lcov50pct)
	soloStatus, soloData, err := runTest(soloCtx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("solo runTest: %v", err)
	}

	// Batch run for the same single project.
	mockBunResolution(t)
	batchCtx, root := makeTestCtx(t)
	batchCtx.Job = pctx.Job{Name: "test"}
	batchCtx.Params = params
	projectPath := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, "test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "test", "foo.test.ts"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectPath, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	batchCtx.SelectedProjects = []pctx.ProjectRef{{
		ID:         "/project",
		Name:       "@test/pkg",
		Path:       "project",
		FullPath:   projectPath,
		OutputPath: batchCtx.OutputPath,
	}}
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		outDir := reporterOutDir(args)
		os.MkdirAll(outDir, 0o755)
		os.WriteFile(filepath.Join(outDir, "results.junit.xml"), []byte(passingJUnitXML), 0o644)
		os.WriteFile(filepath.Join(outDir, "lcov.info"), []byte(lcov50pct), 0o644)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	_, batchData, err := runTestBatch(batchCtx, nil)
	if err != nil {
		t.Fatalf("runTestBatch: %v", err)
	}
	results := batchData["batchResults"].([]testBatchProjectResult)
	if len(results) != 1 {
		t.Fatalf("batch results = %d, want 1", len(results))
	}
	if soloStatus != "OK" || results[0].Status != "OK" {
		t.Fatalf("status solo=%q batch=%q, want both OK", soloStatus, results[0].Status)
	}
	if !reflect.DeepEqual(results[0].Data, soloData) {
		t.Fatalf("batched data\n  %#v\ndiffers from solo\n  %#v", results[0].Data, soloData)
	}
	if results[0].Data["coverageSummary"] == nil {
		t.Fatal("expected coverageSummary in equivalence result")
	}
}

// TestRunTestBatchCrashIsolationRetriesAndDoesNotMaskPeers asserts a crashing
// suite is respawned once, then isolated as that project's FAILED result while
// its batch-mate still passes and the process itself succeeds.
func TestRunTestBatchCrashIsolationRetriesAndDoesNotMaskPeers(t *testing.T) {
	mockBunResolution(t)
	ctx, _ := makeTestBatchContext(t)

	orig := runProjectBunTest
	t.Cleanup(func() { runProjectBunTest = orig })
	var aCalls int
	runProjectBunTest = func(_ /*bunBin*/, projectPath, outputPath string, _ testjob.TestParams) (bool, string, error) {
		if strings.Contains(projectPath, filepath.Join("packages", "a")) {
			aCalls++
			// Non-graceful crash: no parseable result written.
			return false, "", errors.New("bun exited abnormally")
		}
		// Project b runs to completion successfully.
		if err := os.MkdirAll(outputPath, 0o755); err != nil {
			return false, "", err
		}
		if err := os.WriteFile(filepath.Join(outputPath, "results.junit.xml"), []byte(passingJUnitXML), 0o644); err != nil {
			return false, "", err
		}
		return true, "", nil
	}

	status, data, err := runTestBatch(ctx, nil)
	if err != nil || status != "OK" {
		t.Fatalf("crash must not fail the whole batch: status=%q err=%v", status, err)
	}
	if aCalls != 2 {
		t.Fatalf("crashed suite ran %d times, want one respawn (2)", aCalls)
	}
	results := data["batchResults"].([]testBatchProjectResult)
	if results[0].Status != "FAILED" {
		t.Fatalf("crashed project a = %+v, want FAILED", results[0])
	}
	if results[1].Status != "OK" {
		t.Fatalf("healthy project b = %+v, want OK (not masked by crash)", results[1])
	}
	if len(results[0].Diagnostics) == 0 {
		t.Fatal("crashed project must carry an explanatory diagnostic")
	}
}

// TestTestFailureDiagnosticsUseOwnProjectPath pins the follower-attribution fix:
// a failed test in a non-leader project must be reported under THAT project's
// path, not the batch leader's (ctx.Project).
func TestTestFailureDiagnosticsUseOwnProjectPath(t *testing.T) {
	summary := &parse.TestSummary{
		FailedTests: []parse.FailedTest{{
			Name:    "fails",
			File:    "foo.test.ts",
			Line:    7,
			Failure: &parse.TestFailure{Type: "AssertionError", Message: "boom"},
		}},
	}
	proj := pctx.ProjectRef{Name: "b", Path: filepath.Join("packages", "b")}
	diags := testFailureDiagnostics(proj, summary, "")
	if len(diags) != 1 {
		t.Fatalf("want 1 diagnostic, got %d", len(diags))
	}
	want := filepath.Join("packages", "b", "foo.test.ts")
	if diags[0].File != want {
		t.Fatalf("diagnostic file = %q, want %q (own project, not leader)", diags[0].File, want)
	}
}

// TestRunTestBatchSurfacesUnhandledErrors pins the diagnostic-parity fix: a
// batched Bun failure with no JUnit <failure> entry (an import-time crash)
// still surfaces the unhandled error with file/line, exactly as solo does.
func TestRunTestBatchSurfacesUnhandledErrors(t *testing.T) {
	mockBunResolution(t)
	ctx, _ := makeTestBatchContext(t)

	orig := runProjectBunTest
	t.Cleanup(func() { runProjectBunTest = orig })
	runProjectBunTest = func(_ /*bunBin*/, projectPath, outputPath string, _ testjob.TestParams) (bool, string, error) {
		if err := os.MkdirAll(outputPath, 0o755); err != nil {
			return false, "", err
		}
		// A JUnit with zero <failure> entries: the suite failed overall but has
		// no per-test failure to attribute.
		if err := os.WriteFile(filepath.Join(outputPath, "results.junit.xml"), []byte(passingJUnitXML), 0o644); err != nil {
			return false, "", err
		}
		if strings.Contains(projectPath, filepath.Join("packages", "a")) {
			logs := "# Unhandled error between tests\nTypeError: undefined is not an object\nat src/index.ts:12:9"
			return false, logs, nil // graceful failure (has logs), so no crash retry
		}
		return true, "", nil
	}

	status, data, err := runTestBatch(ctx, nil)
	if err != nil || status != "OK" {
		t.Fatalf("status=%q err=%v, want OK/nil", status, err)
	}
	results := data["batchResults"].([]testBatchProjectResult)
	if results[0].Status != "FAILED" {
		t.Fatalf("project a = %+v, want FAILED", results[0])
	}
	found := false
	for _, d := range results[0].Diagnostics {
		if strings.Contains(d.Description, "undefined is not an object") {
			found = true
			want := filepath.Join("packages", "a", "src", "index.ts")
			if d.File != want {
				t.Fatalf("unhandled-error diagnostic file = %q, want %q", d.File, want)
			}
		}
	}
	if !found {
		t.Fatal("batched failure lost the unhandled-error diagnostic that solo emits")
	}
}

// TestTestParamsFromContextCoverageCadence pins the coverage switches at the TS
// param-resolution seam that BOTH solo (test.go:runTest) and batch
// (test_batch.go:runProjectTest) share verbatim. Two independent axes:
// `coverage` decides whether the run measures, `enforce-coverage` decides
// whether the threshold may fail it. Only `coverage:false` turns measurement
// off, and enforce-coverage never overrides that opt-out. This is the TS twin of
// the Go TestGoCoverageCadenceSoloBatchParity and must stay row-for-row
// identical to it.
func TestTestParamsFromContextCoverageCadence(t *testing.T) {
	cases := []struct {
		name          string
		coverage      string // "", "true", or "false"
		enforce       string // "", "true", or "false"
		threshold     string // "" or a number
		wantCoverage  bool
		wantThreshold float64
	}{
		{"default: measure and gate", "", "", "", true, 0},
		{"default with configured threshold", "", "", "70", true, 70},
		{"default with coverage:true", "true", "", "70", true, 70},
		{"explicit opt-out with threshold", "false", "", "70", false, 0},
		{"no-enforce still measures", "", "false", "70", true, 0},
		{"no-enforce without threshold", "", "false", "", true, 0},
		{"no-enforce plus opt-out measures nothing", "false", "false", "70", false, 0},
		{"explicit enforce, no threshold", "", "true", "", true, 0},
		{"explicit enforce with threshold", "", "true", "70", true, 70},
		{"explicit enforce but project opts out", "false", "true", "70", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := pctx.Params{}
			if tc.coverage != "" {
				params["coverage"] = json.RawMessage(tc.coverage)
			}
			if tc.enforce != "" {
				params["enforce-coverage"] = json.RawMessage(tc.enforce)
			}
			if tc.threshold != "" {
				params["coverage-threshold"] = json.RawMessage(tc.threshold)
			}
			got := testParamsFromContext(&pctx.Context{Params: params})
			if got.Coverage != tc.wantCoverage {
				t.Fatalf("Coverage = %v, want %v", got.Coverage, tc.wantCoverage)
			}
			if got.CoverageThreshold != tc.wantThreshold {
				t.Fatalf("CoverageThreshold = %v, want %v", got.CoverageThreshold, tc.wantThreshold)
			}
		})
	}
}

// TestTestRunManifestBatchableAvoidsPerProjectConfig pins the P1 lesson from
// The batch config candidates must be workspace/extension-scoped shared
// files so sibling test projects resolve the SAME digest and actually group. A
// per-project always-present file ({projectRoot}/...) would give every project
// a distinct digest and silently disable batching.
func TestTestRunManifestBatchableAvoidsPerProjectConfig(t *testing.T) {
	batch := readTestRunBatchable(t, filepath.Join("..", "..", "putnami.extension.json"))
	if batch.Tool != "bun" {
		t.Fatalf("test-run batch tool = %q, want bun", batch.Tool)
	}
	if batch.MaxWorkers <= 0 || batch.MaxProjects <= 0 {
		t.Fatalf("conservative gate missing: maxWorkers=%d maxProjects=%d", batch.MaxWorkers, batch.MaxProjects)
	}
	if len(batch.ConfigFiles) == 0 {
		t.Fatal("test-run batch declares no config files")
	}
	for _, cf := range batch.ConfigFiles {
		if strings.Contains(cf, "{projectRoot}") {
			t.Fatalf("config file %q is per-project; siblings would never share a batch key", cf)
		}
		if !strings.HasPrefix(cf, "{workspaceRoot}") && !strings.HasPrefix(cf, "{extensionRoot}") {
			t.Fatalf("config file %q is not a shared workspace/extension file", cf)
		}
	}
}

type manifestBatchable struct {
	Tool        string   `json:"tool"`
	MaxWorkers  int      `json:"maxWorkers"`
	MaxProjects int      `json:"maxProjects"`
	ConfigFiles []string `json:"configFiles"`
}

func readTestRunBatchable(t *testing.T, manifestPath string) manifestBatchable {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Batchable *manifestBatchable `json:"batchable"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	task, ok := manifest.Tasks["test-run"]
	if !ok || task.Batchable == nil {
		t.Fatal("test-run task has no batchable policy")
	}
	return *task.Batchable
}

// TestRunProjectBunTestWithRetryNeverRetriesAFailedSuite pins that test runs
// never retry a failing test: only a suite bun could not start runs twice.
func TestRunProjectBunTestWithRetryNeverRetriesAFailedSuite(t *testing.T) {
	orig := runProjectBunTest
	t.Cleanup(func() { runProjectBunTest = orig })
	calls := 0
	runProjectBunTest = func(string, string, string, testjob.TestParams) (bool, string, error) {
		calls++
		return false, "1 fail", nil
	}
	if success, _, err := runProjectBunTestWithRetry("bun", "p", "o", testjob.TestParams{}); success || err != nil || calls != 1 {
		t.Fatalf("a failed suite: success=%v err=%v calls=%d, want one failed run", success, err, calls)
	}

	calls = 0
	runProjectBunTest = func(string, string, string, testjob.TestParams) (bool, string, error) {
		calls++
		return false, "", errors.New("executing bun: no such file")
	}
	if _, _, err := runProjectBunTestWithRetry("bun", "p", "o", testjob.TestParams{}); err == nil || calls != 2 {
		t.Fatalf("a suite that did not start: err=%v calls=%d, want an error after two attempts", err, calls)
	}
}
