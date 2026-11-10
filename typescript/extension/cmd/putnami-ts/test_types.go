package main

import (
	"path/filepath"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/testjob"
)

// runTestTypes type-checks every TypeScript file of the project, tests
// included, without emitting. Bun strips types when it runs a test and the
// types phase reads only src, bin and .gen, so this step is the one that
// fails on a type error, or an unused @ts-expect-error, in a test. A project
// whose test runner is Vitest or Jest is checked with the ambient types its
// tsconfig names, where the runner's globals are declared.
func runTestTypes(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	projectPath := filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	runner, err := testjob.ResolveRunner(projectPath)
	if err != nil {
		return "FAILED", nil, err
	}

	emit.PhaseStart("typecheck")
	result, err := build.RunTypeCheck(bunBin, projectPath, typesBuildInfoDir(ctx.CacheRoot, "test-types", ctx.Project.Path), runner != testjob.RunnerBun)
	if err != nil {
		emit.PhaseEnd("typecheck", "failed")
		return "FAILED", nil, err
	}
	if !result.Success {
		for _, d := range result.Diagnostics {
			emit.DiagnosticWithCode(d.Category, d.Message, d.File, d.Line, d.Column, d.Code)
		}
		if len(result.Diagnostics) == 0 && result.RawOutput != "" {
			emit.Log("error", result.RawOutput)
		}
		emit.PhaseEnd("typecheck", "failed")
		return "FAILED", nil, nil
	}
	emit.PhaseEnd("typecheck", "success")
	return "OK", nil, nil
}
