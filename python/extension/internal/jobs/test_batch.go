package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/python/extension/internal/parse"
	"go.putnami.dev/python/extension/internal/toolchain"
	"go.putnami.dev/python/extension/internal/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// pyTestBatchDiagnostic mirrors the scheduler's batchWireDiagnostic JSON shape
// (tooling/cli/internal/jobs/scheduler_batch_exec.go). The json tags MUST stay
// byte-identical or the scheduler silently drops the finding.
type pyTestBatchDiagnostic struct {
	Category    string `json:"category,omitempty"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
}

// pyTestBatchArtifact mirrors the scheduler's batchWireArtifact JSON shape.
type pyTestBatchArtifact struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// pyTestBatchProjectResult mirrors the scheduler's batchWireResult JSON shape:
// one independent per-project outcome carried inside {"batchResults":[...]}.
type pyTestBatchProjectResult struct {
	ProjectID   string                  `json:"projectId"`
	Status      string                  `json:"status"`
	Data        map[string]any          `json:"data,omitempty"`
	Diagnostics []pyTestBatchDiagnostic `json:"diagnostics,omitempty"`
	Artifacts   []pyTestBatchArtifact   `json:"artifacts,omitempty"`
}

// batchSuiteTimeout bounds a single project's pytest run inside a batch so one
// hung suite cannot stall the whole group. Package variable so tests can shrink
// it. Matches the TypeScript extension's DefaultTestTimeout.
var batchSuiteTimeout = 10 * time.Minute

// syncWorkspaceFn syncs the shared UV workspace, once per solo run and once per
// batch. It is a package variable so the test jobs can be unit-tested without
// a real uv toolchain.
var syncWorkspaceFn = SyncWorkspace

// pytestCommandRunner runs a prepared pytest command for one project and
// returns combined output plus the process error. Package variable so the solo
// and batch paths (crash / hung suite included) can be unit-tested without a
// real uv/pytest toolchain. Production code always calls runPytestCommand; a
// batch attempt's context deadline bounds a hung suite.
var pytestCommandRunner = runPytestCommand

func runPytestCommand(cmdCtx context.Context, name string, args []string, dir string, env []string) (string, error) {
	cmd := exec.CommandContext(cmdCtx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestBatch runs every selected project's pytest suite SEQUENTIALLY inside a
// single extension process and fans the per-project outcomes back through the
// batch wire. This amortizes the extension-process overhead across the group
// while each project keeps its own pytest invocation and — because the
// scheduler splits the result — its own independent status and cache identity.
//
// The process returns "OK" whenever it produced the split protocol: a project's
// failing (or crashing) suite is recorded as that project's FAILED result
// inside batchResults, never as a process-level failure, so one bad suite can
// neither fail nor mask its batch-mates. A shared-prerequisite failure (the UV
// workspace sync) is the sole whole-group failure, because it legitimately
// blocks every member.
func TestBatch(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	projects := append([]pctx.ProjectRef(nil), ctx.SelectedProjects...)
	if len(projects) == 0 {
		return "FAILED", nil, fmt.Errorf("test batch requires at least one selected project")
	}

	wsRoot := ctx.WorkspaceRoot

	// Sync the UV workspace once for the whole group (mirrors the solo sync). A
	// failure here blocks every member, so failing the batch is correct.
	if !syncWorkspaceFn(wsRoot, emit) {
		return "FAILED", nil, nil
	}

	// Every batch-mate resolved identical effective params (the scheduler folds
	// them into the batch key), so a single flag resolution is correct for all.
	flags := cli.ParseFlags(args)
	withLog := cli.FlagBool(flags, "log", ctx.Params.Bool("log", false))
	updateSnapshots := cli.FlagBool(flags, "update-snapshots", ctx.Params.Bool("update-snapshots", false))
	testFilter := cli.FlagString(flags, "test", cli.FlagString(flags, "t", ctx.Params.String("test")))

	// One materialized spec plugin for the whole group; each member still gets
	// its own fragment directory, so attribution never depends on sharing.
	// A scratch failure costs the reporting, never the tests.
	pluginDir, cleanupPlugin, pluginErr := provisionSpecPlugin()
	if pluginErr != nil {
		emit.Diagnostic("warning", pluginErr.Error(), "", 0)
		pluginDir = ""
	} else {
		defer cleanupPlugin()
	}

	// Every member's test cases share the batch's one result line.
	caseBytes := protocolcli.TestCaseBatchMemberBytes(len(projects))
	results := make([]pyTestBatchProjectResult, 0, len(projects))
	for _, proj := range projects {
		results = append(results, runProjectPytestSuite(wsRoot, proj, withLog, updateSnapshots, testFilter, pluginDir, caseBytes))
	}
	return "OK", map[string]any{"batchResults": results}, nil
}

// runProjectPytestSuite runs one project's pytest suite and captures every
// outcome — pass, test failure, or a hard subprocess crash — into a single
// per-project result. It never returns a process error, so a failure here is
// isolated to this project. Its status/testSummary decisions mirror the solo
// Test path exactly (plus the one-shot crash respawn).
func runProjectPytestSuite(wsRoot string, proj pctx.ProjectRef, withLog, updateSnapshots bool, testFilter, specPluginDir string, caseBytes int) pyTestBatchProjectResult {
	result := pyTestBatchProjectResult{ProjectID: proj.ID, Status: "OK"}
	projectRoot := proj.FullPath

	tests, _ := workspace.DiscoverTestFiles(projectRoot)
	if len(tests) == 0 {
		// Solo returns SKIP when a project has no tests; keep that per-project
		// outcome so an empty suite never masks a real one.
		result.Status = "SKIP"
		return result
	}

	// One spec-verification fragment directory per member: each
	// project runs its own pytest process, so attribution needs no
	// cross-member split.
	var spec specProvision
	specProvisioned := false
	if specPluginDir != "" {
		provisioned, cleanupFragments, specErr := provisionSpecVerification(specPluginDir)
		if specErr != nil {
			result.Diagnostics = append(result.Diagnostics, pyTestBatchDiagnostic{
				Severity: "warning", Description: specErr.Error(),
			})
		} else {
			defer cleanupFragments()
			spec = provisioned
			specProvisioned = true
		}
	}

	// One per-case scratch file per member, so each member reports only the
	// cases its own pytest process ran.
	cases, casesErr := provisionTestCases()
	if casesErr != nil {
		result.Diagnostics = append(result.Diagnostics, pyTestBatchDiagnostic{
			Severity: "warning", Description: casesErr.Error(),
		})
	}
	defer cases.remove()

	cmdArgs := buildProjectPytestArgs(wsRoot, proj, withLog, updateSnapshots, testFilter, spec, cases)

	pyPath := projectRoot
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		pyPath = projectRoot + string(os.PathListSeparator) + existing
	}
	// MakeTestEnv, not MakeEnv: a test process must not inherit the harness
	// host's platform identity. Must match the solo path exactly.
	testExtra := map[string]string{"PYTHONPATH": pyPath}
	if specProvisioned {
		spec.decorateSpecEnv(testExtra)
	}
	cases.decorateEnv(testExtra)
	env := MakeTestEnv(wsRoot, projectRoot, testExtra)

	counts, output, runErr := runProjectPytestWithRetry(cmdArgs, projectRoot, env, cases)

	// Merge and publish the spec-verification fragments, for failing suites
	// too: a failed check is an observation the gate must see.
	if specProvisioned {
		attachBatchSpecVerification(&result, wsRoot, spec.fragmentsDir, projectRoot, batchOutputPath(wsRoot, proj))
	}

	// Mirror the solo fallback: a suite that produced no parseable result but
	// exited non-zero counts as a single failure.
	if counts.Total == 0 && runErr != nil {
		counts.Failed = 1
		counts.Total = 1
	}

	if counts.Total > 0 {
		result.Data = map[string]any{
			"testSummary": map[string]any{
				"total":   counts.Total,
				"passed":  counts.Passed,
				"failed":  counts.Failed,
				"skipped": counts.Skipped,
			},
		}
	}
	keptCases, droppedCases := cases.read(wsRoot, caseBytes)
	result.Data = attachTestCases(result.Data, keptCases, droppedCases)

	if runErr != nil {
		result.Status = "FAILED"
		result.Diagnostics = append(result.Diagnostics, pyTestBatchDiagnostic{
			Severity:    "error",
			Description: fmt.Sprintf("Tests failed for %s:\n%s", proj.Name, toolchain.TailText(output, "", 10)),
		})
		return result
	}

	return result
}

// buildProjectPytestArgs assembles the uv-run pytest command for one project,
// mirroring the solo Test command construction so a batched suite runs the
// exact same tool invocation. No --config is passed, so pytest/uv resolve each
// project's configuration by directory as usual.
func buildProjectPytestArgs(wsRoot string, proj pctx.ProjectRef, withLog, updateSnapshots bool, testFilter string, spec specProvision, cases *testCaseSink) []string {
	packageName := resolvePackageNameForProject(proj)
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
	if spec.fragmentsDir != "" {
		cmdArgs = append(cmdArgs, spec.specPytestArgs()...)
	}
	cmdArgs = append(cmdArgs, cases.pytestArgs()...)
	if strings.TrimSpace(testFilter) != "" {
		cmdArgs = append(cmdArgs, testFilter)
	} else {
		cmdArgs = append(cmdArgs, proj.Path)
	}
	return cmdArgs
}

// resolvePackageNameForProject resolves the Python package name for a selected
// project from its pyproject.toml, falling back to the project name — the
// per-project analog of ResolvePackageName.
func resolvePackageNameForProject(proj pctx.ProjectRef) string {
	name, err := workspace.ParsePyprojectName(filepath.Join(proj.FullPath, "pyproject.toml"))
	if err == nil && name != "" {
		return name
	}
	return proj.Name
}

// runProjectPytestWithRetry runs one project's suite, respawning once when the
// first attempt crashes non-gracefully (the tool dies or hangs without a
// parseable result). A suite that runs to completion and reports test failures
// is NOT a crash and is returned without a retry.
func runProjectPytestWithRetry(cmdArgs []string, dir string, env []string, cases *testCaseSink) (parse.TestCounts, string, error) {
	counts, output, runErr := runProjectPytestOnce(cmdArgs, dir, env, cases)
	if !isSuiteCrash(counts, runErr) {
		return counts, output, runErr
	}
	return runProjectPytestOnce(cmdArgs, dir, env, cases)
}

// runProjectPytestOnce runs one bounded pytest attempt. The context deadline
// bounds a hung suite so it surfaces as a crash rather than stalling the batch.
// It empties the per-case file first, so the cases read after a respawn are
// the respawned attempt's only.
func runProjectPytestOnce(cmdArgs []string, dir string, env []string, cases *testCaseSink) (parse.TestCounts, string, error) {
	cases.reset()
	cmdCtx, cancel := context.WithTimeout(context.Background(), batchSuiteTimeout)
	defer cancel()
	output, runErr := pytestCommandRunner(cmdCtx, cmdArgs[0], cmdArgs[1:], dir, env)
	return parse.PytestResults(output), output, runErr
}

// isSuiteCrash reports a non-graceful failure: the process errored but produced
// no parseable result (tool died, invalid config, or a hung suite killed by the
// deadline). A normal test failure carries parseable counts and is not a crash.
func isSuiteCrash(counts parse.TestCounts, runErr error) bool {
	return runErr != nil && counts.Total == 0
}
