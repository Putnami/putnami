package cli

import (
	"context"
	"maps"
	"os"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/doctor"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/telemetry"
)

// This file is the TERMINAL ADAPTER over engine.Run: it
// translates a parsed command line into an engine.Request, supplies the two
// terminal-only seams (the telemetry observer and the command-scoped flag
// validation), and maps the engine's SessionResult onto a process exit code.
//
// Everything the run itself does — workspace and extension resolution, project
// selection, planning, the production preflight gate, lifecycle hooks, cache
// setup, scheduling, session recording, successful-run markers, profiling,
// opportunistic GC, exit-code derivation — lives in internal/engine.

// drainTelemetry is replaceable by tests so the run path can prove that an
// in-flight telemetry drain never delays command completion.
var drainTelemetry = telemetry.DrainContext

// runJobCommands runs the selected job commands through the engine. A hosted
// run reads the remote cache through hostedCache, whose provider the first-use
// bootstrap may have started before its first repository code.
func (a *App) runJobCommands(ctx context.Context, parsed *ParsedArgs, cfg *wsproto.Config, wsRoot string, tc *telemetry.Client, interactive bool, hostedCache *engine.HostedRemoteCache) int {
	req := terminalRequest(parsed, cfg, wsRoot, tc, interactive)
	req.HostedRemoteCache = hostedCache
	result, _ := engine.New().Run(ctx, req, nil)
	return result.ExitCode
}

// terminalRequest is the whole terminal→engine mapping, in one named place so
// another adapter's mapping can be compared against it rather than against a
// copy. An earlier change pulled it out of runJobCommands for exactly that: the
// MCP equivalence test builds its terminal side here, so "terminal and MCP agree"
// is a claim about the shipping mapping and not about a restatement of it.
func terminalRequest(parsed *ParsedArgs, cfg *wsproto.Config, wsRoot string, tc *telemetry.Client, interactive bool) engine.Request {
	req := engine.Request{
		WorkspaceRoot: wsRoot,
		Config:        cfg,
		Commands:      parsed.Commands,
		Global:        parsed.Global,
		// The params a job sees, and every cache key and run marker derived from
		// them, stay a pure function of the raw job args — with ONE deliberate,
		// named synthesis: applyPublishDryRun below, mirroring the alias
		// adapter's buildExtensionCommandParams. The engine forwards this map,
		// it never rebuilds it.
		CommandParams: buildCommandParams(parsed.RawJobArgs),
		// Lifecycle hooks run for the terminal path only, for the same reason the
		// observer does: a later adapter must opt in deliberately.
		Hooks:                cfg.Hooks,
		Observer:             newSessionObserver(tc, parsed.OriginalArgs, interactive),
		ValidateCommandFlags: commandFlagValidator(parsed),
		// The production doctor gate. Unlike the two seams above it is NOT
		// terminal-only: every adapter wires the same evaluator, because a build
		// that executes jobs must be gated whichever surface asked for it. It is
		// injected only so the engine stops importing internal/commands;
		// a nil gate fails a production run closed.
		Preflight: doctor.DoctorPreflight,
	}
	applyPublishDryRun(&req, parsed)
	return req
}

// applyPublishDryRun opts `putnami publish --dry-run` out of the plan-only
// preview so the publish jobs execute in their own declared dry-run mode:
// every publisher prints the artifact set it WOULD push (npm, Go module,
// docker) and the published-artifacts summary renders it, so executing is
// strictly more informative than the DAG preview — with zero registry side
// effects. A dry run only reads: each publisher asks its registry whether the
// member already exists at the planned version, and the engine fails the run
// on the answers once every job has ended (jobs.MemberProbeReport).
//
// Three deliberate scope decisions:
//   - Exactly the single-command form. A mixed list (`build,publish`) keeps
//     the preview: executing build for real is not what --dry-run promised.
//   - The synthesized param goes INTO cache keys and run-marker keys (no
//     RunMarkerParams override) — the OPPOSITE of the alias adapter's choice,
//     re-derived per adapter: markers key on
//     (commands, params), and a dry-run publish must never satisfy the marker
//     or cache lookup a real publish would have written. The alias keeps its
//     markers clean because its synthesized params would fragment the same
//     logical command; here the fragmentation IS the safety.
//   - A task that never declared a dry-run flag cannot be trusted to
//     interpret the param; the engine excludes side-effecting jobs without
//     the declaration from the plan (dropUndryableSideEffectJobs), so nothing
//     publishes for real under --dry-run.
func applyPublishDryRun(req *engine.Request, parsed *ParsedArgs) {
	if !parsed.Global.DryRun || len(parsed.Commands) != 1 || parsed.Commands[0] != "publish" {
		return
	}
	params := maps.Clone(req.CommandParams)
	params["dry-run"] = true
	req.CommandParams = params
	req.ExecutesUnderDryRun = true
}

// commandFlagValidator returns the command-scoped half of the single
// parse-and-validation pass. It runs inside the engine, after
// extension resolution and before selection, rather than in ParseArgs, because
// only extension discovery knows which flags the selected tasks declare.
//
// Two different verdicts, both settled in the epic's refinement:
//   - a flag two selected tasks declare INCOMPATIBLY cannot be bound at all,
//     so it is a hard usage error naming the tasks;
//   - a flag NO task declares is a deprecation warning for one minor version,
//     because manifests ship on their own cadence and hard-failing them is a
//     known ecosystem-break class.
//
// Neither verdict rewrites RawJobArgs, so commandParams — and every cache key
// and run marker derived from it — is unchanged.
func commandFlagValidator(parsed *ParsedArgs) func(*extension.DiscoveryResult, GlobalFlags) error {
	return func(discovered *extension.DiscoveryResult, resolved GlobalFlags) error {
		jobMap := extension.BuildJobMap(discovered.Extensions)
		if err := conflictingTaskFlags(parsed.Commands, jobMap, parsed.RawJobArgs); err != nil {
			printCommandError(os.Stderr, err)
			return err
		}
		// The resolved flags, not the parsed ones: PUTNAMI_QUIET must suppress
		// these notices exactly as it did when this ran inline after
		// applyEnvOverrides.
		printParseWarnings(os.Stderr, resolved,
			undeclaredFlagWarnings(strings.Join(parsed.Commands, ","), parsed.RawJobArgs, declaredTaskFlags(parsed.Commands, jobMap)))
		return nil
	}
}

// sessionObserver is the terminal adapter's implementation of the engine's
// telemetry seam (ADR 0001 §4). It is the ONLY observer in the tree: MCP, watch,
// and lifecycle adapters pass nil, so routing them through the engine can never
// start reporting sessions from a surface that never reported before (the
// "never send before notice" rule).
//
// TrackSessionEnd deliberately does NOT live here — it stays once per process in
// runTerminalSession, which re-reads persisted consent first.
type sessionObserver struct {
	client      *telemetry.Client
	args        []string
	interactive bool
}

// newSessionObserver returns the terminal observer, or a nil Observer when there
// is no telemetry client to report to. Returning the interface's nil value (not
// a typed nil) matters: the engine's no-op path is `observer == nil`.
func newSessionObserver(tc *telemetry.Client, args []string, interactive bool) engine.Observer {
	if tc == nil {
		return nil
	}
	return &sessionObserver{client: tc, args: args, interactive: interactive}
}

// RunPlanned records session:start once the plan exists. Earlier exits
// deliberately record only session:end because these plan-dependent counts do
// not exist.
func (o *sessionObserver) RunPlanned(ctx context.Context, plan engine.RunPlan) {
	startPreviousTelemetryDrain(ctx, o.client.CanDrainPrevious())
	o.client.TrackSessionStart(telemetry.SessionStart{
		Commands:    plan.Commands,
		Projects:    plan.Projects,
		Jobs:        plan.Jobs,
		Flags:       telemetryFlagPresence(o.args),
		Interactive: o.interactive,
	})
}

// startPreviousTelemetryDrain starts a best-effort upload of events buffered
// by an earlier run. It is intentionally not joined: the sender has its own
// two-second timeout, and process exit abandons it rather than delaying the
// command's result. The current session flushes locally and is eligible to
// drain on the next run.
func startPreviousTelemetryDrain(ctx context.Context, allowed bool) {
	if !allowed {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		default:
			drainTelemetry(ctx)
		}
	}()
}

func telemetryFlagPresence(args []string) telemetry.FlagPresence {
	var presence telemetry.FlagPresence
	for _, arg := range args {
		flag := strings.SplitN(arg, "=", 2)[0]
		switch flag {
		case "--impacted":
			presence.Impacted = true
		case "--coverage":
			presence.Coverage = true
		case "--output", "--json":
			presence.Output = true
		case "--no-cache":
			presence.NoCache = true
		case "--projects":
			presence.Projects = true
		case "--watch", "-w":
			presence.Watch = true
		}
	}
	return presence
}
