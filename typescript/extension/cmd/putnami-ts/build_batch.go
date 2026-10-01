package main

import (
	"fmt"
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/project"
)

// buildBatchProjectResult is one project's slice of a batched build-producer
// invocation. Its Data mirrors EXACTLY what the corresponding solo handler
// returns (nil for transpile/types/compile; {hash,exports,assets,clientOutput?}
// for generate) so the scheduler caches and captures the batched result
// byte-identically to a singleton run. Metrics carry the per-project build
// counters (transpiled-files, type-declarations, generate-hash,
// compiled-executables) that cannot ride the nil Data. Diagnostics carry only
// what the corresponding solo handler emits as diagnostic events, so replayed
// terminal rows match solo; infra errors that solo routes to an error log have
// no per-project channel in batch mode and surface as FAILED status alone.
type buildBatchProjectResult struct {
	ProjectID   string                 `json:"projectId"`
	Status      string                 `json:"status"`
	Data        map[string]any         `json:"data,omitempty"`
	Diagnostics []buildBatchDiagnostic `json:"diagnostics,omitempty"`
	Metrics     []buildBatchMetric     `json:"metrics,omitempty"`
}

// buildBatchDiagnostic mirrors the scheduler's batchWireDiagnostic shape exactly
// (category/severity/description/file/line/column) so replayed rows match solo.
type buildBatchDiagnostic struct {
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
}

// buildBatchMetric mirrors the scheduler's batchWireMetric shape exactly. The
// json tags MUST stay identical on both sides of the batch protocol.
type buildBatchMetric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// batchOutputPath returns the per-project captured-output directory for one
// project in a batch. The scheduler prepares each miss's directory at
// .putnami/out/{project.Path}/{cmd}; the leader's OutputPath ends in that same
// {cmd} segment, so its base names the command. This mirrors the solo handlers'
// filepath.Join(ctx.OutputPath, "lib"|"types"|"compile") exactly.
func batchOutputPath(ctx *pctx.Context, proj pctx.ProjectRef) string {
	cmdName := filepath.Base(ctx.OutputPath)
	return filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", proj.Path, cmdName)
}

// perProjectContext returns a shallow copy of the batch leader's context with the
// Project identity replaced by one batch member's, so per-project code that reads
// ctx.Project (today: doGenerate forwarding the name to pre-build hooks) sees the
// member, not the leader. ctx.Project is a value field, so the copy is isolated;
// Params/SelectedProjects/FilePatterns stay shared (read-only, identical across
// same-kind peers). Only the fields a ProjectRef carries (Name/Path/FullPath) are
// known here — if a producer ever needs richer per-project Project fields
// (Exports/Options/…), the batch protocol's ProjectRef must be extended to carry
// them rather than inheriting the leader's.
func perProjectContext(ctx *pctx.Context, proj pctx.ProjectRef) *pctx.Context {
	member := *ctx
	member.Project.Name = proj.Name
	member.Project.Path = proj.Path
	member.Project.FullPath = proj.FullPath
	return &member
}

// runBuildTranspileBatch transpiles every selected project inside one shared
// process. Each project runs the same build.RunTranspile the solo handler runs,
// writing to its own captured-output lib dir, with per-project failure
// isolation. The subprocess itself returns OK so one project's failure never
// fails its peers.
func runBuildTranspileBatch(ctx *pctx.Context) (string, map[string]any, error) {
	projects := ctx.SelectedProjects
	if len(projects) == 0 {
		return "FAILED", nil, fmt.Errorf("build transpile batch requires at least one selected project")
	}
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}
	params := build.TranspileParams{
		Target:    ctx.Params.String("target", "bun"),
		Bundle:    ctx.Params.String("bundle", "none"),
		Sourcemap: ctx.Params.String("sourcemap", "external"),
		Minify:    ctx.Params.Bool("minify", true),
		Splitting: ctx.Params.Bool("splitting", true),
		Metafile:  ctx.Params.Bool("metafile", false),
	}

	results := make([]buildBatchProjectResult, 0, len(projects))
	for _, proj := range projects {
		projectPath := filepath.Join(ctx.WorkspaceRoot, proj.Path)
		libOutput := filepath.Join(batchOutputPath(ctx, proj), "lib")
		res := buildBatchProjectResult{ProjectID: proj.ID, Status: "OK"}

		files, transErrors, err := build.RunTranspile(bunBin, projectPath, libOutput, params)
		if err != nil {
			// Solo routes this to an error log (no diagnostic event). Batch has no
			// per-project log channel: surface FAILED status alone.
			res.Status = "FAILED"
			results = append(results, res)
			continue
		}
		if len(transErrors) > 0 {
			res.Status = "FAILED"
			for _, e := range transErrors {
				res.Diagnostics = append(res.Diagnostics, buildBatchDiagnostic{
					Category:    errs.CodeTranspileError.String(),
					Severity:    "error",
					Description: e,
				})
			}
			results = append(results, res)
			continue
		}
		res.Metrics = append(res.Metrics, buildBatchMetric{
			Name: "transpiled-files", Value: float64(len(files)), Unit: "count",
		})
		results = append(results, res)
	}
	return "OK", map[string]any{"batchResults": results}, nil
}

// runBuildTypesBatch generates type declarations for every selected project in
// one shared process, mirroring the solo runBuildTypes per project.
func runBuildTypesBatch(ctx *pctx.Context) (string, map[string]any, error) {
	projects := ctx.SelectedProjects
	if len(projects) == 0 {
		return "FAILED", nil, fmt.Errorf("build types batch requires at least one selected project")
	}
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	results := make([]buildBatchProjectResult, 0, len(projects))
	for _, proj := range projects {
		projectPath := filepath.Join(ctx.WorkspaceRoot, proj.Path)
		typesOutput := filepath.Join(batchOutputPath(ctx, proj), "types")
		buildInfoDir := typesBuildInfoDir(ctx.CacheRoot, filepath.Base(ctx.OutputPath), proj.Path)
		res := buildBatchProjectResult{ProjectID: proj.ID, Status: "OK"}

		typesResult, err := build.RunTypes(bunBin, projectPath, typesOutput, buildInfoDir)
		if err != nil {
			// Solo routes this to an error log; batch surfaces FAILED status alone.
			res.Status = "FAILED"
			results = append(results, res)
			continue
		}
		if len(typesResult.GeneratedFiles) > 0 {
			res.Metrics = append(res.Metrics, buildBatchMetric{
				Name: "type-declarations", Value: float64(len(typesResult.GeneratedFiles)), Unit: "count",
			})
		}
		if !typesResult.Success {
			res.Status = "FAILED"
			for _, d := range typesResult.Diagnostics {
				// Mirror solo's emit.DiagnosticWithCode(d.Category, d.Message,
				// d.File, d.Line, d.Column, d.Code): severity=d.Category,
				// message=d.Message, code=d.Code.
				res.Diagnostics = append(res.Diagnostics, buildBatchDiagnostic{
					Category:    d.Code,
					Severity:    d.Category,
					Description: d.Message,
					File:        d.File,
					Line:        d.Line,
					Column:      d.Column,
				})
			}
		}
		results = append(results, res)
	}
	return "OK", map[string]any{"batchResults": results}, nil
}

// runBuildCompileBatch compiles every selected project's entrypoint in one
// shared process, mirroring the solo runBuildCompile per project. A project with
// no resolvable entrypoint is an OK no-op, exactly like solo. Solo emits no
// diagnostic events for compile failures, so batch mirrors that (FAILED status
// only).
func runBuildCompileBatch(ctx *pctx.Context) (string, map[string]any, error) {
	projects := ctx.SelectedProjects
	if len(projects) == 0 {
		return "FAILED", nil, fmt.Errorf("build compile batch requires at least one selected project")
	}
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}
	// One resolved target set for the whole batch: a batch is one subprocess for
	// projects the scheduler grouped by IDENTICAL resolved params, so every
	// member's set is the same one solo would resolve.
	targets, err := compileTargets(ctx)
	if err != nil {
		return "FAILED", nil, err
	}

	results := make([]buildBatchProjectResult, 0, len(projects))
	for _, proj := range projects {
		projectPath := filepath.Join(ctx.WorkspaceRoot, proj.Path)
		res := buildBatchProjectResult{ProjectID: proj.ID, Status: "OK"}

		genResult := loadGenerateResult(projectPath)
		entrypoint := resolveCompileEntrypointForProject(projectPath, genResult)
		if entrypoint == "" {
			results = append(results, res)
			continue
		}

		compileOutput := filepath.Join(batchOutputPath(ctx, proj), "compile")
		files, compileErrors, err := build.RunCompile(bunBin, projectPath, compileOutput, entrypoint, targets)
		if err != nil || len(compileErrors) > 0 {
			res.Status = "FAILED"
			// The compiler's words are the row's only explanation — a FAILED
			// member with no diagnostic reaches CI as a bare "task failed"
			// (a large consumer workspace's identity/libs/oauth, five runs of
			// silence). Carry the infra error and every collected compile
			// error exactly as the generate branch below carries its
			// diagnostics.
			if err != nil {
				res.Diagnostics = append(res.Diagnostics, buildBatchDiagnostic{
					Category: "compile", Severity: "error", Description: err.Error(),
				})
			}
			for _, message := range compileErrors {
				res.Diagnostics = append(res.Diagnostics, buildBatchDiagnostic{
					Category: "compile", Severity: "error", Description: message,
				})
			}
			results = append(results, res)
			continue
		}
		res.Metrics = append(res.Metrics, buildBatchMetric{
			Name: "compiled-executables", Value: float64(len(files)), Unit: "count",
		})
		results = append(results, res)
	}
	return "OK", map[string]any{"batchResults": results}, nil
}

// runBuildGenerateBatch runs the generate phase for every selected project in
// one shared process. doGenerate is emit-coupled for progress; in batch mode the
// scheduler reconstructs the per-project terminal rows, so its intra-project
// progress is discarded and only per-project failure diagnostics (the same set
// solo emits — generateDiagnostics is the shared source, so neither path can
// report a diagnostic-less failure) are carried on the wire. Each project's
// Data mirrors solo runBuildGenerate EXACTLY so the
// scheduler captures .gen/clients into that project's own cache entry.
func runBuildGenerateBatch(ctx *pctx.Context) (string, map[string]any, error) {
	projects := ctx.SelectedProjects
	if len(projects) == 0 {
		return "FAILED", nil, fmt.Errorf("build generate batch requires at least one selected project")
	}

	results := make([]buildBatchProjectResult, 0, len(projects))
	for _, proj := range projects {
		projectPath := filepath.Join(ctx.WorkspaceRoot, proj.Path)
		res := buildBatchProjectResult{ProjectID: proj.ID, Status: "OK"}

		// doGenerate forwards ctx.Project.Name to the pre-build hooks (the putnami
		// project identity the hook subprocess receives), so it MUST see THIS
		// project, not the batch leader — otherwise every follower's hooks run
		// under the leader's name. Hand it a per-project context copy;
		// Params/FilePatterns/WorkspaceRoot are shared and identical across
		// same-kind peers. Progress emissions are discarded (a batch subprocess
		// reports one aggregate result); failure diagnostics are recomputed from
		// the returned error, matching solo.
		genResult, err := doGenerate(perProjectContext(ctx, proj), jsonl.New(), projectPath)
		if err != nil {
			res.Status = "FAILED"
			for _, d := range generateDiagnostics(err) {
				res.Diagnostics = append(res.Diagnostics, buildBatchDiagnostic{
					Category:    d.Code,
					Severity:    "error",
					Description: d.Message,
				})
			}
			results = append(results, res)
			continue
		}

		res.Metrics = append(res.Metrics, buildBatchMetric{
			Name: "generate-hash", Value: 1, Unit: "count",
		})
		data := generateResultData(projectPath, genResult)
		if out, ok := generatedClientOutput(projectPath); ok {
			data[GeneratedClientOutputPort] = out
		}
		res.Data = data
		results = append(results, res)
	}
	return "OK", map[string]any{"batchResults": results}, nil
}

// resolveCompileEntrypointForProject mirrors resolveCompileEntrypoint but reads
// the project's package.json directly (batch ctx.SelectedProjects carry only
// id/name/path, not exports/bin/main).
func resolveCompileEntrypointForProject(projectPath string, genResult *build.GenerateResult) string {
	if genResult != nil {
		if ep, ok := genResult.Exports["bundled-serve"]; ok {
			return ep
		}
	}
	pkg := project.ReadPackageJSONSafe(filepath.Join(projectPath, "package.json"))
	if pkg == nil {
		return ""
	}
	if serveExport := project.ResolveExportPath(pkg.Exports, "./serve"); serveExport != "" {
		return serveExport
	}
	if binStr := pkg.GetBinString(); binStr != "" {
		return binStr
	}
	return pkg.Main
}
