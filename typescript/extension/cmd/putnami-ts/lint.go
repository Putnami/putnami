package main

import (
	"fmt"
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/lint"
	"go.putnami.dev/typescript/extension/internal/parse"
)

// runLint executes the no-fix (read-only) lint path as a single `biome check`
// pass that combines the formatter and linter in one file scan. The fix
// (writer) path is handled by the distinct runLintFormat + runLintCheck phases
// so auto-fix write behavior is preserved exactly.
func runLint(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if len(ctx.SelectedProjects) > 0 {
		return runLintBatch(ctx, lintBatchCombined)
	}
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	maxDiagnostics := ctx.Params.Int("max-diagnostics", 10, "maxDiagnostics")
	diagnosticLevel := ctx.Params.String("diagnostic-level", "diagnosticLevel")

	biomeBin, err := resolveBiomeBinFn(projectPath, ctx.WorkspaceRoot)
	if err != nil {
		return "FAILED", nil, err
	}

	configPath := resolveBiomeConfigFn(projectPath, ctx.WorkspaceRoot, ctx.Extension.Root)

	// Single combined pass: `biome check` runs the formatter and the linter in
	// one file scan, replacing the previous separate `biome format` +
	// `biome lint` invocations that each scanned the project independently.
	emit.Progress(1, 1, "Formatting and linting code")
	emit.PhaseStart("lint")
	report, ok, err := lint.CheckAll(biomeBin, projectPath, configPath, maxDiagnostics, diagnosticLevel)
	if err != nil {
		emit.PhaseEnd("lint", "failed")
		return "FAILED", nil, err
	}
	emitBiomeDiagnostics(emit, report)
	skipFailures, skipWarnings := 0, 0
	if skipGuardEnabled(ctx.Params) {
		skipFailures, skipWarnings = runSkipGuard(emit, ctx.WorkspaceRoot, projectPath)
	}
	if !ok || skipFailures > 0 {
		emit.PhaseEnd("lint", "failed")
		return "FAILED", nil, nil
	}
	emit.PhaseEnd("lint", "success")

	emit.Log("info", "Lint completed for "+ctx.Project.Name)

	// Summary
	errors := report.Summary.Errors
	warnings := report.Summary.Warnings + skipWarnings
	var parts []string
	if errors > 0 {
		parts = append(parts, fmt.Sprintf("%d error%s", errors, plural(errors)))
	}
	if warnings > 0 {
		parts = append(parts, fmt.Sprintf("%d warning%s", warnings, plural(warnings)))
	}
	if len(parts) > 0 {
		emit.Summary(join(parts, ", "))
	}

	return "OK", nil, nil
}

func runLintFormat(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if len(ctx.SelectedProjects) > 0 {
		return runLintBatch(ctx, lintBatchFormat)
	}
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	fix := ctx.Params.Bool("fix", true)

	biomeBin, err := resolveBiomeBinFn(projectPath, ctx.WorkspaceRoot)
	if err != nil {
		return "FAILED", nil, err
	}

	configPath := resolveBiomeConfigFn(projectPath, ctx.WorkspaceRoot, ctx.Extension.Root)

	emit.PhaseStart("format")
	report, ok, err := lint.Format(biomeBin, projectPath, configPath, fix)
	if err != nil {
		emit.PhaseEnd("format", "failed")
		return "FAILED", nil, err
	}
	emitBiomeDiagnostics(emit, report)

	status := "success"
	if !ok {
		status = "failed"
	}
	emit.PhaseEnd("format", status)

	if !ok {
		return "FAILED", nil, nil
	}
	return "OK", nil, nil
}

func runLintCheck(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if len(ctx.SelectedProjects) > 0 {
		return runLintBatch(ctx, lintBatchCheck)
	}
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	fix := ctx.Params.Bool("fix", true)
	maxDiagnostics := ctx.Params.Int("max-diagnostics", 10, "maxDiagnostics")
	diagnosticLevel := ctx.Params.String("diagnostic-level", "diagnosticLevel")

	biomeBin, err := resolveBiomeBinFn(projectPath, ctx.WorkspaceRoot)
	if err != nil {
		return "FAILED", nil, err
	}

	configPath := resolveBiomeConfigFn(projectPath, ctx.WorkspaceRoot, ctx.Extension.Root)

	emit.PhaseStart("lint")
	report, ok, err := lint.Check(biomeBin, projectPath, configPath, fix, maxDiagnostics, diagnosticLevel)
	if err != nil {
		emit.PhaseEnd("lint", "failed")
		return "FAILED", nil, err
	}
	emitBiomeDiagnostics(emit, report)
	if skipGuardEnabled(ctx.Params) {
		if failures, _ := runSkipGuard(emit, ctx.WorkspaceRoot, projectPath); failures > 0 {
			ok = false
		}
	}

	status := "success"
	if !ok {
		status = "failed"
	}
	emit.PhaseEnd("lint", status)

	if !ok {
		return "FAILED", nil, nil
	}
	return "OK", nil, nil
}

func emitBiomeDiagnostics(emit *jsonl.Emitter, report parse.BiomeReport) {
	for _, d := range report.Diagnostics {
		emit.DiagnosticWithCode(d.Severity, d.Description, d.File, d.Line, d.Column, d.Category)
	}
	if report.Summary.Errors > 0 {
		emit.Metric("lint-errors", report.Summary.Errors, "count")
	}
	if report.Summary.Warnings > 0 {
		emit.Metric("lint-warnings", report.Summary.Warnings, "count")
	}
	if report.Summary.Infos > 0 {
		emit.Metric("lint-infos", report.Summary.Infos, "count")
	}
}

func plural(n int) string {
	if n != 1 {
		return "s"
	}
	return ""
}

func join(parts []string, sep string) string {
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += sep
		}
		result += p
	}
	return result
}
