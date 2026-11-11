package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/lint"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// resolveESLintBinFn locates eslint for a project. A package var so tests
// choose the answer.
var resolveESLintBinFn = toolchain.ResolveESLint

// resolveNodeFn locates the node ESLint runs on. A package var so tests choose
// the answer.
var resolveNodeFn = toolchain.ResolveNode

// projectLinter returns the linter of the project at projectPath: the lint
// option, options["@putnami/typescript"].lint.linter, which reaches the task
// as its lint parameter, else the one the project's lint script runs, else
// Biome (lint.ChooseLinter).
func projectLinter(params pctx.Params, projectPath string) (string, error) {
	option, err := lint.ReadLinterOption(params["lint"])
	if err != nil {
		return "", err
	}
	return lint.ChooseLinter(option, projectPath)
}

// eslintProject resolves eslint and node for the project at projectPath and
// runs ESLint over its files, with workspace-relative diagnostic paths.
func eslintProject(workspaceRoot, projectPath string, fix bool, diagnosticLevel string) (parse.BiomeReport, bool, error) {
	eslintBin, err := resolveESLintBinFn(projectPath, workspaceRoot)
	if err != nil {
		return parse.EmptyBiomeReport(), false, err
	}
	node, err := resolveNodeFn()
	if err != nil {
		return parse.EmptyBiomeReport(), false, err
	}
	return lint.ESLint(eslintBin, node, projectPath, workspaceRoot, fix, diagnosticLevel)
}

// runESLint is the lint phase of a project that lints with ESLint. With fix,
// ESLint writes its fixes; without, it reports only. The skip guard runs as it
// does for Biome.
func runESLint(ctx *pctx.Context, emit *jsonl.Emitter, fix bool) (string, map[string]any, error) {
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	maxDiagnostics := ctx.Params.Int("max-diagnostics", 10, "maxDiagnostics")
	diagnosticLevel := ctx.Params.String("diagnostic-level", "diagnosticLevel")

	emit.Progress(1, 1, "Linting code with ESLint")
	emit.PhaseStart("lint")
	report, ok, err := eslintProject(ctx.WorkspaceRoot, projectPath, fix, diagnosticLevel)
	if err != nil {
		emit.PhaseEnd("lint", "failed")
		return "FAILED", nil, err
	}
	emitBiomeDiagnostics(emit, capDiagnostics(report, maxDiagnostics))
	scriptWarnings := 0
	if warning, ok := ignoredScriptWarning(ctx.WorkspaceRoot, ctx.Project.Path); ok {
		emit.DiagnosticWithCode(warning.Severity, warning.Description, warning.File, 0, 0, warning.Category)
		scriptWarnings++
	}
	skipFailures, skipWarnings := 0, 0
	if skipGuardEnabled(ctx.Params) {
		skipFailures, skipWarnings = runSkipGuard(emit, ctx.WorkspaceRoot, projectPath)
	}
	if !ok || skipFailures > 0 {
		emit.PhaseEnd("lint", "failed")
		return "FAILED", nil, nil
	}
	emit.PhaseEnd("lint", "success")
	emit.Log("info", "ESLint lint completed for "+ctx.Project.Name)

	var parts []string
	if errors := report.Summary.Errors; errors > 0 {
		parts = append(parts, fmt.Sprintf("%d error%s", errors, plural(errors)))
	}
	if warnings := report.Summary.Warnings + skipWarnings + scriptWarnings; warnings > 0 {
		parts = append(parts, fmt.Sprintf("%d warning%s", warnings, plural(warnings)))
	}
	if len(parts) > 0 {
		emit.Summary(join(parts, ", "))
	}
	return "OK", nil, nil
}

// ignoredScriptWarning is the warning that the ESLint lint of the project at
// projectRel, relative to the workspace, does not run part of its lint script:
// the lint reads the script to choose ESLint, never to run it, so an argument
// such as --max-warnings 0 belongs in the ESLint configuration.
func ignoredScriptWarning(workspaceRoot, projectRel string) (parse.BiomeDiagnostic, bool) {
	ignored := lint.IgnoredScriptPart(filepath.Join(workspaceRoot, projectRel))
	if ignored == "" {
		return parse.BiomeDiagnostic{}, false
	}
	return parse.BiomeDiagnostic{
		Category: lint.CodeScriptArgumentsIgnored,
		Severity: "warning",
		Description: fmt.Sprintf("the lint script's %q does not run: Putnami runs ESLint over the project with its configuration alone; "+
			"move ESLint options to the ESLint configuration", ignored),
		File: filepath.ToSlash(filepath.Join(projectRel, "package.json")),
	}, true
}

// skipBiomeFormatter reports that a project that lints with ESLint runs no
// Biome formatter: its formatter, Prettier or another, stays the project's.
func skipBiomeFormatter(ctx *pctx.Context, emit *jsonl.Emitter) (string, map[string]any, error) {
	emit.Log("info", ctx.Project.Name+" lints with ESLint: the Biome formatter does not run")
	return "OK", nil, nil
}

// capDiagnostics keeps the first maxDiagnostics diagnostics of report, and
// every one when maxDiagnostics is not positive. The summary keeps its counts.
func capDiagnostics(report parse.BiomeReport, maxDiagnostics int) parse.BiomeReport {
	if maxDiagnostics > 0 && len(report.Diagnostics) > maxDiagnostics {
		report.Diagnostics = report.Diagnostics[:maxDiagnostics]
	}
	return report
}

// eslintBatchResult lints one batch member with ESLint and returns its
// result. A run that fails to start fails that member alone, with the reason
// as its diagnostic.
func eslintBatchResult(workspaceRoot string, project pctx.ProjectRef, fix bool, diagnosticLevel string, maxDiagnostics int) lintBatchProjectResult {
	result := lintBatchProjectResult{ProjectID: project.ID, Status: "OK"}
	report, ok, err := eslintProject(workspaceRoot, filepath.Join(workspaceRoot, project.Path), fix, diagnosticLevel)
	if err != nil {
		result.Status = "FAILED"
		appendBatchDiagnostic(&result, parse.BiomeDiagnostic{
			Category:    parse.ESLintCategory,
			Severity:    "error",
			Description: err.Error(),
			File:        filepath.ToSlash(project.Path),
		}, maxDiagnostics)
		return result
	}
	for _, diagnostic := range report.Diagnostics {
		appendBatchDiagnostic(&result, diagnostic, maxDiagnostics)
	}
	if warning, ok := ignoredScriptWarning(workspaceRoot, project.Path); ok {
		appendBatchDiagnostic(&result, warning, maxDiagnostics)
	}
	if !ok {
		result.Status = "FAILED"
	}
	return result
}

// runESLintMembers calls run for each index, in parallel up to the task's CPU
// budget (PUTNAMI_CPU_BUDGET, else the host's CPUs): each ESLint member of a
// batch is its own Node process, which a singleton dispatch would also run
// in parallel. Each call writes only its own index.
func runESLintMembers(indexes []int, run func(int)) {
	limit := eslintConcurrency()
	if limit > len(indexes) {
		limit = len(indexes)
	}
	slots := make(chan struct{}, max(limit, 1))
	var wg sync.WaitGroup
	for _, i := range indexes {
		wg.Add(1)
		slots <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			run(i)
		}(i)
	}
	wg.Wait()
}

// eslintConcurrency is the number of ESLint members a batch runs at once.
func eslintConcurrency() int {
	if budget, err := strconv.Atoi(os.Getenv("PUTNAMI_CPU_BUDGET")); err == nil && budget > 0 {
		return budget
	}
	return runtime.NumCPU()
}
