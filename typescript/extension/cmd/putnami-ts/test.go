package main

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	coveragecheck "go.putnami.dev/sdk/extension/coverage"
	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/specreport"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/project"
	"go.putnami.dev/typescript/extension/internal/testjob"
)

func runTest(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	// Batched dispatch: when the scheduler groups several ready test suites into
	// one extension process, it hands us the resolved selection. Run each
	// project's suite in turn and fan the results back through the batch wire.
	// Solo dispatch (no selection) stays on the byte-identical path below.
	if len(ctx.SelectedProjects) > 0 {
		return runTestBatch(ctx, emit)
	}

	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)

	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	params := testParamsFromContext(ctx)

	// Phase 1: Discover
	// This is only an emptiness gate — bun re-discovers tests itself — so we
	// short-circuit on the first match instead of walking the whole tree.
	emit.Progress(1, 3, "Discovering tests")
	emit.PhaseStart("discover")
	hasTests, err := testjob.HasTests(projectPath)
	if err != nil {
		emit.PhaseEnd("discover", "failed")
		return "FAILED", nil, err
	}
	if !hasTests {
		emit.PhaseEnd("discover", "skipped")
		emit.Log("info", "No test files found")
		return "OK", nil, nil
	}
	emit.Log("info", "Test files found")
	emit.PhaseEnd("discover", "success")

	// bun's lcov reporter leaves per-worker .lcov.info.*.tmp shards in the
	// output dir alongside the merged lcov.info. Since the output dir is what the
	// build cache captures, those throwaway shards would be stored in every
	// cached test entry and bloat it. Remove them before the task returns, on
	// every exit path below; this runs after ParseResults has consumed
	// lcov.info, and is a no-op when coverage is disabled.
	defer func() {
		if err := testjob.CleanCoverageShards(ctx.OutputPath); err != nil {
			emit.Log("warn", "failed to clean coverage shards: "+err.Error())
		}
	}()

	// The spec-verification fragment directory: tests bound to declared
	// checks through @putnami/runtime/spectest write their verdicts here, and
	// the merge below publishes them as the reserved report artifact. A scratch
	// failure costs the report, never the tests — core resolves an absent
	// report as missing evidence, the fail-closed direction.
	fragmentsDir, cleanupFragments, fragErr := specreport.NewFragmentDirectory()
	if fragErr != nil {
		emit.Log("warn", fragErr.Error())
	} else {
		defer cleanupFragments()
		params.SpecFragmentsDir = fragmentsDir
	}

	// Phase 2: Run
	emit.Progress(2, 3, "Running tests")
	emit.PhaseStart("run")
	success, logs, err := testjob.RunTests(bunBin, projectPath, ctx.OutputPath, params)
	if err != nil {
		emit.PhaseEnd("run", "failed")
		return "FAILED", nil, err
	}
	runStatus := "success"
	if !success {
		runStatus = "failed"
	}
	emit.PhaseEnd("run", runStatus)
	emitTestTranscript(emit, logs, params.Verbose, "")

	// Merge and publish the spec-verification fragments, for failing runs too:
	// a failed check is an observation the gate must see, not a report to
	// withhold.
	if fragErr == nil {
		specreport.MergeSolo(emit, fragmentsDir, projectPath, ctx.OutputPath)
	}

	// Phase 3: Parse
	emit.Progress(3, 3, "Parsing results")
	emit.PhaseStart("parse")
	testSummary, covSummary, covFiles, err := testjob.ParseResults(ctx.OutputPath, projectPath, params.Coverage)
	if err != nil {
		emit.PhaseEnd("parse", "failed")
		return "FAILED", nil, err
	}
	emit.PhaseEnd("parse", "success")

	// Emit test metrics
	if testSummary != nil {
		emit.Metric("tests-total", testSummary.Total, "count")
		emit.Metric("tests-passed", testSummary.Passed, "count")
		emit.Metric("tests-failed", testSummary.Failed, "count")
		emit.Metric("tests-skipped", testSummary.Skipped, "count")
	}

	var failureDiagnostics []testBatchDiagnostic
	failureDetailsTruncated := 0
	if !success {
		failureDiagnostics, failureDetailsTruncated = collectTestFailureDiagnostics(
			pctx.ProjectRef{Name: ctx.Project.Name, Path: ctx.Project.Path},
			testSummary,
			logs,
		)
	}

	// Emit coverage metrics
	if covSummary != nil {
		emit.Metric("coverage-lines", covSummary.LineCoverage, "percent")
		emit.Metric("coverage-functions", covSummary.FunctionCoverage, "percent")
		lcovPath := project.RelativePath(ctx.WorkspaceRoot, filepath.Join(ctx.OutputPath, "lcov.info"))
		emit.Artifact("coverage", "Coverage Report", "coverage", lcovPath)
		emitCoverageSynthesis(emit, covSummary, covFiles)
	}

	// Enforce the coverage threshold (if configured). Evaluated against line
	// coverage and reported as an error diagnostic; a failing gate fails the job
	// even when every test passed.
	thresholdFailed := false
	if params.CoverageThreshold > 0 {
		// A non-nil summary is not proof of usable data: an lcov.info with no
		// instrumented lines (no LF/LH records) parses to LineCoverage 100 over
		// TotalLines 0. Require a real instrumented-line count so an empty report
		// is treated as "no coverage data" rather than a spurious 100% pass.
		hasCoverage := covSummary != nil && covSummary.TotalLines > 0
		actual := 0.0
		if hasCoverage {
			actual = covSummary.LineCoverage
		}
		if ok, msg := coveragecheck.CheckThreshold(params.CoverageThreshold, actual, hasCoverage); ok {
			emit.Log("info", fmt.Sprintf("Coverage %.1f%% meets the required threshold of %.1f%%", actual, params.CoverageThreshold))
		} else {
			emit.DiagnosticWithCode("error", capitalize(msg), "", 0, 0, errs.CodeCoverageThresholdNotMet.String())
			thresholdFailed = true
		}
	}

	// Emit one deterministic recap. Raw Bun output has already been retained as
	// debug events and becomes visible only under --test-verbose.
	if summary := formatTestSummary(testSummary, !success, covSummary, failureDetailsTruncated); summary != "" {
		emit.Summary(summary)
	}

	// JUnit artifact
	junitPath := project.RelativePath(ctx.WorkspaceRoot, filepath.Join(ctx.OutputPath, "results.junit.xml"))
	emit.Artifact("junit", "JUnit Report", "report", junitPath)

	// Canonical result payloads (protocol/runtime: testSummary,
	// coverageSummary) so the orchestrator can aggregate outcomes
	// across languages.
	resultData := testResultData(testSummary, covSummary, params.CoverageThreshold)
	recordFailureDetailsTruncated(resultData, failureDetailsTruncated)
	recordTestCases(resultData, ctx.WorkspaceRoot, ctx.Project.Path, testSummary, protocolcli.TestCaseMaxBytesPerTask)

	// Emit only the bounded causal projection. The complete transcript is kept
	// separately in debug log events above.
	if !success {
		for _, diagnostic := range failureDiagnostics {
			emit.DiagnosticWithCode(
				diagnostic.Severity,
				diagnostic.Description,
				diagnostic.File,
				diagnostic.Line,
				diagnostic.Column,
				diagnostic.Category,
			)
		}
		return "FAILED", resultData, nil
	}

	// Tests passed but coverage fell below the configured threshold.
	if thresholdFailed {
		emit.DiagnosticWithCode("error", "Coverage threshold not met for "+ctx.Project.Name, "", 0, 0, errs.CodeCoverageThresholdNotMet.String())
		return "FAILED", resultData, nil
	}

	emit.Log("info", "Tests passed for "+ctx.Project.Name)
	return "OK", resultData, nil
}

// testParamsFromContext resolves the test job parameters from the job context.
// Solo and batched dispatch share it verbatim so a batched suite always runs
// with the exact parameters its solo run would use (the scheduler additionally
// guarantees every batch-mate resolved identical params by folding them into
// the batch key).
func testParamsFromContext(ctx *pctx.Context) testjob.TestParams {
	coverage := ctx.Params.Bool("coverage", true)
	enforceCoverage := ctx.Params.Bool("enforce-coverage", true, "enforceCoverage")
	coverageThreshold := ctx.Params.Float("coverage-threshold", 0, "coverageThreshold")
	// An explicit `coverage: false` opt-out zeroes the inherited threshold so it is
	// authoritative even under the enforce cadence. Mirrors the Go path
	// (go/extension/internal/jobs/test/test.go).
	if !coverage {
		coverageThreshold = 0
	}
	// The gate is on by default; --no-enforce-coverage zeroes only the threshold so
	// the run still instruments and still reports its percentage. Skipping
	// instrumentation is the `coverage` opt-out above, which this never overrides.
	// Mirrors the Go path (go/extension/internal/jobs/test/test.go).
	if !enforceCoverage {
		coverageThreshold = 0
	}
	coverageEnabled := coverage
	return testjob.TestParams{
		Timeout:           ctx.Params.Int("timeout", 5000),
		WallClockTimeout:  time.Duration(ctx.Params.Int("timeout-budget", 0)) * time.Millisecond,
		UpdateSnapshots:   ctx.Params.Bool("update-snapshots", false, "updateSnapshots"),
		Only:              ctx.Params.Bool("only", false),
		Todo:              ctx.Params.Bool("todo", false),
		Coverage:          coverageEnabled,
		CoverageThreshold: coverageThreshold,
		Bail:              ctx.Params.Int("bail", 0),
		Test:              ctx.Params.String("test"),
		Concurrent:        ctx.Params.String("concurrent"),
		Parallel:          ctx.Params.String("parallel"),
		PassWithNoTests:   ctx.Params.Bool("pass-with-no-tests", true, "passWithNoTests"),
		Verbose:           ctx.Params.Bool("test-verbose", false, "testVerbose"),
		DatabaseBinding:   provisionedDatabaseBinding(ctx),
	}
}

// provisionedDatabaseBinding reads the database binding this invocation's
// `test-env-up` step provisioned, or "" when it provisioned none. The value lives in the invocation's private artifact tree and is
// located through the job context's non-secret `invocation` member — never
// through an inherited environment variable, which every descendant process and
// `ps` can see.
func provisionedDatabaseBinding(ctx *pctx.Context) string {
	binding, _ := dbtestenv.BindingFrom(ctx)
	return binding
}

// testResultData builds the canonical result payload (testSummary +
// coverageSummary) shared by solo and batched dispatch. Returns nil when there
// is nothing to report, matching the solo contract exactly.
func testResultData(testSummary *parse.TestSummary, covSummary *parse.CoverageSummary, coverageThreshold float64) map[string]any {
	if testSummary == nil && covSummary == nil {
		return nil
	}
	resultData := map[string]any{}
	if testSummary != nil {
		resultData["testSummary"] = map[string]any{
			"total":   testSummary.Total,
			"passed":  testSummary.Passed,
			"failed":  testSummary.Failed,
			"skipped": testSummary.Skipped,
		}
	}
	if covSummary != nil && covSummary.TotalLines > 0 {
		cov := map[string]any{
			"percentage":  covSummary.LineCoverage,
			"granularity": "lines",
			"covered":     covSummary.CoveredLines,
			"total":       covSummary.TotalLines,
		}
		if coverageThreshold > 0 {
			cov["threshold"] = coverageThreshold
		}
		resultData["coverageSummary"] = cov
	}
	return resultData
}

// capitalize upper-cases the first rune of s, used to turn lowercase helper
// messages into sentence-cased diagnostics.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	return string(r)
}

func emitCoverageSynthesis(emit *jsonl.Emitter, summary *parse.CoverageSummary, files []parse.FileCoverage) {
	if len(files) == 0 {
		return
	}

	// Sort: lowest coverage first, then by path
	sort.Slice(files, func(i, j int) bool {
		if files[i].Coverage != files[j].Coverage {
			return files[i].Coverage < files[j].Coverage
		}
		return files[i].Path < files[j].Path
	})

	// Header
	emit.Log("info", fmt.Sprintf("Coverage: %d%% lines (%d/%d), %d%% functions (%d/%d)",
		int(math.Round(summary.LineCoverage)),
		summary.CoveredLines, summary.TotalLines,
		int(math.Round(summary.FunctionCoverage)),
		summary.CoveredFunctions, summary.TotalFunctions))

	// Split into below-100% and fully covered
	var below []parse.FileCoverage
	var fullCount int
	for _, f := range files {
		if f.Coverage >= 100 {
			fullCount++
		} else {
			below = append(below, f)
		}
	}

	// All files fully covered
	if len(below) == 0 {
		emit.Log("info", fmt.Sprintf("  All %d files have 100%% line coverage", len(files)))
		return
	}

	// Show files below 100%, cap at 20
	shown := below
	truncated := 0
	if len(shown) > 20 {
		truncated = len(shown) - 20
		shown = shown[:20]
	}

	// Find max path length for alignment
	maxPathLen := 0
	for _, f := range shown {
		if len(f.Path) > maxPathLen {
			maxPathLen = len(f.Path)
		}
	}

	for _, f := range shown {
		padding := strings.Repeat(" ", maxPathLen-len(f.Path)+2)
		// Files never imported by a test carry no instrumented-line counts;
		// label them explicitly rather than printing a misleading (0/0).
		if !f.Analyzed {
			emit.Log("info", fmt.Sprintf("  %s%s  0%%  (not analyzed)", f.Path, padding))
			continue
		}
		emit.Log("info", fmt.Sprintf("  %s%s%3d%%  (%d/%d)",
			f.Path, padding, int(math.Round(f.Coverage)), f.CoveredLines, f.TotalLines))
	}

	if truncated > 0 {
		emit.Log("info", fmt.Sprintf("  ... and %d more files below 100%%", truncated))
	}

	if fullCount > 0 {
		emit.Log("info", fmt.Sprintf("  %d file%s with 100%% coverage", fullCount, plural(fullCount)))
	}
}
