// Package engine owns the job-run lifecycle: one workspace/config snapshot, one
// extension resolution, project selection, planning, the production preflight
// gate, lifecycle hooks, cache setup, scheduling, session recording,
// successful-run markers, profiling, and cleanup.
//
// It is the single composer named by ADR 0001 §3 (tooling/cli/doc/adr): the
// terminal CLI, extension aliases, MCP, watch, and lifecycle jobs are adapters
// over Engine.Run rather than five hand-assembled copies of the same lifecycle.
// A staged migration moved the terminal path, the extension aliases, MCP, the
// workspace lifecycle jobs, and watch onto Engine.Run one at a time — which
// leaves exactly one scheduler construction and one planner call in the tree,
// both in this package. Several stages stay exported until a later cleanup
// privatizes them.
//
// Four seams are deliberate and load-bearing:
//
//   - Observer (ADR 0001 §4) is the telemetry seam. The engine NEVER reports
//     telemetry itself; it hands an observation to an injected observer, and only
//     the terminal adapter supplies one. A nil observer is a total no-op, which is
//     what keeps MCP/watch/lifecycle from silently synthesizing user sessions when
//     they route through the engine (the rule: never send before notice).
//     The structural test in this package fails if internal/telemetry is ever
//     imported here.
//   - EventSink is the surface seam: the renderer that receives the run's task
//     events. A nil sink makes the engine build the default renderer for
//     Request.Global, at exactly the point the terminal path built it before.
//   - Request.Stdout is the human-notice seam, added for MCP: every notice
//     the engine itself prints ("No jobs matched", the plan table, the
//     auto-selection line) goes there instead of straight to os.Stdout, because
//     the MCP adapter's stdout is a JSON-RPC frame stream that any stray byte
//     corrupts. seam_test.go fails if a stage reaches for os.Stdout again.
//   - Request.Preflight is the production doctor gate, added as a seam. The
//     engine decides WHEN the gate runs (production
//     profile, real execution, between plan and execute); the adapter supplies
//     WHAT runs — always commands.DoctorPreflight — so this package no longer
//     imports internal/commands at all. It is the one seam where nil is not a
//     no-op: an unwired gate fails a production run closed.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	protocoljob "go.putnami.dev/protocol/job"
	runner "go.putnami.dev/protocol/runner"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/hooks"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/sessionreporter"
	"go.putnami.dev/tooling/cli/internal/specgate"
)

// Exit codes. The taxonomy is owned by go.putnami.dev/protocol/cli; these
// package-local names alias the canonical constants so the engine and the CLI
// shell cannot drift apart.
const (
	ExitSuccess        = protocolcli.ExitSuccess
	ExitError          = protocolcli.ExitFailure
	ExitUsage          = protocolcli.ExitUsage
	ExitSignalReceived = protocolcli.ExitSignal
)

// EventSink receives the run's task events. It is exactly the scheduler's
// renderer contract, so the adapters that already own a renderer (watch, MCP)
// can pass theirs unchanged. A nil sink makes the engine build the default
// renderer from Request.Global.
type EventSink = jobs.Renderer

// Observer receives the engine's run-lifecycle observations. It is the
// telemetry SEAM required by ADR 0001 §4, not telemetry itself: the engine
// hands the observation over and keeps no reporting code of its own.
//
// Only the terminal adapter supplies an observer. MCP, watch, and lifecycle
// adapters pass nil, and a nil observer is a total no-op — no allocation, no
// buffer write — so routing a new surface through the engine can never start
// reporting sessions as a side effect. Adding an observer to a non-terminal
// adapter requires a new ADR and an explicit consent review.
//
// It is deliberately NOT a subscriber of the session event stream
// (internal/sessionstream). RunPlanned fires before execute creates the session,
// and for runs that never record one — a --dry-run preview and the outer
// --watch/serve loop — so the stream has no record at this point; and a watch
// loop records one session per rebuild, where a stream subscriber would report
// one session:start per file save, the consent incident ADR 0001 §4 forbids.
type Observer interface {
	// RunPlanned is called once, after planning succeeds and the production
	// preflight gate has passed, immediately before execution starts. That is
	// where the terminal path recorded session:start before A3a, so the
	// observation point is unchanged.
	RunPlanned(ctx context.Context, plan RunPlan)
}

// RunPlan is what the engine knows about a run at observation time. It
// deliberately carries counts only: flag presence, TTY interactivity, and every
// other CLI-shaped detail belongs to the adapter that owns the observer.
type RunPlan struct {
	Commands []string
	Projects int
	Jobs     int
}

// notifyRunPlanned is the only place the engine touches an observer. A nil
// observer allocates nothing and calls nothing.
func notifyRunPlanned(ctx context.Context, observer Observer, plan func() RunPlan) {
	if observer == nil {
		return
	}
	observer.RunPlanned(ctx, plan())
}

// Request describes one run of the job lifecycle.
//
// Global is mutated during the run (project selection resolves --impacted into
// concrete IDs, applyEnvOverrides layers env/config onto the flags), which is
// why Run takes the Request by value: the caller's copy is never touched.
type Request struct {
	// WorkspaceRoot is the resolved workspace root; the engine loads the
	// workspace from it exactly once.
	WorkspaceRoot string
	// Config is the loaded workspace config.
	Config *wsproto.Config
	// Commands are the job commands to run, e.g. ["lint", "test", "build"].
	Commands []string
	// Global are the run-shaping flags.
	Global GlobalFlags
	// CommandParams are the job parameters passed to planning and scheduling.
	// The CLI derives them from the raw job args and the flags the selected
	// tasks declare (buildCommandParams); a param value's Go type is part of
	// every cache key, so the engine only ever forwards this map — it never
	// rebuilds or rewrites it.
	CommandParams map[string]any
	// RunMarkerParams key the persisted successful-run state (last-build SHA and
	// remote run markers). Nil means "same as CommandParams", which is the
	// terminal path. They are a separate field because the extension-alias path
	// synthesizes extra job params (dry-run, output) that must not change the
	// marker key those runs look up.
	RunMarkerParams map[string]any
	// PlanExtension, when non-nil, narrows planning to one extension selected by
	// stable identity. The engine rebinds that identity to the post-before-hook
	// discovery result, then overlays the alias-only inherited flags onto the
	// freshly discovered flat command. This keeps lifecycle hooks authoritative:
	// a hook that updates an extension manifest cannot be bypassed by a stale
	// pre-hook ExtensionDescription captured during CLI dispatch.
	//
	// The extension-alias adapter is the only caller. Name and Command identify
	// the owner and flat command; InheritedFlags carries only group/subcommand
	// layers, so the fresh command's own flags remain the base layer.
	PlanExtension *PlanExtensionSelection
	// PlanExtensions, when non-nil, is the extension set the run PLANS against,
	// replacing the discovered one. Everything else — project selection's remote
	// run-marker lookup and the remote cache provider — still sees the full
	// discovered set, because those resolve a provider rather than dispatch a
	// command.
	//
	// The watch adapter uses this to freeze the extension set selected by the
	// outer run for every iteration. Extension aliases use PlanExtension instead
	// because their owner must be rebound after before-hooks.
	PlanExtensions []*extension.ExtensionDescription
	// WorkspaceLifecycle marks a WORKSPACE LIFECYCLE run — `putnami install`'s
	// workspace-install pass and `putnami upgrade`'s deps-upgrade pass — rather
	// than a user build. It was added when the private composition
	// in internal/commands/deps.go (its own jobs.Plan + jobs.NewScheduler, its own
	// dedup filter, no cache, no session) became an adapter over this engine; the
	// lifecycle adapter in internal/cli is the only caller.
	//
	// It changes exactly four things, each of which reproduces what that deleted
	// composition did:
	//
	//  1. The plan is deduplicated to ONE job per extension. A workspace-level job
	//     restores the workspace once per PROVIDER, not once per project, so a
	//     20-project workspace runs one `bun install` and not twenty.
	//  2. Plan-time disable lists are ignored. The old composition planned with
	//     nil disable lists: a workspace that disables a job for its BUILDS must
	//     still get its dependencies installed, and honoring the list here would
	//     silently turn `putnami install` into a no-op.
	//  3. An empty selection or an empty plan ends the run as a success-shaped
	//     no-op (Session stays nil) instead of a usage error, and a contract
	//     mismatch is reported without aborting. A lifecycle run is what REPAIRS
	//     the state those guards describe — telling `putnami install` to "run
	//     `putnami install`" would brick recovery on exactly the workspaces that
	//     need it. The adapter owns the reporting, which is why it also owns
	//     Request.Stdout here.
	//  4. No persisted run state: no session file, no successful-run marker, no
	//     opportunistic cache GC. Installing dependencies is not a build, and a
	//     recorded install session would silently become what `putnami sessions`
	//     reports as the latest run.
	//
	// The build cache stays off through the ordinary flag (Global.NoCache), not
	// through this field: lifecycle jobs are not cacheable work and the adapter
	// states that where every other adapter states it.
	WorkspaceLifecycle bool
	// CacheVerification selects the observation-only cache audit execution
	// policy. The adapter supplies an isolated store, while the engine keeps the
	// ordinary planner, key builder, scheduler, capture, and restore paths. Nil
	// preserves the normal lifecycle exactly.
	CacheVerification *CacheVerificationRequest
	// Portable binds this run to a bound execution request a runner provider
	// delivered (internal/runnerprovider.BoundRequestEnv). It is the typed
	// request decomposition for the EXECUTING side of portable execution:
	// placement resolution is skipped (this engine IS the remote) and the
	// recorded session states the remote placement. For a frozen version 1
	// request the selection stage plans exactly the frozen project ids and the
	// seam before execution refuses a re-planned graph that differs from the
	// expected plan; a version 2 request plans its checkout through the
	// ordinary stages (PortableExecution). Nil preserves the normal lifecycle
	// exactly. Only the bound-request adapter in internal/cli sets it.
	Portable *PortableExecution
	// EphemeralSession records the run's session file as usual but keeps it OUT of
	// the `latest` rotation. Set it when the run is not the user's build.
	//
	// `latest` names the user's last build: `putnami sessions inspect latest`
	// and every `--session latest` reader resolve it. The MCP adapter is the
	// caller — an agent's `run_jobs` landing after a human's build would
	// otherwise become the session those readers open, and session metadata has
	// no origin field, so nobody could tell afterwards.
	EphemeralSession bool
	// ExecutesUnderDryRun keeps --dry-run from short-circuiting into the engine's
	// plan-only preview, so the jobs actually run.
	//
	// Two callers, both forwarding the resolved value as a job param for the
	// task to interpret: the extension-alias adapter (an extension command
	// group declares its OWN dry-run flag — buildExtensionCommandParams), and
	// the terminal adapter's `publish --dry-run` (applyPublishDryRun),
	// where dropUndryableSideEffectJobs excludes any side-effecting job that
	// never declared the flag. Previewing instead of running would silently
	// turn `putnami cloud deploy --dry-run` into a no-op. --plan stays the
	// plan-only preview on both paths.
	ExecutesUnderDryRun bool
	// WithholdServeSteps runs a `serve` request as the PREPARATION of a
	// supervisor that starts the workloads itself: `putnami compose`, which must
	// hand each workload its own process environment (an ephemeral port, the
	// URLs of the workloads it runs with, its database binding) and learn the
	// port it bound, one member at a time.
	//
	// Everything up to the serve step stays the engine's: resolution, runtime
	// synchronization, selection, planning with its guards, and the scheduled
	// execution — cache, session file and all — of every finite step the serve
	// pipelines declare (config merge, generate, describe, and their dependency
	// steps). What changes is the one step no other job depends on: each
	// selected project's uncacheable `serve` step is not started. It is returned
	// on SessionResult.Withheld together with the workspace it was planned
	// against, and the serve force-watch rule does not apply, because nothing
	// long-running is left in the run.
	//
	// The composition adapter (internal/compose) is the only caller. It executes
	// the withheld steps with jobs.RunJobWithProcessEnv, which keeps the
	// injected environment out of every cache key: the steps that DO run here
	// never see it.
	WithholdServeSteps bool
	// watchIteration marks this request as ONE iteration of an outer watch/serve
	// loop. It suppresses the serve force-watch rule in execute(): the outer loop
	// already owns the replan policy, and without this marker a serve iteration
	// re-derives serve mode from Commands, re-enables Watch, and re-enters
	// runWatch — infinite recursion printing "[watch] starting..." forever
	// (introduced when routing iterations back through Engine.Run).
	//
	// UNEXPORTED on purpose: it is set only by the engine's own watch loop
	// (watchIterationRequest), never by an adapter, so it does not count against
	// the six-adapter-field cap A6 placed on this struct — a seventh
	// adapter-shaped field still requires typed policy/request decomposition.
	watchIteration bool
	// preparation accumulates the dependency-preparation stage's phase
	// attribution between the two places the stage runs —
	// phase 1c's command-scoped synchronization and phase 1d's per-provider probe
	// resolutions — and the scheduler, which publishes it on the session.
	//
	// UNEXPORTED for the same reason watchIteration is: it is engine-internal
	// plumbing between two of this package's own phases, not an adapter knob. An
	// adapter that builds a Request gets the attribution for free; nothing it can
	// set changes it.
	preparation *jobs.PreparationReport
	// synchronizedExtensions are the extensions phase 1c synchronizes, whose
	// runtime toolchain resolutions the report after the run reads (Run). Nil
	// when the run ended before phase 1c.
	//
	// UNEXPORTED for the same reason preparation is: it is engine-internal
	// plumbing between two of this package's own phases.
	synchronizedExtensions []*extension.ExtensionDescription
	// specGate carries the spec-verification record from the post-session
	// finalizer to execute's session-persistence block.
	//
	// UNEXPORTED for the same reason preparation is: it is engine-internal
	// plumbing between two of this package's own phases, and nothing an adapter
	// can set changes what the gate decided.
	specGate *specGateOutcome
	// selection is the run's RESOLVED project selection, written by the
	// selection stage and stamped onto every planned node so each job's context
	// document reports how this invocation chose its scope.
	//
	// UNEXPORTED for the same reason preparation is: it is engine-internal
	// plumbing between two of this package's own phases. An adapter supplies the
	// selection FLAGS through Global and gets the resolved answer for free;
	// nothing it can set overrides what selection actually resolved.
	selection *protocoljob.Selection
	// confirmsReleaseSetHead is set when the run's release set changes no
	// member, so the finalizer confirms the head. planning reads it to
	// accept an empty plan, which is then the session's complete answer.
	//
	// UNEXPORTED for the same reason preparation is: it is engine-internal
	// plumbing between two of this package's own phases.
	confirmsReleaseSetHead bool
	// runnerProvider is the reserved runner provider placement resolution found
	// at the run's only extension discovery, or nil when placement is local or
	// no extension declares the command. The seam between the final plan and
	// execution reads it; nothing else does, and it never reaches a key.
	//
	// UNEXPORTED for the same reason preparation is: it is engine-internal
	// plumbing between two of this package's own phases.
	runnerProvider *extension.ResolvedProvider
	// selectionEvidence is what the selection stage learned beyond the resolved
	// selection: requested mode, measured changed paths, printed notices. The
	// portable projection freezes it; nothing else reads it.
	//
	// UNEXPORTED for the same reason selection is.
	selectionEvidence selectionEvidence
	// noCacheProjects is the RESOLVED id set of --no-cache-projects: the
	// projects whose tasks refuse cache reuse while the rest of the run keeps
	// it. The selection stage writes it, because that is where the
	// workspace, the shared selector parser, and the usage-error exit already
	// live; execute hands it to the scheduler.
	//
	// UNEXPORTED for the same reason selection is: it is engine-internal
	// plumbing between two of this package's own phases. An adapter supplies
	// the selector through Global.NoCacheProjects and gets the resolved answer
	// for free.
	noCacheProjects map[string]bool
	// processCapabilityAuthorization is derived inside Engine.Run from the final
	// planned DAG and an in-memory runner contract. It is never adapter input,
	// manifest data, plan output, or session data.
	processCapabilityAuthorization *jobs.ProcessCapabilityAuthorization
	// internalJobs carries framework-owned in-process runners for visible DAG
	// nodes added by this engine run. It is populated only after the final plan
	// is known and never exposed to adapters or manifests.
	internalJobs map[string]jobs.InternalJobRunner
	// HostedRemoteCache is the remote cache a hosted run reads through when
	// its adapter shares one with the first-use bootstrap, which starts its
	// provider before the implicit install's first repository code. Nil gives
	// the run one of its own. The adapter closes it.
	HostedRemoteCache *HostedRemoteCache
	// HostedReporters are the reporters a hosted run hands the run credential
	// when its adapter shares them with the first-use bootstrap, which starts
	// them before the implicit install's first repository code. Nil gives the
	// run its own, started before its first hook. The adapter closes them.
	HostedReporters *HostedReporters
	// hostedRemote is the remote cache whose provider a hosted run started
	// before its first hook (HostedRemoteCache.Start), or nil. Every stage of
	// the run that reads the remote cache reads it through this one; execute
	// sets it to the cache the session runs with (hostedRunRemoteCache).
	hostedRemote *jobs.RemoteCache
	// Hooks are the workspace lifecycle hooks to run around the lifecycle. Nil
	// runs none: only the terminal adapter passes them today, for the same
	// reason Observer is injected.
	Hooks *wsproto.HooksConfig
	// Observer receives run observations. Nil is a total no-op (ADR 0001 §4).
	Observer Observer
	// Preflight is the production doctor gate the engine runs between plan and
	// execute. It is injected because the doctor evaluator lives beside the
	// `putnami doctor` command, and the single call site here was the only edge
	// from the engine into the command subtree.
	//
	// Unlike Observer and Hooks, nil is NOT a no-op: it fails the run closed
	// under the production profile and is ignored under every other one. See
	// PreflightGate.
	Preflight PreflightGate
	// ValidateCommandFlags, when non-nil, runs after extension resolution and
	// before project selection. It is the command-scoped half of the CLI's single
	// parse-and-validation pass, which cannot run any earlier
	// because only the resolved extensions know which flags the selected tasks
	// declare. A non-nil error aborts the run before selection; the callback owns
	// its own reporting.
	//
	// It receives the RESOLVED flags — env and config overrides already layered
	// on — because whether its deprecation notices print at all depends on the
	// resolved Quiet.
	ValidateCommandFlags func(*extension.DiscoveryResult, GlobalFlags) error
	// VersionSnapshot is the git-derived version state captured before any in-run
	// tree mutation. Nil makes the engine capture it itself at the start
	// of Run, before hooks — which is what the terminal path did.
	VersionSnapshot *git.VersionInfo
	// versions is resolved during serial planning and then read by execution
	// and evidence recovery. Each run, including a watch iteration, owns it.
	versions jobs.RunVersions
	// versionsErr is why versions could not read a line from git, or nil. The
	// run proceeds either way (jobs.BuildRunVersions); --debug prints it, so a
	// 0.0.0 stamp can be told apart from a line that is really untagged.
	versionsErr error
	// ancestry is the bound commit's ancestry, read before the first hook by a
	// run that may publish (readsAncestry), nil for every other run.
	//
	// UNEXPORTED for the same reason preparation is: Engine.Run reads it
	// itself, and nothing an adapter can set replaces it.
	ancestry *AncestrySnapshot
	// Stdout receives the notices the ENGINE itself prints on the human stdout
	// stream: "No impacted projects found", "No projects matched", "No jobs
	// matched", the --plan table, the auto-selection line, and a blocked
	// production preflight's structured envelope. Nil means os.Stdout, which is
	// every terminal-shaped adapter.
	//
	// It exists for MCP: that adapter's stdout IS the JSON-RPC frame
	// stream, so a single "Nothing to do." line written there desynchronizes the
	// session's framing and kills the agent connection. Job output does NOT come
	// through here — that is the EventSink seam, which MCP also discards.
	// Diagnostics on stderr are untouched: stderr is not a protocol channel for
	// any adapter.
	Stdout io.Writer
}

// CacheVerificationRequest contains the one execution override the verifier
// needs. StoreRoot is a temporary, command-owned namespace; it never changes a
// key, only where the existing key is looked up and published.
type CacheVerificationRequest struct {
	StoreRoot string
}

// PlanExtensionSelection identifies one extension-owned command without
// retaining a pre-hook extension description.
type PlanExtensionSelection struct {
	Name           string
	Command        string
	InheritedFlags map[string]extension.FlagDefinition
}

// stdout returns the run's human-notice stream. It is the ONLY place the engine
// resolves a default stdout, so an adapter that supplies one cannot be bypassed
// by a stage that reaches for os.Stdout directly.
func (r *Request) stdout() io.Writer {
	if r.Stdout != nil {
		return r.Stdout
	}
	return os.Stdout
}

// runMarkerParams returns the params that key persisted successful-run state.
func (r *Request) runMarkerParams() map[string]any {
	if r.RunMarkerParams != nil {
		return r.RunMarkerParams
	}
	return r.CommandParams
}

// planExtensions returns the extension set the run plans against. A stable
// single-extension selection is rebound to the current discovery result first;
// a frozen set supplied by watch comes next; otherwise every extension is used.
func (r *Request) planExtensions(discovered *extension.DiscoveryResult) []*extension.ExtensionDescription {
	if selection := r.PlanExtension; selection != nil {
		fresh := extension.FindExtensionByName(discovered.Extensions, selection.Name)
		if fresh == nil {
			return nil
		}
		job := fresh.Jobs[selection.Command]
		if job == nil || len(selection.InheritedFlags) == 0 {
			return []*extension.ExtensionDescription{fresh}
		}

		jobCopy := *job
		jobCopy.Flags = extension.MergeFlagLayers(job.Flags, selection.InheritedFlags)

		extensionCopy := *fresh
		extensionCopy.Jobs = make(map[string]*extension.JobDefinition, len(fresh.Jobs))
		maps.Copy(extensionCopy.Jobs, fresh.Jobs)
		extensionCopy.Jobs[selection.Command] = &jobCopy
		return []*extension.ExtensionDescription{&extensionCopy}
	}
	if r.PlanExtensions != nil {
		return r.PlanExtensions
	}
	return discovered.Extensions
}

// previewsOnly reports whether --dry-run means "print the plan and stop" for
// this run. It does for every adapter except the extension alias, whose
// --dry-run is a task parameter the extension itself interprets, so the jobs
// have to execute (see Request.ExecutesUnderDryRun). Every stage that asks "does
// this run actually execute?" reads this, not Global.DryRun.
func (r *Request) previewsOnly() bool {
	return r.Global.DryRun && !r.ExecutesUnderDryRun
}

// SessionResult is what a run produced.
type SessionResult struct {
	// ExitCode is the process exit code for the run. It is always authoritative,
	// including when Run returns a nil error (a failed build is a result, not an
	// engine error).
	ExitCode int
	// Session is the canonical reduction of the run, or nil when no scheduler ran
	// (--plan, --dry-run, watch mode, an aborted-before-execution stage).
	Session *jobs.SessionResult
	// Results are the raw job results, keyed by job key. Nil when nothing ran.
	Results map[string]*jobs.JobResult
	// Plan is the scheduled plan, or nil when planning did not complete.
	Plan []*jobs.ScheduledJob
	// Projects are the selected projects, or nil when selection did not complete.
	Projects []*workspace.Project
	// Workspace is the workspace the run planned against. Set only by a
	// Request.WithholdServeSteps run, whose withheld steps execute against it.
	Workspace *workspace.Workspace
	// Withheld are the serve steps a Request.WithholdServeSteps run planned and
	// did not start, one per selected project that has one, in plan order.
	Withheld []*jobs.ScheduledJob
	// ancestry is the snapshot the run read before its first hook, or nil
	// (Request.ancestry).
	ancestry *AncestrySnapshot
}

// Engine runs the job lifecycle. It is stateless today; the type exists so
// adapters depend on a value they can substitute in tests rather than on
// package-level functions.
type Engine struct{}

// New returns an Engine.
func New() *Engine { return &Engine{} }

// Run executes one job-command lifecycle: version stamp, before hooks,
// workspace + extension resolution, command-flag validation, project selection,
// planning, production preflight, observation, execution, after hooks.
//
// SessionResult.ExitCode is always the process exit code. The error is non-nil
// only for the stages that own an error value (the command-flag validation
// callback and lifecycle hooks); every other failure is already reported on the
// terminal streams and carried by ExitCode. Adapters that need richer errors get
// them when they migrate (A3b/A4/A5a/A5b).
func (e *Engine) Run(ctx context.Context, request Request, sink EventSink) (SessionResult, error) {
	ctx = sessionreporter.Capture(ctx)
	// App.Run captures before its pre-discovery/bootstrap work. Capture again at
	// this public engine boundary for MCP/tests/adapters that invoke it directly,
	// before version discovery or repository lifecycle hooks can spawn.
	ctx = jobs.CaptureProcessCapabilities(ctx)
	req := &request
	req.versions, req.versionsErr = nil, nil
	// A version 2 bound request names the selection this engine resolves; it
	// binds onto the selection flags before any stage reads them.
	req.Portable.bindSelection(&req.Global)

	// The version stamp reflects the tree as the user left it, so it is captured
	// BEFORE the before-hooks and any codegen job runs. Computing it afterwards
	// would fold putnami-driven, in-run tree changes into a false "-<dirtyhash>"
	// suffix on an otherwise clean checkout.
	// A run PUTNAMI_SOURCE_REVISION binds to a commit whose version cannot be
	// read fails here, before any hook or job, instead of running with no
	// version (captureVersionSnapshot explains why).
	if req.VersionSnapshot == nil {
		snapshot, err := captureVersionSnapshot(req.WorkspaceRoot)
		if err != nil {
			iox.Fprintf(os.Stderr, "putnami: %v\n", err)
			return SessionResult{ExitCode: ExitError}, err
		}
		req.VersionSnapshot = snapshot
	}
	// A publish or a deploy states which commit it ships. Where Git does not
	// manage the workspace root there is none, so a run that executes stops
	// here, before any hook, job or remote call. A selected command whose
	// extension is not installed is reported instead (reportRepositoryRefusal).
	if err := requireRepository(req); err != nil {
		return SessionResult{ExitCode: ExitError}, reportRepositoryRefusal(req, err)
	}

	// A run that may publish reads its bound commit's ancestry here, before
	// the first hook runs repository code that could rewrite refs or replace
	// objects (AncestrySnapshot). The snapshot an adapter read at process
	// start for the same workspace, before its bootstrap or install ran any
	// repository code, is that read (CaptureAncestry).
	req.ancestry = nil
	if readsAncestry(req) {
		req.ancestry = capturedAncestry(ctx, req.WorkspaceRoot)
		if req.ancestry == nil {
			req.ancestry = captureAncestrySnapshot(req.WorkspaceRoot, MaxAncestryCommits)
		}
	}

	// Hook verbosity is resolved once, here, from the flags as parsed. The run
	// stages layer env/config overrides onto Global later (applyEnvOverrides);
	// re-reading it for the after-hooks would make a PUTNAMI_VERBOSE run print
	// hook output for one half of the pair and not the other.
	verbose := req.Global.Verbose || req.Global.Debug

	// A hosted run starts its cache provider before the first hook, because
	// no process that starts after repository code receives the run
	// credential. The first-use bootstrap may have started it already, before
	// the implicit install's first repository code: the run then reads the
	// remote cache through that provider and starts none.
	hosted := req.HostedRemoteCache
	if hosted == nil {
		hosted = &HostedRemoteCache{}
		defer hosted.Close()
	}
	hostedRemote, err := hosted.Start(ctx, req)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return SessionResult{ExitCode: ExitError}, err
	}
	req.hostedRemote = hostedRemote
	// Its reporters start before the first hook too, so that each can hold
	// the run credential; the session's reporting adopts them.
	if req.HostedReporters == nil {
		req.HostedReporters = &HostedReporters{}
		defer req.HostedReporters.Close()
	}
	req.HostedReporters.Start(ctx, req)

	if err := runBeforeHooks(ctx, req, verbose); err != nil {
		return SessionResult{ExitCode: ExitError}, err
	}

	result, runErr := e.run(ctx, req, sink)
	// A workspace-install run starts without a pinned toolchain it may install
	// itself, so runtime synchronization logs that at debug level only. Once
	// the run ended, however it ended, say whether the toolchain is there: a
	// run that plans no job installs nothing, and its warning must not be
	// lost. The report reads the extensions synchronization resolved and those
	// the plan's jobs carry, which differ when the run plans a copy of one
	// (Request.PlanExtension). A run that ended before synchronization has
	// nothing to report.
	if req.synchronizedExtensions != nil {
		jobs.ReportProvisionedRuntimeToolchains(ctx, req.WorkspaceRoot, req.synchronizedExtensions, result.Plan, req.Commands)
	}

	// After hooks run even if jobs failed.
	code, hookErr := runAfterHooks(ctx, req, verbose, result.ExitCode)
	result.ExitCode, result.ancestry = code, req.ancestry
	if runErr == nil {
		runErr = hookErr
	}
	return result, runErr
}

// run is the lifecycle proper, between the before and after hooks.
func (e *Engine) run(ctx context.Context, req *Request, sink EventSink) (SessionResult, error) {
	// Phase 1: load the workspace and resolve extensions — once per run.
	ws, discovered, code := loadWorkspaceAndExtensions(req)
	if code != ExitSuccess {
		return SessionResult{ExitCode: code}, nil
	}
	// The dependency fetch of a hosted install sees only the extensions
	// installed from the artifact store: no path extension starts beside the
	// job credential.
	discovered = jobs.CredentialedFetchView(ctx, req.WorkspaceRoot, discovered)
	extensions := discovered.Extensions

	// Phase 1b: the command-scoped half of the single parse-and-validation pass.
	if req.ValidateCommandFlags != nil {
		if err := req.ValidateCommandFlags(discovered, req.Global); err != nil {
			return SessionResult{ExitCode: protocolcli.ExitCodeForError(err)}, err
		}
	}

	// Phase 1b': the committed verification policy (options.sdd.verification)
	// is validated for the whole workspace before anything executes. The policy
	// decides whether a run may fail, so an unknown domain or value anywhere is
	// an invalid-config stop, never a silent fallback (no runtime weakener
	// exists, which makes the committed spelling the only one).
	if err := specgate.ValidatePolicy(ws); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return SessionResult{ExitCode: protocolcli.ExitCodeForError(err)}, err
	}

	// Phase 1c: synchronize extension runtimes before project selection,
	// planning, scheduler setup, and every task-scoped timeout. This is the
	// generic extension-resolution boundary: job invocation only consumes the
	// resolved executable and never owns preparation.
	//
	// Independent extension runtimes synchronize CONCURRENTLY inside the stage
	// and the stage reports where its time went; the
	// report is created here so phase 1d's probe-driven resolutions land in the
	// same accounting, and is handed to the scheduler in phase 4 to publish.
	//
	// The toolchain report after the run reads the extensions synchronized
	// here (Request.synchronizedExtensions), including after a failed
	// synchronization.
	//
	// The path-extension fetch of a hosted install plans only the extensions
	// the dependency fetch left out.
	planningExtensions := jobs.PathExtensionFetchPlan(ctx, req.WorkspaceRoot, req.planExtensions(discovered))
	req.preparation, req.synchronizedExtensions = &jobs.PreparationReport{}, planningExtensions
	if err := jobs.SynchronizeExtensionRuntimesForCommands(
		ctx, ws, planningExtensions, req.Commands, req.preparation,
	); err != nil {
		iox.Fprintf(os.Stderr, "putnami: synchronize extension runtimes: %v\n", err)
		return SessionResult{ExitCode: ExitError}, nil
	}

	// Phase 1d: synchronize the workspace probe — SNAPSHOT FIRST. When the
	// persisted index still describes the tree, this phase starts no extension
	// process and prepares no runtime; when a declared metadata input moved, it
	// starts exactly one process per provider that owns the change. It runs
	// after runtime synchronization so a provider that IS demanded by this
	// command is already resolved, and before selection so the provider view is
	// in place for everything that reads the workspace.
	if code := synchronizeWorkspaceProbe(ctx, req, ws, discovered); code != ExitSuccess {
		return SessionResult{ExitCode: code}, nil
	}

	// Phase 2: select and filter projects. The resolved extensions go with it:
	// auto-selection's speculative run-marker lookup resolves its cache provider
	// from them, so dropping them here would silently demote every bare command
	// to the local-marker-only path.
	// A release-set channel cannot tolerate --impacted's ordinary convenience
	// fallback to every project: widening here would republish unchanged
	// upstreams and make the release-set claim false. The mode is derived from
	// the already-parsed request and enforced before selection resolves.
	//
	// Channel-backed --impacted, --all, and tagged publication let the
	// COORDINATOR decide which projects the run PUBLISHES, and only that: the
	// commands sharing the session keep the selection the caller asked for.
	// Both selections are resolved here, before the coordinator's override.
	releaseSetOptions, verification, verificationEvidence, code := resolveReleaseSetSelections(req, ws, extensions...)
	if code != ExitSuccess {
		return SessionResult{ExitCode: code}, nil
	}
	if deployNarrowsToChannelMembers(req, releaseSetOptions) {
		req.Global.ImpactedStrict = true
	}
	selectedProjects, code := selectProjects(req, ws, extensions...)
	if code != ExitSuccess {
		return SessionResult{ExitCode: code}, nil
	}

	// The cache manager is built HERE, not in execute, and unconditionally.
	// Two computations need it: the execution keys of this run, and the
	// selection keys a release-set publish derives below. Sharing one manager
	// makes them share one memoized file-hash pass over the tree instead of
	// walking it twice. Under --no-cache it is still built — the memo is not a
	// cache of results — and execute drops it before anything consumes it as one.
	cacheManager := newRunCacheManager(req, ws)

	// Phase 3: build the execution plan. The alias adapter narrows the extension
	// set here and only here, so `putnami <group> <sub>` still plans exactly the
	// one extension's flat command it names.
	//
	// A release-set publish needs one selection fingerprint per member BEFORE
	// the plan it will actually run exists: the fingerprint is the identity of
	// the member's package task, and which members are republished decides which
	// package tasks the run plans at all. So this first plan is the KEYING plan,
	// and the run's own plan is rebuilt below from the projects and extensions
	// the coordinator leaves. It is never filtered in place: a narrowed plan
	// would key a different task graph than it measured.
	planned, code := keyingPlan(req, ws, selectedProjects, planningExtensions, discovered, releaseSetOptions)
	if code != ExitSuccess {
		return SessionResult{ExitCode: code}, nil
	}

	// Every release-set publish resolves every listed channel exactly once
	// after selection and before package planning: an existing head is that
	// channel's CAS expectation, the first one is the plan's baseline, and an
	// empty channel is a null head. Every later stage retains that immutable
	// answer and never resolves a channel again.
	preparation, err := jobs.PrepareReleaseSetRun(
		ctx, releaseSetOptions, ws, selectedProjects, verification, planningExtensions, discovered,
		planned, req.CommandParams, cacheManager, req.selection, slices.Contains(req.Commands, "deploy"),
	)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: release-set publish: %v\n", err)
		return SessionResult{ExitCode: protocolcli.ExitCodeForError(err), Projects: selectedProjects}, nil
	}
	// sessionProjects is the selection as the SESSION made it, bound at the same
	// seam because the next statement narrows selectedProjects to the owners of
	// the release-set plan's members. A project that declares an archives
	// publish and owns no member disappears there, so the archive guard below
	// would never see its missing uploader. The guard reads THIS list;
	// every other stage keeps reading the narrowed one, because that is what
	// the run plans and executes. A deploy session WIDENS instead of narrowing,
	// and every project it adds owns a member the coordinator accounts for, so
	// the guard's answer there is unchanged.
	releaseSetRun, sessionProjects := preparation.Run, selectedProjects
	if releaseSetRun != nil {
		selectedProjects, planningExtensions = preparation.Projects, preparation.Extensions
		req.selection, req.confirmsReleaseSetHead = preparation.Selection, preparation.ConfirmsHead
		adoptVerificationEvidence(req, preparation, verificationEvidence)
		planned, code = buildPlan(req, ws, selectedProjects, planningExtensions, discovered)
		if code != ExitSuccess {
			return SessionResult{ExitCode: code}, nil
		}
	}

	// Every node, not just the workspace-scoped ones that carry
	// SelectedProjects: a project-scoped task that reports on the workspace has
	// to know the run was narrowed just as much as a workspace-scoped one does.
	jobs.AttachRunSelection(planned, req.selection)
	// A dependent command can expand publish only after planning. Prepare its
	// release set from that exact relation-local project selection; unlike an
	// explicit publish, the outer deploy plan must remain intact and therefore
	// is never globally narrowed through ScopePlanning.
	//
	// The same seam binds the workload contract of a deploy that names an
	// environment: ONE provider resolve for the whole environment, the
	// workloads its rules select out of this run's selection, and the members
	// of the set each of them owns. A session that also publishes leaves the
	// set to the barrier below, which completes every contract from the
	// snapshot it has just released.
	releaseSetRun, err = jobs.PrepareDependentRun(ctx, releaseSetRun, preparation.Options,
		jobs.BuildDeployOptions(req.Commands, req.WorkspaceRoot, req.CommandParams), ws, planned, discovered)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return SessionResult{ExitCode: protocolcli.ExitCodeForError(err), Plan: planned, Projects: selectedProjects}, nil
	}
	if releaseSetRun != nil {
		planned, req.internalJobs, err = attachReleaseSet(req, releaseSetRun, planned)
		if err != nil {
			iox.Fprintf(os.Stderr, "putnami: release-set publish: %v\n", err)
			return SessionResult{ExitCode: ExitError, Plan: planned, Projects: selectedProjects}, nil
		}
	}
	if req.WorkspaceLifecycle {
		planned = dedupePlanByExtension(planned)
		// A lifecycle run that planned nothing is a no-op its ADAPTER reports (see
		// Request.WorkspaceLifecycle). Executing the empty plan instead would hand
		// back a success-shaped session for work that never happened, and the
		// adapter reads "Session == nil" as exactly that no-op.
		if len(planned) == 0 {
			return SessionResult{ExitCode: ExitSuccess, Projects: selectedProjects}, nil
		}
	}

	// Under an executing dry-run, exclude side-effecting jobs that never
	// declared the dry-run flag they would have to interpret — BEFORE
	// the archives detector below, so a dropped uploader surfaces as the
	// existing actionable unpublished-archives failure instead of publishing.
	//
	// A supervisor that starts the serve steps itself then takes them out of the
	// run, once the plan is final and before anything reads it for
	// authorization, preview or execution (Request.WithholdServeSteps).
	planned, withheld := withholdRequestedServeSteps(req, dropUndryableSideEffectJobs(req, planned), selectedProjects)

	// A repository can declare side-effect traits and dependency edges, so the
	// trait alone never carries authority. Build an opaque proof from the exact
	// final plan and the runner-owned AFTER contract; the scheduler will still
	// require successful terminal results before granting a job.
	req.processCapabilityAuthorization, err = jobs.PrepareProcessCapabilityAuthorization(
		ctx, planned, !req.Global.Plan && !req.previewsOnly(),
	)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: cloud capability authorization: %v\n", err)
		return SessionResult{ExitCode: ExitError, Plan: planned, Projects: selectedProjects}, nil
	}

	// Detect a publish that leaves declared release archives with no uploader.
	// Computed from the plan against the pre-scope selection, and reported at
	// the end so it is the last thing seen. Release-set members are left to
	// the coordinator, which already fails the plan when a selected member has
	// no publish step; a no-impact release keeps every member project for
	// verification but plans no publish job, and must not read as missing
	// uploaders. A project the release-set plan never announced is not
	// coordinated by anything, so it is reported even though scoping dropped
	// it.
	unpublishedArchives, unannouncedArchives := unpublishedArchiveProjects(req.Commands, req.CommandParams, sessionProjects, planned, releaseSetRun.MemberProjectIDs())

	// --plan: show the plan and exit.
	if req.Global.Plan {
		code := renderPlan(req, planned, selectedProjects)
		reportUnpublishedArchives(unpublishedArchives, unannouncedArchives, discovered.Skipped, req.Global, false)
		return SessionResult{ExitCode: code, Plan: planned, Projects: selectedProjects}, nil
	}

	// Production preflight gate: under --profile production, an unwaived
	// high/critical doctor finding aborts here, between plan and execute, BEFORE
	// any job runs.
	if code, blocked := runProductionPreflight(req, selectedProjects); blocked {
		return SessionResult{ExitCode: code, Plan: planned, Projects: selectedProjects}, nil
	}

	// The portable seam: a run whose placement resolved to a runner
	// provider leaves the engine HERE, with its final plan and the selection it
	// was computed from, and comes back as an imported canonical session; the
	// executing side of that same seam proves its re-planned graph equals the
	// expected one before anything is scheduled. It sits after the preflight so
	// a blocked run transfers nothing, and before the release-set handoff so no
	// publication capability is ever armed for a request that leaves.
	if result, handled := e.portableSeam(ctx, req, ws, discovered, planned, releaseSetRun); handled {
		result.Projects = selectedProjects
		return result, nil
	}

	// Arm publication only after final plan authorization and production checks.
	if err := jobs.HandoffReleaseSetPlan(ctx, releaseSetRun, planned, !req.previewsOnly()); err != nil {
		iox.Fprintf(os.Stderr, "putnami: release plan capability handoff: %v\n", err)
		return SessionResult{ExitCode: ExitError, Plan: planned, Projects: selectedProjects}, nil
	}

	// Observe the run after the plan is complete: the plan-dependent counts do
	// not exist before this point, which is why earlier exits are unobserved.
	notifyRunPlanned(ctx, req.Observer, func() RunPlan {
		return RunPlan{Commands: req.Commands, Projects: len(selectedProjects), Jobs: len(planned)}
	})

	if result, nothingToRun := servePreparationWithoutFiniteSteps(req, planned, selectedProjects, ws, withheld); nothingToRun {
		return result, nil
	}

	// Phase 4: execute.
	fatalUnpublishedArchives := len(unpublishedArchives) > 0 && !req.previewsOnly()
	var finalizers []func(map[string]*jobs.JobResult)
	if fatalUnpublishedArchives {
		finalizers = append(finalizers, unpublishedArchiveFailure(unpublishedArchives, unannouncedArchives, discovered.Skipped))
	}
	// The workspace map rides the same seam: it is core-owned work that
	// attaches to a COMPLETED build session, and in check mode it has to be able
	// to fail that session. A preview never executes, so the finalizer it would
	// carry never runs.
	if refreshMap := contextMapFinalizer(req, ws, selectedProjects); refreshMap != nil {
		finalizers = append(finalizers, refreshMap)
	}
	req.specGate = &specGateOutcome{}
	// The gate's cache-recovery source reuses THIS run's cache manager, so the
	// memoized file-hash pass built for the execution keys is the one a recovery
	// lookup keys against; a second manager would walk the tree again.
	if gate := specGateFinalizer(req, ws, planned,
		newCachedObservationRecovery(req, ws, planningExtensions, cacheManager), req.specGate); gate != nil {
		finalizers = append(finalizers, gate)
	}
	ctx, finalizers, closeOutboxes, err := releaseSetExecution(ctx, req, releaseSetRun, finalizers)
	if err != nil {
		iox.Fprintf(os.Stderr, "putnami: release-set publish: %v\n", err)
		return SessionResult{ExitCode: ExitError, Plan: planned, Projects: selectedProjects}, nil
	}
	defer closeOutboxes()
	result := e.execute(ctx, req, ws, selectedProjects, discovered, planned, cacheManager, sink, finalizers...)
	result.Plan, result.Projects = planned, selectedProjects
	result = attachWithheldServeSteps(result, req, ws, withheld)
	if len(unpublishedArchives) > 0 {
		reportUnpublishedArchives(unpublishedArchives, unannouncedArchives, discovered.Skipped, req.Global, fatalUnpublishedArchives)
		result.ExitCode = archivePublishExitCode(result.ExitCode, unpublishedArchives, fatalUnpublishedArchives)
	}
	return result, nil
}

// releaseSetExecution prepares the release set's part of the execution. It
// appends the publish finalizers after every session gate (publishFinalizers),
// and returns the context that carries the private outboxes of a
// publication-v1 run's publication jobs, and the function that removes them
// when the run ends.
func releaseSetExecution(ctx context.Context, req *Request, run *jobs.ReleaseSetRun, finalizers []func(map[string]*jobs.JobResult)) (context.Context, []func(map[string]*jobs.JobResult), func(), error) {
	finalizers = append(finalizers, publishFinalizers(ctx, req, run)...)
	ctx, closeOutboxes, err := run.PublicationContext(ctx)
	return ctx, finalizers, closeOutboxes, err
}

// attachReleaseSet adds the release set's nodes to the plan, and returns the
// plan with the in-process runners of those nodes. A run that publishes
// through a publication-v1 provider first binds what open sends: its plan
// tuple and the ancestry snapshot the run read before any repository code ran.
func attachReleaseSet(req *Request, run *jobs.ReleaseSetRun, planned []*jobs.ScheduledJob) ([]*jobs.ScheduledJob, map[string]jobs.InternalJobRunner, error) {
	planned, err := run.AttachPlan(planned)
	if err != nil {
		return planned, nil, err
	}
	var ancestry jobs.AncestryReader
	if req.ancestry != nil {
		ancestry = req.ancestry
	}
	var barrier []string
	if invocation := req.Portable.invocation(); invocation != nil && invocation.Publication != nil {
		barrier = invocation.Publication.Barrier
	}
	if err := run.BindPublication(ancestry, barrier); err != nil {
		return planned, nil, err
	}
	return run.AttachBarrier(planned)
}

// publishFinalizers returns the finalizers a publish session ends with, in
// order.
//
// The first reads the registry probes of a dry-run publish once, after every
// job, and is nil for any other run. The dry-run parameter the jobs receive
// selects it, as it selects the release-set mode: a preview executes no job and
// runs no finalizer.
//
// The release-set coordinator comes last. It sees the synthetic failure of a
// session gate or of the probe report, and publishes no successful outcome
// after one. A run whose deploy barrier releases the set has no release
// finalizer.
func publishFinalizers(ctx context.Context, req *Request, releaseSetRun *jobs.ReleaseSetRun) []func(map[string]*jobs.JobResult) {
	dryRun, _ := req.CommandParams["dry-run"].(bool)
	finalizers := []func(map[string]*jobs.JobResult){jobs.MemberProbeReport{
		Run: releaseSetRun, Commands: req.Commands, DryRun: dryRun, Quiet: req.Global.Quiet, Out: os.Stderr,
	}.Finalizer()}
	if release := releaseSetRun.Finalizer(ctx); release != nil {
		finalizers = append(finalizers, release)
	}
	return finalizers
}

// buildReleaseSetOptions reads the publication-shaping parameters this run was
// invoked with and resolves the release-set mode from them. The parameters are
// only ever read here, so nothing downstream re-derives a channel or a
// visibility from the raw map.
func buildReleaseSetOptions(req *Request, ws *workspace.Workspace) (jobs.ReleaseSetOptions, error) {
	channel, _ := req.CommandParams["channel"].(string)
	baselineChannel, _ := req.CommandParams["baseline-channel"].(string)
	visibility, _ := req.CommandParams["visibility"].(string)
	scope, _ := req.CommandParams["scope"].(string)
	dryRun, _ := req.CommandParams["dry-run"].(bool)
	return jobs.BuildReleaseSetOptions(jobs.ReleaseSetRequest{
		Commands: req.Commands, WorkspaceRoot: req.WorkspaceRoot,
		Channel: channel, BaselineChannel: baselineChannel, Visibility: visibility, Scope: scope, DryRun: dryRun,
		Impacted: req.Global.Impacted, All: req.Global.All,
		Versions: req.runVersions(ws),
	})
}

// keyingPlan builds the run's first plan, the keying plan, and refuses it
// with ExitError when it belongs to a bound request that may publish without
// invocation.publication (refusesUnauthorizedPublication).
func keyingPlan(
	req *Request,
	ws *workspace.Workspace,
	selectedProjects []*workspace.Project,
	planningExtensions []*extension.ExtensionDescription,
	discovered *extension.DiscoveryResult,
	options jobs.ReleaseSetOptions,
) ([]*jobs.ScheduledJob, int) {
	planned, code := buildPlan(req, ws, selectedProjects, planningExtensions, discovered)
	if code == ExitSuccess && refusesUnauthorizedPublication(req, options, planned) {
		return planned, ExitError
	}
	return planned, code
}

// refusesUnauthorizedPublication refuses a bound request that carries no
// invocation.publication when its keying plan may publish: a planned task of a
// publication command or with declared registry or cloud effects, or a
// release-set publication the request enables. It runs before the release-set
// preparation and every later stage that can start a provider, so a gate-only
// request starts none. It prints the refusal and reports whether it refused.
func refusesUnauthorizedPublication(req *Request, options jobs.ReleaseSetOptions, planned []*jobs.ScheduledJob) bool {
	if invocation := req.Portable.invocation(); invocation == nil || invocation.Publication != nil {
		return false
	}
	var err error
	for _, job := range planned {
		if slices.Contains(runner.PublicationCommands, job.CommandName()) || jobs.HasExternalEffects(job) {
			err = fmt.Errorf("runner: plan task %s publishes, but the request carries no invocation.publication", job.TypedIdentity().Key)
			break
		}
	}
	if err == nil && jobs.RequestedReleaseSetMode(options) != jobs.ReleaseSetDisabled {
		err = errors.New("runner: the request starts a release-set publication, but carries no invocation.publication")
	}
	if err == nil {
		return false
	}
	iox.Fprintf(os.Stderr, "putnami: portable execution refused: %v\n", err)
	return true
}

// deployNarrowsToChannelMembers reports whether an --impacted deploy must
// resolve STRICTLY. A channel-backed deploy names the members it deploys, so
// the ordinary convenience fallback to every project would deploy workloads the
// channel never released.
func deployNarrowsToChannelMembers(req *Request, options jobs.ReleaseSetOptions) bool {
	return req.Global.Impacted && len(options.Channels) > 0 && slices.Contains(req.Commands, "deploy")
}

// runBeforeHooks runs the CLI and command "before" lifecycle hooks. A failure
// aborts the run before any workspace state is touched.
//
// req.WorkspaceRoot is the directory the hook commands run in, so a relative
// hook path means the same thing whichever subdirectory the user invoked from.
func runBeforeHooks(ctx context.Context, req *Request, verbose bool) error {
	if req.Hooks == nil {
		return nil
	}
	if err := hooks.RunCLIHooks(ctx, req.Hooks, "before", req.WorkspaceRoot, verbose); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return err
	}
	if err := hooks.RunCommandHooks(ctx, req.Hooks, "before", req.Commands, req.WorkspaceRoot, verbose); err != nil {
		iox.Fprintf(os.Stderr, "putnami: %v\n", err)
		return err
	}
	return nil
}

// abortCleanupBudget bounds the after-hooks when the run was already aborted.
// A package variable so tests can shrink it; hooks.AbortCleanupBudget carries
// the reasoning for its value.
var abortCleanupBudget = hooks.AbortCleanupBudget

// runAfterHooks runs the command and CLI "after" lifecycle hooks. They run even
// when the jobs failed, and a hook failure can only turn a success into a
// failure — never mask a code the run already produced.
//
// They also run when the run was ABORTED. An after-hook is the workspace's
// cleanup edge — releasing a lock, tearing down a container, restoring a
// checkout — and cleanup that only happens on the happy path is not cleanup. So
// once the run context is done, the remaining after-hooks are detached from it
// with context.WithoutCancel; passing the canceled context straight through
// killed every after-hook on the first Ctrl-C, a state a past regression found.
//
// Detaching is paired with a hard bound: both after-hook calls share ONE
// abortCleanupBudget, well inside the 10s deadline cmd/putnami's signal handler
// gives the whole shutdown, so a cleanup hook that ignores cancellation cannot
// make Ctrl-C feel unresponsive.
func runAfterHooks(ctx context.Context, req *Request, verbose bool, code int) (int, error) {
	if req.Hooks == nil {
		return code, nil
	}

	// Cancellation can arrive BETWEEN the command and CLI hook groups, not only
	// before this function starts. Resolve the context immediately before each
	// group so cleanup that remains after that transition still gets its bounded
	// chance to run. The detached context is created once and shared by every
	// group that observes the aborted run.
	var cleanupCtx context.Context
	var cleanupCancel context.CancelFunc
	contextForHooks := func() context.Context {
		if ctx.Err() == nil {
			return ctx
		}
		if cleanupCtx == nil {
			cleanupCtx, cleanupCancel = context.WithTimeout(context.WithoutCancel(ctx), abortCleanupBudget)
		}
		return cleanupCtx
	}
	defer func() {
		if cleanupCancel != nil {
			cleanupCancel()
		}
	}()

	commandCtx := contextForHooks()
	var firstErr error
	if err := hooks.RunCommandHooks(commandCtx, req.Hooks, "after", req.Commands, req.WorkspaceRoot, verbose); err != nil {
		iox.Fprintf(os.Stderr, "putnami: hook after error: %v\n", err)
		firstErr = err
		if code == ExitSuccess {
			code = ExitError
		}
	}
	if err := hooks.RunCLIHooks(contextForHooks(), req.Hooks, "after", req.WorkspaceRoot, verbose); err != nil {
		iox.Fprintf(os.Stderr, "putnami: hook after error: %v\n", err)
		if firstErr == nil {
			firstErr = err
		}
		if code == ExitSuccess {
			code = ExitError
		}
	}
	return code, firstErr
}
