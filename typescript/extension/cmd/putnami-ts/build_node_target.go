package main

import (
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/errs"
)

// checkBunAPIsOnNode runs the node-target check after a transpile built with
// params, and returns its error diagnostics: one typescript.bun_api_on_node
// diagnostic per module that needs Bun, or one TRANSPILE_ERROR diagnostic when
// the check could not read the graph. A check that cannot read the graph
// fails the build: a node build is never reported loadable unread. A
// transpile for any other target returns nothing.
func checkBunAPIsOnNode(bunBin, projectPath string, params build.TranspileParams) []buildBatchDiagnostic {
	if !build.NodeTargetChecked(params) {
		return nil
	}
	uses, err := checkBunAPIsOnNodeFunc(bunBin, projectPath)
	if err != nil {
		return []buildBatchDiagnostic{{
			Category:    errs.CodeTranspileError.String(),
			Severity:    "error",
			Description: err.Error(),
		}}
	}
	diagnostics := make([]buildBatchDiagnostic, 0, len(uses))
	for _, use := range uses {
		diagnostic := buildBatchDiagnostic{
			Category:    errs.CodeBunAPIOnNode.String(),
			Severity:    "error",
			Description: use.Message(),
		}
		// A module of the project is located in the project; a dependency is
		// named, with its path from the project, in the message only.
		if !strings.HasPrefix(use.Module, "../") {
			diagnostic.File = use.Module
			if len(use.Globals) > 0 {
				diagnostic.Line, diagnostic.Column = use.Globals[0].Line, use.Globals[0].Column
			}
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	return diagnostics
}

// checkBunAPIsOnNodeFunc is the node-target check; tests replace it.
var checkBunAPIsOnNodeFunc = build.CheckBunAPIsOnNode

// transpileParams reads the transpile parameters of a build invocation, the
// same for the solo build, the transpile command and its batch.
func transpileParams(ctx *pctx.Context) build.TranspileParams {
	return build.TranspileParams{
		Target:    ctx.Params.String("target", "bun"),
		Bundle:    ctx.Params.String("bundle", "none"),
		Sourcemap: ctx.Params.String("sourcemap", "external"),
		Minify:    ctx.Params.Bool("minify", true),
		Splitting: ctx.Params.Bool("splitting", true),
		Metafile:  ctx.Params.Bool("metafile", false),
	}
}

// emitDiagnostics emits each diagnostic as a solo build reports it, and says
// whether there was any.
func emitDiagnostics(emit *jsonl.Emitter, diagnostics []buildBatchDiagnostic) bool {
	for _, d := range diagnostics {
		emit.DiagnosticWithCode(d.Severity, d.Description, d.File, d.Line, d.Column, d.Category)
	}
	return len(diagnostics) > 0
}
