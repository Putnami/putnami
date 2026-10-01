package test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/parse"
	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

func TestRunBatchUsesOneGoInvocationAndSplitsFailure(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-failure-attribution", "a-batched-failure-is-charged-to-the-project-that-produced-it")
	ctx := makeBatchTestContext(t, false)
	t.Setenv("GOWORK", "off")
	mustWrite(t, filepath.Join(ctx.WorkspaceRoot, "b", "b_test.go"), "package b\n")
	aOutput := strings.Join([]string{
		testEvent("run", "example.com/a", "TestA", ""),
		testEvent("pass", "example.com/a", "TestA", ""),
		testEvent("pass", "example.com/a", "", ""),
	}, "\n") + "\n"
	bOutput := strings.Join([]string{
		testEvent("run", "example.com/b", "TestB", ""),
		testEvent("output", "example.com/b", "TestB", "    b_test.go:9: boom\n"),
		testEvent("fail", "example.com/b", "TestB", ""),
		testEvent("fail", "example.com/b", "", ""),
	}, "\n") + "\n"

	var calls int
	mockGoCommand(t, func(_ string, args []string, _ string, env []string) ([]byte, error) {
		calls++
		for _, pattern := range []string{"./a/...", "./b/..."} {
			if !slices.Contains(args, pattern) {
				t.Fatalf("batch args %v missing %q", args, pattern)
			}
		}
		wantGoWork := "GOWORK=" + physicalTestPath(t, filepath.Join(ctx.WorkspaceRoot, "go.work"))
		if !slices.Contains(env, wantGoWork) || slices.Contains(env, "GOWORK=off") {
			t.Fatalf("batch env GOWORK = %v, want only %q", env, wantGoWork)
		}
		return []byte(aOutput + bOutput), errors.New("exit status 1")
	})

	var status string
	var data map[string]any
	var err error
	events := captureJobEvents(t, func(emit *jsonl.Emitter) {
		status, data, err = runBatch(ctx, emit)
	})
	if err != nil || status != "OK" {
		t.Fatalf("runBatch status=%q err=%v", status, err)
	}
	if calls != 1 {
		t.Fatalf("go test calls = %d, want one", calls)
	}
	results := data["batchResults"].([]batchProjectResult)
	if results[0].ProjectID != "/a" || results[0].Status != "OK" {
		t.Fatalf("project a = %+v", results[0])
	}
	if results[1].ProjectID != "/b" || results[1].Status != "FAILED" {
		t.Fatalf("project b = %+v", results[1])
	}
	wantAData := buildResultData(parse.TestJSON(aOutput), parse.CoverageResult{}, false, 0)
	wantAData["testCases"] = []protocolcli.TestCase{
		{Name: "TestA", Suite: "example.com/a", Status: protocolcli.TestCaseStatusPassed},
	}
	if !reflect.DeepEqual(results[0].Data, wantAData) {
		t.Fatalf("project a data = %#v, want singleton %#v", results[0].Data, wantAData)
	}
	wantBData := buildResultData(parse.TestJSON(bOutput), parse.CoverageResult{}, false, 0)
	wantBData["testCases"] = []protocolcli.TestCase{{
		Name:   "TestB",
		Suite:  "example.com/b",
		Status: protocolcli.TestCaseStatusFailed,
		Output: "b_test.go:9: boom",
		File:   "b/b_test.go",
		Line:   9,
	}}
	if !reflect.DeepEqual(results[1].Data, wantBData) {
		t.Fatalf("project b data = %#v, want singleton %#v", results[1].Data, wantBData)
	}
	wantBDiagnostics := []parse.ToolDiagnostic{
		{Severity: "error", Description: "boom", File: "b/b_test.go", Line: 9},
	}
	if !reflect.DeepEqual(results[1].Diagnostics, wantBDiagnostics) {
		t.Fatalf(
			"project b diagnostics = %+v, want singleton %+v",
			results[1].Diagnostics,
			wantBDiagnostics,
		)
	}
	foundTranscript := false
	for _, event := range events {
		if event["type"] != "log" || event["message"] != "    b_test.go:9: boom" {
			continue
		}
		contextData, _ := event["context"].(map[string]any)
		if contextData[protocolcli.BatchProjectLogContextKey] != "/b" {
			t.Fatalf("batch transcript context = %#v, want /b", contextData)
		}
		foundTranscript = true
	}
	if !foundTranscript {
		t.Fatalf("batch producer emitted no captured transcript event: %+v", events)
	}
}

func TestRunBatchEmitsUnattributedFailureOnceForAllMembers(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-failure-attribution", "an-unattributable-batch-failure-is-reported-once-for-every-member")
	ctx := makeBatchTestContext(t, false)
	output := strings.Join([]string{
		testEvent("output", "example.com/a", "", "a transcript\n"),
		testEvent("output", "example.com/b", "", "b transcript\n"),
		"go: shared workspace setup failed",
	}, "\n") + "\n"
	mockGoCommand(t, func(_ string, _ []string, _ string, _ []string) ([]byte, error) {
		return []byte(output), errors.New("exit status 1")
	})

	var data map[string]any
	var runErr error
	events := captureJobEvents(t, func(emit *jsonl.Emitter) {
		_, data, runErr = runBatch(ctx, emit)
	})
	if runErr != nil {
		t.Fatalf("runBatch: %v", runErr)
	}
	for _, result := range data["batchResults"].([]batchProjectResult) {
		if result.Status != "FAILED" {
			t.Fatalf("result = %+v, want shared failure", result)
		}
	}

	shared := 0
	aIndex, bIndex, sharedIndex := -1, -1, -1
	for i, event := range events {
		switch event["message"] {
		case "a transcript":
			aIndex = i
		case "b transcript":
			bIndex = i
		}
		if event["type"] != "log" || event["message"] != "go: shared workspace setup failed" {
			continue
		}
		shared++
		sharedIndex = i
		contextData, _ := event["context"].(map[string]any)
		projectIDs, _ := contextData[protocolcli.BatchProjectLogsContextKey].([]any)
		if len(projectIDs) != 2 || projectIDs[0] != "/a" || projectIDs[1] != "/b" {
			t.Fatalf("shared transcript context = %#v, want both project ids", contextData)
		}
	}
	if shared != 1 {
		t.Fatalf("shared transcript events = %d, want one aggregate emission: %+v", shared, events)
	}
	if aIndex < 0 || bIndex < 0 || sharedIndex <= aIndex || sharedIndex <= bIndex {
		t.Fatalf("aggregate transcript order a=%d b=%d shared=%d, want source order", aIndex, bIndex, sharedIndex)
	}
}

func TestRunBatchSplitsCoverageAndAppliesPerProjectGate(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-failure-attribution", "each-coverage-verdict-is-charged-to-its-own-project")
	ctx := makeBatchTestContext(t, true)
	ctx.Params["coverage-threshold"] = json.RawMessage(`60`)

	var calls int
	mockGoCommand(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
		calls++
		// Validation cadence: instrument (-cover -coverpkg) and force a fresh run
		// (-count 1) so the merged profile is not polluted by stale cached
		// fragments.
		if !slices.Contains(args, "-count") || !slices.Contains(args, "1") {
			t.Fatalf("coverage gate args = %v, want -count 1", args)
		}
		if !slices.Contains(args, "-cover") || !slices.Contains(args, "-coverpkg") {
			t.Fatalf("coverage args = %v, want -cover -coverpkg", args)
		}
		coverFile := argumentAfter(t, args, "-coverprofile")
		profile := strings.Join([]string{
			"mode: set",
			"example.com/a/a.go:1.1,1.20 1 1",
			"example.com/b/b.go:1.1,1.20 1 0",
			"",
		}, "\n")
		if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
			t.Fatal(err)
		}
		return []byte(strings.Join([]string{
			testEvent("pass", "example.com/a", "TestA", ""),
			testEvent("pass", "example.com/a", "", ""),
			testEvent("pass", "example.com/b", "TestB", ""),
			testEvent("pass", "example.com/b", "", ""),
		}, "\n") + "\n"), nil
	})

	status, data, err := runBatch(ctx, nil)
	if err != nil || status != "OK" {
		t.Fatalf("runBatch status=%q err=%v", status, err)
	}
	if calls != 1 {
		t.Fatalf("go test calls = %d, want one", calls)
	}
	results := data["batchResults"].([]batchProjectResult)
	if results[0].Status != "OK" {
		t.Fatalf("covered project = %+v", results[0])
	}
	if results[1].Status != "FAILED" {
		t.Fatalf("uncovered project = %+v", results[1])
	}
	if got := results[0].Data["coverageSummary"].(map[string]any)["percentage"]; got != float64(100) {
		t.Fatalf("project a coverage = %v, want 100", got)
	}
	if got := results[1].Data["coverageSummary"].(map[string]any)["percentage"]; got != float64(0) {
		t.Fatalf("project b coverage = %v, want 0", got)
	}
	if !hasDiagnosticCode(results[1].Diagnostics, "COVERAGE_THRESHOLD_NOT_MET") {
		t.Fatalf("project b diagnostics = %+v, want threshold failure", results[1].Diagnostics)
	}

	aProfile := filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", "a", "test", "coverage.out")
	bProfile := filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", "b", "test", "coverage.out")
	assertProfileOwnership(t, aProfile, "example.com/a/", "example.com/b/")
	assertProfileOwnership(t, bProfile, "example.com/b/", "example.com/a/")
	if got := results[0].Artifacts; len(got) != 1 ||
		got[0].Path != ".putnami/out/a/test/coverage.out" {
		t.Fatalf("project a artifacts = %+v", got)
	}
	if got := results[1].Artifacts; len(got) != 1 ||
		got[0].Path != ".putnami/out/b/test/coverage.out" {
		t.Fatalf("project b artifacts = %+v", got)
	}
}

// TestRunBatchSplitsCoverageLinePastScannerLimit pins that a profile line
// longer than bufio.Scanner's 64 KiB default neither fails the split nor ends
// the per-project parse early.
func TestRunBatchSplitsCoverageLinePastScannerLimit(t *testing.T) {
	ctx := makeBatchTestContext(t, true)
	ctx.Params["coverage-threshold"] = json.RawMessage(`60`)

	mockGoCommand(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
		profile := strings.Join([]string{
			"mode: set",
			"example.com/a/" + strings.Repeat("x", 70*1024) + ".go:1.1,1.20 1 0",
			"example.com/a/a.go:1.1,1.20 3 1",
			"example.com/b/b.go:1.1,1.20 1 1",
			"",
		}, "\n")
		if err := os.WriteFile(argumentAfter(t, args, "-coverprofile"), []byte(profile), 0o644); err != nil {
			t.Fatal(err)
		}
		return []byte(strings.Join([]string{
			testEvent("pass", "example.com/a", "TestA", ""),
			testEvent("pass", "example.com/b", "TestB", ""),
		}, "\n") + "\n"), nil
	})

	status, data, err := runBatch(ctx, nil)
	if err != nil || status != "OK" {
		t.Fatalf("runBatch status=%q err=%v data=%+v", status, err, data)
	}
	results := data["batchResults"].([]batchProjectResult)
	if got := results[0].Data["coverageSummary"].(map[string]any)["percentage"]; got != float64(75) {
		t.Fatalf("project a coverage = %v, want 75", got)
	}
	if got := results[1].Data["coverageSummary"].(map[string]any)["percentage"]; got != float64(100) {
		t.Fatalf("project b coverage = %v, want 100", got)
	}
}

// TestEvaluateBatchProjectReportsUnreadableProfile pins that a profile that
// exists but cannot be read yields no percentage: with a threshold the project
// fails on COVERAGE_PROFILE_UNREADABLE, never on a guessed number; without one
// it warns and passes.
func TestEvaluateBatchProjectReportsUnreadableProfile(t *testing.T) {
	for _, test := range []struct {
		name       string
		threshold  float64
		wantStatus string
		wantLevel  string
	}{
		{name: "with threshold", threshold: 60, wantStatus: "FAILED", wantLevel: "error"},
		{name: "without threshold", wantStatus: "OK", wantLevel: "warning"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A directory opens, then every read fails.
			project := &batchProject{ref: pctx.ProjectRef{ID: "/a"}, coverFile: t.TempDir()}
			options := batchOptions{coverage: true, coverageThreshold: test.threshold}
			result := evaluateBatchProject("go", options, project, testEvent("pass", "example.com/a", "TestA", "")+"\n", false, "")

			if result.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, test.wantStatus)
			}
			if _, ok := result.Data["coverageSummary"]; ok {
				t.Fatalf("an unreadable profile produced a coverageSummary: %+v", result.Data)
			}
			if hasDiagnosticCode(result.Diagnostics, "COVERAGE_THRESHOLD_NOT_MET") {
				t.Fatalf("diagnostics = %+v, want the unreadable profile, not a threshold verdict", result.Diagnostics)
			}
			var found bool
			for _, diagnostic := range result.Diagnostics {
				if diagnostic.Category != "COVERAGE_PROFILE_UNREADABLE" {
					continue
				}
				found = true
				if diagnostic.Severity != test.wantLevel || !strings.Contains(diagnostic.Description, project.coverFile) {
					t.Fatalf("diagnostic = %+v, want %s naming %s", diagnostic, test.wantLevel, project.coverFile)
				}
			}
			if !found {
				t.Fatalf("diagnostics = %+v, want COVERAGE_PROFILE_UNREADABLE", result.Diagnostics)
			}
		})
	}
}

// TestRunBatchNoEnforceStillMeasuresButNeverFails pins the batch escape hatch:
// --no-enforce-coverage keeps instrumentation on and still reports every
// project's coverageSummary, but a project under its threshold stays OK. This is
// the whole point of the flag — you buy out of failing, not out of seeing. It is
// the batch twin of the solo TestRun_NoEnforceStillMeasuresButNeverFails.
func TestRunBatchNoEnforceStillMeasuresButNeverFails(t *testing.T) {
	ctx := makeBatchTestContext(t, false)
	ctx.Params["coverage-threshold"] = json.RawMessage(`70`)
	t.Setenv("GOWORK", "off")

	var calls int
	mockGoCommand(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
		calls++
		if !slices.Contains(args, "-cover") || !slices.Contains(args, "-coverpkg") {
			t.Fatalf("args = %v, want -cover/-coverpkg (measurement survives --no-enforce-coverage)", args)
		}
		// The forced -count 1 exists only to keep a gated profile accurate. With
		// nothing to fail, the run keeps Go's test cache instead.
		if slices.Contains(args, "-count") {
			t.Fatalf("args = %v, want NO forced -count (gate is inert)", args)
		}
		coverFile := argumentAfter(t, args, "-coverprofile")
		profile := strings.Join([]string{
			"mode: set",
			"example.com/a/a.go:1.1,1.20 1 1",
			"example.com/b/b.go:1.1,1.20 1 0",
			"",
		}, "\n")
		if err := os.WriteFile(coverFile, []byte(profile), 0o644); err != nil {
			t.Fatal(err)
		}
		return []byte(strings.Join([]string{
			testEvent("pass", "example.com/a", "TestA", ""),
			testEvent("pass", "example.com/a", "", ""),
			testEvent("pass", "example.com/b", "TestB", ""),
			testEvent("pass", "example.com/b", "", ""),
		}, "\n") + "\n"), nil
	})

	status, data, err := runBatch(ctx, nil)
	if err != nil || status != "OK" {
		t.Fatalf("runBatch status=%q err=%v", status, err)
	}
	if calls != 1 {
		t.Fatalf("go test calls = %d, want one", calls)
	}
	results := data["batchResults"].([]batchProjectResult)
	for _, result := range results {
		if result.Status != "OK" {
			t.Fatalf("project %s = %+v, want OK (gate inert without enforcement)", result.ProjectID, result)
		}
		if _, ok := result.Data["coverageSummary"]; !ok {
			t.Fatalf("project %s lost its coverageSummary; --no-enforce-coverage must still measure", result.ProjectID)
		}
	}
	// Project b sits at 0% against a threshold of 70 and still passes.
	if got := results[1].Data["coverageSummary"].(map[string]any)["percentage"]; got != float64(0) {
		t.Fatalf("project b coverage = %v, want 0", got)
	}
	if hasDiagnosticCode(results[1].Diagnostics, "COVERAGE_THRESHOLD_NOT_MET") {
		t.Fatalf("project b was failed by a gate that is off: %+v", results[1].Diagnostics)
	}
}

// TestRunBatchCoverageOptOutSkipsInstrumentation pins the remaining way to buy
// the CPU back: `coverage: false` skips instrumentation entirely, and the
// default-on gate never overrides it.
func TestRunBatchCoverageOptOutSkipsInstrumentation(t *testing.T) {
	ctx := makeBatchTestContext(t, true)
	ctx.Params["coverage"] = json.RawMessage(`false`)
	ctx.Params["coverage-threshold"] = json.RawMessage(`70`)
	t.Setenv("GOWORK", "off")

	mockGoCommand(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
		for _, flag := range []string{"-cover", "-coverpkg", "-coverprofile"} {
			if slices.Contains(args, flag) {
				t.Fatalf("args = %v, want no %s (coverage:false opt-out)", args, flag)
			}
		}
		return []byte(strings.Join([]string{
			testEvent("pass", "example.com/a", "TestA", ""),
			testEvent("pass", "example.com/a", "", ""),
			testEvent("pass", "example.com/b", "TestB", ""),
			testEvent("pass", "example.com/b", "", ""),
		}, "\n") + "\n"), nil
	})

	status, data, err := runBatch(ctx, nil)
	if err != nil || status != "OK" {
		t.Fatalf("runBatch status=%q err=%v", status, err)
	}
	for _, result := range data["batchResults"].([]batchProjectResult) {
		if result.Status != "OK" {
			t.Fatalf("project %s = %+v, want OK (opt-out zeroes the threshold)", result.ProjectID, result)
		}
		if _, ok := result.Data["coverageSummary"]; ok {
			t.Fatalf("project %s carries coverageSummary %v, want none", result.ProjectID, result.Data["coverageSummary"])
		}
	}
}

func TestBatchPackageParallelMatchesSoloArguments(t *testing.T) {
	ctx := &pctx.Context{Params: pctx.Params{
		"package-parallel": json.RawMessage(`4`),
		"parallel":         json.RawMessage(`7`),
		"test-json":        json.RawMessage(`false`),
		// Opt out of the default instrumentation so this asserts only the parallel
		// flags; coverage args are covered by the cadence parity table.
		"coverage": json.RawMessage(`false`),
	}}
	options := readBatchOptions(ctx)
	got := batchTestArgs(options, t.TempDir(), "", nil, []string{"./..."})
	wantPrefix := appendParallelArgs([]string{"test"}, "4", "7")
	if !slices.Equal(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("batch parallel args = %v, want solo prefix %v", got, wantPrefix)
	}
	if got[len(got)-1] != "./..." {
		t.Fatalf("batch args = %v, want package pattern last", got)
	}
}

// TestGoCoverageCadenceSoloBatchParity pins the TS/Go issue's central invariant
// for Go: the solo (Run) and batch (readBatchOptions + coverageEnabled +
// resolveCount) paths reach the IDENTICAL decision for the same params.
// coverageEnabled(options) is byte-identical to the solo inline expression in
// test.go, and both paths zero the threshold when !coverage or !enforce and
// share resolveCount, so the gate behaves the same in either dispatch.
//
// The table also pins the two axes apart. `coverage` decides whether the run
// measures; `enforce-coverage` decides whether it may fail. Only the first can
// turn instrumentation off.
func TestGoCoverageCadenceSoloBatchParity(t *testing.T) {
	cases := []struct {
		name        string
		coverage    string // "", "true", or "false"
		enforce     string // "", "true", or "false"
		threshold   string // "" or a number
		wantEnabled bool
		wantCount   string
	}{
		{"default: measure and gate", "", "", "", true, ""},
		{"default with configured threshold", "", "", "70", true, "1"},
		{"default with coverage:true", "true", "", "70", true, "1"},
		{"explicit opt-out with threshold", "false", "", "70", false, ""},
		{"no-enforce still measures", "", "false", "70", true, ""},
		{"no-enforce without threshold", "", "false", "", true, ""},
		{"no-enforce plus opt-out measures nothing", "false", "false", "70", false, ""},
		{"explicit enforce, no threshold", "", "true", "", true, ""},
		{"explicit enforce with threshold", "", "true", "70", true, "1"},
		{"explicit enforce but project opts out", "false", "true", "70", false, ""},
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
			ctx := &pctx.Context{Params: params}

			// Batch decision via the real batch helpers.
			opts := readBatchOptions(ctx)
			gotEnabled := coverageEnabled(opts)
			gotCount := resolveCount(opts.count, opts.coverageThreshold)

			// Solo decision replicated from test.go's exact lines to assert the two
			// paths agree (any drift in either fails this parity check).
			soloCoverage := ctx.Params.Bool("coverage", true)
			soloEnforce := ctx.Params.Bool("enforce-coverage", true, "enforceCoverage")
			soloThreshold := ctx.Params.Float("coverage-threshold", 0, "coverageThreshold")
			if !soloCoverage {
				soloThreshold = 0
			}
			if !soloEnforce {
				soloThreshold = 0
			}
			soloEnabled := soloCoverageEnabled(soloCoverage, "", "", false)
			soloCount := resolveCount("", soloThreshold)

			if gotEnabled != tc.wantEnabled || soloEnabled != tc.wantEnabled {
				t.Fatalf("coverageEnabled batch=%v solo=%v, want %v", gotEnabled, soloEnabled, tc.wantEnabled)
			}
			if gotCount != tc.wantCount || soloCount != tc.wantCount {
				t.Fatalf("resolveCount batch=%q solo=%q, want %q", gotCount, soloCount, tc.wantCount)
			}
			if opts.coverageThreshold != soloThreshold {
				t.Fatalf("threshold batch=%v solo=%v, want identical", opts.coverageThreshold, soloThreshold)
			}
		})
	}
}

func TestRunBatchSeparatesProjectsWithDifferentTestBindings(t *testing.T) {
	ctx := makeBatchTestContext(t, false)
	t.Setenv(envDatabaseTestBindings, "")
	for _, project := range []string{"a", "b"} {
		confDir := filepath.Join(ctx.WorkspaceRoot, project, "conf")
		if err := os.MkdirAll(confDir, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(confDir, ".env.test.yaml"), "database:\n  default:\n    host: localhost\n    database: "+project+"_test\n")
	}

	var calls int
	mockGoCommand(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
		calls++
		pkg := "example.com/a"
		if slices.Contains(args, "./b/...") {
			pkg = "example.com/b"
		}
		return []byte(testEvent("pass", pkg, "", "") + "\n"), nil
	})

	_, data, err := runBatch(ctx, nil)
	if err != nil {
		t.Fatalf("runBatch: %v", err)
	}
	if calls != 2 {
		t.Fatalf("go test calls = %d, want one per incompatible binding", calls)
	}
	for _, result := range data["batchResults"].([]batchProjectResult) {
		if result.Status != "OK" {
			t.Fatalf("result = %+v", result)
		}
	}
}

func makeBatchTestContext(t *testing.T, coverage bool) *pctx.Context {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.work"), "go 1.25.7\n\nuse (\n\t./a\n\t./b\n)\n")
	for _, project := range []string{"a", "b"} {
		projectDir := filepath.Join(root, project)
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(projectDir, "go.mod"), "module example.com/"+project+"\n\ngo 1.25.7\n")
		mustWrite(t, filepath.Join(projectDir, project+".go"), "package "+project+"\n\nfunc Value() int { return 1 }\n")
	}
	return &pctx.Context{
		WorkspaceRoot: root,
		CacheRoot:     filepath.Join(root, ".putnami", "cache"),
		Job:           pctx.Job{Name: "test"},
		// enforce-coverage selects whether the threshold can fail the run. It no
		// longer controls instrumentation: measurement follows the per-project
		// coverage policy, which stays at its default (on) so opt-outs are
		// exercised explicitly by the tests that need them.
		Params: pctx.Params{
			"enforce-coverage": json.RawMessage(map[bool]string{true: "true", false: "false"}[coverage]),
		},
		SelectedProjects: []pctx.ProjectRef{
			{ID: "/a", Name: "a", Path: "a", FullPath: filepath.Join(root, "a")},
			{ID: "/b", Name: "b", Path: "b", FullPath: filepath.Join(root, "b")},
		},
	}
}

// mockGoCommand replaces the job's subprocess seam for the test. `go list`
// is answered here for every fixture module of this package, whose import
// path is example.com/<directory name>, so mock only sees `go test`.
func mockGoCommand(
	t *testing.T,
	mock func(binary string, args []string, dir string, env []string) ([]byte, error),
) {
	t.Helper()
	original := runGoCommand
	runGoCommand = func(binary string, args []string, dir string, env []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			return []byte("example.com/" + filepath.Base(dir) + "\n"), nil
		}
		return mock(binary, args, dir, env)
	}
	t.Cleanup(func() { runGoCommand = original })
}

func testEvent(action, pkg, testName, output string) string {
	event := parse.TestEvent{Action: action, Package: pkg, Test: testName, Output: output}
	data, _ := json.Marshal(event)
	return string(data)
}

func argumentAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("args %v missing %s value", args, flag)
	return ""
}

func hasDiagnosticCode(diagnostics []parse.ToolDiagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Category == code {
			return true
		}
	}
	return false
}

func assertProfileOwnership(t *testing.T, path, want, unwanted string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, want) || strings.Contains(content, unwanted) {
		t.Fatalf("profile %s = %q, want %q and no %q", path, content, want, unwanted)
	}
}
