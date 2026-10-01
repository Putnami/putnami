package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/python/extension/internal/parse"
	"go.putnami.dev/python/extension/internal/toolchain"
	"go.putnami.dev/python/extension/internal/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/specreport"
)

// Test runs pytest on a Python project.
func Test(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	// Batched dispatch: when the scheduler groups several ready test suites into
	// one extension process it hands us the resolved selection. Run each
	// project's pytest suite in turn and fan the results back through the batch
	// wire. Solo dispatch (no selection) stays on the byte-identical path below.
	if len(ctx.SelectedProjects) > 0 {
		return TestBatch(ctx, emit, args)
	}

	if ctx.Project.Name == "" {
		return "SKIP", nil, nil
	}

	flags := cli.ParseFlags(args)
	withLog := cli.FlagBool(flags, "log", ctx.Params.Bool("log", false))
	updateSnapshots := cli.FlagBool(flags, "update-snapshots", ctx.Params.Bool("update-snapshots", false))
	testFilter := cli.FlagString(flags, "test", cli.FlagString(flags, "t", ctx.Params.String("test")))

	wsRoot := ctx.WorkspaceRoot
	projectRoot := ctx.Project.FullPath

	if !syncWorkspaceFn(wsRoot, emit) {
		return "FAILED", nil, nil
	}

	// Phase: discover
	emit.PhaseStart("discover")
	tests, _ := workspace.DiscoverTestFiles(projectRoot)
	if len(tests) == 0 {
		emit.PhaseEnd("discover", "skipped")
		emit.Log("info", "No test files found")
		return "SKIP", nil, nil
	}
	emit.Log("info", fmt.Sprintf("Found %d test files", len(tests)))
	emit.PhaseEnd("discover", "success")

	packageName := ResolvePackageName(ctx)
	cacheDir := filepath.Join(wsRoot, ".putnami/bin/extensions/putnami-python/.pytest_cache")

	cmdArgs := []string{
		"uv", "run",
		"--package", packageName,
		"--directory", wsRoot,
		"--extra", "dev",
		"pytest",
		"-o", "cache_dir=" + cacheDir,
		"--color=yes",
	}
	if withLog {
		cmdArgs = append(cmdArgs, "-s", "-o", "log_cli=true")
	}
	if updateSnapshots {
		cmdArgs = append(cmdArgs, "--snapshot-update")
	}

	// The spec-verification producer seam: materialize the pytest
	// plugin, activate it, and hand the run a fresh fragment directory. Tests
	// bound with the putnami_proves marker write their verdicts there. A
	// scratch failure costs the reporting, never the tests.
	pluginDir, cleanupPlugin, specErr := provisionSpecPlugin()
	var spec specProvision
	if specErr == nil {
		defer cleanupPlugin()
		var cleanupFragments func()
		spec, cleanupFragments, specErr = provisionSpecVerification(pluginDir)
		if specErr == nil {
			defer cleanupFragments()
			cmdArgs = append(cmdArgs, spec.specPytestArgs()...)
		}
	}
	if specErr != nil {
		emit.Diagnostic("warning", specErr.Error(), "", 0)
	}

	// The per-case producer: the plugin records one line per test item into a
	// scratch file this run reads back after pytest exits.
	cases, casesErr := provisionTestCases()
	if casesErr != nil {
		emit.Diagnostic("warning", casesErr.Error(), "", 0)
	}
	defer cases.remove()
	cmdArgs = append(cmdArgs, cases.pytestArgs()...)

	if strings.TrimSpace(testFilter) != "" {
		cmdArgs = append(cmdArgs, testFilter)
	} else {
		cmdArgs = append(cmdArgs, ctx.Project.Path)
	}

	// Merge PYTHONPATH
	pyPath := projectRoot
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		pyPath = projectRoot + string(os.PathListSeparator) + existing
	}

	// Phase: run
	emit.PhaseStart("run")
	// MakeTestEnv, not MakeEnv: a test process must not inherit the harness
	// host's platform identity.
	testExtra := map[string]string{"PYTHONPATH": pyPath}
	if specErr == nil {
		spec.decorateSpecEnv(testExtra)
	}
	cases.decorateEnv(testExtra)
	env := MakeTestEnv(wsRoot, projectRoot, testExtra)
	combined, runErr := pytestCommandRunner(context.Background(), cmdArgs[0], cmdArgs[1:], projectRoot, env)

	if len(combined) > 0 {
		_, _ = os.Stderr.WriteString(combined)
	}

	counts := parse.PytestResults(combined)
	if counts.Total == 0 && runErr != nil {
		counts.Failed = 1
		counts.Total = 1
	}

	// Merge and publish the spec-verification fragments, for failing runs too:
	// a failed check is an observation the gate must see, not a report to
	// withhold.
	if specErr == nil && ctx.OutputPath != "" {
		specreport.MergeSolo(emit, spec.fragmentsDir, projectRoot, ctx.OutputPath)
	}

	if counts.Total > 0 {
		emit.Metric("tests-total", counts.Total, "count")
		emit.Metric("tests-passed", counts.Passed, "count")
		emit.Metric("tests-failed", counts.Failed, "count")
		emit.Metric("tests-skipped", counts.Skipped, "count")

		var parts []string
		parts = append(parts, fmt.Sprintf("%d/%d passed", counts.Passed, counts.Total))
		if counts.Failed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", counts.Failed))
		}
		if counts.Skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", counts.Skipped))
		}
		emit.Summary(strings.Join(parts, ", "))
	}

	// Canonical result payload (protocol/runtime: testSummary) so the
	// orchestrator can aggregate outcomes across languages.
	var resultData map[string]any
	if counts.Total > 0 {
		resultData = map[string]any{
			"testSummary": map[string]any{
				"total":   counts.Total,
				"passed":  counts.Passed,
				"failed":  counts.Failed,
				"skipped": counts.Skipped,
			},
		}
	}
	keptCases, droppedCases := cases.read(wsRoot, protocolcli.TestCaseMaxBytesPerTask)
	resultData = attachTestCases(resultData, keptCases, droppedCases)

	if runErr != nil {
		emit.PhaseEnd("run", "failed")
		emit.Diagnostic("error", fmt.Sprintf("Tests failed for %s:\n%s", ctx.Project.Name, toolchain.TailText(combined, "", 10)), "", 0)
		return "FAILED", resultData, nil
	}

	emit.PhaseEnd("run", "success")
	emit.Log("info", "Tests passed for "+ctx.Project.Name)
	return "OK", resultData, nil
}
