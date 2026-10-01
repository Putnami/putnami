// Package test runs Go tests using `go test -json` with coverage support.
// Emits JSONL events compatible with the Putnami orchestrator.
package test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.putnami.dev/go/extension/internal/parse"
	"go.putnami.dev/go/extension/internal/toolchain"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	coveragecheck "go.putnami.dev/sdk/extension/coverage"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/sdk/extension/specreport"
)

const (
	envAppEnv               = "APP_ENV"
	defaultTestEnvironment  = "test"
	envDatabaseTestBindings = "DATABASE_TEST_BINDINGS"
)

// Run executes the test job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if len(ctx.SelectedProjects) > 0 {
		return runBatch(ctx, emit)
	}

	coverage := ctx.Params.Bool("coverage", true)
	enforceCoverage := ctx.Params.Bool("enforce-coverage", true, "enforceCoverage")
	race := ctx.Params.Bool("race", false)
	timeout := ctx.Params.String("timeout")
	short := ctx.Params.Bool("short", false)
	runPattern := ctx.Params.String("run")
	count := ctx.Params.String("count")
	shuffle := ctx.Params.Bool("shuffle", false)
	bench := ctx.Params.String("bench")
	benchtime := ctx.Params.String("benchtime")
	verbose := ctx.Params.Bool("test-verbose", false, "testVerbose", "verbose", "v")
	useJSON := ctx.Params.Bool("test-json", true, "testJson", "json")
	packageParallel := ctx.Params.String("package-parallel", "packageParallel")
	parallel := ctx.Params.String("parallel")
	failfast := ctx.Params.Bool("failfast", false)
	coverprofile := ctx.Params.String("coverprofile")
	covermode := ctx.Params.String("covermode")
	outputdir := ctx.Params.String("outputdir")
	coverhtml := ctx.Params.Bool("coverhtml", false)
	coverageThreshold := ctx.Params.Float("coverage-threshold", 0, "coverageThreshold")

	// An explicit `coverage: false` opt-out is authoritative: it disables both
	// coverage collection and the inherited coverage-threshold gate by zeroing the
	// threshold here. Otherwise a workspace-wide threshold silently overrides the
	// project's opt-out and fails pure-vocabulary / generated-only modules (no
	// instrumented statements) with "no coverage data was produced".
	if !coverage {
		coverageThreshold = 0
	}

	// The gate is on by default: a coverage drop must fail the run that caused it,
	// not surface later in CI. --no-enforce-coverage zeroes only the threshold, so
	// the run still instruments, still reports the percentage, and still emits
	// UNCOVERED_FILE diagnostics — it just cannot fail on them. Skipping the
	// instrumentation tax entirely is the separate `coverage` param
	// (--coverage=false, or `coverage: false` in project config), whose opt-out
	// already zeroed the threshold above and stays authoritative here.
	if !enforceCoverage {
		coverageThreshold = 0
	}

	// A solo run already instruments only this project's packages, so both
	// scopes measure the same thing here (ADR 0008). The value is still
	// validated: a typo must fail the same way whichever path dispatches it.
	if _, err := resolveCoverageScope(ctx.Params); err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}

	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "FAILED", nil, err
	}

	// Discover tests
	emit.PhaseStart("discover")
	if _, err := os.Stat(filepath.Join(ctx.Project.FullPath, "go.mod")); err != nil {
		emit.Log("info", "No go.mod found, skipping tests")
		emit.PhaseEnd("discover", "skipped")
		return "SKIP", nil, nil
	}
	emit.PhaseEnd("discover", "success")

	// Resolve output directory
	outDir := ctx.OutputPath
	if outputdir != "" {
		if filepath.IsAbs(outputdir) {
			outDir = outputdir
		} else {
			outDir = filepath.Join(ctx.OutputPath, outputdir)
		}
	}
	os.MkdirAll(outDir, 0o755)

	emit.PhaseStart("run")
	emit.Progress(1, 3, "Running tests...")
	testEnv, err := buildTestEnv(ctx, ctx.Project.FullPath, race, goBinary)
	if err != nil {
		emit.PhaseEnd("run", "failed")
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}

	// Build go test command
	testArgs := []string{"test"}
	if outputdir != "" {
		testArgs = append(testArgs, "-outputdir", outDir)
	}
	if short {
		testArgs = append(testArgs, "-short")
	}
	count = resolveCount(count, coverageThreshold)
	if count != "" {
		testArgs = append(testArgs, "-count", count)
	}
	if shuffle {
		testArgs = append(testArgs, "-shuffle", "on")
	}
	if bench != "" {
		testArgs = append(testArgs, "-bench", bench)
	}
	if benchtime != "" {
		testArgs = append(testArgs, "-benchtime", benchtime)
	}

	// Coverage is collected whenever the project allows it (`coverage`, default
	// true) or a profile is explicitly requested. Measurement is deliberately
	// independent of --enforce-coverage: turning the gate off must not also blind
	// the run, otherwise a drop goes unseen for exactly the runs that opted out of
	// failing on it.
	coverageEnabled := coverage || coverprofile != "" || covermode != "" || coverhtml
	var coverFile string
	if coverageEnabled {
		// -coverpkg names the packages this module owns, listed with the same
		// env `go test` runs under. `./...` selects the tests below but must
		// not instrument: it would also reach a module nested under this one
		// and charge its statements here. An empty module keeps
		// -cover alone and `go test` reports the empty selection itself.
		packages, listErr := listModulePackages(goBinary, ctx.Project.FullPath, testEnv)
		if listErr != nil {
			emit.PhaseEnd("run", "failed")
			emit.Diagnostic("error", listErr.Error(), "", 0)
			return "FAILED", nil, nil
		}
		testArgs = append(testArgs, "-cover")
		if len(packages) > 0 {
			testArgs = append(testArgs, "-coverpkg="+strings.Join(packages, ","))
		}
		coverFile = coverprofile
		if coverFile == "" {
			coverFile = "coverage.out"
		}
		if !filepath.IsAbs(coverFile) {
			coverFile = filepath.Join(outDir, coverFile)
		}
		testArgs = append(testArgs, "-coverprofile", coverFile)
	}

	if covermode != "" {
		testArgs = append(testArgs, "-covermode", covermode)
	}
	if race {
		testArgs = append(testArgs, "-race")
	}

	if timeout != "" {
		// Convert plain numeric to milliseconds
		if isNumeric(timeout) {
			testArgs = append(testArgs, "-timeout", timeout+"ms")
		} else {
			testArgs = append(testArgs, "-timeout", timeout)
		}
	}

	if runPattern != "" {
		testArgs = append(testArgs, "-run", runPattern)
	}
	if verbose {
		testArgs = append(testArgs, "-v")
	}
	if useJSON {
		testArgs = append(testArgs, "-json")
	}
	testArgs = appendParallelArgs(testArgs, packageParallel, parallel)
	if failfast {
		testArgs = append(testArgs, "-failfast")
	}
	testArgs = append(testArgs, "./...")

	// Execute. The spec-verification fragment directory: tests bound
	// to declared checks through protocol/features/spectest write their
	// verdicts here, and the merge below publishes them as the reserved report
	// artifact. A scratch failure costs the report, never the tests — core
	// resolves an absent report as missing evidence, the fail-closed direction.
	fragmentsDir, cleanupFragments, fragErr := specreport.NewFragmentDirectory()
	if fragErr != nil {
		emit.Diagnostic("warning", fragErr.Error(), "", 0)
	} else {
		defer cleanupFragments()
		testEnv = append(testEnv, spectest.FragmentDirEnv+"="+fragmentsDir)
	}
	if goScratch, err := scratch.New("putnami-go-tmp-"); err != nil {
		emit.Diagnostic("warning", "create Go work directory scratch: "+err.Error(), "", 0)
	} else {
		defer func() { _ = goScratch.Remove() }()
		if testEnv, err = withGoTempDir(testEnv, goScratch.Path()); err != nil {
			emit.Diagnostic("warning", err.Error(), "", 0)
		}
	}

	output, err := runGoCommand(goBinary, testArgs, ctx.Project.FullPath, testEnv)
	outputStr := string(output)
	testFailed := err != nil
	emitTestTranscript(emit, outputStr, useJSON, verbose, "")

	if testFailed {
		emit.PhaseEnd("run", "failed")
	} else {
		emit.PhaseEnd("run", "success")
	}

	// Parse results
	emit.PhaseStart("parse")
	emit.Progress(2, 3, "Parsing results...")

	var testCounts parse.TestCounts
	if useJSON {
		testCounts = parse.TestJSON(outputStr)
		emit.Metric("tests-total", testCounts.Total(), "count")
		emit.Metric("tests-passed", testCounts.Passed, "count")
		emit.Metric("tests-failed", testCounts.Failed, "count")
		emit.Metric("tests-skipped", testCounts.Skipped, "count")
	}
	// One locator resolves the test files that both the test cases and the
	// failure diagnostics name.
	locate := parse.ModuleTestFileLocator(
		ctx.WorkspaceRoot,
		ctx.Project.FullPath,
		readModulePath(ctx.Project.FullPath),
	)
	testCases := moduleTestCases(outputStr, useJSON, locate)

	var failureDiagnostics []parse.ToolDiagnostic
	failureDetailsTruncated := 0
	if testFailed {
		failureDiagnostics, failureDetailsTruncated = boundFailureDiagnostics(
			parse.TestFailureDiagnosticList(outputStr, locate),
		)
	}

	// Parse coverage
	var coverageResult parse.CoverageResult
	hasCoverage := false
	var coverageUnreadable error
	if coverageEnabled && coverFile != "" {
		result, coverErr := parse.CoverageDetails(coverFile)
		if errors.Is(coverErr, parse.ErrCoverageProfileUnreadable) {
			coverageUnreadable = coverErr
		}
		if coverErr == nil {
			coverageResult = result
			hasCoverage = true
			emit.Metric("coverage", result.Percentage, "percent")
			emit.Metric("coverage-statements-total", result.TotalStatements, "count")
			emit.Metric("coverage-statements-covered", result.CoveredStatements, "count")
			emit.Artifact("coverage", "Coverage Profile", "coverage", coverFile)

			// Emit diagnostics for files with zero coverage
			parse.CoverageDiagnostics(result, ctx.WorkspaceRoot, ctx.Project.FullPath, emit)

			// Generate HTML report
			if coverhtml || coverage {
				htmlPath := filepath.Join(outDir, "coverage.html")
				htmlCmd := exec.Command(goBinary, "tool", "cover", "-html="+coverFile, "-o", htmlPath)
				htmlCmd.Env = testEnv
				htmlCmd.Run() //nolint:errcheck // best-effort HTML report
			}
		}
	}

	// Merge and publish the spec-verification fragments, for failing runs too:
	// a failed check is an observation the gate must see, not a report to
	// withhold.
	if fragErr == nil {
		specreport.MergeSolo(emit, fragmentsDir, ctx.Project.FullPath, ctx.OutputPath)
	}

	// Emit one deterministic recap. Raw output has already been retained as
	// debug events and becomes visible only under --test-verbose.
	if summary := formatTestSummary(
		testCounts,
		testFailed,
		hasCoverage,
		coverageResult.Percentage,
		coverageUnreadable != nil,
		failureDetailsTruncated,
	); summary != "" {
		emit.Summary(summary)
	}

	emit.PhaseEnd("parse", "success")
	emit.Progress(3, 3, "Done")

	// Build result data
	resultData := buildResultData(testCounts, coverageResult, hasCoverage, coverageThreshold)
	recordFailureDetailsTruncated(resultData, failureDetailsTruncated)
	recordTestCases(resultData, testCases, protocolcli.TestCaseMaxBytesPerTask)

	// Enforce the coverage threshold (if configured). A failing gate is reported
	// as an error diagnostic and fails the job even when every test passed.
	thresholdFailed := false
	if coverageUnreadable != nil {
		diagnostic := coverageUnreadableDiagnostic(coverageUnreadable, coverageThreshold)
		emit.DiagnosticWithCode(diagnostic.Severity, diagnostic.Description, "", 0, 0, diagnostic.Category)
		thresholdFailed = diagnostic.Severity == "error"
	} else if coverageThreshold > 0 {
		// A parsed profile with no instrumented statements carries no usable
		// signal, so treat it as "no coverage data" rather than reporting a
		// misleading 0%-below-threshold failure.
		hasData := hasCoverage && coverageResult.TotalStatements > 0
		if ok, msg := coveragecheck.CheckThreshold(coverageThreshold, coverageResult.Percentage, hasData); ok {
			emit.Log("info", fmt.Sprintf("Coverage %.1f%% meets the required threshold of %.1f%%", coverageResult.Percentage, coverageThreshold))
		} else {
			emit.DiagnosticWithCode("error", capitalize(msg), "", 0, 0, "COVERAGE_THRESHOLD_NOT_MET")
			thresholdFailed = true
		}
	}

	// Diagnostics for failures. This runs whatever the output format is: with
	// `--test-json=false` there are no events to parse, and gating on useJSON
	// used to leave a failed run with no diagnostic at all. The failure path
	// always emits at least one diagnostic, falling back to the raw output.
	if testFailed {
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
		return "FAILED", resultData, nil
	}

	return "OK", resultData, nil
}

// appendParallelArgs keeps Go's package build/test width (-p) separate from
// the test binary's t.Parallel width (-parallel). In particular, absence of
// packageParallel must leave Go's native package scheduling untouched.
func appendParallelArgs(args []string, packageParallel, parallel string) []string {
	if packageParallel != "" {
		args = append(args, "-p", packageParallel)
	}
	if parallel != "" {
		args = append(args, "-parallel", parallel)
	}
	return args
}

// coverageUnreadableDiagnostic reports a coverage profile that exists but could
// not be read to its end. No percentage is derived from it: with a threshold
// the gate cannot be evaluated, so the job fails; without one it warns.
func coverageUnreadableDiagnostic(err error, coverageThreshold float64) parse.ToolDiagnostic {
	severity := "warning"
	if coverageThreshold > 0 {
		severity = "error"
	}
	return parse.ToolDiagnostic{
		Category:    "COVERAGE_PROFILE_UNREADABLE",
		Severity:    severity,
		Description: capitalize(err.Error()),
	}
}

func buildResultData(
	testCounts parse.TestCounts,
	coverageResult parse.CoverageResult,
	hasCoverage bool,
	coverageThreshold float64,
) map[string]any {
	resultData := map[string]any{
		"testSummary": map[string]any{
			"passed":  testCounts.Passed,
			"failed":  testCounts.Failed,
			"skipped": testCounts.Skipped,
			"total":   testCounts.Total(),
		},
	}
	if !hasCoverage {
		return resultData
	}

	files := make([]map[string]any, 0, len(coverageResult.Files))
	uncoveredFiles := 0
	for _, f := range coverageResult.Files {
		files = append(files, map[string]any{
			"file":              f.File,
			"totalStatements":   f.TotalStatements,
			"coveredStatements": f.CoveredStatements,
			"percentage":        f.Percentage,
		})
		if f.Percentage == 0 && f.TotalStatements > 0 {
			uncoveredFiles++
		}
	}
	coverageSummary := map[string]any{
		// Canonical CoverageSummary fields (protocol/runtime payloads):
		// percentage at a declared granularity plus covered/total counts.
		"percentage":  coverageResult.Percentage,
		"granularity": "statements",
		"covered":     coverageResult.CoveredStatements,
		"total":       coverageResult.TotalStatements,
		// Go-specific detail, kept for existing consumers.
		"totalStatements":   coverageResult.TotalStatements,
		"coveredStatements": coverageResult.CoveredStatements,
		"fileCount":         len(coverageResult.Files),
		"uncoveredFiles":    uncoveredFiles,
		"files":             files,
	}
	if coverageThreshold > 0 {
		coverageSummary["threshold"] = coverageThreshold
	}
	resultData["coverageSummary"] = coverageSummary
	return resultData
}

// packageFailureSummary labels a `go test` exit that no failing test explains,
// so the summary of a failed step can never read as all-passed.
func packageFailureSummary(packagesFailed int) string {
	if packagesFailed > 0 {
		return fmt.Sprintf("%d package(s) failed", packagesFailed)
	}
	return "go test failed"
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

// resolveCount decides the value for `go test -count`. Coverage-gated runs
// bypass Go test-result caching to prevent stale coverage blocks; an explicit
// count is honored.
func resolveCount(count string, coverageThreshold float64) string {
	if count == "" && coverageThreshold > 0 {
		return "1"
	}
	return count
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
