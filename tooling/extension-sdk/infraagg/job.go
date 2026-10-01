package infraagg

import (
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// PhaseName is the phase every language's aggregation task reports under, so a
// build's live display names the same work the same way whichever extension
// owns the pipeline.
const PhaseName = "infra"

// Job adapts Aggregate to an extension job entry point: the whole task body,
// shared, with the language supplying only its runtime compatibility.
//
// A language extension registers it directly:
//
//	"build-infra": infraagg.Job(infraagg.Options{RuntimeCompatibility: noHTTP2})
//
// FINDINGS ARE WARNINGS, ALWAYS. Aggregation diagnostics are advisory by
// design: a build that produced binaries must not be failed because a library
// declared a conflicting bucket retention, and the CLI gate this replaced said
// the same thing by printing its findings after the run without touching the
// exit code. Emitting them as warnings keeps that contract while putting each
// finding on the task that produced it — which is strictly better than the
// end-of-run block, because a finding now names the workload whose build
// surfaced it.
func Job(opts Options) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		emit.PhaseStart(PhaseName)
		result := Aggregate(ctx, opts)
		reportDiagnostics(emit, result.Diagnostics)

		if result.Outcome == OutcomeSkipped {
			emit.PhaseEnd(PhaseName, "skipped")
			return "SKIP", result.Data(), nil
		}
		emit.PhaseEnd(PhaseName, "success")
		if result.Outcome == OutcomeEmitted {
			emit.Artifact("infra-requirements", "requirements.json", "manifest", result.ManifestPath)
		}
		return "OK", result.Data(), nil
	}
}

// reportDiagnostics forwards each finding, prefixed so a reader knows which
// subsystem produced it and carrying the protocol's own severity and code in
// the message (diag.Diagnostic.String()).
func reportDiagnostics(emit *jsonl.Emitter, diags []diag.Diagnostic) {
	for _, d := range diags {
		emit.Warn("infra requirements: " + d.String())
	}
}
