package dbtestenv

import (
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// PhaseName is the phase both tasks report under, so a build's live display
// names the same work the same way whichever language extension owns the
// pipeline.
const PhaseName = "test-env"

// UpJob is the producer entry point a language extension registers directly:
//
//	"test-env-up": dbtestenv.UpJob()
//
// The step it backs declares the two invocation-scoped outputs (the sensitive
// bindings artifact and the non-secret lease) and sets cache:false, which the
// task contract requires of any task with an invocation-scoped output.
//
// FINDINGS ARE WARNINGS, ALWAYS, and the task never FAILS. A test environment
// that could not be provisioned is reported by the TEST task — it fails under
// `require` and skips its live cases otherwise — and failing setup as well would
// block a project's unit tests on a docker hiccup while reporting the same
// problem twice with the less useful message. That is also what the CLI planner
// this replaced did: its findings printed after the run and never touched the
// exit code.
func UpJob() cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		return runPhase(ctx, emit, Up)
	}
}

// DownJob is the finalizer entry point:
//
//	"test-env-down": dbtestenv.DownJob()
//
// It runs with `runOn: "finally"` and an explicit `finalizes` relation naming
// the producer and the complete consumer frontier, so the orchestrator runs it
// exactly once whenever the producer STARTED — consumer failure and
// cancellation included — and never when the producer never started.
func DownJob() cli.JobFunc {
	return func(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
		return runPhase(ctx, emit, Down)
	}
}

// runPhase is the shared task body: one phase, the findings as warnings, and
// ALWAYS a successful status.
//
// "The producer succeeded" is deliberately not "a database was provisioned".
// The orchestrator reads a producer's terminal status as the invocation
// relation's setup verdict: anything other than success marks the relation
// setupFailed, and every listed consumer is then skipped with
// `sensitive.setup_failed` (scheduler.shouldSkip). A unit-only project — whose
// closure declares no database and for which the correct answer is to provision
// nothing — must not have its tests skipped for it, so deciding that nothing
// needed provisioning is a SUCCESSFUL setup. The outcome word in the result data
// is what distinguishes the cases, and the phase end reports "skipped" so the
// live display still says which projects did no work.
func runPhase(
	ctx *pctx.Context,
	emit *jsonl.Emitter,
	action func(*pctx.Context, Provider) Result,
) (string, map[string]any, error) {
	emit.PhaseStart(PhaseName)
	// SelectProvider reads the environment only, so both halves of the relation
	// pick the same provider and a finalizer never acts on another provider's
	// lease.
	result := action(ctx, SelectProvider())
	reportDiagnostics(emit, result.Diagnostics)

	phase := "skipped"
	if didWork(result.Outcome) {
		phase = "success"
	}
	emit.PhaseEnd(PhaseName, phase)
	return "OK", result.Data(), nil
}

// didWork reports whether the outcome represents work performed, as opposed to
// a gate that (correctly) did nothing.
func didWork(outcome Outcome) bool {
	switch outcome {
	case OutcomeProvisioned, OutcomeTornDown, OutcomeRetained:
		return true
	default:
		return false
	}
}

// reportDiagnostics forwards each finding, prefixed so a reader knows which
// subsystem produced it and carrying the protocol's own severity and code in the
// message (diag.Diagnostic.String()).
//
// Every finding this package produces is credential-free by construction: the
// only value that could carry one is the synthesized binding, and no diagnostic
// interpolates it.
func reportDiagnostics(emit *jsonl.Emitter, diags []diag.Diagnostic) {
	for _, d := range diags {
		emit.Warn("test environment: " + d.String())
	}
}
