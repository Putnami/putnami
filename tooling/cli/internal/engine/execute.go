package engine

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hooks"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/profiler"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// detectServeRunModes reports whether the command set includes serve and/or
// run, which both stream the child workload's output live.
func detectServeRunModes(commands []string) (serve, run bool) {
	for _, cmd := range commands {
		switch cmd {
		case "serve":
			serve = true
		case "run":
			run = true
		}
	}
	return serve, run
}

func executionRenderer(
	req *Request,
	sessStore *workspace_state.SessionStore,
	sink EventSink,
	isServeMode, isRunMode bool,
) jobs.Renderer {
	if sink != nil {
		return sink
	}
	noColor := req.Global.Color != nil && !*req.Global.Color
	return output.NewRenderer(output.Config{
		Output:  req.Global.Output,
		Command: strings.Join(req.Commands, ","),
		Verbose: req.Global.Verbose,
		Debug:   req.Global.Debug,
		Quiet:   req.Global.Quiet,
		NoColor: noColor,
		// serve and run both stream the child workload's output live; run differs
		// only in that it is one-shot (no forced watch — see below).
		ServeMode:      isServeMode || isRunMode,
		CoverageReplay: sessStore.LatestValidationCoverage(req.Commands, nil),
	})
}

func executionSessionRenderer(
	renderer jobs.Renderer,
	session *workspace_state.Session,
	verbose bool,
) jobs.Renderer {
	if session != nil {
		return output.WithSessionRecording(renderer, session, verbose)
	}
	// A machine stream without a real session must never advertise a synthetic
	// artifact id. This also covers intentional no-session lifecycle runs; text
	// and adapter renderers are unchanged by the fallback.
	return output.WithoutSessionArtifact(renderer)
}

// execute runs the planned jobs (normal or watch mode) and owns everything that
// only happens around a real execution: the cache manager, the remote cache, the
// session file, the profiler, successful-run markers, opportunistic GC, and the
// exit-code derivation.
// recordedSessionDocument is the session.json this run records: the run view's
// own projection, plus the two AMBIENT facts that view cannot see.
//
// They are stamped here rather than derived inside machine.Run because neither
// is a property of the work. The parent session id comes from the process
// environment a parent CLI exported into the task that started this run, and
// the selection is the one the plan was computed from — machine.Run is handed
// results, not the request that selected them.
func recordedSessionDocument(
	run machine.Run,
	req *Request,
	sessionID string,
	scheduler, cache any,
) *protocolcli.SessionFile {
	document := run.SessionFile(req.Commands, scheduler, cache)
	document.ParentSessionID = protocolcli.ParentSessionFromEnv(sessionID)
	document.Selection = machine.SessionSelection(req.selection)
	requested := req.Global.Where
	if requested == "" {
		requested = "local"
	}
	document.Placement = &protocolcli.SessionPlacement{Requested: requested, Actual: "local"}
	if req.Portable != nil {
		// The executing side of a portable request IS the remote placement, and
		// the snapshot it runs in carries no Git history: the tree and branch it
		// records are the ones the submitter bound into the request, which the
		// provider materialized byte for byte.
		document.Placement = &protocolcli.SessionPlacement{Requested: "remote", Actual: "remote"}
		source := req.Portable.Request.Source
		if source.Tree != nil {
			document.Tree = &protocolcli.SessionTree{Fingerprint: source.Tree.Fingerprint, Dirty: source.Tree.Dirty, HeadSHA: source.Tree.HeadSHA}
		}
		document.Git = &protocolcli.SessionGit{Branch: source.Git.Branch, Baseline: req.Portable.Request.Selection.Baseline}
		// Provenance is this engine's own statement of the request it executed,
		// from the request itself: the submitting side compares it with what it
		// submitted before adopting the imported session. It is observational
		// and reaches no task key; the digest excludes the negotiated capabilities
		// the provider echoed, so both sides compute the same identity.
		if inputDigest, err := runner.ExecutionInputDigest(req.Portable.Request); err == nil {
			document.Placement.Provenance = &protocolcli.SessionProvenance{
				SourceDigest: source.Digest, InputDigest: inputDigest, Submission: req.Portable.Request.Control.IdempotencyKey,
			}
		} else {
			// Never silent: an importer reads an absent block as an older
			// producer, so the reason the block is missing must be on record.
			iox.Fprintf(os.Stderr, "putnami: provenance not recorded: %v\n", err)
		}
	}
	return document
}

func (e *Engine) execute(
	ctx context.Context,
	req *Request,
	ws *workspace.Workspace,
	selectedProjects []*workspace.Project,
	discovered *extension.DiscoveryResult,
	planned []*jobs.ScheduledJob,
	cache *store.CacheManager,
	sink EventSink,
	resultFinalizers ...func(map[string]*jobs.JobResult),
) SessionResult {
	if !req.previewsOnly() {
		lease, err := store.AcquireScratch(req.WorkspaceRoot)
		if err != nil {
			iox.Fprintf(os.Stderr, "putnami: %v\n", err)
			return SessionResult{ExitCode: ExitError}
		}
		defer func() { _ = lease.Close() }()
		// A hosted run executes with the cache provider it started before the
		// hooks, and fails here, before it records a session, when it cannot.
		// The provider stays open until the session has published its marker:
		// Engine.Run, or the adapter that shares it, closes a provider started
		// before the hooks, and execute the one it loaded here.
		remote, err := hostedRunRemoteCache(ctx, req, ws, discovered, cache)
		if err != nil {
			iox.Fprintf(os.Stderr, "putnami: %v\n", err)
			return SessionResult{ExitCode: ExitError}
		}
		if remote != req.hostedRemote {
			defer remote.Close()
		}
		req.hostedRemote = remote
	}
	return e.executeSession(ctx, req, ws, selectedProjects, discovered, planned, cache, sink, resultFinalizers...)
}

// executeSession runs while execute holds the scratch generation across task
// gaps, capture, and watch iterations. Read-only previews never acquire it.
func (e *Engine) executeSession(
	ctx context.Context,
	req *Request,
	ws *workspace.Workspace,
	selectedProjects []*workspace.Project,
	discovered *extension.DiscoveryResult,
	planned []*jobs.ScheduledJob,
	cache *store.CacheManager,
	sink EventSink,
	resultFinalizers ...func(map[string]*jobs.JobResult),
) SessionResult {
	wsRoot := req.WorkspaceRoot
	extensions := discovered.Extensions
	// Watch re-plans every iteration, so it must plan against the same set the
	// first plan used — otherwise an extension alias would widen to every
	// extension the moment it entered the watch loop.
	planExtensions := req.planExtensions(discovered)
	isServeMode, isRunMode := detectServeRunModes(req.Commands)
	// Retention comes from the merged workspace config (`sessions.keep`), the
	// same document store.ConfigFromWorkspace reads below. Only Prune consults
	// it; every read-only caller keeps the default constructor.
	sessStore := workspace_state.NewSessionStoreWithRetention(wsRoot, workspace_state.RetentionFromWorkspace(req.Config))

	if req.previewsOnly() {
		iox.Fprintf(os.Stderr, "putnami: dry-run mode — showing plan only\n")
		return SessionResult{ExitCode: renderPlan(req, planned, selectedProjects)}
	}

	// Serve commands always run in watch mode (auto-restart on file changes).
	if forcesServeWatch(req, isServeMode) {
		req.Global.Watch = true
	}

	// --watch: hand over to the replan policy. Every iteration comes back through
	// Engine.Run (watch.go), so nothing below this point is watch's lifecycle —
	// the iteration builds its own cache manager, scheduler config and session.
	if req.Global.Watch {
		return e.runWatch(ctx, req, ws, selectedProjects, planExtensions,
			executionRenderer(req, sessStore, sink, isServeMode, isRunMode), isServeMode)
	}

	// The cache manager arrives from Engine.run, which builds it before
	// planning so this execution and a release-set publish's selection keys
	// share one memoized file-hash pass. It is built even under --no-cache,
	// because the memo is not a cache of RESULTS.
	//
	// --no-cache drops it for everything below that means "reuse is allowed" —
	// the remote cache and the opportunistic store GC, both gated on `cache !=
	// nil`. The SCHEDULER keeps it. The flag resolves into the per-job bypass
	// (jobs.NewCacheBypass), which already refuses every lookup, restore,
	// lease, publication and failure replay one task at a time, while the
	// scheduler still needs the local store for the one write a bypassed run
	// may make: deleting a recorded failure its own SUCCESS has just disproved.
	// Taking the store away made --no-cache — the flag a user reaches
	// for precisely to get past a stuck verdict — the single path that provably
	// could not clear it.
	schedulerCache := cache
	if req.Global.NoCache {
		cache = nil
	}

	// Version info, computed once for all jobs from the pre-suspend snapshot.
	versionInfo := req.runVersions(ws)

	// Build scheduler config
	schedCfg := jobs.SchedulerConfig{
		MaxParallel:       req.Global.MaxParallel,
		MaxParallelMode:   req.Global.MaxParallelMode,
		ResourceBudgets:   req.Global.ResourceBudgets,
		CPUBudgetPolicy:   req.Global.CPUBudgetPolicy,
		ContinueOnError:   req.Global.ContinueOnErr,
		Retry:             req.Global.Retry,
		NoCache:           req.Global.NoCache,
		NoCacheProjects:   req.noCacheProjects,
		RetryFailed:       req.Global.RetryFailed,
		Debug:             req.Global.Debug,
		CacheVerification: req.CacheVerification != nil,
		VersionInfo:       versionInfo,
		// Phase 1c/1d's runtime-preparation attribution rides to the session
		// recorder here. The scheduler never adds to it; it only publishes it.
		Preparation: req.preparation,
	}
	attachResultFinalizers(&schedCfg, resultFinalizers)

	// Normal (non-watch) execution
	var session *workspace_state.Session
	var sessionCreateErr error
	// A lifecycle run records no session (Request.WorkspaceLifecycle note 4):
	// `putnami install` is not a build, and a recorded install session would
	// become what `putnami sessions show` reads as the latest run.
	if !req.WorkspaceLifecycle {
		session, sessionCreateErr = sessStore.Create()
		if sessionCreateErr != nil && req.Global.Debug {
			iox.Fprintf(os.Stderr, "[debug] session create failed: %v\n", sessionCreateErr)
		}
		if session != nil && req.Portable == nil {
			// Here, and nowhere later: the recorded fingerprint must describe the
			// tree this run's tasks CONSUME, and the first of them has not started
			// yet. A capture taken at finalize would describe what the run
			// produced — the tree after `lint --fix` rewrote a file — which is a
			// different claim, and not the one a consumer asking "was this gate run
			// on the code I am looking at?" needs. A portable execution records the
			// submitter's bound tree instead (recordedSessionDocument): its snapshot
			// has no history, and a parent directory's repository is not its tree.
			session.CaptureTree(wsRoot)
		}
	}

	if session != nil {
		// Every task subprocess of this run learns which session spawned it, so a
		// CLI a task invokes records itself as nested instead of looking like a
		// second gate. The scheduler delivers it on the execution-only channel
		// (jobProcessEnv): a session id is unique per run, so a value of it
		// reaching a cache key would make every task in the workspace miss
		// forever.
		schedCfg.SessionID = session.ID

		// The recorded plan snapshot is the versioned v2 document, and only that:
		// it was written behind an opt-in, later made the default, and the v1
		// writer was then deleted. Sessions ALREADY on disk stay readable —
		// internal/commands/sessions_helpers.go keeps the v1 reader.
		_ = session.WritePlanV2(machine.SessionPlanFile(session.ID, req.Commands, planned))
	}
	reporter := startSessionReporting(ctx, req, ws, discovered, session)

	// The surface seam: an adapter that owns a renderer passes it, everyone else
	// gets the one the resolved flags ask for. Session attachment happens after
	// plan recording so every real renderer — terminal, machine, cloud, or MCP —
	// feeds the same complete sanitized events.jsonl artifact. A machine renderer
	// shares that recorder with its bounded live selector; other renderers are
	// wrapped without changing their display behavior.
	renderer := executionSessionRenderer(
		executionRenderer(req, sessStore, sink, isServeMode, isRunMode),
		session, req.Global.Verbose || req.Global.Debug,
	)

	prof := profiler.New(req.Global.TraceProfile != "")

	// Enable the remote build cache when configured (.putnami/cache.json or the
	// PUTNAMI_CACHE_* env overrides). Local caching must be on for it to apply.
	// Config load happens before the run timer starts, so its cost is timed here
	// and attributed to the cache stats — otherwise it is the invisible "before
	// negotiate" overhead the session clock misses. The bearer is resolved lazily
	// inside the first negotiate, so its cost falls within the run timer.
	// A hosted run reads the remote cache execute resolved (hostedRunRemoteCache).
	var cacheSetup time.Duration
	remote := req.hostedRemote
	if cache != nil && !runcredential.Hosted() {
		setupStart := time.Now()
		var cacheNotice string
		remote, cacheNotice = jobs.LoadRemoteCache(ctx, wsRoot, extensions, discovered.Skipped, store.CacheTrust(req.Global.CacheTrust), remoteCacheOptions()...)
		// Per the missing-cloud policy, a configured-but-unusable remote cache must
		// never degrade to local-only silently — surface the one-line notice.
		if cacheNotice != "" {
			iox.Fprintln(os.Stderr, cacheNotice)
		}
		cacheSetup = time.Since(setupStart)
		remote.RecordSetup(cacheSetup)
		// The scheduler's end-of-run Stop leaves the provider session open so the
		// post-run success marker (recordSuccessfulBuild, below) can still reach it.
		// Release it here, after this function returns and the marker is published.
		defer remote.Close()
	}

	// Wire scheduler/recovery audit signals into the v2 artifact and profiler.
	// Canonical task ends are recorded by renderer.JobComplete, so the recorder
	// ignores the scheduler's legacy job:end projection.
	var sessionEvents jobs.SessionEventHandler
	if session != nil || prof.IsEnabled() {
		sessionEvents = func(record jobs.SessionRecord) {
			if recorder, ok := renderer.(interface {
				RecordSessionEvent(jobs.SessionRecord)
			}); ok {
				recorder.RecordSessionEvent(record)
			}
			if prof.IsEnabled() {
				recordProfilerSessionEvent(prof, record)
			}
		}
	}

	// The single execution site of the whole CLI (ADR 0001 §3): construction,
	// remote-cache wiring and session wiring are one call, so no adapter can
	// hold a half-configured scheduler.
	// Framework resource/cache globals shape extension execution but not its
	// content identity. Deliver only selected overrides through the task context
	// while keeping Request.CommandParams unchanged for cache keys and markers.
	extensionParams := make(extension.ParamMap, 3)
	if req.Global.MaxParallel > 0 {
		extensionParams["max-parallel"] = strconv.Itoa(req.Global.MaxParallel)
	} else if req.Global.MaxParallelMode != "" {
		extensionParams["max-parallel"] = req.Global.MaxParallelMode
	}
	if req.Global.NoCache && req.Global.NoCacheExplicit {
		// Only a --no-cache the USER typed is forwarded. The host also
		// sets NoCache for itself — every lifecycle job, every non-serve watch
		// iteration — and that choice is about how the HOST runs the job, not an
		// instruction to the extension: forwarding it made the Cloud extension's
		// workspace-install read `cache: false` and skip writing
		// .putnami/cache.json.
		//
		// Both spellings of one fact. `no-cache` is the framework's own
		// name for the global; `cache: false` is what an SDK flag parser produces
		// from a `--no-cache` token (tooling/extension-sdk/cli.ParseFlags) and
		// therefore the only name a manifest-declared `cache` boolean reads. The
		// host consumes the token before the extension can parse it, so it has to
		// deliver the value under the name the extension asks for; delivering only
		// `no-cache` is how an explicit `--no-cache` ran WARM inside the extension.
		// Both stay OUT of cache keys and run markers: ExtensionParams is the
		// execution-only overlay (RunRequest.ExtensionParams), which is also why
		// this is not gated on a declared flag surface the way the interactive
		// path's copy is — the overlay is run-wide, and a task that never declared
		// `cache` ignores it exactly as it already ignores `no-cache`.
		extensionParams["no-cache"] = true
		extensionParams["cache"] = false
	}

	// StoreBudget is the in-run half of the store budget;
	// boundMachineCachesAfterRun holds the post-build half, and both are off
	// with caching (a nil cache, or a relocated verification store, arms no
	// budget). It is armed in this literal, as the run starts, because the
	// instant it is created is the run's protection boundary: an in-run
	// collection pass spares every entry used at or after it.
	result := jobs.RunPlan(ctx, jobs.RunRequest{
		Workspace:                      ws,
		Plan:                           planned,
		CommandParams:                  req.CommandParams,
		ExtensionParams:                extensionParams,
		Config:                         schedCfg,
		Renderer:                       renderer,
		Cache:                          schedulerCache,
		Remote:                         remote,
		StoreBudget:                    store.NewRunBudget(wsRoot, store.ConfigFromWorkspace(req.Config), cache.Store()),
		SessionEvents:                  sessionEvents,
		OpeningSessionEvents:           req.selectionEvidence.openingEvents,
		ProcessCapabilityAuthorization: req.processCapabilityAuthorization,
		InternalJobs:                   req.internalJobs,
	})
	if recordErr := output.SessionRecordingError(renderer); recordErr != nil {
		iox.Fprintf(os.Stderr, "putnami: session recording incomplete: %v\n", recordErr)
	}

	if prof.IsEnabled() {
		if err := prof.Write(req.Global.TraceProfile); err != nil {
			iox.Fprintf(os.Stderr, "putnami: write profile: %v\n", err)
		} else if req.Global.Verbose {
			iox.Fprintf(os.Stderr, "  Profile written to %s\n", req.Global.TraceProfile)
		}
	}

	// Surface the scheduler tuning decision and DAG wait metrics. Verbose/debug
	// summarize the selected parallelism; debug and --trace-profile add the
	// critical path and ready wait so profiling needs no external trace analysis.
	reportSchedulerTuning(os.Stderr, &req.Global, result.Tuning)

	// Surface the remote build cache's payoff (hits, time saved vs spent). The
	// summary is the only signal that the cache helped, so it shows whenever the
	// cache did work, not just under verbose.
	reportCacheSummary(os.Stderr, &req.Global, result.Cache)

	// The size of the change and the mixed-intent warning come last, where a
	// reader of the gate looks for its outcome. Both are information: the exit
	// code is the run's alone.
	iox.Fprint(os.Stderr, req.selectionEvidence.closingNotes)

	if result.Success && shouldRecordSuccessfulBuild(req) {
		recordSuccessfulBuild(sessStore, ws, wsRoot, req.Commands, req.runMarkerParams(), req.Global.Debug, remote, observedRunMarkerSHA(req))
	}

	if session != nil {
		// The recorded session is the v2 document, and only that.
		// cacheSummary keeps the local-only run's typed-nil snapshot out of the
		// any-typed member, which would otherwise defeat omitempty and write
		// "cache": null instead of omitting it.
		run := machine.RunFrom(result.Session, planned, result.Results)
		finalizeErr := session.FinalizeV2WithCoverage(
			recordedSessionDocument(run, req, session.ID, result.Tuning, cacheSummary(result.Cache)),
			wsRoot, req.Global.Baseline, req.CommandParams,
			planned, result.Results, result.Session, versionInfo.Primary())
		finishSessionReporting(reporter, session.ID, finalizeErr)
		// `latest` names the user's last build, and every `--session latest`
		// reader resolves it. An adapter whose runs are not the user's build must
		// therefore stay OUT of the rotation, or an agent tool call landing after
		// a human's build silently becomes the session those readers open — and
		// session metadata carries no origin field, so nobody can tell
		// afterwards. The session itself is still written and still listed by
		// `putnami sessions`; only the pointer is withheld.
		if !req.EphemeralSession {
			_ = sessStore.UpdateLatest(session)
		}
		_ = sessStore.Prune()
		// The run's contracted bilan, synthesized from the same run
		// view the session was written from and recorded beside it. It comes last
		// and is best-effort like FinalizeV2 above: the report is a projection,
		// never an input, so a missing one is a missing file and never a failed run.
		_ = writeRunReport(wsRoot, session, run, req, result.Tuning, result.Cache)
		// The spec gate's decided record, persisted beside the session
		// documents so `specs verify --session` replays what the finalizer
		// decided. Best-effort like everything here: the sanction already
		// happened inside the reduction, so losing the audit file never changes
		// the verdict.
		if req.specGate != nil {
			_ = session.WriteSpecVerification(req.specGate.record)
		}
	}

	boundMachineCachesAfterRun(req, ws, extensions, cache != nil)

	return SessionResult{
		ExitCode: sessionExitCode(result.Session, func() (int, bool) {
			// `putnami run` forwards the workload's exact exit code so callers can
			// assert specific gate codes (e.g. a migration returning 3). The run job
			// reports it in its result data; the generic failure code stands when a
			// build step failed before the workload ran.
			if !isRunMode {
				return 0, false
			}
			return runForwardedExitCode(planned, result.Results)
		}),
		Session: result.Session,
		Results: result.Results,
	}
}

// boundMachineCachesAfterRun is the post-run half of every machine cache's
// budget: the build store, the artifact store and each extension's own
// language caches. The build store's in-run half is RunRequest.StoreBudget;
// this pass also records the usage the next run's in-run budget starts from.
// cachingOn is false under --no-cache.
func boundMachineCachesAfterRun(req *Request, ws *workspace.Workspace, extensions []*extension.ExtensionDescription, cachingOn bool) {
	// Opportunistically bound the shared global store's disk use. Throttled to
	// at most once per machine per hour, non-blocking (it never waits for a busy
	// store), and best-effort. While it evicts from a store it holds that
	// store's exclusive lock, in sections of about 200 ms, and other sessions'
	// lookups, publishes and restores on it wait for those. Skipped when caching
	// is disabled (the user opted out).
	if cachingOn {
		store.MaybeOpportunisticGC(req.WorkspaceRoot, store.ConfigFromWorkspace(req.Config))
		artifactstore.MaybeOpportunisticGC(store.ResolveArtifactStoreRoot(req.WorkspaceRoot))
	}
	// A lifecycle run triggers no opportunistic machine-cache collection on exit
	// (Request.WorkspaceLifecycle note 4): `putnami install` populates those
	// caches, and GC-ing them on the way out is both new behavior and the wrong
	// moment for it.
	if !req.WorkspaceLifecycle {
		// The language caches remain active even when Putnami's action cache is
		// disabled, so they are bounded independently — but core no longer knows
		// which caches those are. It knows only which extensions declare a
		// `cache-gc` command. Throttled to at most once
		// per extension per hour and started detached, so a finishing run never
		// waits on a cache walk.
		hooks.MaybeOpportunisticCacheGC(ws, extensions, hooks.OpportunisticCacheGCInterval)
	}
}

// cacheSummary is the v2 session document's cache member. The branch is the
// point: boxing a local-only run's typed-nil snapshot into an any-typed member
// defeats omitempty and records "cache": null instead of omitting it, so the
// nil has to be returned untyped. (The v1 session writer guarded the same trap
// with a conditional SetCache call; both it and that path are deleted, and this
// is the one guard left.)
func cacheSummary(cache *jobs.CacheStatsSnapshot) any {
	if cache == nil {
		return nil
	}
	return cache
}

// sessionExitCode derives the process exit code from the canonical reduction.
// It is the single derivation: the run verdict it reads is
// SessionResult.Success, the same strict predicate that decides
// SchedulerResult.Success and the recorded session's stats.
//
// forwarded supplies `putnami run`'s workload code, consulted only once the run
// is already a failure.
//
// Note the ordering: an ABORT wins over a failure here, because the process code
// has to say "someone stopped this" (130) rather than bucket a killed run with a
// genuine build failure. That convergence has SHIPPED: the v1 envelope, which
// deliberately ordered it the other way so a machine reading the object saw the
// failures named, is gone, and the v2 envelope agrees
// with this function — status "aborted", exitCode 130 — while still counting and
// listing the failures the run collected (machine.Run.Summary). One reduction,
// one precedence, on both the process and the wire.
func sessionExitCode(session *jobs.SessionResult, forwarded func() (int, bool)) int {
	if session.Aborted {
		return ExitSignalReceived
	}
	if session.Success() {
		return ExitSuccess
	}
	if forwarded != nil {
		if code, ok := forwarded(); ok {
			return code
		}
	}
	return ExitError
}

func attachResultFinalizers(schedCfg *jobs.SchedulerConfig, resultFinalizers []func(map[string]*jobs.JobResult)) {
	if len(resultFinalizers) == 0 {
		return
	}
	schedCfg.FinalizeResults = func(results map[string]*jobs.JobResult) {
		for _, finalize := range resultFinalizers {
			if finalize != nil {
				finalize(results)
			}
		}
	}
}

// runForwardedExitCode returns the first non-zero exit code reported by a `run`
// command's workload, so the CLI process can exit with it. It returns
// (0, false) when no run workload reported a non-zero code (e.g. an upstream
// build step failed before the workload ran), leaving the caller to use the
// generic failure code.
func runForwardedExitCode(planned []*jobs.ScheduledJob, results map[string]*jobs.JobResult) (int, bool) {
	for _, job := range planned {
		if job.CommandName() != "run" {
			continue
		}
		res, ok := results[job.Key()]
		if !ok || res == nil || res.Data == nil {
			continue
		}
		raw, ok := res.Data["exit-code"]
		if !ok {
			continue
		}
		if code := exitCodeFromData(raw); code != 0 {
			return code, true
		}
	}
	return 0, false
}

// exitCodeFromData coerces a JSONL result-data value (decoded as float64 for
// numbers) into an int exit code.
func exitCodeFromData(raw any) int {
	switch v := raw.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return 0
	}
}
