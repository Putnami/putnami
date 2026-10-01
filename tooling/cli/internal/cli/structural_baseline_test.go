package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Slice A0 of the epic: structural ratchets over the duplication the epic
// removes, so a later slice cannot silently add a sixth scheduler construction
// or a seventh command table.
//
// Design notes, because slice A6a extends whatever this builds:
//
//   - No packages.Load, no AST, no new dependency. Line-oriented token counting
//     over the .go files of the modules that hold the CLI is enough to pin a
//     call-site count, and it stays legible when a later slice has to move a pin.
//   - The walk spans MODULES (cliModules), not one directory. An earlier change
//     lifts the pure workspace/job/extension model out of tooling/cli into
//     go.putnami.dev/cli/model, and a declaration that crossed that boundary did
//     not disappear — it changed module. A single-module walk would read 0 for
//     it and invite the pin being lowered to 0, which is a vacuous test that
//     silently stops protecting anything. Counting the union keeps every pin
//     below at the value it was pinned at, and keeps working as later slices
//     move more code: a new module is one entry in cliModules.
//   - Production vs test is decided by filename: a file is test-only iff its
//     name ends in "_test.go". The ~60 jobs.NewScheduler constructions in
//     internal/jobs/*_test.go are excluded from the production count that way,
//     and pinned separately below so a slice that moves construction into a
//     helper is still visible.
//   - Call-site pins are RATCHETS, not just ceilings: exceeding one fails ("a
//     slice added one"), and falling under it also fails ("a slice removed one
//     — lower the pin"). Every movement gets reviewed. That is the point.
//   - Module-wide counts the epic drives down (map[string]any) are ceilingOnly
//     instead: a reduction is the goal, so failing on it would fail CI for
//     succeeding, and would fire on unrelated CLI work.
//   - This file excludes ITSELF from the walk (structuralPinFile): it spells the
//     counted tokens out literally, so counting itself would make every pin a
//     function of its own prose.

// structuralPinFile is this file. It names every counted token literally, so it
// is excluded from the walk; otherwise editing a pin's prose would move the pin.
const structuralPinFile = "structural_baseline_test.go"

// structuralPin is one counted token with its current value.
type structuralPin struct {
	// name is what the pin means, quoted verbatim in failures.
	name string
	// token is the literal substring counted, once per matching LINE.
	token string
	// scope selects which files are counted.
	scope structuralScope
	// want is the current count in this tree.
	want int
	// ceilingOnly makes the pin fail on growth but not on reduction.
	//
	// Use it for counts the epic exists to DRIVE DOWN across the whole module,
	// where a drop is the goal rather than a reviewable event. A two-way ratchet
	// on those would fail CI for succeeding, and would fire on unrelated CLI
	// work that happens to delete an untyped map. Structural counts that name a
	// specific, small set of call sites (scheduler/planner construction) stay
	// two-way: there, a disappearance really does need a reviewer.
	ceilingOnly bool
	// why explains what a movement means, so a failure is actionable.
	why string
}

type structuralScope int

const (
	productionFiles structuralScope = iota
	testFiles
	allFiles
)

func (s structuralScope) String() string {
	switch s {
	case productionFiles:
		return "production (*.go, excluding *_test.go)"
	case testFiles:
		return "test-only (*_test.go)"
	default:
		return "all *.go"
	}
}

var structuralPins = []structuralPin{
	{
		name:  "production jobs.RunPlan call sites",
		token: "jobs.RunPlan(",
		scope: productionFiles,
		want:  1,
		why: "the ONE execution site left: internal/engine/execute.go. An earlier change " +
			"unexported jobs.NewScheduler and its two setters, so 'construct, maybe wire the " +
			"remote cache, maybe wire session events, run' became one indivisible jobs.RunPlan " +
			"call — this token replaced `jobs.NewScheduler(`, which no package outside " +
			"internal/jobs can spell any more. A second site is a run with no session file, no " +
			"run marker, no profiler and no remote cache; " +
			"internal/cli/engine_boundary_test.go enforces the PACKAGE, this pin the COUNT. " +
			"The count reached 1 like this: it was the terminal path's construction, " +
			"moved into internal/engine/execute.go by an earlier change — same site, new " +
			"owner. A0 pinned 5; the next change deleted " +
			"internal/cli/extension_command.go's runScheduledExtensionPlan — the first ratchet " +
			"DOWN, because extension aliases now route through Engine.Run and inherit the " +
			"session file, remote cache, run markers, and 130-on-Ctrl-C the private " +
			"construction never had. Another change took it to 3 by deleting " +
			"internal/mcp/adapter.go's: run_jobs is an adapter over Engine.Run now, so it " +
			"inherits the missing-extension and starved-command guards, DiscoverExtensionsDetailed's " +
			"skip records, and the canonical reducer, instead of a private SchedulerConfig with " +
			"no session, no profiler and a hand-rolled tally. The next change took it to 2 by " +
			"deleting internal/commands/deps.go's runWorkspaceJob composition: `putnami install`, " +
			"`upgrade`, `projects sync`, `workspace init` and the first-use bootstrap now run " +
			"their workspace-level jobs through cli.RunWorkspaceJob over Engine.Run, with the " +
			"dedup-by-extension filter, the disable-list bypass and the no-session/no-marker/" +
			"no-GC rules stated once on engine.Request.WorkspaceLifecycle. A later change took " +
			"it to 1 by deleting internal/watch/session_iteration.go's: a watch iteration is now a " +
			"replan over Engine.Run (internal/engine/watch.go binds the watch.RunIteration seam), so " +
			"it inherits contract validation, the missing-extension guards and the canonical " +
			"session, and states its own no-marker / no-remote-cache / no-preflight policy in one " +
			"place instead of forking the lifecycle",
	},
	{
		name:  "scheduler constructor declaration",
		token: "func newScheduler(",
		scope: productionFiles,
		want:  1,
		why: "there is exactly one scheduler constructor (internal/jobs/scheduler.go); a second is a " +
			"fork. An earlier change unexported it: an unexported identifier is unreachable from " +
			"every other package by the COMPILER, so ADR 0001 §3's 'no adapter constructs or " +
			"configures jobs.Scheduler' stopped being a convention. An EXPORTED spelling " +
			"reappearing is caught by engine_boundary_test.go, which reads the declaration",
	},
	{
		name:  "test-only newScheduler constructions",
		token: "newScheduler(",
		scope: testFiles,
		want:  60,
		why: "all in internal/jobs/*_test.go, exercising the scheduler directly. Pinned separately " +
			"so they can never inflate the production count above. An earlier change removes the inferred " +
			"capture lifecycle but retains its supported scheduler coverage under explicit task " +
			"contracts: local event replay, project and port-resolved output materialization, " +
			"batch retry/cancellation, key precomputation, and canonical session reduction. The " +
			"count is the actual retained test construction count (including shared fixtures), so " +
			"future additions or deletions remain reviewed rather than being hidden by a lowered " +
			"pin. 47→49 comes from the invocation-lifecycle conformance " +
			"suite, which runs a real plan through the real scheduler because the properties it pins — a " +
			"finalizer running exactly once across success, setup failure, consumer failure and " +
			"cancellation — are properties of the coordinator's terminal paths and of nothing " +
			"smaller. Both sites are the fixture's shared runner and the one case that also needs " +
			"a session-event handler. 49→50 is the SIGKILL crash-recovery case: it " +
			"needs its own construction because it wires a session-event handler to read the " +
			"`invocation:reaped` recovery record, and it is the one property that cannot be " +
			"proven below the scheduler — reaping happens inside arm(), on the producer's own " +
			"open path, and 'before provisioning' is a statement about that ordering. The second " +
			"site of that change is the batch-key isolation case: a batch is one subprocess with " +
			"one job context, so a consumer of an invocation-scoped resource must never share a " +
			"dispatch, and readyBatchKey/takePendingGroup are scheduler methods. 51\u219253 " +
			"follows the same one-runner rule the batch " +
			"suite set: scheduler_shared_test.go funnels its five cases through a single shared " +
			"runSharedScheduler, because \"this work ran once\" is a property of the real " +
			"coordinator electing a leader across two dispatches and of nothing smaller. The " +
			"second site is the batch-exclusion case, which asks readyBatchKey directly — a " +
			"shared-node member must never join a dispatch group, and that is a scheduler method. " +
			"53→54 comes from the named resource budgets, and it holds the same one-runner rule: " +
			"the single site is resource_budgets_test.go's group-formation case, which drives " +
			"takePendingGroup over the shared batch fixture across three budget states from ONE " +
			"construction, because \"a batch stops growing at the budget\" is a property of " +
			"takePendingGroup/readyBatchKey and of nothing smaller. Every other case in that file " +
			"exercises the pool and the plan-time validator directly and constructs no scheduler. " +
			"54→55 is the physical batch-stream completeness proof: only the real scheduler can " +
			"show that the aggregate callback runs before per-member retention and is not replayed twice. " +
			"55→56 is the long-pole dispatch guard: a lone batch candidate carrying the queue's longest " +
			"critical path must not yield to shorter ordinary work, and takePendingGroup is a scheduler method. " +
			"56→57 comes from the object-cache job environment, and it holds the same one-runner rule: " +
			"remote_object_cache_test.go funnels every case through a single fixture runner, because " +
			"\"a job subprocess receives the provider's socket\" is a property of the scheduler wiring " +
			"a live RemoteCache into the exec seam and of nothing smaller — and the same runner is what " +
			"proves the two variables do NOT move a cache key, by running the same plan twice against " +
			"two different sockets and requiring a hit. " +
			"57\u219258 comes from the workspace batch cap, and it holds the same one-runner rule: " +
			"batch_project_limit_test.go drives takePendingGroup and readyBatchKey from ONE " +
			"construction while the workspace option moves through unset, 2, 3, a project " +
			"override, 1 and an invalid value, because \"a batch holds at most N projects and the " +
			"cap partitions the batch key\" is a property of those two scheduler methods together " +
			"and of nothing smaller. 58\u219259 comes from the cross-session task-output lock, and it holds " +
			"the same one-runner rule: scheduler_output_lock_test.go builds both sessions of each of " +
			"its cases through ONE fixture method, because \"a second session waits for the holder and " +
			"observes its complete output\" is a property of two real schedulers dispatching into one " +
			"workspace and of nothing smaller; the lock's own cases drive the manager directly and " +
			"never call newScheduler. 59\u219260 is source_state_test.go's one-answer case: \"the version " +
			"stamp's marker and every execution key of a run read one answer\" is a property of " +
			"newScheduler wiring the stamp's memo to the run's cache manager, which a Scheduler " +
			"literal skips, and of nothing smaller; its four cases share ONE construction site",
	},
	{
		name:  "production jobs.Plan call sites",
		token: "jobs.Plan(",
		scope: productionFiles,
		want:  2,
		why: "internal/engine/plan.go only (moved from internal/cli/jobs_run.go by an earlier change, " +
			"where planning is now an engine stage every adapter calls). A second is a new " +
			"fork. A0 pinned 4; the next change took it to 3 by deleting " +
			"internal/mcp/adapter.go's planJobs — MCP planned WITHOUT the missing-extension and " +
			"starved-command guards, so an agent could be told a broken workspace had nothing " +
			"to do. The next change took it to 2 by deleting internal/commands/deps.go's: " +
			"lifecycle jobs plan through engine.BuildPlan now, which is also what gives them " +
			"the workspace-once dedup as a documented Request seam instead of a private filter. " +
			"A later change took it to 1 by deleting internal/watch/session_iteration.go's: " +
			"watch re-planned every iteration with no contract validation and no " +
			"missing-extension guard, so a broken install kept rebuilding green in the loop. " +
			"A later change froze it there: engine_boundary_test.go now rejects a jobs.Plan " +
			"reference from any package but internal/engine, so this pin only has to hold the count. " +
			"An earlier change raises 1\u21922, and the second site is deliberately NOT a fork of the run's plan: " +
			"spec_gate_cache.go plans `test` over the spec gate's candidate attesters purely to " +
			"DERIVE THEIR CACHE KEYS. Nothing it produces is scheduled, executed, rendered or " +
			"attached — jobs.Plan is pure, and the plan is discarded the moment PrecomputeKeys has " +
			"read it. It is the only way to ask \"what key would this project's test task have?\" " +
			"without duplicating the planner, and it stays inside internal/engine so the boundary " +
			"test above is untouched. A THIRD site that hands its plan to a scheduler is still a fork",
	},
	{
		name:  "jobs.Plan declaration",
		token: "func Plan(",
		scope: productionFiles,
		want:  1,
		why:   "there is exactly one planner entrypoint (internal/jobs/planner.go)",
	},
	{
		name:  "canonical result reducer declaration",
		token: "type SessionReducer struct {",
		scope: productionFiles,
		want:  1,
		why: "ADR 0001 §2: one canonical result model, ONE reducer. A0 counted ~20 ad-hoc result " +
			"shapes — every renderer (text, live, JSON, JSONL, cloud logging, MCP, session files, " +
			"profiling, watch, telemetry) re-derived counts, diagnostics and cache attribution " +
			"from raw scheduler events, and no two agreed on what 'cached' meant. An earlier change " +
			"collapsed them onto internal/jobs/result_reduce.go's SessionReducer and " +
			"A2b/A3a–A5b moved every consumer behind it. A second reducer type is the whole class " +
			"growing back: an adapter that needs a field the canonical model lacks means the " +
			"MODEL is wrong, not that the adapter may re-parse events. " +
			"An earlier change moved the declaration to cli-model/jobs/result_reduce.go with the rest of " +
			"the pure result model; the count is unchanged because the walk spans both modules — " +
			"the one reducer changed module, it did not disappear",
	},
	{
		name:  "canonical session fold implementations",
		token: "func reduceSession(",
		scope: productionFiles,
		want:  1,
		why: "the single fold both entry points run through: reduceSchedulerSession (the " +
			"scheduler's own end-of-run result) and ReduceRun (the renderers'). Pinned beside the " +
			"reducer type because a second FOLD is how two callers start disagreeing about the " +
			"same counts even while sharing one reducer struct. " +
			"An earlier change moved it to cli-model/jobs/result_reduce.go with the reducer type it " +
			"folds into; the pin follows it across the module boundary rather than dropping to 0",
	},
	{
		name:  "production jobs.ReduceRun call sites",
		token: "jobs.ReduceRun(",
		scope: productionFiles,
		want:  3,
		why: "the adapters that fold a finished run into the canonical model: " +
			"internal/output's text and cloud-logging finishers, plus internal/machine.Run. This " +
			"is the ADDITIVE half of the one-reducer pin above — the type and fold pins catch a " +
			"fork by RENAME, this one catches a new surface that starts deriving its own tally. A " +
			"new consumer is not forbidden, it is REVIEWED: raise the pin here and say which wire " +
			"it feeds. What must not happen silently is a renderer going back to re-deriving " +
			"counts from raw scheduler events, which is the ~20 ad-hoc result shapes A0 measured " +
			"(ADR 0001 §2). " +
			"An earlier change raised 4→5: internal/machine.Run (machine/run.go) folds the same " +
			"canonical model into the v2 machine documents — every v2 wire (--output json and " +
			"jsonl, MCP run_jobs, session files) derives from that one fold, so v2 added one " +
			"consumer, not four. A later change lowers 5→3 by DELETION, not by consolidation: " +
			"internal/output/jsonl.go's finisher went with the v1 JSONL renderer, and the CLI " +
			"shell's buildSessionStats went with the v1 session writer (both surfaces are now " +
			"machine.Run's). Two folds fewer for the same wires is the direction this pin " +
			"exists to protect",
	},
	{
		name:        "production lines mentioning map[string]any",
		token:       "map[string]any",
		scope:       productionFiles,
		ceilingOnly: true,
		want:        255,
		why: "untyped payload plumbing the canonical result model (A2a/A2b) replaces. " +
			"Counted per line, matching the epic's convention. A0 pinned 208; an earlier change " +
			"typed the session-event seam — SessionEventHandler now takes a jobs.SessionRecord " +
			"carrying a typed *TaskResult, and both readers (the profiler projection in " +
			"jobs_run.go and workspace_state's job table) read that instead of the map. The " +
			"reduction is small on purpose: historical events.jsonl is a PERSISTED wire the sessions " +
			"gate parses, so jobs.JobEndPayload remains as the one legacy projection beside its " +
			"compatibility reader even though new writers use typed v2 records. " +
			"Another change took it back from 207 to 208: engine.Request carries the job " +
			"params as data (CommandParams, RunMarkerParams, and the accessor that defaults one " +
			"to the other) instead of threading them through two stage signatures, so three " +
			"declarations replaced two parameters. The params map itself is the ONE untyped " +
			"payload this epic cannot type: its Go types are part of every cache key " +
			"(store.hashParams, workspace_state.LastBuildParamsHash). " +
			"A later change took it back to 207 with runScheduledExtensionPlan; the pin " +
			"stayed at 208 through A4/A5a/A5b because this one is a ceiling and a reduction is " +
			"the goal. A further change closes the epic, so the ceiling is ratcheted down to the " +
			"tree's real count (205: A4/A5a/A5b's deleted composers, minus jobs.RunRequest's " +
			"CommandParams field, which is the params map arriving as data at the one execution " +
			"site). From here a ceiling of 205 is a real ceiling rather than 3 lines of slack. " +
			"Another change raised 205→206: machine/documents.go's eventPayload alias " +
			"names the opaque runtime-event object the v2 task:event record forwards — " +
			"protocols/runtime owns its members and a later change versions them; the result " +
			"contract does not inspect it. A further change raises 206→207: direct-extension cache " +
			"identity consumes the existing untyped command params map to resolve the exact " +
			"invocation; it does not add a new payload boundary. " +
			"Another change raises 207→210: declared-output capture resolves a v3 " +
			"`pathFrom` output against the executed task's RESULT DATA, which is the second " +
			"untyped payload this epic cannot type (extension-owned members, already read by " +
			"multiCaptureDirs/clientOutputRel). Three signatures name it — the resolver, the " +
			"per-output path step, and the port reader — and nothing new is introduced: the " +
			"same map already reached this file's neighbors. " +
			"A later change raises 210→211 while DELETING a reader: canonicalTaskEvents now " +
			"takes a batch member's own result data (that same extension-owned map) instead of " +
			"the batch wire struct, which is what makes the rendered stream a projection of the " +
			"canonical records rather than a second parse of the wire. Two inline " +
			"`make(map[string]any)` config-defaults sites collapsed into one accessor at the " +
			"same time, so the net is one line for one fewer wire reader. " +
			"Another change raises 211\u2192214 for the sensitive-artifact leak " +
			"guard, and every one of the three lines is a READER of an untyped payload rather " +
			"than a new one: redactData walks the string leaves of a runtime event's Data (the " +
			"decoded JSON object protocols/runtime owns) and of a task result's extension-owned " +
			"data, which is where a leaked credential would travel, and the third is the " +
			"invocation-reaped session record — a projection of the same PERSISTED events.jsonl " +
			"wire every other session record uses. A typed guard would have to enumerate the " +
			"members an extension may invent, which is the enumeration the untyped payload " +
			"exists because nobody can write. A further change raises it 214→215 for " +
			"store.projectConfigDigest, and the line is a normalizer rather than a payload: the " +
			"digest decodes a putnami.json into a map so it can DELETE the execution-only `tasks` " +
			"block before hashing, which is what stops a cpuWeight or timeoutMs edit from " +
			"re-keying every task that declares the config as an input. Typing it would mean " +
			"decoding into wsproto.ProjectConfig and re-marshaling, which silently drops every " +
			"key that struct does not model — turning a cache key blind to fields it must see. " +
			"It mirrors versionStampDigest, which takes the same map for the same reason. A later change " +
			"raises it 215→219, and all four lines thread the ONE payload " +
			"this pin already names as untypable: the command params map. Three are signatures of " +
			"the plan-level shared-node pass (the pass, the identity it derives, and the " +
			"declared-param projection inside it), which has to resolve a task's DECLARED param " +
			"inputs to decide whether two commands are running identical work — the same question " +
			"taskCacheParams answers for the cache key, from the same map, and for the same reason " +
			"it cannot be typed: the values' Go TYPES are what separate an int 1 from a string " +
			"\"1\". The fourth is the config-defaults declaration that projection needs, mirroring " +
			"taskCacheParams line for line. No new payload boundary appears: the pass runs at plan " +
			"time, reads the map that already reached this package, and produces a string. Another change " +
			"raises 219→226: four lines are the one step-local literal parameter bag and its " +
			"type-preserving materializer (including explicit null); three thread the existing command " +
			"parameter bag through the recursive producer identity so shared execution cannot ignore a " +
			"literal that changes upstream work. Both are readers/projections of the same extension-owned " +
			"parameter values already delivered in job context and hashed in cache keys. A later change raises " +
			"226→232: the complete session recorder recursively sanitizes the runtime-owned JSON object, " +
			"projects legacy scheduler audit signals into the v2 event object, exposes that object through " +
			"the existing SessionEvent compatibility reader, classifies a reconstructed runtime event, " +
			"and stamps a stable workspace identity on the physical batch stream — existing persisted and " +
			"runtime wire boundaries, not new result models. Another change raises 232→235: the batch transcript " +
			"projection reads the runtime-owned log context and makes defensive copies of event data and " +
			"context before removing its private routing member. The aggregate event stays immutable and " +
			"no new public payload boundary is introduced. A later change raises 235→236: specgate's policy " +
			"binding names wsproto's own open options type (map[string]map[string]any) once, to hand " +
			"the committed options.sdd.verification blocks to the ONE resolver in protocols/features — " +
			"the options map is authored per-extension configuration whose keys core must not " +
			"enumerate, the same wire cpu_policy.go and cache_trust.go already read. Another change raises " +
			"236→237 for JobContextWorkspace.Options, the one additive wire carrier that hands the " +
			"already-loaded workspace option blocks to workspace-scoped extension tasks; it is the " +
			"same open authored config boundary, not a new result model. A later change raises 237→243 for " +
			"the release-set SELECTION key, and all four lines thread the ONE payload this pin " +
			"already names as untypable: the command params map. Three are the selection variants " +
			"of the existing key derivation (computeJobCacheHashWith, taskCacheParamsWith, and the " +
			"producer recursion between them) — the same map taskCacheParams already reads, for " +
			"the same reason it cannot be typed: the values' Go TYPES separate an int 1 from a " +
			"string \"1\". The fourth is jobs.SelectionFingerprints, which hands that same map to " +
			"that derivation once per run. The last two are the release-set seam the engine calls " +
			"between selection and planning (jobs.PrepareReleaseSetRun and the fingerprint " +
			"resolution inside it), which forward that same map one hop further so the engine " +
			"holds no release-set policy. No new payload boundary appears: nothing decodes, " +
			"stores or forwards a new untyped document. That same change also raises 243→246 for the DEPLOY " +
			"contract, and the three lines are the same two open authored boundaries this pin " +
			"already names. DeployTarget.Constraints and deployWorkload.constraints carry " +
			"ciproto.Environment.Constraints verbatim — the CI document declares `approval: " +
			"manual` and leaves the rest open, so the protocol itself types the block as an open " +
			"object and the CLI forwards it to the backend without enumerating its keys. The " +
			"third is jobs.BuildDeployOptions, which reads --env and --release out of the ONE " +
			"command params map, exactly as the release-set option builder beside it does. Another change " +
			"raises 246→248, and both lines are the SAME two parameters this pin already names as " +
			"untypable, on one more hop of the path they already travel: jobs.runJob is RunJob plus " +
			"the run-level process environment the scheduler owns (the cache provider's object-cache " +
			"socket), so the two exported call sites keep their signature and the scheduler reaches " +
			"the seam directly. Nothing decodes, stores or forwards a new untyped document, and the " +
			"added environment is deliberately NOT part of that payload: it is a []string that " +
			"reaches cmd.Env and never a cache key. A later change raises 248→249 for the ONE line in " +
			"jobs/task_drift.go that projects a drift verdict onto the runtime event wire: a " +
			"diagnostic event's Data is the decoded JSON object protocols/runtime owns, and the " +
			"engine's own diagnostic has to take the exact shape a subprocess diagnostic has so " +
			"every consumer reads it through the one existing path (the batch projection in " +
			"scheduler_batch_exec.go builds the same object the same way). Another change (ADR 0037) " +
			"raises 249→250 for jobs.PortableInputs, and the line is the ONE payload this pin " +
			"already names as untypable on one more hop of the path it already travels: the " +
			"portable admission hands the command params map to keyFilePatterns, exactly as " +
			"the cache key and the mutation detector do, so the files it binds are the files " +
			"the key hashes. Nothing decodes, stores or forwards a new untyped document. " +
			"A later change (`putnami compose`) raises 250→253: two lines are jobs.RunJobWithProcessEnv's " +
			"copy of runJob's command params and config defaults, the same pair itemized above for " +
			"runJob itself, and the third is internal/compose's serveParams, the one param a " +
			"composed serve step receives (port 0) stated in the existing untyped params bag. The " +
			"environment compose adds is a []string that reaches cmd.Env and never a cache key. A further change adds one: ImpactTraceRecord.EventData, the typed selection trace projected onto the untyped session-event seam (jobs.SessionRecord.Data) at the single point it enters it. " +
			"The final change raises 254→255 for testCasesOf in cli-model/jobs/result_model.go: a test task hands its per-case results to the CLI in the result's untyped data bag, beside testSummary, and this is the one decode that turns them into typed protocolcli.TestCase values",
	},
	{
		name:        "test lines mentioning map[string]any",
		token:       "map[string]any",
		scope:       testFiles,
		ceilingOnly: true,
		want:        639,
		why: "test fixtures built from untyped maps; they shrink as the canonical model lands. " +
			"A0 pinned 516; A2b's scheduler and profiler tests now assert against typed records. " +
			"An earlier change took it from 513 to 515: the two auto-selection tests that used to call " +
			"the CLI's buildCommandParams now state the params map literally, because the params " +
			"reach the engine on the Request instead of being re-derived inside selection. " +
			"Another change added one: the alias run-marker test states the two candidate marker " +
			"keys literally, because proving the synthesized dry-run param stays OUT of the key " +
			"requires naming both maps. A later change added one more for the same reason on the " +
			"lifecycle side: TestUpgradeDeps_ForwardsParamsWithTheirTypes states the expected " +
			"deps-upgrade params literally, because proving their Go TYPES survive the trip to " +
			"the engine adapter (they are part of every cache key) requires naming the map. " +
			"Another change added one on the watch side, for the third time and the same reason: " +
			"TestWatchIterationRequest_ForwardsTheRunsIdentity states the outer run's params so " +
			"the per-iteration Request can be proved to forward that exact map, types included. " +
			"A further change added the fourth and last: the params now cross ONE more hop " +
			"(engine.Request → jobs.RunRequest → the scheduler), so " +
			"TestRunRequest_ForwardsCommandParamsWithTheirGoTypes states a map with one value per " +
			"Go type buildCommandParams produces and proves the scheduler receives that same map, " +
			"unconverted — a stringified int there would invalidate every warm cache entry. " +
			"Another change added four: the v2 session-reader projection test " +
			"(internal/commands/sessions_helpers_v2_test.go) states three historical job:end Data maps " +
			"that preserve the gate's v1 reader, plus one opaque scheduler member whose shape belongs " +
			"to the scheduler. " +
			"A later change added two: the declared-capture tests' one shared result constructor and " +
			"the single literal `clientOutput` port map that proves a pathFrom output resolves " +
			"from the extension's result data. " +
			"Another change adds a NET one: the watch package's readiness proof gains three (the " +
			"decoded RawJobEvent.Data a readiness payload arrives in, the log event whose text no " +
			"longer arms anything, and the serve loop's port param) while the renderer tests it " +
			"replaces give back two. RawJobEvent.Data is a decoded JSON object — the wire's shape, " +
			"not a choice the test makes — and the readiness payload itself is stated through " +
			"protocols/runtime's ReadyData wherever the protocol admits it. " +
			"A further change adds one: the batch-attribution test decodes its aggregate from JSON " +
			"into the one JobResult.Data map, so the split is exercised against the shape a " +
			"subprocess actually delivers rather than a hand-built literal. " +
			"Another change adds one: the end-to-end serve proof runs the readiness path through a " +
			"real subprocess, and the serve loop's port param is the one input it must state as " +
			"a params map (CommandParams is the untyped payload the loop reads `port` from). " +
			"Everything else in that test is stated through protocols/runtime's own types and " +
			"its conformance fixture. 528→529 is not from a reviewed change — that change added " +
			"none: it is drift this branch inherited. A later change (`.gen/version.json` re-stamping " +
			"after a cross-revision cache hit) added six such lines — the stamp is a JSON " +
			"document the tests decode and mutate as an untyped object, which is the shape it " +
			"has on disk — without moving the pin, so origin/main has been over the ceiling " +
			"since that merge and every branch cut from it inherits the failure. Corrected here " +
			"rather than left red, because a ceiling nobody can satisfy stops being read. Another change raises " +
			"529→535 for the literal-binding context/cache fixtures: they state conflicting command and " +
			"bound parameter bags so precedence, explicit null, non-mutation and cache-key separation are " +
			"proved with the values' original Go types. A later change raises 535→536: the --cpu-policy " +
			"resolver's precedence tables name the workspace `options` block ONCE, through a local " +
			"alias, rather than spelling the untyped map at each of their nine literals. It is " +
			"wsproto.Config.Options' own declared type reaching a test, not a new payload boundary. " +
			"Another change raises 536→540 for the MCP workspace_map tool tests: two `tools/call` argument " +
			"bags and the shared call helper's params bag are the JSON-RPC WIRE reaching a test " +
			"(a client sends an object, not a Go type), and the fourth is the decoded tool result " +
			"the mcp fixtures already hand back as map[string]any. The tool's own payload is stated " +
			"through declared fixture types, and the commands-side subset assertion walks " +
			"mapgen.ProjectEntry reflectively instead of round-tripping it through generic maps. " +
			"A later change raises 542→559 for the wire-shaped flood/failure fixtures and v2 session-gate " +
			"records that prove sanitization, hard partitioning, complete callbacks, lossless numeric " +
			"tokens, batch attribution and dual reads. Another change raises 559→580: the Go, TypeScript, " +
			"scheduler and report tests state JSON-shaped runtime contexts and result payloads " +
			"literally so exact routing, non-mutation and wire omission fields are asserted. The " +
			"final two lines model the decoded multi-project log context that proves one shared " +
			"batch event routes to both owners without amplification. These are test-only " +
			"protocol-boundary fixtures. A later change raises 580→589: specgate's policy tests state " +
			"committed options.sdd.verification blocks literally — the workspace/project options " +
			"wire is an authored JSON object, and proving that an unknown domain, an unknown value " +
			"or a wrong TYPE is refused before jobs execute requires naming maps that carry those " +
			"exact shapes, the same reason the --cpu-policy precedence tables state theirs. Another change " +
			"raises 598→603 for the producer round-trip and workspace-policy fixtures: five lines " +
			"state wsproto.Config.Options or its decoded JSON object so the v2 carrier preserves " +
			"types and the architecture task ignores a project override. A later change raises 603→615 for " +
			"the release-set selection-key and published-member fixtures. Seven are in " +
			"precompute_test.go: the selection-fingerprint proof states the command params bag at " +
			"each invocation it must show does NOT move the key (a channel, another channel, a " +
			"dry run, no flags at all, the bound plan) — proving a key ignores a value requires " +
			"naming that value with its Go type. Five are in release_set_test.go: the " +
			"published-member event fixtures, whose Data is the DECODED runtime-event object " +
			"(ParseRawEvent hands the coordinator a map), and the mutators that prove one unknown " +
			"or malformed member field fails the release closed. That same change also raises 615→617 for the two " +
			"deploy-flag literals in environments_test.go: proving that --env and --release are " +
			"read out of the run's parameter bag, and that a run which does not deploy asks for " +
			"no environment, requires stating that bag with its Go types. Another change raises 617\u2192618 " +
			"for the ONE committed-policy literal in specgate/recover_test.go: the gate's enforce " +
			"mode is read out of the open options surface (options.sdd.verification.specs), and a " +
			"fixture that proves an enforce-mode group must state that surface with its Go type. " +
			"A later change raises 618→619 for the ONE payload literal in jobs/task_failure_test.go: the " +
			"failure cache memoizes one negative entry per key per run, and proving that two " +
			"members of a plan-level shared node are served that record WITHOUT sharing a mutable " +
			"alias of its payload requires stating the payload — one member rewrites a key and the " +
			"other must still read the original. JobResult.Data is that untyped bag, so the " +
			"fixture states it with its Go type. Another change raises 619→620 for the ONE result-data " +
			"literal in jobs/task_drift_test.go: proving that a pathFrom output is judged at the " +
			"path the task REPORTED requires stating the task's result data — the port value is the " +
			"extension-owned payload the existing capture fixtures already state the same way. That same change also " +
			"raises 620→621 for the ONE diagnostic-event literal in output/restored_failure_test.go: " +
			"a restored failure's diagnostic is the decoded runtime event object the renderers read, " +
			"stated the way every other renderer fixture states one. " +
			"Another change raises 621→623 for the two literals in engine/portable_test.go: the command params " +
			"map the portable projection must carry with its Go TYPES intact (a stringified int would " +
			"re-key every task on the executing side), and the one non-portable map value it must " +
			"refuse. Production adds none: the projection reads the existing Request.CommandParams. " +
			"A later change raises 623→624 for the ONE literal in engine/release_set_session_test.go: a mixed " +
			"gate+publish session is a session whose `--channel` is a command PARAM, so the fixture " +
			"Request has to state that one existing untyped bag to exist at all. It decodes, stores " +
			"and forwards nothing new, and production adds none. " +
			"Another change raises 624→625 for ONE more line: the command params a publish is invoked with, " +
			"stating that --baseline-channel reaches the release-set coordinator as a typed option " +
			"beside the channels it is the baseline of. The second case of that test clones the " +
			"same map rather than writing a second literal, and production adds none — " +
			"buildReleaseSetOptions reads one more key out of the map it already reads. " +
			"A later change raises 625→630 for five lines, each stating an existing untyped bag a " +
			"composition test needs: two watch session CommandParams with port 0 and a bound port " +
			"(the ephemeral-port release wait), one RunJobWithProcessEnv call's params, and two " +
			"ProjectConfig.Options literals carrying options.serve.port for the proxy port default. " +
			"Another change raises 631\u2192634 for THREE lines: the workspace documents the declared-edges mode " +
			"and the two option blocks a declared-input backing test builds. " +
			"test builds. A further change raises 630\u2192631 for ONE line: the workspace document one visibility-mode " +
			"test builds, stating the switch value whatever its JSON type; production adds none, " +
			"because the resolver reads the Options bag core already carries. " +
			"A later change (ADR 0044) raises 631→635 for four lines in ONE helper, " +
			"workspace/task_index_test.go projectOptions: the Options map it builds and the three " +
			"layer literals inside it (filePatterns, the generate assets list's entries, and the " +
			"assets layer). ProjectConfig.Options is untyped BY THE PROTOCOL — extension options " +
			"are not part of the project schema — and the task index attributes those layers onto " +
			"the tasks that read them, so a test of that attribution has to state one. The four " +
			"lines are a single constructor every case calls; writing the layers per case would " +
			"have cost six. " +
			"A later change raises 638→639 for ONE line: treecmd/verify_test.go names the untyped " +
			"JSON object once (fxObj) to build the dossier, session and report documents `tree verify` " +
			"reads. Those documents are the execute skill's wire, written by agents as JSON, so the " +
			"fixtures state them as the decoded JSON they are; production adds none, because the " +
			"verifier reads them through its own ordered JSON values",
	},
	{
		name:        "all lines mentioning map[string]any",
		token:       "map[string]any",
		scope:       allFiles,
		ceilingOnly: true,
		want:        894,
		why: "the epic body's headline number: production + test lines. A0 pinned 724; " +
			"an earlier change took it from 720 to 723 for the reasons named on the two scoped pins " +
			"above, and a later change closed it at 724 = 205 production + 519 test. Another change " +
			"raised it to 725 = 206 production + 519 test; a further change raises it to 726 = 207 " +
			"production + 519 test because computing a direct local extension's cache identity " +
			"consumes the existing command params map. Another change raises it to 730 = 207 " +
			"production + 523 test: the four fixture maps named on the test pin above, all of " +
			"them projections of the PERSISTED v1 events.jsonl wire. One change raises it by a " +
			"NET one for the watch readiness proof, and another raises it by one more for the serve " +
			"loop's port param in the end-to-end version of that proof, both itemized on the " +
			"test pin above. The params map " +
			"remains the one untyped payload this code cannot type: its Go types are cache-key " +
			"input. A later change raises it by three, all of them production " +
			"READERS of payloads that already existed, itemized on the production pin above. " +
			"Another change raises it by one more, for store.projectConfigDigest's normalizing decode, " +
			"itemized on the production pin above. " +
			"The ceiling remains intentional; it should make any further untyped payload " +
			"growth a reviewed decision rather than a silent regression. A later change raises 748→761 by the " +
			"seven production and six test lines itemized on the scoped pins above; every line preserves " +
			"the typed literal value in the existing untyped task-parameter boundary. Another change raises " +
			"761→762 for the single test line itemized on the test pin above; production adds none. " +
			"A later change raises 762→765 = 225 production + 540 test: the four wire-shaped test lines " +
			"itemized on the test pin above, against a production side that adds none and had " +
			"already drifted one under this ceiling. Another change raises 767→791 for the six production " +
			"and seventeen test lines itemized above, plus the prior one-line arithmetic drift between " +
			"the scoped ceilings and this aggregate pin. A later change raises 791→815 for the production " +
			"projection and test-fixture sites reviewed on the two scoped pins above. Another change raises " +
			"815→834 = 236 production + 598 test for the specgate policy binding, its " +
			"invalid-config refusal fixtures, and the collector and finalizer wire-shaped fixture " +
			"lines, itemized on the two scoped pins above. A later change raises 834→840 = 237 production + " +
			"603 test for the additive workspace-options carrier and its exact wire/policy proofs, " +
			"itemized on the two scoped pins above. Another change raises 840→858 = 243 production + 615 " +
			"test for the release-set selection key: six production lines threading the existing " +
			"command params map through the selection variant of the one key derivation, and " +
			"twelve test lines stating those params and the decoded published-member event, all " +
			"itemized on the two scoped pins above. That same change also raises 858→863 = 246 production + 617 " +
			"test for the deploy contract: three production lines carrying the CI document's own " +
			"open constraints block and reading the two deploy flags out of the command params " +
			"map, and two test lines stating that bag, all itemized on the two scoped pins above. " +
			"A later change raises it by two for the object-cache job environment: the two production lines " +
			"are runJob's copy of the command params and config defaults, itemized on the " +
			"production pin above; the tests add none. Another change raises 866→867 = 246 production + " +
			"619 test for the failure cache's one payload fixture, itemized on the test pin " +
			"above; production adds none. A later change raises 867→870 = 249 production + 621 test for " +
			"the drift diagnostic's wire projection, its one port-value fixture and the restored " +
			"failure's one diagnostic fixture, itemized on the two scoped pins above" +
			" Another change raises 870→872 for the two portable-projection test lines itemized on the test pin above; production adds none." +
			" That same change also raises 872→873 = 250 production + 623 test for jobs.PortableInputs, itemized on the production pin above; tests add none. " +
			"A later change raises 873→874 = 250 production + 624 test for the mixed gate+publish session fixture, itemized on the test pin above; production adds none. " +
			"Another change raises 874→875 = 250 production + 625 test for the one publish-params line itemized on the same pin; production adds none. " +
			"A later change raises 875→883 = 253 production + 630 test for the composition lines itemized on the two scoped pins above. " +
			"Another change raises 884→885 = 253 production + 631 test for the one visibility-mode test line itemized on the test pin above; production adds none. " +
			"A later change raises 885→889 = 253 production + 635 test for the one option-layer constructor itemized on the test pin above; production adds none. " +
			"A further change raises 889→892 = 253 production + 638 test for the three declared-edges test lines itemized on the test pin above; production adds none. " +
			"The final change raises 892→893 = 255 production + 638 test for the one test-case decode itemized on the production pin above; tests add none. " +
			"A later change raises 893→894 = 255 production + 639 test for the one `tree verify` fixture alias itemized on the test pin above; production adds none",
	},
	{
		name:  "lines mentioning map[string]interface{}",
		token: "map[string]interface{}",
		scope: allFiles,
		want:  0,
		why:   "the tree uses the `any` spelling exclusively; reintroducing the long form splits the count",
	},
}

func TestStructuralBaseline_PinnedCounts(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	production, test := moduleSources(t, root)

	for _, pin := range structuralPins {
		var files []string
		switch pin.scope {
		case productionFiles:
			files = production
		case testFiles:
			files = test
		case allFiles:
			files = append(append([]string{}, production...), test...)
		}

		got, sites := countTokenLines(t, root, files, pin.token)
		switch {
		case got > pin.want:
			t.Errorf("%s: %d, pinned at %d — something ADDED %d.\n  %s\n  Sites:\n%s",
				pin.name, got, pin.want, got-pin.want, pin.why, indentSites(sites))
		case got < pin.want && !pin.ceilingOnly:
			t.Errorf("%s: %d, pinned at %d — something REMOVED %d. Lower the pin in %s so the reduction is reviewed.\n  %s",
				pin.name, got, pin.want, pin.want-got, "structural_baseline_test.go", pin.why)
		}
	}
}

// commandTableDeclaration is one of the six hand-maintained command tables the
// epic collapses into a single CommandCatalog.
type commandTableDeclaration struct {
	file string
	decl string
	note string
}

// commandTables pins that exactly the remaining hand-maintained tables exist,
// each declared exactly once. It is EMPTY, which is the epic's terminal state:
// the catalog below is the only table describing commands.
//
// A0 pinned six. An earlier change deleted three of them as hand-maintained
// source — structuredCommandCategories, relatedCommands, and
// structuredCommandHelp are now derived from the command catalog — dropping the
// pin to three. A later change deleted the last two duplicates:
// jsonlCapableStructuredCommands (with the hand-keyed canonicalStructuredCommand
// switch beside it) became Command.StructuredOutput plus Command.DefaultSub, and
// completion's `structured`/`structuredDesc`/`subcommands` literals became a
// projection of the catalog.
//
// A further change took the last one to zero WITHOUT deleting commandRegistry,
// because the map is not the duplicate — its VOCABULARY ROLE was. commandRegistry
// answered "which names are structured commands" (isStructuredCommand read it)
// as well as "what runs for this name". A6a moved the first question to
// commandmeta.IsStructuredRoot and made registerCommand panic on a name the
// catalog does not declare, so what is left is a dispatch table binding names to
// Go function values — something the catalog package cannot hold without
// importing internal/cli. Every fact ABOUT a command now has exactly one home.
//
// Honest limitation: "a table" is a semantic notion, so this cannot detect an
// arbitrary seventh table by shape alone. What it does guarantee is that the
// catalog does not silently split or move, and that adding a table back here is
// a reviewed edit. The catalog↔registry agreement itself is asserted, in both
// directions, by TestCatalog_CoversCommandRegistry (A1a) plus registerCommand's
// own panic — which is why A6a could delete A0's command_tables_baseline_test.go
// scaffolding in this package and in internal/commands.
var commandTables = []commandTableDeclaration{}

// commandCatalog is the single source the derived tables read. It is pinned the
// same way, in the opposite direction: it must exist exactly once, so a slice
// cannot fork a second catalog beside it. Its global-flag half is pinned with
// it — A1b collapsed five copies of the flag vocabulary (help, completion, zsh,
// fish, and the deleted completion.flags list) onto that one declaration.
var commandCatalog = commandTableDeclaration{
	"internal/commandmeta/catalog_data.go", "var catalog = []Command{", "the single command catalog (ADR 0001)",
}

var commandCatalogGlobalFlags = commandTableDeclaration{
	"internal/commandmeta/catalog_data.go", "var globalFlags = []GlobalFlag{", "the single global-flag table (ADR 0001)",
}

func TestStructuralBaseline_CommandTableCount(t *testing.T) {
	t.Parallel()
	// Zero: the epic's terminal state. This is a two-way ratchet like the call
	// site pins — a table reappearing fails, and so does deleting the pin's own
	// bookkeeping without review.
	const wantTables = 0
	if len(commandTables) != wantTables {
		t.Fatalf("commandTables lists %d tables, pinned at %d — a hand-maintained command table "+
			"came back. ADR 0001 §1: the catalog is the single command vocabulary and everything "+
			"else is derived from it.", len(commandTables), wantTables)
	}

	root := moduleRoot(t)
	sources := append([]commandTableDeclaration(nil), commandTables...)
	sources = append(sources, commandCatalog, commandCatalogGlobalFlags)
	for _, table := range sources {
		data, err := os.ReadFile(resolveSource(root, table.file))
		if err != nil {
			t.Errorf("command table %q (%s): %v", table.decl, table.note, err)
			continue
		}
		if occurrences := strings.Count(string(data), table.decl); occurrences != 1 {
			t.Errorf("%s declares %q %d times, want exactly 1 (%s).\n"+
				"  If a slice removed it, delete the entry here and drop wantTables to %d.",
				table.file, table.decl, occurrences, table.note, wantTables-1)
		}
	}
}

// cliModule is one module a structural scan reads: dir locates it relative to
// tooling/cli's module root, and prefix is prepended to every path it reports.
type cliModule struct {
	dir    string
	prefix string
}

// cliModules are the modules that together hold the CLI, and the single place
// this package learns about a new one.
//
// Structure is a property of the CODE, not of the module that happens to store
// it. An earlier change lifts the pure workspace, job and extension model out of
// tooling/cli into go.putnami.dev/cli/model, so a declaration, a call site or a
// provider spelling can now walk across a module boundary without changing
// anything about what it does — and a scan that lost sight of it would report an
// invariant nothing guards. Every scan in this package that pins a property of
// the CLI as a whole (the structural pins, the provider-vocabulary ratchet, the
// runtime-event acceptance ratchet) reads this list, so the next move is one
// entry here rather than a new mechanism per scan.
//
// tooling/cli's own files keep their bare module-relative keys
// (`internal/jobs/…`); the model module's files are keyed under its directory
// (`cli-model/jobs/…`), so an allowlist key, a pinned file and a failure message
// name a file unambiguously whichever module holds it. resolveSource maps a key
// back to a path, so a key is never just a label.
var cliModules = []cliModule{
	{dir: ".", prefix: ""},
	{dir: "../cli-model", prefix: "cli-model/"},
}

// moduleSources returns every .go file across cliModules, split into production
// and test, keyed by the module's prefix + its module-relative slash path.
//
// Each module is walked by goSources, the same walk the single-module scans use,
// so a module whose directory moved or emptied fails loudly there rather than
// silently contributing nothing here.
func moduleSources(t *testing.T, root string) (production, test []string) {
	t.Helper()
	for _, module := range cliModules {
		moduleProduction, moduleTest := goSources(t, filepath.Join(root, filepath.FromSlash(module.dir)))
		for _, rel := range moduleProduction {
			production = append(production, module.prefix+rel)
		}
		for _, rel := range moduleTest {
			test = append(test, module.prefix+rel)
		}
	}
	sort.Strings(production)
	sort.Strings(test)
	return production, test
}

// resolveSource maps a key in the cliModules key space back to a filesystem
// path under root. The longest matching prefix wins, so tooling/cli's own bare
// keys (which match the empty prefix) still resolve against root.
func resolveSource(root, key string) string {
	owner := cliModule{dir: ".", prefix: ""}
	for _, module := range cliModules {
		if strings.HasPrefix(key, module.prefix) && len(module.prefix) >= len(owner.prefix) {
			owner = module
		}
	}
	rel := strings.TrimPrefix(key, owner.prefix)
	return filepath.Join(root, filepath.FromSlash(owner.dir), filepath.FromSlash(rel))
}

// moduleRoot walks up from the test's working directory to the directory
// holding tooling/cli's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above the test working directory")
		}
		dir = parent
	}
}

// goSources returns every .go file under root, split into production and test,
// as paths relative to root and sorted for deterministic failure output.
func goSources(t *testing.T, root string) (production, test []string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", ".putnami", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || info.Name() == structuralPinFile {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(info.Name(), "_test.go") {
			test = append(test, rel)
		} else {
			production = append(production, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(production)
	sort.Strings(test)
	if len(production) == 0 {
		t.Fatal("found no production Go files — the walk root is wrong")
	}
	return production, test
}

// countTokenLines counts LINES containing token (a line with two occurrences
// counts once) and returns up to a handful of "file:line" sites for the
// failure message. Files are named in the cliModules key space, so the same
// call counts over one module or over all of them.
func countTokenLines(t *testing.T, root string, files []string, token string) (int, []string) {
	t.Helper()
	const maxReportedSites = 12
	count := 0
	var sites []string
	for _, rel := range files {
		data, err := os.ReadFile(resolveSource(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, token) {
				continue
			}
			count++
			if len(sites) < maxReportedSites {
				sites = append(sites, rel+":"+strconv.Itoa(i+1))
			}
		}
	}
	return count, sites
}

func indentSites(sites []string) string {
	if len(sites) == 0 {
		return "    (none)"
	}
	return "    " + strings.Join(sites, "\n    ")
}
