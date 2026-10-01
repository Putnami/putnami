// Package testjob implements the test job (bun test + JUnit + LCOV parsing).
// Named testjob to avoid collision with Go's testing package.
package testjob

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/hostenv"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/project"
)

// DefaultTestTimeout bounds the wall-clock duration of the `bun test` subprocess.
// Unlike the per-test `--timeout` flag, this guards against a hung beforeAll/
// afterAll or an import-time deadlock that would otherwise never terminate.
// Consistent with the bounded timeouts on build (5m) and lint (2m) jobs.
const DefaultTestTimeout = 10 * time.Minute

// envDatabaseTestBindings is the environment variable carrying the resolved
// database test binding (JSON) a test run reads. It MUST stay byte-identical to
// the SDK's shared constant (go.putnami.dev/sdk/extension/dbtestenv.EnvBindings),
// the Go extension's own constant
// (go/extension/internal/jobs/test.envDatabaseTestBindings), and this
// extension's manifest env-input name (putnami.extension.json: test-run
// inputs.DATABASE_TEST_BINDINGS). The manifest declares it as a "from":"env"
// cache-key input, so an EXTERNALLY exported value folds into the test job's
// cache key exactly as it does for Go; a value this run provisioned arrives
// through the invocation-scoped artifact instead, and the job's key folds the
// producing action's digest.
const envDatabaseTestBindings = "DATABASE_TEST_BINDINGS"

// execRunFunc is the function used to run subprocesses. Defaults to exec.Run.
var execRunFunc = exec.Run

// TestResult holds the aggregated test output.
type TestResult struct {
	Success         bool
	Logs            string
	TestSummary     *parse.TestSummary
	CoverageSummary *parse.CoverageSummary
}

// HasTests reports whether the project contains at least one test file. It is
// the emptiness gate used before launching `bun test` (which performs its own
// discovery anyway), so it short-circuits the directory walk on the first match.
func HasTests(projectPath string) (bool, error) {
	found := false
	err := filepath.WalkDir(projectPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".gen" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(projectPath, path)
		if matchTestFile(rel) {
			found = true
			// Stop walking entirely — the gate only needs one match.
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

var testFileRe = regexp.MustCompile(`\.(test|spec)\.(ts|tsx|js|jsx)$`)

func matchTestFile(path string) bool {
	return testFileRe.MatchString(path)
}

// buildTestArgs builds the bun test args from parameters.
func buildTestArgs(outputPath string, params TestParams) []string {
	args := []string{"test"}
	if params.Timeout > 0 {
		args = append(args, fmt.Sprintf("--timeout=%d", params.Timeout))
	}
	if params.Concurrent != "" {
		args = append(args, "--concurrent="+params.Concurrent)
	}
	switch params.Parallel {
	case "", "false":
		// Off by default: the putnami DAG already runs projects in parallel, so
		// in-suite workers multiply contention unless a project opts in.
	case "true":
		args = append(args, "--parallel")
	default:
		args = append(args, "--parallel="+params.Parallel)
	}
	if params.PassWithNoTests {
		args = append(args, "--pass-with-no-tests")
	}
	if params.UpdateSnapshots {
		args = append(args, "--update-snapshots")
	}
	if params.Test != "" {
		args = append(args, "-t="+params.Test)
	}
	if params.Only {
		args = append(args, "--only")
	}
	if params.Todo {
		args = append(args, "--todo")
	}
	if params.Coverage {
		args = append(args, "--coverage", "--coverage-reporter=lcov", "--coverage-dir="+outputPath)
	}
	if params.Bail > 0 {
		args = append(args, fmt.Sprintf("--bail=%d", params.Bail))
	}
	args = append(args, "--reporter=junit", "--reporter-outfile="+filepath.Join(outputPath, "results.junit.xml"))
	return args
}

// resolveTestTimeout returns the wall-clock budget for the test subprocess,
// falling back to DefaultTestTimeout when not explicitly configured.
func resolveTestTimeout(params TestParams) time.Duration {
	if params.WallClockTimeout > 0 {
		return params.WallClockTimeout
	}
	return DefaultTestTimeout
}

// buildTestEnv returns the environment additions for the `bun test` subprocess.
//
// FORCE_COLOR keeps bun's colored output. The database binding follows the same
// precedence as the Go extension (go/extension/internal/jobs/test/
// test_binding.go):
//
//  1. An EXTERNALLY set DATABASE_TEST_BINDINGS wins and is forwarded UNTOUCHED.
//     It is also the value the test task declares as a `{"from": "env"}` cache
//     input, so it folds into the job's cache key by itself.
//  2. The binding `test-env-up` provisioned for this invocation, delivered here
//     through TestParams rather than an inherited environment variable.
//
// The conf-YAML fallback Go synthesizes lives, for TypeScript, in the runtime
// instead: the database test provider (typescript/framework/database) falls
// back to the committed conf datasources when no binding is injected.
func buildTestEnv(params TestParams) map[string]string {
	env := map[string]string{"FORCE_COLOR": "1"}
	// The spec-verification fragment directory: tests bound to
	// declared checks through @putnami/runtime/spectest write their verdicts
	// here, and the adapter merges them into the reserved report artifact
	// after the run. Deliberately NOT a cache input: the path is fresh per
	// run, and bun has no test-result cache that could serve a stale verdict.
	if dir := strings.TrimSpace(params.SpecFragmentsDir); dir != "" {
		env[spectest.FragmentDirEnv] = dir
	}
	if raw := os.Getenv(envDatabaseTestBindings); strings.TrimSpace(raw) != "" {
		env[envDatabaseTestBindings] = raw
		return env
	}
	if binding := strings.TrimSpace(params.DatabaseBinding); binding != "" {
		env[envDatabaseTestBindings] = binding
	}
	return env
}

// RunTests executes bun test and returns raw output.
//
// The subprocess inherits the extension's environment MINUS the host platform
// identity block (go.putnami.dev/sdk/extension/hostenv). A `putnami test` run
// on a managed runtime — the reported case is a CI worker that is itself a
// Cloud Run service — would otherwise hand `bun test` the HOST's `K_SERVICE`,
// which application code reads as "I am the deployed production workload".
// The scrub covers the inherited environment only: buildTestEnv's
// additions are applied after it and still win.
func RunTests(bunBin, projectPath, outputPath string, params TestParams) (bool, string, error) {
	os.MkdirAll(outputPath, 0755)

	args := buildTestArgs(outputPath, params)

	result, err := execRunFunc(bunBin, args,
		exec.Dir(projectPath),
		exec.UnsetEnv(hostenv.PlatformIdentityVars()...),
		exec.Env(buildTestEnv(params)),
		exec.Timeout(resolveTestTimeout(params)),
	)
	if err != nil {
		return false, "", err
	}

	logs := result.Stdout + result.Stderr
	return result.Success, logs, nil
}

// ParseResults parses JUnit and optionally LCOV results.
// When projectPath is provided, uncovered source files are included in coverage totals.
func ParseResults(outputPath, projectPath string, coverage bool) (*parse.TestSummary, *parse.CoverageSummary, []parse.FileCoverage, error) {
	junitPath := filepath.Join(outputPath, "results.junit.xml")
	testSummary, err := parse.ParseJUnitFile(junitPath)
	if err != nil {
		return nil, nil, nil, err
	}

	var covSummary *parse.CoverageSummary
	var files []parse.FileCoverage
	if coverage {
		lcovPath := filepath.Join(outputPath, "lcov.info")
		if project.FileExists(lcovPath) {
			cov, covFiles, err := parse.ParseLCOVFileForProject(lcovPath, projectPath)
			if err == nil {
				covSummary = &cov
				files = covFiles
			}
		}
	}

	return &testSummary, covSummary, files, nil
}

// CleanCoverageShards removes the per-worker ".lcov.info.*.tmp" shards that
// bun's lcov coverage reporter leaves in outputPath next to the merged
// lcov.info. Those shards are throwaway intermediates, never read once
// lcov.info exists; outputPath is the task's captured output directory, so
// leftover shards would otherwise be stored in every cached test entry. It
// deletes only the .tmp shards, never lcov.info or results.junit.xml, and is
// a safe no-op when coverage is disabled (no shards present) or the
// directory does not exist.
func CleanCoverageShards(outputPath string) error {
	matches, err := filepath.Glob(filepath.Join(outputPath, ".lcov.info.*.tmp"))
	if err != nil {
		return err
	}
	var firstErr error
	for _, shard := range matches {
		if err := os.Remove(shard); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// TestParams holds test job parameters.
type TestParams struct {
	Timeout           int
	WallClockTimeout  time.Duration
	UpdateSnapshots   bool
	Only              bool
	Todo              bool
	Coverage          bool
	CoverageThreshold float64
	Bail              int
	TestNamePattern   string
	Test              string
	Concurrent        string
	// Parallel opts a project's suite into bun test's worker processes
	// (Bun 1.4): "" or "false" = off (default), "true" = --parallel,
	// a number = --parallel=N. Off by default because the putnami DAG
	// already parallelizes across projects.
	Parallel        string
	PassWithNoTests bool
	// Verbose changes transcript event visibility only. It never reaches Bun's
	// command line, so summaries, verdicts, exits and test selection are stable.
	Verbose bool
	// DatabaseBinding is the resolved database test binding this run provisioned
	// for the project, or "" when it provisioned none. It is a CREDENTIAL, and
	// it travels as a parameter rather than being read from the environment on
	// purpose: its only source is the invocation-scoped `sensitive` artifact the
	// `test-env-up` task wrote at mode 0600 in the invocation's private tree,
	// which the orchestrator destroys when the
	// invocation ends. It is never logged, never reported, and reaches exactly
	// one place — the `bun test` subprocess environment.
	DatabaseBinding string
	// SpecFragmentsDir is the fresh per-run directory the spec-verification
	// helper (@putnami/runtime/spectest) writes its fragments into, or ""
	// outside a Putnami-provisioned run. The adapter merges the fragments into
	// the reserved putnami-feature-verification report artifact after the
	// suite returns.
	SpecFragmentsDir string
}

// ANSI escape code stripper
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// ExtractAssertionDetails extracts assertion details from bun test output.
func ExtractAssertionDetails(logs string) map[string]string {
	results := make(map[string]string)
	lines := strings.Split(logs, "\n")
	for i := range lines {
		lines[i] = ansiRe.ReplaceAllString(lines[i], "")
	}

	failRe := regexp.MustCompile(`✗\s+(.+?)\s+\[[\d.]+m?s\]`)

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "✗") {
			continue
		}
		match := failRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		fullPath := strings.TrimSpace(match[1])
		testName := fullPath
		if idx := strings.LastIndex(fullPath, " > "); idx >= 0 {
			testName = strings.TrimSpace(fullPath[idx+3:])
		}

		var detailLines []string
		for j := i - 1; j >= max(0, i-15); j-- {
			prev := strings.TrimSpace(lines[j])
			if prev == "" {
				continue
			}
			if strings.HasPrefix(prev, "error:") || strings.HasPrefix(prev, "Expected") || strings.HasPrefix(prev, "Received") {
				detailLines = append([]string{prev}, detailLines...)
			}
			if strings.HasPrefix(prev, "✓") || strings.HasPrefix(prev, "✗") || strings.HasSuffix(prev, ".ts:") || strings.HasSuffix(prev, ".tsx:") {
				break
			}
		}

		if len(detailLines) > 0 {
			results[testName] = strings.Join(detailLines, "\n")
		}
	}

	return results
}

// UnhandledError represents a module-level error from bun test.
type UnhandledError struct {
	Message string
	Type    string
	File    string
	Line    int
}

// ExtractUnhandledErrors extracts "# Unhandled error between tests" blocks.
func ExtractUnhandledErrors(logs string) []UnhandledError {
	var errors []UnhandledError
	lines := strings.Split(logs, "\n")
	for i := range lines {
		lines[i] = ansiRe.ReplaceAllString(lines[i], "")
	}

	errorRe := regexp.MustCompile(`^(\w+Error)\s*:\s*(.+)`)
	locationRe := regexp.MustCompile(`^at\s+(.+?):(\d+):\d+`)

	for i, line := range lines {
		if strings.TrimSpace(line) != "# Unhandled error between tests" {
			continue
		}

		var errType, errMsg, file string
		var lineNum int

		for j := i + 1; j < min(len(lines), i+20); j++ {
			next := strings.TrimSpace(lines[j])

			if m := errorRe.FindStringSubmatch(next); m != nil && errType == "" {
				errType = m[1]
				errMsg = m[2]
			}

			if m := locationRe.FindStringSubmatch(next); m != nil && file == "" {
				file = m[1]
				_, _ = fmt.Sscanf(m[2], "%d", &lineNum)
			}

			if next == "-------------------------------" && j > i+2 {
				break
			}
		}

		if errMsg != "" {
			if errType == "" {
				errType = "Error"
			}
			errors = append(errors, UnhandledError{
				Message: errType + ": " + errMsg,
				Type:    errType,
				File:    file,
				Line:    lineNum,
			})
		}
	}

	return errors
}
