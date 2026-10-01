package main

import (
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/skipguard"
)

// skipGuardParam turns the skip guard off when false. It rides with the
// read-only lint and the fixing lint check, never with the format pass.
const skipGuardParam = "skip-guard"

func skipGuardEnabled(params pctx.Params) bool {
	return params.Bool(skipGuardParam, true, "skipGuard")
}

// skipGuardDiagnostics reports the focused tests and the test skips that hide
// a failing test under projectPath, with workspace-relative paths. A reviewed
// exception is a warning.
func skipGuardDiagnostics(workspaceRoot, projectPath string) []parse.BiomeDiagnostic {
	findings, err := skipguard.ScanProject(projectPath)
	if err != nil {
		return []parse.BiomeDiagnostic{{
			Category:    skipguard.Category,
			Severity:    "error",
			Description: "skip guard could not read the project: " + err.Error(),
		}}
	}
	diagnostics := make([]parse.BiomeDiagnostic, 0, len(findings))
	for _, finding := range findings {
		diagnostics = append(diagnostics, parse.BiomeDiagnostic{
			Category:    skipguard.Category,
			Severity:    finding.Severity,
			Description: finding.Message + " (" + skipguard.Category + ")",
			File:        batchWorkspacePath(workspaceRoot, finding.File),
			Line:        finding.Line,
			Column:      finding.Column,
		})
	}
	return diagnostics
}

// runSkipGuard emits the skip guard's diagnostics for one project and returns
// its error and warning counts.
func runSkipGuard(emit *jsonl.Emitter, workspaceRoot, projectPath string) (int, int) {
	failures, warnings := 0, 0
	for _, d := range skipGuardDiagnostics(workspaceRoot, projectPath) {
		emit.DiagnosticWithCode(d.Severity, d.Description, d.File, d.Line, d.Column, d.Category)
		switch d.Severity {
		case "error":
			failures++
		case "warning":
			warnings++
		}
	}
	// The renderer sums repeated metrics, so these add to Biome's.
	if failures > 0 {
		emit.Metric("lint-errors", failures, "count")
	}
	if warnings > 0 {
		emit.Metric("lint-warnings", warnings, "count")
	}
	return failures, warnings
}

// addSkipGuardDiagnostics appends each project's skip guard findings to its
// batch result; an error fails the project.
func addSkipGuardDiagnostics(workspaceRoot string, projects []pctx.ProjectRef, results []lintBatchProjectResult, maxDiagnostics int) {
	for i, project := range projects {
		for _, diagnostic := range skipGuardDiagnostics(workspaceRoot, filepath.Join(workspaceRoot, project.Path)) {
			appendBatchDiagnostic(&results[i], diagnostic, maxDiagnostics)
			if diagnostic.Severity == "error" {
				results[i].Status = "FAILED"
			}
		}
	}
}
