package main

import (
	"errors"
	"os"
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
)

// The data output ports build-tsc reports its outputs on, project-relative.
// The manifest declares a pathFrom output of each name; the manifest-contract
// test pins them.
const (
	TscOutDirPort         = "outDir"
	TscDeclarationDirPort = "declarationDir"
	TscBuildInfoPort      = "buildInfo"
)

// runBuildTsc compiles a project with the tsc its build script runs, into
// the outDir its tsconfig names, where the packages that depend on it
// resolve it. A project that compiles with Bun reports no output, so its
// outputs are recorded empty and the step changes nothing.
func runBuildTsc(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	compiler, err := build.ResolveCompiler(ctx.Params.String("compiler"), projectPath)
	if err != nil {
		return tscRefused(emit, err)
	}
	if compiler != build.CompilerTsc {
		emit.Info("the project compiles with Bun: its build script does not run tsc alone")
		return "OK", map[string]any{}, nil
	}

	plan, err := build.PlanTsc(projectPath, ctx.WorkspaceRoot)
	if err != nil {
		return tscRefused(emit, err)
	}
	if err := build.CheckTscOutputs(projectPath, plan); err != nil {
		return tscRefused(emit, err)
	}
	if err := build.CleanTscOutputs(plan); err != nil {
		return "FAILED", nil, err
	}
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	emit.PhaseStart("tsc")
	result, err := build.RunTsc(bunBin, projectPath, plan)
	if err != nil {
		emit.PhaseEnd("tsc", "failed")
		return "FAILED", nil, err
	}
	if !result.Success {
		for _, d := range result.Diagnostics {
			emit.DiagnosticWithCode(d.Category, d.Message, d.File, d.Line, d.Column, d.Code)
		}
		if len(result.Diagnostics) == 0 && result.RawOutput != "" {
			emit.Log("error", result.RawOutput)
		}
		emit.PhaseEnd("tsc", "failed")
		return "FAILED", nil, nil
	}
	if info, err := os.Stat(plan.OutDir); err != nil || !info.IsDir() {
		emit.PhaseEnd("tsc", "failed")
		emit.DiagnosticWithCode("error", "tsc emitted nothing into "+relPath(projectPath, plan.OutDir)+", the outDir of "+relPath(projectPath, plan.Config), relPath(projectPath, plan.Config), 0, 0, build.CodeTscEmittedNothing)
		return "FAILED", nil, nil
	}
	emit.PhaseEnd("tsc", "success")

	data := map[string]any{TscOutDirPort: relPath(projectPath, plan.OutDir)}
	for port, path := range map[string]string{TscDeclarationDirPort: plan.DeclarationDir, TscBuildInfoPort: plan.BuildInfo} {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			data[port] = relPath(projectPath, path)
		}
	}
	return "OK", data, nil
}

// tscRefused reports a refusal as a diagnostic with its code, and any other
// error as the job's error.
func tscRefused(emit *jsonl.Emitter, err error) (string, map[string]any, error) {
	var refusal *build.TscError
	if errors.As(err, &refusal) {
		emit.DiagnosticWithCode("error", refusal.Message, refusal.File, 0, 0, refusal.Code)
		return "FAILED", nil, nil
	}
	return "FAILED", nil, err
}

func relPath(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// compilesWithTsc reports whether build-tsc compiles the project. The Bun
// transpile and types steps then compile nothing for it: under auto they are
// planned for every project, and only the build script tells them apart.
func compilesWithTsc(ctx *pctx.Context, projectPath string) bool {
	compiler, err := build.ResolveCompiler(ctx.Params.String("compiler"), projectPath)
	return err == nil && compiler == build.CompilerTsc
}
