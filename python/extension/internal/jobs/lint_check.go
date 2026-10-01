package jobs

import (
	"fmt"
	"path/filepath"

	"go.putnami.dev/python/extension/internal/parse"
	"go.putnami.dev/python/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// LintCheck runs ruff check on a Python project.
func LintCheck(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	flags := cli.ParseFlags(args)
	fix := cli.FlagBool(flags, "fix", ctx.Params.Bool("fix", true))

	// When the scheduler groups compatible projects, run ruff once for all of
	// them and split the results back per project.
	if len(ctx.SelectedProjects) > 0 {
		return runLintBatch(ctx, emit, lintBatchCheck, fix)
	}

	if ctx.Project.Name == "" {
		return "SKIP", nil, nil
	}

	wsRoot := ctx.WorkspaceRoot

	if !syncWorkspace(wsRoot, emit) {
		return "FAILED", nil, nil
	}

	// Match the batch invocation: run from the workspace root and pass this
	// project as a workspace-relative target. That keeps Ruff's config discovery
	// and file selection identical whether the scheduler batches this job or not.
	targets, err := ruffBatchTargets([]pctx.ProjectRef{{
		Name: ctx.Project.Name,
		Path: ctx.Project.Path,
	}})
	if err != nil {
		return "FAILED", nil, err
	}

	packageName := ResolvePackageName(ctx)
	cacheDir := filepath.Join(wsRoot, ".putnami/bin/extensions/putnami-python/.ruff_cache")

	ruffArgs := []string{"check", "--output-format=json", targets[0], "--cache-dir", cacheDir}
	if fix {
		ruffArgs = append(ruffArgs, "--fix")
	}
	cmdArgs := toolchain.UVRunWithToolArgs("ruff", packageName, wsRoot, ruffArgs...)

	emit.PhaseStart("check")
	result, err := ruffSingleCheckRun(cmdArgs[0], cmdArgs[1:], wsRoot, MakeEnv(wsRoot, wsRoot, nil))

	if err == nil {
		emit.Metric("lint-errors", 0, "count")
		emit.PhaseEnd("check", "success")
		return "OK", nil, nil
	}

	findings, parsed := parse.RuffCheckJSONFindings(string(result.Stdout))
	errors := len(findings)
	if errors == 0 {
		errors = 1
	}
	emit.Metric("lint-errors", errors, "count")
	plural := "s"
	if errors == 1 {
		plural = ""
	}
	emit.Summary(fmt.Sprintf("%d error%s", errors, plural))

	emit.PhaseEnd("check", "failed")

	// Match the batch path's JSON diagnostics: one diagnostic per finding with
	// workspace-relative file paths, location, rule code, and severity. Fall
	// back to the raw tail only when Ruff did not emit a JSON finding list.
	for _, f := range findings {
		emit.DiagnosticWithCode(severityOrError(f.Severity), f.Message,
			batchWorkspacePath(wsRoot, f.File), f.Line, f.Column, f.Code)
	}
	if !parsed || len(findings) == 0 {
		emit.Diagnostic("error", fmt.Sprintf("Ruff lint failed for %s:\n%s", ctx.Project.Name,
			toolchain.TailText(string(result.Stderr), string(result.Stdout), 10)), "", 0)
	}

	return "FAILED", map[string]any{
		"lintSummary": map[string]any{"errors": errors},
	}, nil
}
