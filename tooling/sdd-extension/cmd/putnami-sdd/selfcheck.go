package main

import (
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// runSelfcheck reports what the extension IS and what the orchestrator handed
// it, and nothing else.
//
// It exists for two reasons, one of them structural. The extension protocol
// requires a manifest to declare at least one command or MCP tool
// (extension.ValidateManifest), so a manifest with a prepared runtime and
// nothing to run is not a legal document — the scaffold needs one command.
// Given that, the useful one is the check that the wiring works end to end:
// the runtime was prepared, core spawned it, and the job context arrived with
// the members the SDD engines will read.
//
// The one it reports in most detail is `selection`. SDD verdicts
// depend on it: told it ran over three projects, a validator cannot tell a
// deliberate `--projects` narrowing from a whole-workspace run of a
// three-project tree, and the two license different conclusions. Reporting it
// here means a workspace whose CLI does not send it is visible NOW, from one
// command, instead of as a wrong verdict later.
//
// It reads nothing from disk and mutates nothing, which is what lets the task
// declare no effects at all.
func runSelfcheck(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	report := selfcheckReport(ctx)
	emit.Info("@putnami/sdd runtime is wired: " + describeSelection(ctx))
	return "OK", report, nil
}

// selfcheckReport is the structured half of the self-check: the identity of the
// running binary plus the job-context members the SDD engines depend on.
//
// A nil Selection is reported as a present `selection: null` rather than an
// omitted key, because "the CLI resolved nothing" is the answer a consumer must
// act on, and an absent key is indistinguishable from a report that forgot to
// look.
func selfcheckReport(ctx *pctx.Context) map[string]any {
	report := map[string]any{
		"extension":       extensionName,
		"runtimeVersion":  runtimeVersion,
		"protocolVersion": 0,
		"workspaceRoot":   "",
		"project":         "",
		"selection":       nil,
	}
	if ctx == nil {
		return report
	}

	report["protocolVersion"] = ctx.ProtocolVersion
	report["workspaceRoot"] = ctx.WorkspaceRoot
	report["project"] = ctx.Project.Name
	if ctx.Selection == nil {
		return report
	}
	report["selection"] = map[string]any{
		"mode":           ctx.Selection.Mode,
		"scoped":         ctx.Selection.Scoped,
		"baseline":       ctx.Selection.Baseline,
		"baselineSource": ctx.Selection.BaselineSource,
		"projects":       ctx.Selection.ProjectIDs,
		"emptyImpact":    ctx.Selection.EmptyImpact,
	}
	return report
}

// describeSelection renders the selection as the one human line the command
// prints. "unresolved" is deliberately not spelled as a mode: it is the absence
// of one, and naming it like a mode would let it be read as a fourth member of
// the closed vocabulary.
func describeSelection(ctx *pctx.Context) string {
	if ctx == nil || ctx.Selection == nil {
		return "selection unresolved (the CLI sent no selection block)"
	}
	line := "selection mode=" + ctx.Selection.Mode
	if ctx.Selection.Baseline != "" {
		line += " baseline=" + ctx.Selection.Baseline
	}
	return line
}
