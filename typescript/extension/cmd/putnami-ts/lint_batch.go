package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/typescript/extension/internal/lint"
	"go.putnami.dev/typescript/extension/internal/parse"
)

const (
	lintBatchCombined = "combined"
	lintBatchFormat   = "format"
	lintBatchCheck    = "check"
)

type lintBatchProjectResult struct {
	ProjectID   string                  `json:"projectId"`
	Status      string                  `json:"status"`
	Diagnostics []parse.BiomeDiagnostic `json:"diagnostics,omitempty"`
	Summary     parse.BiomeSummary      `json:"summary,omitempty"`
}

func runLintBatch(ctx *pctx.Context, mode string) (string, map[string]any, error) {
	projects := append([]pctx.ProjectRef(nil), ctx.SelectedProjects...)
	if len(projects) == 0 {
		return "FAILED", nil, fmt.Errorf("lint batch requires at least one selected project")
	}
	projectPaths := make([]string, 0, len(projects))
	for _, project := range projects {
		projectPaths = append(projectPaths, filepath.Join(ctx.WorkspaceRoot, project.Path))
	}

	biomeBin, err := resolveBiomeBinFn(projectPaths[0], ctx.WorkspaceRoot)
	if err != nil {
		return "FAILED", nil, err
	}
	configPath := resolveBiomeConfigFn(projectPaths[0], ctx.WorkspaceRoot, ctx.Extension.Root)
	maxDiagnostics := ctx.Params.Int("max-diagnostics", 10, "maxDiagnostics")
	diagnosticLevel := ctx.Params.String("diagnostic-level", "diagnosticLevel")

	var report parse.BiomeReport
	var ok bool
	switch mode {
	case lintBatchCombined:
		report, ok, err = lint.CheckAllProjects(
			biomeBin,
			projectPaths,
			ctx.WorkspaceRoot,
			configPath,
			-1,
			diagnosticLevel,
		)
	case lintBatchFormat:
		report, ok, err = lint.FormatProjects(
			biomeBin,
			projectPaths,
			ctx.WorkspaceRoot,
			configPath,
			ctx.Params.Bool("fix", true),
		)
	case lintBatchCheck:
		report, ok, err = lint.CheckProjects(
			biomeBin,
			projectPaths,
			ctx.WorkspaceRoot,
			configPath,
			ctx.Params.Bool("fix", true),
			-1,
			diagnosticLevel,
		)
	default:
		return "FAILED", nil, fmt.Errorf("unsupported lint batch mode %q", mode)
	}
	if err != nil {
		return "FAILED", nil, err
	}

	// Biome's singleton default is 20 when the flag is omitted. The format
	// phase never consumes the command's max-diagnostics parameter, while the
	// check phases override that default only with a positive value.
	displayMaxDiagnostics := 20
	if mode != lintBatchFormat && maxDiagnostics > 0 {
		displayMaxDiagnostics = maxDiagnostics
	}
	results := splitBiomeBatchReport(ctx.WorkspaceRoot, projects, report, ok, displayMaxDiagnostics)
	if mode != lintBatchFormat && skipGuardEnabled(ctx.Params) {
		addSkipGuardDiagnostics(ctx.WorkspaceRoot, projects, results, displayMaxDiagnostics)
	}
	// The subprocess itself succeeded in producing the split protocol. Per-job
	// failures stay in batchResults so the scheduler can preserve independent
	// DAG and cache outcomes without turning every peer into a process failure.
	return "OK", map[string]any{"batchResults": results}, nil
}

func splitBiomeBatchReport(
	workspaceRoot string,
	projects []pctx.ProjectRef,
	report parse.BiomeReport,
	toolSucceeded bool,
	maxDiagnostics int,
) []lintBatchProjectResult {
	results := make([]lintBatchProjectResult, len(projects))
	indexByID := make(map[string]int, len(projects))
	for i, project := range projects {
		results[i] = lintBatchProjectResult{ProjectID: project.ID, Status: "OK"}
		indexByID[project.ID] = i
	}

	type projectPrefix struct {
		id   string
		path string
	}
	prefixes := make([]projectPrefix, 0, len(projects))
	rootProjectID := ""
	for _, project := range projects {
		projectPath := filepath.ToSlash(filepath.Clean(project.Path))
		if projectPath == "." {
			rootProjectID = project.ID
			continue
		}
		prefixes = append(prefixes, projectPrefix{
			id:   project.ID,
			path: projectPath,
		})
	}
	sort.SliceStable(prefixes, func(i, j int) bool {
		return len(prefixes[i].path) > len(prefixes[j].path)
	})

	unattributedFailure := false
	for _, diagnostic := range report.Diagnostics {
		file := batchWorkspacePath(workspaceRoot, diagnostic.File)
		diagnostic.File = file
		projectID := ""
		for _, prefix := range prefixes {
			if file == prefix.path || strings.HasPrefix(file, prefix.path+"/") {
				projectID = prefix.id
				break
			}
		}
		if projectID == "" {
			projectID = rootProjectID
		}
		if projectID == "" {
			if diagnostic.Severity == "error" {
				unattributedFailure = true
			}
			for i := range results {
				appendBatchDiagnostic(&results[i], diagnostic, maxDiagnostics)
			}
			continue
		}
		appendBatchDiagnostic(&results[indexByID[projectID]], diagnostic, maxDiagnostics)
	}

	anyAttributedError := false
	if !toolSucceeded {
		for i := range results {
			if results[i].Summary.Errors > 0 {
				results[i].Status = "FAILED"
				anyAttributedError = true
			}
		}
	}
	if !toolSucceeded && (!anyAttributedError || unattributedFailure) {
		for i := range results {
			results[i].Status = "FAILED"
		}
	}
	return results
}

func appendBatchDiagnostic(result *lintBatchProjectResult, diagnostic parse.BiomeDiagnostic, maxDiagnostics int) {
	switch diagnostic.Severity {
	case "error":
		result.Summary.Errors++
	case "warning":
		result.Summary.Warnings++
	default:
		result.Summary.Infos++
	}
	if maxDiagnostics <= 0 || len(result.Diagnostics) < maxDiagnostics {
		result.Diagnostics = append(result.Diagnostics, diagnostic)
	}
}

func batchWorkspacePath(workspaceRoot, path string) string {
	if path == "" {
		return ""
	}
	native := filepath.Clean(filepath.FromSlash(path))
	if filepath.IsAbs(native) {
		if rel, err := filepath.Rel(workspaceRoot, native); err == nil &&
			rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			native = rel
		}
	}
	return filepath.ToSlash(strings.TrimPrefix(filepath.Clean(native), "."+string(filepath.Separator)))
}
