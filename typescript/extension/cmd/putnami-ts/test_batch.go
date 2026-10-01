package main

import (
	"fmt"
	"path/filepath"

	protocolcli "go.putnami.dev/protocol/cli"
	features "go.putnami.dev/protocol/features"
	pctx "go.putnami.dev/sdk/extension/context"
	coveragecheck "go.putnami.dev/sdk/extension/coverage"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/specreport"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/project"
	"go.putnami.dev/typescript/extension/internal/testjob"
)

// testBatchDiagnostic mirrors the scheduler's batchWireDiagnostic JSON shape
// (tooling/cli/internal/jobs/scheduler_batch_exec.go). The json tags MUST stay
// byte-identical or the scheduler silently drops the finding.
type testBatchDiagnostic struct {
	Category    string `json:"category,omitempty"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
}

// testBatchArtifact mirrors the scheduler's batchWireArtifact JSON shape.
type testBatchArtifact struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// testBatchProjectResult mirrors the scheduler's batchWireResult JSON shape:
// one independent per-project outcome carried inside {"batchResults":[...]}.
type testBatchProjectResult struct {
	ProjectID   string                `json:"projectId"`
	Status      string                `json:"status"`
	Data        map[string]any        `json:"data,omitempty"`
	Diagnostics []testBatchDiagnostic `json:"diagnostics,omitempty"`
	Artifacts   []testBatchArtifact   `json:"artifacts,omitempty"`
}

// runTestBatch runs every selected project's test suite SEQUENTIALLY inside a
// single extension process and fans the per-project outcomes back through the
// batch wire. This amortizes the extension-process overhead (fork/exec +
// extension boot) across the group while each project keeps its own bun test
// invocation, its own captured output directory, its own coverage evaluation,
// and — because the scheduler splits the result — its own cache identity.
//
// The process itself returns "OK" whenever it produced the split protocol: a
// project's failing (or crashing) suite is recorded as that project's FAILED
// result inside batchResults, never as a process-level failure, so one bad
// suite can neither fail nor mask its batch-mates.
func runTestBatch(ctx *pctx.Context, emit *jsonl.Emitter) (string, map[string]any, error) {
	projects := append([]pctx.ProjectRef(nil), ctx.SelectedProjects...)
	if len(projects) == 0 {
		return "FAILED", nil, fmt.Errorf("test batch requires at least one selected project")
	}

	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	// Every batch-mate resolved identical effective params (the scheduler folds
	// them into the batch key), so a single resolution is correct for all.
	params := testParamsFromContext(ctx)

	results := make([]testBatchProjectResult, 0, len(projects))
	for _, proj := range projects {
		results = append(results, runProjectTestSuite(ctx, emit, proj, bunBin, params))
	}
	return "OK", map[string]any{"batchResults": results}, nil
}

// runProjectTestSuite runs one project's suite and captures every outcome —
// pass, test failure, coverage-threshold miss, discovery/parse error, or a
// hard subprocess crash — into a single per-project result. It never returns a
// process error, so a failure here is isolated to this project.
func runProjectTestSuite(
	ctx *pctx.Context,
	emit *jsonl.Emitter,
	proj pctx.ProjectRef,
	bunBin string,
	params testjob.TestParams,
) testBatchProjectResult {
	result := testBatchProjectResult{ProjectID: proj.ID, Status: "OK"}
	projectPath := filepath.Join(ctx.WorkspaceRoot, proj.Path)
	outputPath := projectBatchOutputPath(ctx, proj)

	hasTests, err := testjob.HasTests(projectPath)
	if err != nil {
		return failTestBatchProject(result, proj.Name, "Test discovery failed", err, errs.CodeTestsFailed.String())
	}
	if !hasTests {
		// Matches the solo contract for an empty project: OK with no payload.
		return result
	}

	// bun's lcov reporter leaves per-worker shards next to the merged lcov.info.
	// Because outputPath is what the cache captures per project, drop them on
	// every exit path, exactly as the solo path does.
	defer func() { _ = testjob.CleanCoverageShards(outputPath) }()

	// One spec-verification fragment directory per member: each project
	// keeps its own bun invocation, so attribution needs no cross-member split —
	// the merge below still applies the same containment rule as everywhere
	// else. A scratch failure costs the report, never the tests.
	fragmentsDir, cleanupFragments, fragErr := specreport.NewFragmentDirectory()
	if fragErr != nil {
		result.Diagnostics = append(result.Diagnostics, testBatchDiagnostic{
			Severity: "warning", Description: fragErr.Error(), Category: specVerificationCategory,
		})
	} else {
		defer cleanupFragments()
		params.SpecFragmentsDir = fragmentsDir
	}

	success, logs, runErr := runProjectBunTestWithRetry(bunBin, projectPath, outputPath, params)
	emitTestTranscript(emit, logs, params.Verbose, proj.ID)
	if runErr != nil {
		// A crash (failed to spawn / timed out) survived one respawn: isolate it
		// as this project's failure and move on to the next suite.
		return failTestBatchProject(result, proj.Name, "Test suite crashed", runErr, errs.CodeTestsFailed.String())
	}

	testSummary, covSummary, _, parseErr := testjob.ParseResults(outputPath, projectPath, params.Coverage)
	if parseErr != nil {
		return failTestBatchProject(result, proj.Name, "Parsing test results failed", parseErr, errs.CodeTestsFailed.String())
	}

	result.Data = testResultData(testSummary, covSummary, params.CoverageThreshold)
	// Every member's cases share the batch's one result line.
	recordTestCases(result.Data, ctx.WorkspaceRoot, proj.Path, testSummary, protocolcli.TestCaseBatchMemberBytes(len(ctx.SelectedProjects)))
	result.Artifacts = testBatchArtifacts(ctx, outputPath, covSummary)
	// Merge and publish the spec-verification fragments, for failing suites
	// too: a failed check is an observation the gate must see.
	if fragErr == nil {
		attachBatchSpecVerification(&result, ctx, fragmentsDir, projectPath, outputPath)
	}

	// Enforce the coverage threshold per project against this project's own
	// coverage (evaluated against line coverage, same as solo).
	thresholdFailed := false
	if params.CoverageThreshold > 0 {
		hasCoverage := covSummary != nil && covSummary.TotalLines > 0
		actual := 0.0
		if hasCoverage {
			actual = covSummary.LineCoverage
		}
		if ok, msg := coveragecheck.CheckThreshold(params.CoverageThreshold, actual, hasCoverage); !ok {
			result.Diagnostics = append(result.Diagnostics, testBatchDiagnostic{
				Severity:    "error",
				Description: capitalize(msg),
				Category:    errs.CodeCoverageThresholdNotMet.String(),
			})
			thresholdFailed = true
		}
	}

	if !success {
		failureDiagnostics, omitted := collectTestFailureDiagnostics(proj, testSummary, logs)
		result.Diagnostics = append(result.Diagnostics, failureDiagnostics...)
		recordFailureDetailsTruncated(result.Data, omitted)
		result.Status = "FAILED"
		return result
	}

	if thresholdFailed {
		result.Diagnostics = append(result.Diagnostics, testBatchDiagnostic{
			Severity:    "error",
			Description: "Coverage threshold not met for " + proj.Name,
			Category:    errs.CodeCoverageThresholdNotMet.String(),
		})
		result.Status = "FAILED"
		return result
	}

	return result
}

// projectBatchOutputPath resolves the per-project captured output directory.
// It prefers the explicit OutputPath the orchestrator supplies (protocol/job
// ProjectRef.outputPath) and reconstructs the same location from the command
// name when an older orchestrator omits it, so this extension keeps working
// against a producer that predates the field.
func projectBatchOutputPath(ctx *pctx.Context, proj pctx.ProjectRef) string {
	if proj.OutputPath != "" {
		return proj.OutputPath
	}
	return filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", proj.Path, ctx.Job.Name)
}

// runProjectBunTestWithRetry runs one project's bun test suite, retrying once
// only when RunTests returns an error: bun failed to start, so no test ran. A
// suite that exits, crashes or is killed at its timeout returns a result, not
// an error, and is never retried. A failing or flaky test therefore fails the
// run; retrying it would hide the failure.
func runProjectBunTestWithRetry(bunBin, projectPath, outputPath string, params testjob.TestParams) (bool, string, error) {
	success, logs, err := runProjectBunTest(bunBin, projectPath, outputPath, params)
	if err == nil {
		return success, logs, nil
	}
	return runProjectBunTest(bunBin, projectPath, outputPath, params)
}

// runProjectBunTest is the single-attempt suite runner. It is a package
// variable so batch isolation (crash/hung) can be unit-tested without a real
// bun; production code always calls testjob.RunTests, whose wall-clock timeout
// bounds a hung suite.
var runProjectBunTest = testjob.RunTests

// specVerificationCategory tags diagnostics from the spec-verification merge,
// mirroring the Go extension's batch category.
const specVerificationCategory = "SPEC_VERIFICATION"

// attachBatchSpecVerification merges the fragments one member's suite wrote
// into its own report artifact, keeping per-project attribution exactly as a
// solo run produces it. Reporting problems become warning diagnostics and
// never change the member's test verdict.
func attachBatchSpecVerification(result *testBatchProjectResult, ctx *pctx.Context, fragmentsDir, projectRoot, outputPath string) {
	fragments, warnings := specreport.ReadFragments(fragmentsDir)
	report, foreign, buildWarnings := specreport.ProjectReport(fragments, projectRoot)
	warnings = append(warnings, buildWarnings...)
	for _, fragment := range foreign {
		warnings = append(warnings, fmt.Sprintf(
			"spec observation (%s, %s, %s) dropped: declaration %s is outside the reporting project",
			fragment.Feature, fragment.Requirement, fragment.Check, fragment.File))
	}
	for _, warning := range warnings {
		result.Diagnostics = append(result.Diagnostics, testBatchDiagnostic{
			Severity: "warning", Description: warning, Category: specVerificationCategory,
		})
	}
	if report == nil {
		return
	}
	destination, err := specreport.EmitReport(nil, report, outputPath)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, testBatchDiagnostic{
			Severity: "warning", Description: err.Error(), Category: specVerificationCategory,
		})
		return
	}
	result.Artifacts = append(result.Artifacts, testBatchArtifact{
		ID:   features.VerificationReportArtifactID,
		Name: "Feature Verification Report",
		Kind: "report",
		Path: project.RelativePath(ctx.WorkspaceRoot, destination),
	})
}

// testBatchArtifacts builds the coverage/junit artifact rows for a project,
// mirroring the artifacts the solo path emits.
func testBatchArtifacts(ctx *pctx.Context, outputPath string, covSummary *parse.CoverageSummary) []testBatchArtifact {
	var artifacts []testBatchArtifact
	if covSummary != nil {
		artifacts = append(artifacts, testBatchArtifact{
			ID:   "coverage",
			Name: "Coverage Report",
			Kind: "coverage",
			Path: project.RelativePath(ctx.WorkspaceRoot, filepath.Join(outputPath, "lcov.info")),
		})
	}
	artifacts = append(artifacts, testBatchArtifact{
		ID:   "junit",
		Name: "JUnit Report",
		Kind: "report",
		Path: project.RelativePath(ctx.WorkspaceRoot, filepath.Join(outputPath, "results.junit.xml")),
	})
	return artifacts
}

// testFailureDiagnostics builds one diagnostic per failed test, mirroring the
// per-test diagnostics the solo path emits so batched failures stay just as
// actionable. It prefixes each file with proj.Path — the suite's own project,
// NOT the batch leader — so a failure in a follower project links correctly.
func testFailureDiagnostics(proj pctx.ProjectRef, testSummary *parse.TestSummary, logs string) []testBatchDiagnostic {
	if testSummary == nil || len(testSummary.FailedTests) == 0 {
		return nil
	}
	assertionDetails := testjob.ExtractAssertionDetails(logs)
	diagnostics := make([]testBatchDiagnostic, 0, len(testSummary.FailedTests))
	for _, ft := range testSummary.FailedTests {
		msg := "Test failed: " + ft.Name
		if ft.Failure != nil && ft.Failure.Message != "" {
			msg += "\n" + ft.Failure.Message
		} else if detail, ok := assertionDetails[ft.Name]; ok {
			msg += "\n" + detail
		}
		file := ft.File
		if file != "" {
			file = filepath.Join(proj.Path, file)
		}
		code := errs.CodeTestFailed.String()
		if ft.Failure != nil && ft.Failure.Type != "" {
			code = ft.Failure.Type
		}
		diagnostics = append(diagnostics, testBatchDiagnostic{
			Severity:    "error",
			Description: msg,
			File:        file,
			Line:        ft.Line,
			Category:    code,
		})
	}
	return diagnostics
}

// failTestBatchProject marks a project FAILED with a single explanatory
// diagnostic, used for hard failures (discovery, crash, parse) that never
// produced a parseable per-test result.
func failTestBatchProject(result testBatchProjectResult, projectName, reason string, cause error, code string) testBatchProjectResult {
	result.Status = "FAILED"
	description := fmt.Sprintf("%s for %s", reason, projectName)
	if cause != nil {
		description += ": " + cause.Error()
	}
	result.Diagnostics = append(result.Diagnostics, testBatchDiagnostic{
		Severity:    "error",
		Description: description,
		Category:    code,
	})
	return result
}
