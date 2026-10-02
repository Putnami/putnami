package cli

import (
	"go/ast"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Complexity ceilings for the five packages the epic rebuilt.
//
// The epic's deletions (one execution engine, one result model, one machine
// contract, one lock format) all shrank these packages. Nothing keeps them
// shrunk. Every one of them is somewhere a slice would naturally add "just one
// more helper", and the sum of those is how the five run loops and ~20 result
// shapes the epic measured got built the first time — never by a decision,
// always by an increment.
//
// # What is measured, and why these two numbers
//
// Per unit: the number of production FILES and the number of top-level
// FUNCTION DECLARATIONS (methods included). Both are read from the AST, so
// neither moves when a doc comment grows — which matters in this tree, where
// the prose-to-code ratio is deliberately high and a line-count ceiling would
// fire on every well-explained change and be raised until it meant nothing.
//
// Files catch SPRAWL (a package growing new corners); functions catch the
// increment (a package growing new behavior in place). A refactor that splits
// one large file into two, or extracts a helper, is a real movement of both —
// and is exactly the kind of movement that should cost one line of review here.
//
// # A third quantity, for the units that need it: CONCENTRATION
//
// Files and functions are both TOTALS, and a total cannot see distribution. A
// unit that merges its five files back into one keeps the same function count
// and reports FEWER files — which a ceiling-only pin passes — while recreating
// the single mixed-concern file that made a change expensive to review in the
// first place. perFileFuncs pins the unit's LARGEST file in that same
// AST quantity, and it is opt-in rather than universal: a per-file cap on a
// unit whose file boundaries are incidental would fire on ordinary work and
// teach nobody anything, while on a unit whose files ARE its invariants it
// fails exactly when a second invariant moves into one of them.
//
// # Ceiling-only, on purpose
//
// Growth fails, shrinkage passes. These are pins on complexity the epic exists
// to have REMOVED, so failing CI for a reduction would fail it for succeeding
// (the same reasoning as structural_baseline_test.go's ceilingOnly pins). A
// slice that legitimately grows a unit raises its number here and says why in
// the `why` — that sentence is the deliverable, not the integer.
//
// # Units, not packages
//
// The planner and the scheduler are file groups inside internal/jobs rather
// than packages of their own, because A6a made the whole execution surface
// package-private (engine_boundary_test.go). Splitting them out to satisfy a
// test would reopen that boundary, so the unit is a path PREFIX and the
// package stays whole.

// complexityCeiling is one unit's pinned size. Both counts are ceilings.
type complexityCeiling struct {
	// unit is the name used in failures.
	unit string
	// prefix selects the unit's production files by module-relative path.
	prefix string
	// files is the current production file count.
	files int
	// funcs is the current top-level function declaration count, methods
	// included.
	funcs int
	// perFileFuncs, when non-zero, caps the top-level function declarations of
	// the unit's LARGEST single file. files and funcs bound how big a unit is;
	// this bounds how CONCENTRATED it is, which the two totals cannot see.
	// Opt-in: it is set by the units whose file boundaries carry meaning, and
	// the number is the measured maximum rather than a round one.
	perFileFuncs int
	// why states what growth would mean for this unit specifically.
	why string
}

var complexityCeilings = []complexityCeiling{
	{
		unit:   "engine",
		prefix: "internal/engine/",
		files:  24,
		funcs:  163,
		why: "RAISED 157→163 (ADR 0057): a run that may publish reads its ancestry at process start, " +
			"before the first-use bootstrap or an install runs repository code, and Engine.Run reuses " +
			"that read. ancestry.go holds CaptureAncestry, the one entry adapters call, capturedAncestry, " +
			"which hands Engine.Run the snapshot only for the same workspace, mayPublish, the predicate " +
			"both share, and Position, which the open operation's ancestry statement reads. engine.go " +
			"holds attachReleaseSet, which binds the open operation's plan tuple and barrier before the " +
			"barrier nodes attach, and releaseSetExecution, which appends the release finalizer and gives " +
			"each publication job its private outbox; both keep Engine.run inside its statement budget. " +
			"The upload and session logic lives in internal/jobs, not here. " +
			"RAISED 23/148→24/157 (cli/provider-publication): a run that may publish reads its " +
			"bound commit's ancestry before the first hook, because repository code can rewrite refs, " +
			"replace refs and grafts afterwards. ancestry.go holds AncestrySnapshot (five nil-safe " +
			"readers), captureAncestrySnapshot and readsAncestry; only Engine.Run sees the point before " +
			"the hooks. engine.go holds keyingPlan and refusesUnauthorizedPublication, which refuse a " +
			"bound request without invocation.publication on its keying plan, before the release-set " +
			"preparation can start a provider; only this unit holds that plan, and keyingPlan keeps " +
			"Engine.run inside its statement budget. No flag, no mode. " +
			"RAISED 146→148 (a missing extension before the repository refusal): " +
			"reportRepositoryRefusal reports a selected command whose extension is not installed " +
			"with the missing-extension guard's own message instead of the refusal, because that " +
			"install is the first remedy; discoverDeclaredExtensions resolves the extensions for " +
			"that question before any hook, as startHostedRemoteCache does, and only for a " +
			"workspace that declares a registry extension. Run gains no branch; files stay at 23. " +
			"RAISED 145→146 (one source-state answer per run): newRunCacheManager builds the run's " +
			"one cache manager out of run, carrying the cache-verification store override with it, " +
			"and records the workspace root as managed when this run's own tree-state capture " +
			"succeeded, so no execution key and no version stamp of the run asks git again. run " +
			"loses a branch; files stay at 23. " +
			"RAISED 144→145 (a workspace Git does not manage): requireRepository refuses a " +
			"`publish` or a `deploy` that executes at a root with no repository, before any hook, " +
			"and asks git only when the tree-state capture found no commit. impactedFailureEndsRun " +
			"takes the --impacted-strict case of selectProjects in place and adds the same refusal " +
			"for --impacted, so the selection stage gains no branch. Both print one line that names " +
			"git and add no stage; files stay at 23. " +
			"RAISED 141→144 (ADR 0055 part 4): a hosted `build` whose first-use bootstrap " +
			"runs an install must start its cache provider after the install's credentialed " +
			"workspace-fetch and before the install's first repository code, and the run that follows " +
			"must read the remote cache through that provider instead of starting a second one after " +
			"the hooks. run_credential.go holds HostedRemoteCache.Start (the once-only start the " +
			"bootstrap and Engine.Run share), HostedRemoteCache.Close, and readsHostedRemoteCache (a " +
			"request that reads no remote cache neither starts nor reuses it). The adapter owns the " +
			"value; the engine only reads it. No flag, no stage; files stay at 23. " +
			"RAISED 139→141 (ADR 0055): a hosted run hands its credential to no process " +
			"that starts after repository code, so the cache provider starts before the first hook. " +
			"run_credential.go holds startHostedRemoteCache, which Engine.Run calls before the before-hooks, " +
			"and hostedRunRemoteCache, which gives execute that provider and fails the run, before it " +
			"records a session, when a cache configured only after the hooks would start one late. Only " +
			"Engine.Run sees the point before the hooks. No flag, no mode; files stay at 23. " +
			"RAISED 22/138→23/139 (ADR 0055): run_credential.go holds remoteCacheOptions, " +
			"which hands the hosted run's credential to the cache provider over its RPC. The engine " +
			"loads the remote cache in two places, and both pass the option. It adds no stage. " +
			"RAISED 21/134→22/136 (ADR 0042): impact_scope.go holds the one new plan " +
			"stage, narrowToTaskScopes, and its extension matcher. An --impacted selection can hold " +
			"a project for the tasks of a changed extension only; the planner plans the project " +
			"whole, and the narrowing — keep the scoped extensions' jobs, publish/deploy, then the " +
			"predecessor closure — needs the plan and the request's selection evidence, which only " +
			"this unit holds. It is selection finishing on the plan, not a caller policy: no flag, " +
			"no mode, the same stage on every adapter and on the executing side of a portable " +
			"request. " +
			"RAISED 20/127→21/133 (`putnami compose`): serve_withhold.go holds the one " +
			"new stage, Request.WithholdServeSteps — the serve pipeline's finite steps keep running " +
			"through this engine (planning, guards, cache, session), and only each selected " +
			"project's terminal uncacheable serve step is handed back to the composition that " +
			"supervises it. Six small functions keep Engine.run and executeSession inside their " +
			"complexity and statement budgets rather than adding branches to them. " +
			"the ONE place a run is assembled: selection, planning, preflight, cache lifetime, the " +
			"session file, run markers and the canonical result (ADR 0001 §3). The epic collapsed " +
			"five run loops into it, so this package is where every adapter's special case wants to " +
			"land — the MCP one, the watch one, the lifecycle one. Growth here is usually a caller's " +
			"policy migrating INTO the engine as a flag, which is the shape that made the five loops " +
			"diverge in the first place. A new stage is a real decision; raise this and name it. " +
			"RAISED ONCE, from 13/65: workspace_probe.go adds the " +
			"snapshot-first workspace-probe STAGE — the second of the three lifecycle primitives that " +
			"epic's contract names, beside runtime preparation. It is a stage and not a flag: before " +
			"selection it decides whether this run resolves the workspace from the persisted index " +
			"(zero extension processes) or from a provider probe, and no caller can express that " +
			"decision. It is also the LAST run stage the epic adds — the third primitive extends " +
			"task outputs, not run assembly — and a later slice gives functions BACK here as the core " +
			"language parsers it replaces are deleted. RAISED A SECOND TIME, from 14/73: " +
			"context_map.go attaches the workspace map to a completed build session. It is NOT a " +
			"caller's policy migrating in as a flag — the policy (which mode, what a stale map " +
			"means, what the failure says) lives in internal/mapgen, and this file holds only the " +
			"ATTACHMENT: the finalizer closure, the session-succeeded predicate the reduction cannot " +
			"answer before finalizers run, the synthetic failed result, and the two stderr notices. " +
			"It has to live here because a core-owned artifact cannot be a plan node (only extension " +
			"manifests declare those) and because internal/engine is forbidden from importing " +
			"internal/commands (provider_vocabulary_ratchets_test.go), so the seam is the only place the " +
			"work can attach. RAISED A THIRD TIME, from 15/78: the run " +
			"report (report.go's writeRunReport and its origin/cadence/git readers) attaches the " +
			"contracted report.json after FinalizeV2. Like the workspace map it is an attachment, " +
			"not a policy: the projection lives in internal/machine, the store in " +
			"internal/workspace_state, and the engine holds only the best-effort call at the one " +
			"seam where the settled run, the session interval and the request's origin meet. " +
			"Files stay at 15 — the attachment lives in the existing report.go. RAISED A FOURTH " +
			"TIME, from 15/83: dropUndryableSideEffectJobs in plan.go is the safety half " +
			"of `publish --dry-run` — under an executing dry-run it excludes side-effecting jobs " +
			"that never declared the flag they would have to interpret. It is plan filtering, not " +
			"a caller's policy: no adapter can see the resolved JobDef flags it reads. Files stay " +
			"at 15. CORRECTED from 84 to 85: the parent revision already declared 85 " +
			"engine functions while retaining the older pin. Cache verification adds request data " +
			"only — no engine function or stage — so this records the existing AST count instead of " +
			"charging an unrelated function to the adapter. RAISED A FIFTH TIME, from 15/85: " +
			"reportUnservedSDDCommands in plan.go names the extension that now serves " +
			"features/specs/architecture/contracts when a workspace selected one of them and no " +
			"loaded extension provides it. It belongs to the zero-jobs diagnosis this file already " +
			"owns beside reportMissingExtensions, and it cannot be a caller's policy: only the " +
			"planner knows a command produced no jobs AND that no manifest declares it. Files stay " +
			"at 15. This raises 86→88 for the two renderer/session setup projections extracted from " +
			"execute: one constructs the selected surface, the other attaches a real session recorder " +
			"or its explicit creation-failure fallback. They keep the run seam below its complexity " +
			"ceiling and add no execution stage or caller policy. RAISED A SIXTH TIME, from 15/88: " +
			"spec_gate.go attaches the executable-spec verification finalizer to sessions " +
			"containing test. It is context_map.go's exact shape, for that file's exact reason: the " +
			"policy (what to collect, how to join, when a group blocks, what the sanction says) " +
			"lives in internal/specgate and protocols/features, and the engine holds only the " +
			"attachment — the factory's off-everywhere/no-test refusals, the incomplete-session " +
			"guard that keeps a failed run from acquiring secondary missing-report failures, and " +
			"the per-project synthetic failed result the canonical reducer sanctions. It cannot be " +
			"a plan node (fixed decision 9: no verify DAG job in v1) and cannot live in a command " +
			"(every adapter must receive identical behavior through Engine.Run), so the seam is " +
			"the only place the work can attach. One file, one function. This raises 89→91 for " +
			"renderPlan, the one human/machine preview projection shared by --plan and --dry-run, and " +
			"machinePreviewRequested, which keeps selection/planning no-op prose off explicit JSON " +
			"stdout before that projection runs; neither adds an execution stage. RAISED A SEVENTH " +
			"TIME, from 16/92: session_baseline.go is a new run STAGE, and it is named " +
			"here because it is one. Between planning and execution — the last point at which " +
			"nothing has been written, since a cache restore happens when a node runs — it captures " +
			"the workspace's generated-artifact closures and publishes an immutable reference to " +
			"the planned tasks that declared a workspace-scoped read and no cache. It cannot be a " +
			"caller's policy or a plan node: no adapter and no manifest can express an ordering " +
			"that precedes every node, and a verifier that takes its own snapshot when it starts " +
			"reads the tree the session's own build just rewrote. " +
			"The count is the stage's walk, its digest, its capture record and its publication, " +
			"plus two predicates extracted OUT of run to keep it under the cyclomatic and length " +
			"ceilings it was already at. " +
			"RAISED AN EIGHTH TIME, from 17/104 (the file count is 18: this raise and the seventh each add one): spec_gate_cache.go is the CACHE half of " +
			"the spec gate, and it is here for the same reason spec_gate.go is. The policy — when " +
			"to ask, which projects may be asked, what an answer means — stays in " +
			"internal/specgate, whose package doc forbids it from importing the CLI's planner, " +
			"store or extension model. Turning a candidate project into cache bytes needs all " +
			"three, so the lookup sits where they already are, and it cannot move to a package of " +
			"its own: engine_boundary_test.go admits a jobs.Plan reference from internal/engine " +
			"ONLY, and a new package would need a new ADR to break that boundary. The eight " +
			"functions are one constructor, the interface method, three projections of it (the " +
			"all-unconsulted answer, the per-candidate consulted-versus-unreachable rule, and the " +
			"entry-descriptor read that locates the captured report), the memoized version resolve " +
			"that keeps a green run from paying for a git read, one path renderer, and — in " +
			"spec_gate.go — the stderr rendering of the gate's non-blocking findings, which may " +
			"not travel as a synthetic result because the reducer turns every result it is handed " +
			"into a verdict. None of it is a caller policy: no adapter can express \"recover an " +
			"unselected dependency's observation from its own cache entry\", and none can see the " +
			"keys it derives. RAISED A NINTH TIME, from 18/112: describeProjectSelection " +
			"and summarizeProjectIDs render what a FAILING no-op refused. Both empty-set guards " +
			"wrote their only explanation to the human stdout stream, which machine output " +
			"suppresses, so `--output=json` exited 2 with nothing on either stream and a caller " +
			"could not tell a refused selection from a crash. They are the same class as " +
			"reportUnservedSDDCommands and reportMissingExtensions, which this unit already owns " +
			"for the zero-jobs case: only the engine knows what the selection asked for and what " +
			"planning resolved it to, so no adapter can produce the sentence. Pure projections of " +
			"data the guards already hold — no stage, no flag, no policy. Files stay at 18. " +
			"RAISED A TENTH TIME, from 18/114: resolveNoCacheProjects turns the raw " +
			"--no-cache-projects selector into the project ids the scheduler consults per task. " +
			"It is in selection.go because it IS selection: the same shared FilterProjects parser, " +
			"the same workspace, the same ExitUsage report an unmatched --projects already makes. " +
			"It is not a caller policy migrating in as a flag — no adapter can resolve a selector " +
			"against a workspace it has not loaded, and the resolved set is execution policy that " +
			"reaches no cache key, run marker or task param. One function, files stay at 18. " +
			"RAISED AN ELEVENTH TIME, from 18/115: recordedSessionDocument stamps the two " +
			"AMBIENT facts onto the recorded session document — the parent session that spawned " +
			"this run, and the selection the plan was computed over. It is an earlier raise's exact shape and " +
			"for that raise's exact reason: a projection extracted OUT of execute to keep the run " +
			"seam under the length ceiling it had reached, adding no execution stage and no caller " +
			"policy. Neither fact can be derived in internal/machine, which is handed results and " +
			"never the request that selected them, nor expressed by an adapter: the parent id is " +
			"read from the process environment an outer CLI exported into the task that started " +
			"this run. One function, files stay at 18. This raises 116→117 for the scratch " +
			"lifetime boundary: execute holds one shared generation lease around executeSession, " +
			"including task gaps, capture and watch iterations, while previews remain unleased. " +
			"This is a run-wide safety invariant that no individual task or adapter can own. The " +
			"wrapper keeps the existing execution body within its length ceiling; no new flag or " +
			"adapter-specific policy is introduced. ADR 0031 records the lifecycle. " +
			"This raises 18/117 to 19/118 for placement admission immediately after the engine's " +
			"post-bootstrap extension discovery: absent optional providers retain the one local " +
			"lifecycle; present or ambiguous providers fail closed before runtime preparation. " +
			"Only this seam sees the authoritative discovered provider set, so an adapter cannot " +
			"own the check. placement.go adds one function, no planner or scheduler; ADR 0032 " +
			"records the foundation and the still-missing portable execution admission. ADR 0033 " +
			"raises 19/118 to 20/122 for native session reporting: one attachment file with " +
			"start, finish, the normal extension resolver and retained-session replay. Only the " +
			"engine owns the recorded graph lifecycle, including failure and cancellation; an " +
			"extension task cannot observe its own enclosing graph's finalization. Transport, " +
			"process supervision, cursors and credentials stay in internal/sessionreporter. " +
			"No second execution loop or consumer policy enters the engine. LOWERED, from 20/122 " +
			"to 19/112: session_baseline.go — the seventh raise's stage — is deleted. " +
			"Its only consumer, the clientgen guard's render comparison, no longer exists: the " +
			"question it captured evidence for is answered by the scheduler on each generator " +
			"task's declared output (jobs/task_drift.go, protocols/extension ADR 0004), at the " +
			"one moment the pre-write bytes are still there. ADR 0034 records the decision. " +
			"RAISED A TWELFTH TIME, from 19/112 to 20/124 (ADR 0032): portable.go is the " +
			"portable-execution SEAM, and it is named here because it is one. Between the " +
			"production preflight and execution — after the final plan, the last point at which the " +
			"request is complete and nothing has run — a run whose placement resolved to a runner " +
			"provider leaves the engine as a typed request (invocation with typed parameters, frozen " +
			"selection, expected plan through the ChangePlan projection, pinned environment) and " +
			"comes back as an imported canonical session; the executing side of the same seam " +
			"plans the frozen selection (selectFrozenProjects) and refuses a re-planned graph that " +
			"differs from the expected one (validateExpectedPlan) before anything is scheduled. No " +
			"adapter can own either half: only the engine holds the final plan, the resolved " +
			"selection with its evidence, and the discovered provider set. Transport, process " +
			"supervision and import stay in internal/runnerprovider; no planner, scheduler or " +
			"reducer is added. The twelve functions are the seam, the unsupported-shape refusal, " +
			"the request projection, the plan projection and its executing-side validation with " +
			"its difference renderer, the environment/version/resource/cwd projections and the " +
			"frozen selection stage. " +
			"This raises 124→127 for the three functions a session that names a gate command " +
			"beside a channel publish needs: it resolves TWO selections, because it asks two " +
			"questions — which members the head republishes, and which projects the caller asked " +
			"to verify. resolveReleaseSetSelections is the seam (and holds the whole-workspace " +
			"override the run body used to spell inline), verificationSelection is the second " +
			"projection, and adoptVerificationEvidence binds the evidence for the half the " +
			"coordinator did not decide. They belong to this unit because they ARE selection: the " +
			"same stage, run once more on a copy of the request. No adapter can own them (only " +
			"the engine sees the release-set mode at that point), they add no stage and no flag, " +
			"and two of the three exist so Engine.run keeps its cyclomatic ceiling instead of " +
			"growing two more branches. Files stay at 20. " +
			"This raises 133→134 for boundMachineCachesAfterRun, the post-run half of every " +
			"machine cache's budget (build store, artifact store, extension cache-gc), moved " +
			"verbatim out of executeSession. executeSession sat at the funlen cap " +
			"(.golangci.yml), and the in-run store budget's single arming site " +
			"(RunRequest.StoreBudget) had to attach at a named seam rather than extend it. It adds " +
			"no stage, no flag and no branch: the same two guarded blocks, in the same order, at the " +
			"same point after the session is recorded. " +
			"ADR 0044 raises 136→138 for the two matchers narrowToTaskScopes now " +
			"needs, both inside impact_scope.go: jobInScope reads one scope entry " +
			"([<extension-project-id>#]<command>~<step>) and jobTaskScope derives a planned job's " +
			"task scope from its own command and step rather than from its display name, which " +
			"namespacing rewrites. extensionInScope is renamed extensionProjectIs and is unchanged. " +
			"The stage is still the one narrowToTaskScopes already was; no flag, no mode, no second " +
			"loop, and files stay at 22",
	},
	{
		unit:   "planner",
		prefix: "internal/jobs/plan",
		files:  10,
		funcs:  92,
		why: "internal/jobs/plan*.go: the single planner entrypoint (jobs.Plan), its dependency, " +
			"activation, matching and resource passes, plus the plan contract and its validation. " +
			"Planning is pure — selection in, typed plan out — and that purity is what lets the " +
			"plan be validated, cached and replanned by watch without re-deriving anything. Growth " +
			"here is usually execution knowledge leaking backwards into the plan (a scheduler " +
			"concern computed early because it was convenient), which is precisely what makes a " +
			"plan stop being reviewable. RAISED ONCE, from 8/72: " +
			"plan_producers.go moves a check the RUNNER used to perform into the plan, and the " +
			"movement is the point. `traits.preflight: config-schema` made RunJob stat a project's " +
			"infra markers and parse its Go imports mid-run to decide whether an artifact core does " +
			"not own had to exist; the replacement reads a declaration the consuming task already " +
			"makes (a required `from: \"task\"` input port) and answers from the plan alone. It is " +
			"knowledge moving FORWARD into planning, not execution knowledge leaking backwards — " +
			"the direction this ceiling exists to protect — and it is paid for with more than it " +
			"costs: the same slice deleted internal/jobs/config_preflight.go (7 functions, " +
			"including a Go AST walker) and three provider-gate files from internal/extension. The " +
			"four functions are the entrypoint, the diagnostic pass, the per-port verdict, and the " +
			"producer identity they share. RAISED A SECOND TIME, from 9/76: " +
			"ScheduledJob.EffectiveTimeoutMs is CONSOLIDATION, the inverse of what this ceiling " +
			"guards against. Four execution sites — the armed context, the coalescing budget, the " +
			"finalizer budget, the batch group budget — each read job.JobDef.TimeoutMs directly, so " +
			"per-project `tasks.<job>.timeoutMs` tuning would have had to be re-derived four times " +
			"and could differ between them. The accessor adds no PASS and computes nothing at plan " +
			"time: it resolves lazily on call, so it is one more question a plan node can ANSWER, " +
			"not one more thing planning DOES. It is paid for at the other end — the same change " +
			"deletes the JobDefinition copy batchLeader made to widen a deadline. RAISED A THIRD " +
			"TIME, from 9/77: plan_shared.go decides which nodes " +
			"several commands scheduled are ONE physical execution. It is the direction this " +
			"ceiling protects, not the one it guards against — the decision is derived entirely " +
			"from DECLARED facts already in the plan (the manifest task, its contract digest, its " +
			"declared inputs, writes and params, the project's own option layers) and it computes " +
			"nothing about execution: the plan it produces has the same nodes, the same edges and " +
			"the same cache keys it had without the pass, which TestSharedExecutions_DoNotMoveThePlan " +
			"pins. The alternative the epic rejected (refinement decision F3) is what would have " +
			"leaked: normalizing the CACHE KEY across commands would have moved every existing " +
			"entry's address and put an execution economy inside an identity. The seven functions " +
			"are the pass, the eligibility gate, the identity, its two canonicalizers, the declared- " +
			"param projection, and the accessor a plan preview reads the grouping through; the file " +
			"is the pass itself, kept apart from planner.go because the gate's conditions are the " +
			"reviewable part and each one names a way a shared run could fail to be what the second " +
			"node would have produced. This raises 84→90 to make that identity recursively producer-aware " +
			"before literal step bindings become live. The six functions are one declared-fact projector, " +
			"one digest, and the constructor/upstream fold/recursive identity/comparability gate of the " +
			"producer resolver. They add no execution knowledge: every input is already present in the " +
			"validated DAG, and a missing producer or cycle fails the optimization closed. " +
			"90→91: ownerGateLeafFrontier folds one owner's session-prerequisite relations into one " +
			"functional gate frontier, so a sibling relation's gate leaves bind every prerequisite " +
			"root and the process-capability contract (every protected job after every leaf of each " +
			"required command over the FINAL plan) holds by construction. It reads only declared " +
			"relations and already-computed command leaves — planning answering one more question, " +
			"not doing one more thing — and the per-relation policy/no-op/provenance bookkeeping " +
			"is untouched. 91→92: orderContractClients resolves one derived relation the graph " +
			"already holds — a generated client and the provider whose contract it was generated " +
			"from — into the ordering-only family the plan already has (SerializeAfter). It reads " +
			"the same `^` step references resolveExternalDeps resolves and the same planned index, " +
			"adds no stage, no flag and no cache-key edge, and its one refusal (an edge the plan's " +
			"existing ordering already runs the other way) is what keeps the DAG a DAG",
	},
	{
		unit:   "scheduler",
		prefix: "internal/jobs/scheduler",
		files:  6,
		funcs:  97,
		why: "internal/jobs/scheduler*.go: dispatch, batching, per-task cache boundaries, retry and " +
			"metrics. The largest unit here by function count and the one with the most concurrency " +
			"per line, so it is also the one where an added branch is hardest to reason about — " +
			"per-task batching is why it is this size. An earlier change removed the v2 " +
			"inferred-capture branches, so a new cache route is a deliberate expansion that must " +
			"raise this ceiling with its ownership and atomicity proof. 77→78 is " +
			"restampRestoredVersionStamp, the SECOND writer of a project's build stamp: OWNERSHIP — " +
			"it writes only <project>/.gen/version.json, only after a restore replaced the subtree " +
			"holding it, and deliberately does not claim the versionFiles slot refreshVersionFile " +
			"owns, so the two writers never disagree about whose turn it is; ATOMICITY — it takes " +
			"the same versionFilesMu as its sibling (so the two cannot interleave on one path) and " +
			"runs at the single convergence point of the local, remote and coalesced restores, " +
			"BEFORE the hit is published, so no consumer can observe the producing run's revision. " +
			"It exists because generation keys stopped being commit-aware; a third stamp writer " +
			"would be a different decision. 78→84 is the one movement this " +
			"pin's header calls the RIGHT kind: Scheduler.Run had regrown to 185 lines / 103 " +
			"statements — the largest function in the module by statements — coordinating setup, " +
			"remote negotiation, worker creation, dispatch, cancellation, finalizers, upload " +
			"draining, canceled-result synthesis, rendering, persistence and reduction in one " +
			"body. The six extracted phases (beginRun, prepareRemote, startWorkers, " +
			"recordCompletedGroup, queueReady, drainRemote, finalizeRun) add function DECLARATIONS " +
			"while removing behavior FROM the one function nobody could review, and they add no " +
			"file: Run is 106/53 now and .golangci.yml's funlen ceiling moved 190/115→187/101 " +
			"in the same change, so the budget a future feature can spend inside Run went down, " +
			"not up. The seam this pin actually protects is unchanged: the coordinator loop, the " +
			"close of readyCh and the join of the worker pool all stay INLINE in Run, and every " +
			"helper takes the run's one mutex and directional channel types as parameters so no " +
			"extraction can acquire the close or the WaitGroup. A NEW route through the " +
			"scheduler still has to raise this with an ownership and atomicity proof; a further " +
			"split of an existing one does not need to grow it again. 84→85 is " +
			"batchGroupTimeoutMs, and it is not a new route: OWNERSHIP — it is a pure reduction over " +
			"the work slice (max member deadline, unbounded if any member is, then the existing " +
			"batchLeaderTimeoutMs scaling) that reads nothing outside its argument and writes " +
			"nothing at all; its single consumer, batchLeader, stamps the answer on the leader NODE, " +
			"which is already a copy, so the JobDefinition every member shares stays untouched and a " +
			"later singleton run or per-task retry still gets its own per-suite budget. ATOMICITY — " +
			"it takes no lock because it needs none: it runs once on the dispatch path before the " +
			"shared subprocess starts, over nodes no other goroutine is mutating, so no reader can " +
			"observe a half-applied group budget. It exists because members can now carry DIFFERENT " +
			"deadlines, which made the group's budget a decision that had to be written down " +
			"somewhere rather than fall out of work[0]. 85→86 is " +
			"boundedDeadlineMs, a review fix, and it is the opposite of a new route: it is the saturating " +
			"multiply the three deadline-scaling sites now share. OWNERSHIP — pure integer " +
			"arithmetic over its two arguments, reading and writing nothing; its callers " +
			"(batchLeaderTimeoutMs, cacheLeaseWaitTimeout's retry expansion, and the two " +
			"time.Duration conversions) previously each multiplied inline. ATOMICITY — no state, " +
			"so nothing to serialize. It exists because an overflow in that multiply produces a " +
			"NEGATIVE deadline, which every consumer reads as the \"no deadline\" sentinel, " +
			"silently disarming hang protection; manifest timeouts never pass through " +
			"project-config validation, so the arithmetic has to be total on its own input " +
			"rather than correct only on a validated one. RAISED 86→91 and 5→6: this pin's header asks a NEW route through the scheduler for its " +
			"ownership and atomicity proof, so: OWNERSHIP — scheduler_shared.go owns exactly one " +
			"map, sharedSlots, keyed on the plan-stamped shared-node id, and one rendezvous per " +
			"key. It writes nothing else: a follower's result is DERIVED from the leader's and " +
			"then travels the ordinary lifecycle, so the cache entry, the registered hash, the " +
			"session row and the renderer sequence are the ones that task would have produced " +
			"alone, and the ONE branch that skips the subprocess is a single early return in " +
			"runSharedAttempt. It is not a new CACHE route at all — the entry a follower publishes " +
			"is its own, at its own unchanged key, which is what makes a warm run bit-identical to " +
			"one taken before this slice (TestSharedExecution_KeepsWarmRunsIdentical). ATOMICITY — " +
			"the map is taken under sharedMu for the whole claim, and the leader/follower " +
			"rendezvous is the closed-channel handshake runPreBuildHook already uses, so a " +
			"follower's read of the published result happens-after the leader's write by the " +
			"channel's own ordering; publish is idempotent (sync.Once) so every terminal path of " +
			"the leader can call it unconditionally, and there is no path where a leader takes the " +
			"slot without reaching one. Deadlock is bounded by construction: a follower exists only " +
			"because a leader already took the slot on the worker running it, followers hold no CPU " +
			"grant while parked (the slot is consulted before the group budget), and a shared-node " +
			"member never joins a batch, so a leader can never be waiting behind its own follower's " +
			"open. The five functions are the claim, the publish, the wait, the follower's derived " +
			"result, and replayResultEvents — which is not new behavior but the second half of " +
			"replayCacheEvents, extracted so a caller that has already emitted job:start reuses the " +
			"cache path's replay instead of growing a second one. 91→92 is plannedCeiling, " +
			"and it is the opposite of a new route: it is the ceiling lookup the singleton and batch " +
			"arms of acquireGroupCPUBudget now share. OWNERSHIP — it reads two maps that " +
			"prepareScheduling finished writing before any worker existed (resourcePlanByJob, " +
			"batchCPUBudgetByJob) and writes nothing at all; both arms previously inlined the same " +
			"three lookups plus a recommend() whose result they then discarded, which is what made " +
			"it possible for the two arms to disagree about which of the plan ceiling and the class " +
			"budget wins. ATOMICITY — no state, so nothing to serialize: the maps are read-only for " +
			"the whole dispatch phase, which is the same reason a grant no longer depends on " +
			"acquisition order. It exists because the ceiling is now FIXED before dispatch, so " +
			"reading it is a lookup rather than a computation, and a lookup written twice is a " +
			"lookup that can drift. This raises 92→93 for the two batch-transcript projection " +
			"helpers. OWNERSHIP — batchProjectLogs alone reads the completed aggregate event stream, " +
			"copies matching event data and removes the private project-routing member; " +
			"insertBatchProjectLogs alone places those owned logs before the synthetic phase end. " +
			"Neither creates an execution or cache route. ATOMICITY — both run synchronously after " +
			"the shared subprocess has completed, mutate only fresh slices and maps, and touch no " +
			"scheduler state or lock. This raises 93→94 for jobProcessEnv, the run-level process " +
			"environment every job subprocess receives (the cache provider's object-cache socket and " +
			"the resolved trust policy). OWNERSHIP — it reads two fields it does not write " +
			"(cfg.NoCache and the RemoteCache the engine handed the run) and returns a []string that " +
			"reaches cmd.Env and nothing else: not the job context, not taskParams, not a cache key, " +
			"not a run marker. It is NOT a new cache route — the scheduler neither reads nor writes " +
			"an object through it; it only tells a job where the provider listens, and a warm run " +
			"stays bit-identical to one taken before this slice " +
			"(TestObjectCacheEnvStaysOutOfCacheKeys). ATOMICITY — the value is derived per dispatch " +
			"from the RemoteCache accessor, which takes the provider mutex for its read, and the " +
			"socket is negotiated once at initialize before any worker exists, so every job of a run " +
			"observes the same path or none at all. This raises 94\u219295 for readyBatch, which returns " +
			"a job's batch key together with the project cap that key folds; readyBatchKey is now its " +
			"one-line wrapper for the callers that need only the key. OWNERSHIP — the cap a group is " +
			"filled to is the cap its key was computed with, from ONE resolution of the job's " +
			"parameters, instead of takePendingGroup resolving the workspace option a second time " +
			"and discarding its error. It is not a cache route and writes nothing. ATOMICITY — it " +
			"runs exactly where readyBatchKey ran before, reads only configuration fixed before " +
			"dispatch, and touches no scheduler state or lock. This raises 95\u219297 for prepareTask " +
			"and abandonTask, the split that lets a task take its cross-session task-output locks " +
			"between openTask's lock-free waits and its first write. OWNERSHIP — openTask keeps the " +
			"lookup, the restore-or-claim and the shared-execution rendezvous; prepareTask is its " +
			"former second half in the same order (drift reference, output detach, version refresh, " +
			"preBuild hook, invocation arming, JobStart); abandonTask is the one terminal path of an " +
			"opened task that never executes, shared by a preparation failure and a failed lock " +
			"acquisition, so a shared leader answers its followers on both. Neither is a cache route: " +
			"no key, entry or lookup changes. ATOMICITY — executeJobGroup takes the whole group's keys " +
			"in ONE sorted acquisition after every member's openTask returned and releases them after " +
			"closeTask, except a project key only a preparation needs, released once every member's " +
			"prepareTask returned, so no key is held across a lease, follower or restore wait, and a " +
			"member's preparation and capture both run inside the hold (task_output_lock.go)",
	},
	{
		unit:         "invocation",
		prefix:       "internal/jobs/invocation",
		files:        5,
		funcs:        62,
		perFileFuncs: 16,
		why: "internal/jobs/invocation*.go: the invocation-scoped lifecycle — resolving a " +
			"`finalizes` relation onto the plan and pruning it, the private scratch and the lease " +
			"that outlives a SIGKILL, the sensitive-artifact leak guard (needle derivation and " +
			"redaction), and exactly-once finalization. This unit is pinned for a different reason " +
			"than the five above it. It is not a unit an epic SHRANK; it arrived as a single " +
			"58-function file and was split along its four invariants in a later change, " +
			"so what is worth preventing here is not the unit growing but the split UNDOING " +
			"itself. `files` cannot catch that: it is a ceiling, so four deletions and one re-merge " +
			"report FEWER files, an unchanged function total, and exactly the mixed-concern file the " +
			"split removed. perFileFuncs is what fails instead — 14 is invocation_needles.go, the " +
			"largest of the five, and any file that takes on a second invariant clears it long " +
			"before the unit total moves. The invariants are one file each, except CONFINEMENT, " +
			"which is two because its halves fail differently (what counts as a secret, derived " +
			"under a bounded read; and what may never publish one). A sixth file therefore means a " +
			"fifth invariant, and a function added to any of the five means a bigger invariant: " +
			"both are real decisions, so raise the number and name the invariant here. This raises " +
			"58→62 and 14→16 for CONFINEMENT's bounded producer spool and per-job leak side channel: " +
			"the spool keeps pre-needle output off the heap and consumer-visible artifact root, while " +
			"the side channel makes a late redacted event fail the task even after JobResult retention " +
			"is exhausted. Creation, replay, leak accounting and cleanup are separate fail-closed edges",
	},
	{
		unit:   "cache",
		prefix: "internal/store/",
		files:  33,
		funcs:  275,
		why: "internal/store: the content-addressed local cache — entries, task entries, CAS, " +
			"leases, GC, generations and the remote bridge. Cache-key completeness and atomicity " +
			"under concurrency are the two invariants this repository has been burned by most " +
			"often, and both get harder with every function that can observe or mutate an entry. A " +
			"new file here means a new way to reach the store; that is a review, not a detail. " +
			"RAISED ONCE, from 23/210: invocation.go plus its two " +
			"build-tagged liveness halves are the invocation-scoped private scratch and its " +
			"non-secret lease. They are NOT a new way to reach the store, and that is the review: " +
			"nothing in them opens, reads, writes, addresses or evicts a cache ENTRY — the tree " +
			"they manage is per-workspace, deliberately outside the CAS, and an invocation-scoped " +
			"output is by contract never captured and never restored (protocols/extension: a task " +
			"declaring one must set cache.enabled to false). They live here because the property " +
			"they need is this package's actual contract — durable state written by one process " +
			"and read by another after it dies — and because the liveness primitive they decide " +
			"orphan recovery with is the same internal/flock advisory lock lease.go and gc.go " +
			"already coordinate on. Splitting the liveness rule across two packages is what would " +
			"be dangerous. The pair invocation_unix.go/invocation_other.go exists because signal-0 " +
			"liveness is unavailable on non-unix hosts, where the fallback reports every owner as " +
			"alive so recovery degrades to unavailable rather than to reaping a live build. " +
			"RAISED A SECOND TIME, from 25/224: isProjectConfig and " +
			"projectConfigDigest add NO way to reach an entry — they add a second SEMANTIC file " +
			"digest, beside versionStampDigest, on the same fileContentDigest dispatch and in the " +
			"same file. They make cache keys MORE complete, not less: a project config's `tasks` " +
			"block is execution-only by contract (protocols/workspace: ProjectTaskTuning), yet " +
			"eight Go-extension tasks declare putnami.json as a `config` input, so editing a " +
			"cpuWeight or a timeoutMs used to re-key a project's whole pipeline. Excluding the " +
			"block is what makes \"tuning never moves a key\" true of the FILE and not only of the " +
			"struct nothing read. Both are pure functions of one path, both fall back to the raw " +
			"bytes on any parse failure, and neither opens, writes, addresses or evicts an entry. " +
			"A THIRD semantic digest would be a real decision — that is three files whose bytes " +
			"the key deliberately does not see — and should raise this again with the contract " +
			"that justifies it. RAISED A THIRD TIME, from 25/226: SelectsPath adds NO " +
			"way to reach an entry and no new file — it is a pure, filesystem-free projection of " +
			"collectFiles' selection rule onto one named path, sharing splitGlobPatterns and " +
			"matchesAnyGlob with the selection it describes so the two cannot drift. It exists " +
			"because a caller that owns a file whose content must never be answered from cache — " +
			"a recorded verdict, a committed baseline — could previously only assert that " +
			"coverage by restating the glob semantics in its own test, which is the failure mode " +
			"this package's ceiling is meant to prevent: a second, quietly diverging definition " +
			"of what a cache key sees. Exporting the one already here makes cache-key " +
			"COMPLETENESS assertable from outside the package, which is the invariant the ceiling " +
			"protects, not one it trades away. ADR 0024 raises 91→92 for PrepareWorkspaceGraph, " +
			"the shared minimal graph-preparation entrypoint used by MCP cold startup. It composes " +
			"the existing provider binding and workspace synchronization paths and adds no Engine.Run " +
			"stage or second graph-preparation engine. RAISED A FOURTH TIME, from 25/227: " +
			"task_failure.go is the negative (failure) entry — the record that says \"this " +
			"exact key already failed\". It IS a new way to reach the store, and that is the " +
			"review. Three properties bound it: it is addressed from its OWN domain constant, so " +
			"no positive-entry reader and no remote path can name it; it holds no declared output " +
			"and no CAS blob, so it can never materialize a tree; and every unreadable, foreign or " +
			"non-failing record is a MISS, so the worst outcome of a wrong read is one " +
			"re-execution. It makes cache-key completeness MORE load-bearing, not less: the " +
			"negative entry is invalidated by exactly the key that invalidates the positive one, " +
			"with no second key and no extra input. Twelve functions: eight here (address, look " +
			"up, replay, record, forget, the single read-modify-write, the atomic write and the " +
			"fail-closed load) and four thin CacheManager verbs in cache_task.go that add no rule " +
			"of their own. See doc/adr/0030-a-failed-task-is-cached-until-its-inputs-change.md. " +
			"239→240 for the remote-cache recurrence: carryOverSubtrees is " +
			"the restore half of a declared carve-out — a ceding task's directory swap now keeps the " +
			"ceded subtrees another task owns, by rename, instead of deleting them and trusting the " +
			"owner's later restore to win a race other commands of the session can observe. " +
			"This raises 26/240→27/242 for scratch.go: AcquireScratch, AttachScratch and " +
			"reapScratch give the existing mutable workspace root a generic generation lifetime. " +
			"It is deliberately a separate file/invariant from CAS eviction: a shared consumer " +
			"lease and explicit child handoff exclude expiry, and confined deletion advances its " +
			"stamp only on success. It adds no CAS entry format, cache-key input, remote path, " +
			"or extension-specific slot knowledge. ADR 0031 records the retention tradeoff. " +
			"RAISED, from 27/242 to 27/244 (ADR 0037): CollectKeyFiles and " +
			"ExtraKeyFiles add NO way to reach an entry and no new file — they are pure " +
			"projections of collectFiles and of hashPathContent's directory walk onto the paths " +
			"they select, in the SelectsPath shape (the third raise): the portable admission has " +
			"to know WHICH files a key hashes to bind the git-ignored ones into a snapshot, and a " +
			"second collector outside this package is exactly the quietly diverging definition of " +
			"what a cache key sees that this ceiling exists to prevent. " +
			"RAISED, from 27/244 to 27/250, with no new file. gc_trigger.go gains the " +
			"in-run budget (NewRunBudget, AfterBatch, Close, recordGCUsage) and updateGCUsage, the " +
			"one locked read-modify-write of the shared usage record that measurements and a run's " +
			"growth both go through. gc.go trades sweepUnreferenced and liveBlobPaths for " +
			"exclusiveSections (acquire, yield, close), liveBlobs.refresh and evictStores (RunGC's " +
			"Phase B, moved out so RunGC stays under the gocyclo cap): measured by " +
			"BenchmarkEvictStore_ExclusiveLockHold, one pass held a store's exclusive lock for " +
			"0.9-5.9 s at 1,000-10,000 entries, stalling every lookup, publish and restore on it, " +
			"so the lock is now held in ~200 ms sections, each sweep section re-reading only the " +
			"entries published since. It is an existing way to reach the store, not a new one: " +
			"every removal still happens under the exclusive lock, a victim leaves the index by one " +
			"atomic rename, and the one new eviction rule, GCOptions.ProtectSince, only SPARES " +
			"entries. The counter the budget reads (LocalStore.added) is incremented after the CAS " +
			"rename and never reaches a key. " +
			"RAISED, from 27/250 to 30/256. task_materialize_linux.go, " +
			"task_materialize_darwin.go and task_materialize_other.go are the three build-tagged " +
			"halves of exchangePaths (renameat2 RENAME_EXCHANGE, renamex_np RENAME_SWAP, and a stub " +
			"that is always unsupported), which swaps an existing directory output in one syscall " +
			"instead of two renames that left it absent in between. mergeIn, refuseCededBytes, " +
			"pruneAround and keptUnder replace carryOverSubtrees: a ceding output is merged into its " +
			"destination entry by entry, so a ceded subtree is never moved at all, where the carry-over " +
			"moved it out and back and a reader of the migration bundle hit the gap as ENOENT. It is " +
			"not a new way to reach the store: nothing in them opens, addresses or evicts an entry, and " +
			"every failed exchange falls back to the two-rename swap that was already here. " +
			"RAISED, from 30/256 to 32/262. task_staging_unix.go and task_staging_other.go " +
			"are the build-tagged halves of sameFilesystem and crossDevice, which decide whether a " +
			"restore can stage in the caller's staging root (the per-workspace scratch root declared " +
			"capture already stages under) instead of beside its destination, so a task that walks " +
			"the destination's parent never meets a staging path that appears and vanishes. The " +
			"non-unix half answers false, so a restore there stages beside its destination as before. " +
			"stagingDirFor selects the staging directory, and publishFrom is the previous stage, " +
			"populate and swap body, run a second time from beside the destination when a swap from " +
			"the staging root fails with EXDEV (two mounts of one filesystem). It is not a new way to " +
			"reach the store: nothing in them opens, addresses or evicts an entry, and the swap and " +
			"merge rules are unchanged. " +
			"RAISED, from 32/262 to 32/266 (Windows port), with no new " +
			"file. osClassFor names the host's file model for CacheKey.OSClass, which the key writes " +
			"only on Windows, so no Linux or macOS key moves and no Windows key equals a POSIX one. " +
			"slashPathLess folds hashFiles and hashPathContent in the slash order of each ABSOLUTE " +
			"path, so one tree hashes to one key whatever the separator; on '/' hosts it is plain " +
			"string order. manifestModes and recordedModes.of make a restore apply the mode its own " +
			"manifest recorded, not the mode of a CAS blob that every entry holding the same bytes " +
			"shares and each capture chmods. None is a new way to reach the store: no entry " +
			"format, remote path or eviction rule, and one key field that is empty off Windows. " +
			"RAISED, from 32/266 to 33/270 (Windows port). " +
			"held_windows.go and held_other.go are the build-tagged halves of hostHeldOpen, which " +
			"says whether an error means another process holds a handle on the path (Windows " +
			"refuses a rename, and some opens, while any reader holds the file or a file inside the " +
			"directory); the non-windows half answers false and waits zero. holderWait.retry paces " +
			"one operation's retries within that budget, readReplacedFile is the failure record's " +
			"read that waits out a replacing rename, and publishBlob treats a CAS rename that fails " +
			"while the digest's blob exists as the lost race it is. None is a new way to reach the " +
			"store: no entry format, key input, remote path or eviction rule changes, and off " +
			"Windows every operation runs exactly once as before. " +
			"RAISED, from 33/270 to 33/272, with no new file. lstatSettled is " +
			"MaterializeDirSymlink's Lstat under the same holderWait budget, because Windows answers " +
			"ERROR_ACCESS_DENIED for a link whose delete is pending while another process detaches " +
			"the same path. detachedDirectory is the one classification both of the function's " +
			"Lstat sites share: a directory is done, an fs.ModeIrregular answer (a junction another " +
			"process replaced between the Lstat and IsLink's Readlink) is re-read, and anything " +
			"else fails as before. Neither is a new way to reach the store: they touch a declared " +
			"output path, never an entry, and on Unix, where a link is fs.ModeSymlink, the " +
			"outcome of every call is unchanged. " +
			"RAISED, from 33/272 to 33/273, with no new file. leaseNow is the one " +
			"clock the lease code reads: time.Now in production, a clock a test freezes so a lease " +
			"test that is not about expiry cannot see its lease expire on a loaded host. It reads " +
			"no entry and writes none. " +
			"RAISED, from 33/273 to 33/274, with no new file. SourceState names, once per " +
			"workspace root for the life of the manager, whether Git manages that root, for " +
			"CacheKey.SourceState. The key writes the field only where Git does not manage the root, " +
			"so no key computed inside a repository moves and no key of a root without one equals " +
			"it. It makes keys MORE complete: such a root is stamped with no source binding, the " +
			"stamp is not a declared input of the tasks that read it, and without the field an " +
			"output built in one state would serve the other. It reads no entry and writes none. " +
			"RAISED, from 33/274 to 33/275, with no new file. RecordManagedRoot records a root a git " +
			"command of the run already answered for, so SourceState answers it without a process; " +
			"it never replaces an answer the manager holds, so one invocation keeps one answer per " +
			"root. It reads no entry and writes none",
	},
	{
		unit:   "output",
		prefix: "internal/output/",
		files:  29,
		funcs:  270,
		why: "internal/output: the renderers. This is the unit the epic shrank most directly — " +
			"An earlier change deleted jsonl.go and the v1 JSON renderer, leaving ONE machine renderer " +
			"(machine_v2.go) that projects the canonical reduction for both --output=json and " +
			"--output=jsonl. Its one additional scheduler-cap reporter keeps Docker publication " +
			"facts on that same projection rather than adding a second machine renderer. Everything " +
			"left is human-facing (live, text, cloud-logging). A new machine renderer is the second " +
			"output contract this slice exists to prevent, and it would show up here first. " +
			"This raises 27/223→28/248 for machine_stream.go and the small hooks around it: " +
			"the new file is ONE shared sanitizer, fixed-budget selector and complete session " +
			"recorder used by every renderer; machine_v2 delegates to it rather than creating a " +
			"second machine renderer. The functions separately name online admission, exact " +
			"elision, artifact append, legacy session-audit projection and result-only terminal " +
			"records because those are independently tested protocol invariants. The final 248→258 " +
			"review increment adds the lossless token sanitizer, physical-batch recording seam, and " +
			"explicit no-artifact/error fallback; each closes a bounded-output completeness or " +
			"truthfulness edge without adding another renderer. This raises 258→259, with files " +
			"unchanged, for WritePreviewPlan beside machine_v2: it is the plan-only projection of the " +
			"same v2 contract and cannot construct a renderer or session. ADR 0024 raises 259→262 " +
			"for the lifecycle renderer constructor and its failure-diagnostic helpers: they compose " +
			"the existing live and text renderers, suppress successful nested-run chrome, and replay " +
			"warnings and failures without adding a machine renderer or output protocol. This raises " +
			"262→267, with files unchanged, for the replayed-failure annotation: one formatter, one " +
			"status-line suffix, one age formatter, one run-level predicate and one per-command " +
			"lookup, shared by the text and live renderers. They add NO renderer and NO output " +
			"contract — the machine surfaces emit a replayed failure exactly as they emit a fresh " +
			"one, because the two differ only in an optional provenance field no wire shape reads. " +
			"This raises 267→268 for reusedFailureSuffix, the same shape as replayedFailureSuffix: " +
			"a cache hit whose declared output drifted from this checkout is a FAILED result with " +
			"cache provenance, and the row has to say where the verdict came from or the reader " +
			"looks for a subprocess that never ran. No renderer and no output contract. " +
			"This raises 28/268→29/270 for tty_windows.go, the Windows half of the terminal probe " +
			"tty_unix.go already implements: isTTY, termSize, and the init that turns on virtual " +
			"terminal processing for stdout and stderr. StderrIsTTY moves beside ShouldUseLiveRenderer " +
			"so both platforms share it. No renderer and no output contract",
	},
}

// TestComplexityCeilings_PerUnit fails when a unit grows past its pinned size.
func TestComplexityCeilings_PerUnit(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	_, files := productionASTs(t, root)

	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	concentrationChecked := 0
	for _, ceiling := range complexityCeilings {
		fileCount, funcCount := 0, 0
		var members []string
		widest, widestFuncs := "", 0
		for _, rel := range rels {
			if !strings.HasPrefix(rel, ceiling.prefix) {
				continue
			}
			fileCount++
			members = append(members, rel)
			fileFuncs := 0
			for _, decl := range files[rel].Decls {
				if _, ok := decl.(*ast.FuncDecl); ok {
					funcCount++
					fileFuncs++
				}
			}
			if fileFuncs > widestFuncs {
				widest, widestFuncs = rel, fileFuncs
			}
		}

		if fileCount == 0 {
			t.Errorf("unit %q matched no production files under %q — the unit moved; move the pin with it",
				ceiling.unit, ceiling.prefix)
			continue
		}
		if ceiling.perFileFuncs > 0 {
			concentrationChecked++
			if widestFuncs > ceiling.perFileFuncs {
				t.Errorf("unit %q: %s declares %d top-level functions, per-file ceiling %d — %d added.\n"+
					"  One file is taking on more of this unit than the split left it with.\n  %s",
					ceiling.unit, widest, widestFuncs, ceiling.perFileFuncs,
					widestFuncs-ceiling.perFileFuncs, ceiling.why)
			}
		}
		if fileCount > ceiling.files {
			t.Errorf("unit %q: %d production files, ceiling %d — %d added.\n  %s\n  Files:\n    %s",
				ceiling.unit, fileCount, ceiling.files, fileCount-ceiling.files, ceiling.why,
				strings.Join(members, "\n    "))
		}
		if funcCount > ceiling.funcs {
			t.Errorf("unit %q: %d top-level functions, ceiling %d — %d added.\n  %s",
				ceiling.unit, funcCount, ceiling.funcs, funcCount-ceiling.funcs, ceiling.why)
		}
	}

	// Non-vacuity: perFileFuncs is opt-in, so a table where every unit dropped it
	// would still pass every assertion above while measuring nothing about
	// concentration. That is the shape of ratchet this file exists not to have.
	if concentrationChecked == 0 {
		t.Error("no unit pins perFileFuncs — the concentration ratchet measures nothing; " +
			"a unit whose files ARE its invariants must keep its per-file number")
	}
}

// TestComplexityCeilings_UnitsAreDisjoint keeps the units from overlapping. The
// planner and scheduler prefixes both select inside internal/jobs, so a file
// named for both (a "scheduler_plan*.go") would be counted twice and could sit
// under one ceiling while breaching the other — a pin that lies is worse than
// no pin.
func TestComplexityCeilings_UnitsAreDisjoint(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	_, files := productionASTs(t, root)

	owner := map[string]string{}
	for _, ceiling := range complexityCeilings {
		for rel := range files {
			if !strings.HasPrefix(rel, ceiling.prefix) {
				continue
			}
			if previous, taken := owner[rel]; taken {
				t.Errorf("%s is counted by both %q and %q — the ceilings overlap, so neither number "+
					"means what it says", rel, previous, ceiling.unit)
				continue
			}
			owner[rel] = ceiling.unit
		}
	}
	if len(owner) == 0 {
		t.Fatal("no production file matched any unit — the prefixes are wrong")
	}
}

// TestComplexityCeilings_ReportCurrentSizes is not an assertion: it records the
// measured sizes in the test log, so raising a ceiling starts from a number
// somebody read rather than from a number somebody guessed.
func TestComplexityCeilings_ReportCurrentSizes(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	_, files := productionASTs(t, root)

	for _, ceiling := range complexityCeilings {
		fileCount, funcCount, widestFuncs := 0, 0, 0
		for rel, file := range files {
			if !strings.HasPrefix(rel, ceiling.prefix) {
				continue
			}
			fileCount++
			fileFuncs := 0
			for _, decl := range file.Decls {
				if _, ok := decl.(*ast.FuncDecl); ok {
					funcCount++
					fileFuncs++
				}
			}
			if fileFuncs > widestFuncs {
				widestFuncs = fileFuncs
			}
		}
		perFile := "unpinned"
		if ceiling.perFileFuncs > 0 {
			perFile = "ceiling " + strconv.Itoa(ceiling.perFileFuncs)
		}
		t.Log(ceiling.unit + ": files=" + strconv.Itoa(fileCount) +
			" (ceiling " + strconv.Itoa(ceiling.files) + "), funcs=" + strconv.Itoa(funcCount) +
			" (ceiling " + strconv.Itoa(ceiling.funcs) + "), widest file=" +
			strconv.Itoa(widestFuncs) + " (" + perFile + ")")
	}
}
