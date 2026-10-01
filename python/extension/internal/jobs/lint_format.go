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

// LintFormat runs ruff format on a Python project.
func LintFormat(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	flags := cli.ParseFlags(args)
	fix := cli.FlagBool(flags, "fix", ctx.Params.Bool("fix", true))

	// When the scheduler groups compatible projects, run ruff once for all of
	// them and split the results back per project.
	if len(ctx.SelectedProjects) > 0 {
		return runLintBatch(ctx, emit, lintBatchFormat, fix)
	}

	if ctx.Project.Name == "" {
		return "SKIP", nil, nil
	}

	wsRoot := ctx.WorkspaceRoot
	projectRoot := ctx.Project.FullPath

	if !syncWorkspace(wsRoot, emit) {
		return "FAILED", nil, nil
	}

	packageName := ResolvePackageName(ctx)
	cacheDir := filepath.Join(wsRoot, ".putnami/bin/extensions/putnami-python/.ruff_cache")

	ruffArgs := []string{"format", ".", "--cache-dir", cacheDir}
	if !fix {
		ruffArgs = append(ruffArgs, "--check")
	}
	cmdArgs := toolchain.UVRunWithToolArgs("ruff", packageName, projectRoot, ruffArgs...)

	emit.PhaseStart("format")
	output, err := ruffSingleRun(cmdArgs[0], cmdArgs[1:], projectRoot, MakeEnv(wsRoot, projectRoot, nil))

	if err == nil {
		emit.PhaseEnd("format", "success")
		return "OK", nil, nil
	}

	combined := string(output)
	fileCount := parse.RuffFormatIssues(combined)
	if fileCount > 0 {
		emit.Metric("format-issues", fileCount, "count")
		plural := "s"
		if fileCount == 1 {
			plural = ""
		}
		emit.Summary(fmt.Sprintf("%d file%s need formatting", fileCount, plural))
	} else {
		emit.Metric("format-issues", 1, "count")
	}

	emit.PhaseEnd("format", "failed")
	emit.Diagnostic("error", fmt.Sprintf("Ruff format failed for %s:\n%s", ctx.Project.Name, toolchain.TailText(string(output), "", 10)), "", 0)
	return "FAILED", nil, nil
}
