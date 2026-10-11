package infraagg

import (
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
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
//
// It reports OK only when the run wrote the manifest and kept the runtime
// defaults sidecar in step with it, so the files on disk are the ones the
// inputs determine. Every other run reports SKIP and never fails:
//
//   - a project that is not a workload, which writes nothing;
//   - a workload that declares nothing, whose run removes an earlier manifest;
//   - a failed write or removal (Result.Err), which can leave an earlier
//     manifest or sidecar in place.
//
// A language that caches the task declares the manifest as a required output,
// so a SKIP is never cached and the next run executes again: it removes or
// rewrites the files itself. A failed write does not fail the task, for the
// same reason a finding does not.
func Job(opts Options) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		emit.PhaseStart(PhaseName)
		result := Aggregate(ctx, opts)
		reportDiagnostics(emit, result.Diagnostics)

		switch {
		case result.Err != nil:
			emit.PhaseEnd(PhaseName, "failed")
			return "SKIP", result.Data(), nil
		case result.Outcome != OutcomeEmitted:
			emit.PhaseEnd(PhaseName, "skipped")
			return "SKIP", result.Data(), nil
		}
		emit.PhaseEnd(PhaseName, "success")
		emit.Artifact("infra-requirements", "requirements.json", "manifest", result.ManifestPath)
		return "OK", result.Data(), nil
	}
}

// DeploymentPhaseName is the phase every language's deployment declaration
// task reports under.
const DeploymentPhaseName = "deployment"

// DeploymentJob adapts Deployment to an extension job entry point, the whole
// body of a language's deployment package step, with the language supplying
// only its runtime compatibility:
//
//	"package-deployment": infraagg.DeploymentJob(infraagg.Options{RuntimeCompatibility: noHTTP2})
//
// It reports OK when it wrote the declaration, and SKIP for a project that is
// not a workload and for a withheld aggregate, whose findings it reports as
// warnings, exactly as Job does. A step that writes no declaration does not
// fail: the publisher that needs the declaration refuses its absence. A failed
// write or removal fails the task, because it can leave an earlier declaration
// in place.
func DeploymentJob(opts Options) cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		emit.PhaseStart(DeploymentPhaseName)
		result, err := Deployment(ctx, opts)
		reportDiagnosticsWithPrefix(emit, "deployment declaration: ", result.Diagnostics)
		if err != nil {
			emit.PhaseEnd(DeploymentPhaseName, "failed")
			return "FAILED", result.Data(), err
		}
		if result.Outcome != OutcomeEmitted {
			emit.PhaseEnd(DeploymentPhaseName, "skipped")
			return "SKIP", result.Data(), nil
		}
		emit.PhaseEnd(DeploymentPhaseName, "success")
		emit.Artifact("deployment-declaration", infra.DeploymentFilename, "manifest", result.ManifestPath)
		return "OK", result.Data(), nil
	}
}

// reportDiagnostics forwards each finding, prefixed so a reader knows which
// subsystem produced it and carrying the protocol's own severity and code in
// the message (diag.Diagnostic.String()).
func reportDiagnostics(emit *jsonl.Emitter, diags []diag.Diagnostic) {
	reportDiagnosticsWithPrefix(emit, "infra requirements: ", diags)
}

func reportDiagnosticsWithPrefix(emit *jsonl.Emitter, prefix string, diags []diag.Diagnostic) {
	for _, d := range diags {
		emit.Warn(prefix + d.String())
	}
}
