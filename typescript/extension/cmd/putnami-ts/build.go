package main

import (
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/errs"
	"go.putnami.dev/typescript/extension/internal/project"
)

func runBuild(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)

	flags := build.ResolveBuildFlags(
		ctx.Params.Bool("generate", false),
		ctx.Params.Bool("transpile", false),
		ctx.Params.Bool("types", false),
		ctx.Params.Bool("compile", false),
	)

	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	// Phase 1: Generate
	var genResult *build.GenerateResult
	if flags.RunGenerate {
		emit.PhaseStart("generate")
		genResult, err = doGenerate(ctx, emit, projectPath)
		if err != nil {
			emit.PhaseEnd("generate", "failed")
			if !emitCapabilityDiagnostics(emit, err.Error()) {
				emit.DiagnosticWithCode("error", "Generate phase failed: "+err.Error(), "", 0, 0, codeOr(err, errs.CodeGenerateFailed).String())
			}
			return "FAILED", nil, nil
		}
		emit.PhaseEnd("generate", "success")
	} else {
		genResult = loadGenerateResult(projectPath)
	}

	if !flags.RunTranspile && !flags.RunTypes && !flags.RunCompile {
		emit.Log("info", "Generated artifacts for "+ctx.Project.Name)
		emit.Summary("generated")
		return "OK", nil, nil
	}

	// Phase 2: Transpile
	if flags.RunTranspile {
		emit.PhaseStart("transpile")
		libOutput := filepath.Join(ctx.OutputPath, "lib")
		files, errors, err := build.RunTranspile(bunBin, projectPath, libOutput, build.TranspileParams{
			Target:    ctx.Params.String("target", "bun"),
			Bundle:    ctx.Params.String("bundle", "none"),
			Sourcemap: ctx.Params.String("sourcemap", "external"),
			Minify:    ctx.Params.Bool("minify", true),
			Splitting: ctx.Params.Bool("splitting", true),
			Metafile:  ctx.Params.Bool("metafile", false),
		})
		if err != nil {
			emit.PhaseEnd("transpile", "failed")
			return "FAILED", nil, err
		}
		if len(errors) > 0 {
			emit.PhaseEnd("transpile", "failed")
			for _, e := range errors {
				emit.DiagnosticWithCode("error", e, "", 0, 0, errs.CodeTranspileError.String())
			}
			return "FAILED", nil, nil
		}
		emit.Metric("transpiled-files", len(files), "count")
		emit.PhaseEnd("transpile", "success")
	}

	// Phase 3: Types
	if flags.RunTypes {
		emit.PhaseStart("types")
		typesOutput := filepath.Join(ctx.OutputPath, "types")
		buildInfoDir := typesBuildInfoDir(ctx.CacheRoot, filepath.Base(ctx.OutputPath), ctx.Project.Path)
		typesResult, err := build.RunTypes(bunBin, projectPath, typesOutput, buildInfoDir)
		if err != nil {
			emit.PhaseEnd("types", "failed")
			return "FAILED", nil, err
		}
		if len(typesResult.GeneratedFiles) > 0 {
			emit.Metric("type-declarations", len(typesResult.GeneratedFiles), "count")
		}
		if !typesResult.Success {
			for _, d := range typesResult.Diagnostics {
				emit.DiagnosticWithCode(d.Category, d.Message, d.File, d.Line, d.Column, d.Code)
			}
			if len(typesResult.Diagnostics) == 0 && typesResult.RawOutput != "" {
				emit.Log("error", typesResult.RawOutput)
			}
			emit.PhaseEnd("types", "failed")
			return "FAILED", nil, nil
		}
		emit.PhaseEnd("types", "success")
	}

	// Phase 4: Compile
	if flags.RunCompile {
		emit.PhaseStart("compile")
		compileOutput := filepath.Join(ctx.OutputPath, "compile")
		entrypoint := resolveCompileEntrypoint(ctx, genResult)
		if entrypoint == "" {
			emit.Log("debug", "No compile entrypoint found — skipping compile phase.")
			emit.PhaseEnd("compile", "success")
		} else {
			targets, err := compileTargets(ctx)
			if err != nil {
				emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, errs.CodeCompileError.String())
				emit.PhaseEnd("compile", "failed")
				return "FAILED", nil, nil
			}
			files, errors, err := build.RunCompile(bunBin, projectPath, compileOutput, entrypoint, targets)
			if err != nil {
				emit.PhaseEnd("compile", "failed")
				return "FAILED", nil, err
			}
			if len(errors) > 0 {
				for _, e := range errors {
					emit.DiagnosticWithCode("error", e, "", 0, 0, errs.CodeCompileError.String())
				}
				emit.PhaseEnd("compile", "failed")
				return "FAILED", nil, nil
			}
			emit.Metric("compiled-executables", len(files), "count")
			emit.PhaseEnd("compile", "success")
		}
	}

	return "OK", nil, nil
}

func runBuildTranspile(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if len(ctx.SelectedProjects) > 0 {
		return runBuildTranspileBatch(ctx)
	}
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	emit.PhaseStart("transpile")
	libOutput := filepath.Join(ctx.OutputPath, "lib")
	files, errors, err := build.RunTranspile(bunBin, projectPath, libOutput, build.TranspileParams{
		Target:    ctx.Params.String("target", "bun"),
		Bundle:    ctx.Params.String("bundle", "none"),
		Sourcemap: ctx.Params.String("sourcemap", "external"),
		Minify:    ctx.Params.Bool("minify", true),
		Splitting: ctx.Params.Bool("splitting", true),
		Metafile:  ctx.Params.Bool("metafile", false),
	})
	if err != nil {
		emit.PhaseEnd("transpile", "failed")
		return "FAILED", nil, err
	}
	if len(errors) > 0 {
		for _, e := range errors {
			emit.DiagnosticWithCode("error", e, "", 0, 0, errs.CodeTranspileError.String())
		}
		emit.PhaseEnd("transpile", "failed")
		return "FAILED", nil, nil
	}
	emit.Metric("transpiled-files", len(files), "count")
	emit.PhaseEnd("transpile", "success")
	return "OK", nil, nil
}

func runBuildTypes(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if len(ctx.SelectedProjects) > 0 {
		return runBuildTypesBatch(ctx)
	}
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	emit.PhaseStart("types")
	typesOutput := filepath.Join(ctx.OutputPath, "types")
	buildInfoDir := typesBuildInfoDir(ctx.CacheRoot, filepath.Base(ctx.OutputPath), ctx.Project.Path)
	typesResult, err := build.RunTypes(bunBin, projectPath, typesOutput, buildInfoDir)
	if err != nil {
		emit.PhaseEnd("types", "failed")
		return "FAILED", nil, err
	}
	if len(typesResult.GeneratedFiles) > 0 {
		emit.Metric("type-declarations", len(typesResult.GeneratedFiles), "count")
	}
	if !typesResult.Success {
		for _, d := range typesResult.Diagnostics {
			emit.DiagnosticWithCode(d.Category, d.Message, d.File, d.Line, d.Column, d.Code)
		}
		if len(typesResult.Diagnostics) == 0 && typesResult.RawOutput != "" {
			emit.Log("error", typesResult.RawOutput)
		}
		emit.PhaseEnd("types", "failed")
		return "FAILED", nil, nil
	}
	emit.PhaseEnd("types", "success")
	return "OK", nil, nil
}

// typesBuildInfoDir derives the persistent warm directory that backs incremental
// `tsc` type-declaration builds for one project. It lives under ctx.CacheRoot —
// mutable per-workspace scratch that survives across cache misses (unlike the
// content-addressed OutputPath, which the scheduler clears each miss) — so the
// .tsbuildinfo and warm declaration output persist and a miss re-type-checks
// only the changed delta.
//
// The path is keyed by the command discriminator (the output-dir base: "build"
// for a combined build vs "build-types" for the solo/batched types command) and
// the project path, so two commands that can target the same project
// concurrently never share — and corrupt — one .tsbuildinfo.
//
// Returns "" when cacheRoot is empty (e.g. unit tests with no CacheRoot), which
// makes RunTypes fall back to cold, non-incremental compilation.
func typesBuildInfoDir(cacheRoot, cmd, projectPath string) string {
	if cacheRoot == "" {
		return ""
	}
	return filepath.Join(cacheRoot, "ts-types", cmd, projectPath)
}

func runBuildCompile(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	if len(ctx.SelectedProjects) > 0 {
		return runBuildCompileBatch(ctx)
	}
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	genResult := loadGenerateResult(projectPath)
	entrypoint := resolveCompileEntrypoint(ctx, genResult)
	if entrypoint == "" {
		return "OK", nil, nil
	}

	targets, err := compileTargets(ctx)
	if err != nil {
		return "FAILED", nil, err
	}

	emit.PhaseStart("compile")
	compileOutput := filepath.Join(ctx.OutputPath, "compile")
	files, errors, err := build.RunCompile(bunBin, projectPath, compileOutput, entrypoint, targets)
	if err != nil {
		emit.PhaseEnd("compile", "failed")
		return "FAILED", nil, err
	}
	if len(errors) > 0 {
		// The compiler's own words are the verdict's only explanation: a failed
		// task with no diagnostic leaves CI (and its failure digest) naming a
		// key with no cause. Emit every collected error before failing, exactly
		// as the batched path reports per-project rows.
		for _, message := range errors {
			emit.Diagnostic("error", message, "", 0)
		}
		emit.PhaseEnd("compile", "failed")
		return "FAILED", nil, nil
	}
	emit.Metric("compiled-executables", len(files), "count")
	emit.PhaseEnd("compile", "success")
	return "OK", nil, nil
}

// compileTargets resolves the bun targets this invocation compiles, from
// plan-time parameters ONLY.
//
// `package --docker` schedules its compile step for one reason — the image —
// and an image carries one executable, so the compile is bound to the image's
// `--platform` and the other three targets are never built. `build --compile`
// is bound to no channel and keeps the full matrix.
//
// The docker intent is read as a PARAMETER (`--docker`, or the project's
// `options.package.docker`) rather than from the project's publish declaration,
// because the parameter is what the task's cache key can see and what a batched
// invocation shares across its projects. The pipeline's own `if: params.docker`
// gate resolves declared publish channels too, so a project that declares the
// channel without the option still packages an image — it just keeps the full
// target set, which is the safe direction: extra targets, never a missing one.
func compileTargets(ctx *pctx.Context) ([]string, error) {
	return build.ResolveCompileTargets(build.CompileTargetRequest{
		CompileTarget:  ctx.Params.String("compile-target", "compileTarget"),
		Docker:         ctx.Params.Bool("docker", false),
		DockerPlatform: ctx.Params.String("platform"),
	})
}

// codeOr returns the structured diagnostic code carried by err, falling back to
// fallback when err carries no code (e.g. a plain fmt.Errorf). This lets emit
// sites thread the code through the error value instead of re-typing a literal.
func codeOr(err error, fallback errs.Code) errs.Code {
	if c := errs.CodeOf(err); c != errs.CodeUnknown {
		return c
	}
	return fallback
}

// resolveCompileEntrypoint picks the module the compile step bundles.
//
// The package.json facts it consults — `exports["./serve"]`, `bin`, `main` —
// arrive in this extension's OWN namespaced metadata block since an earlier
// migration (`project.metadata["@putnami/typescript"]`, published by this
// extension's workspace probe). Before that they were npm-shaped members on the
// job context itself, stamped there by a core that had parsed package.json; core
// no longer parses it, and no longer carries fields only one language can mean
// anything by.
func resolveCompileEntrypoint(ctx *pctx.Context, genResult *build.GenerateResult) string {
	if genResult != nil {
		if ep, ok := genResult.Exports["bundled-serve"]; ok {
			return ep
		}
	}

	metadata := workspaceMetadataOf(ctx)

	// Try ./serve from package.json
	serveExport := project.ResolveExportPath(metadata.Exports, "./serve")
	if serveExport != "" {
		return serveExport
	}

	// Try bin
	if binStr := metadata.BinString(); binStr != "" {
		return binStr
	}

	// Try main
	return metadata.Main
}
