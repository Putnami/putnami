package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/cli/model/workspace"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/specgate"
	"go.putnami.dev/tooling/cli/internal/store"
)

// The engine's half of the unplanned-attester seam.
//
// internal/specgate is the policy half and stays there: it decides when to ask,
// which projects may be asked, and what an answer means. Turning a project into
// cache bytes needs the planner, the run's command params, its version lines,
// its cache manager and its remote cache — five things this package already
// holds and that specgate's package doc says it must not grow ("imports protocol
// types and the CLI's own result plumbing only"). So the lookup lives here and
// travels as an interface.
//
// Nothing is materialized into the workspace. The report is read IN PLACE out of
// the entry's files/ directory: a gate that restored bytes into .putnami/out
// would leave a project that never ran looking like it did, and the next
// session's collector would read a stale artifact as a fresh one. An entry only
// the run's remote cache holds is fetched into the local store, never further,
// and read there the same way.

// Bounds of the remote lookups of one gate. A lease another process holds or a
// provider that does not answer costs one lookup specGateRemoteLookupTimeout at
// most, and every remote lookup of the gate together specGateRemoteBudget.
const (
	specGateRemoteLookupTimeout = 15 * time.Second
	specGateRemoteBudget        = 45 * time.Second
)

// remoteTaskEntries is the one remote-cache operation the recovery needs
// (remotecache.RemoteCache.FetchTaskEntry): fetch the task-owned entry at hash
// into the local store, never into the workspace, and return it. ctx is the
// run's context; within bounds the lookup itself. A miss is (nil, nil).
type remoteTaskEntries interface {
	FetchTaskEntry(ctx context.Context, within time.Duration, hash string,
		job *scheduler.ScheduledJob, cm *store.CacheManager) (*store.TaskEntry, error)
}

// executionRemoteEntries is the remote cache the session executes with, as the
// recovery reads it, or nil when the session reads none.
func (req *Request) executionRemoteEntries() remoteTaskEntries {
	if req == nil || req.executionRemote == nil {
		return nil
	}
	return req.executionRemote
}

// cachedObservationRecovery answers specgate.ObservationRecovery from the
// task-owned cache: the local store first, then the run's remote cache.
type cachedObservationRecovery struct {
	// req is held whole rather than copied field by field. A recovery lookup has
	// to key EXACTLY as the run keyed: the same command params, the same disable
	// lists, and above all the run's OWN pre-mutation git snapshot
	// (Request.VersionSnapshot) — re-reading the tree here would key against
	// whatever a codegen job left behind, and a dirty suffix the execution keys
	// never carried moves EmbeddedVersion and misses every entry.
	req        *Request
	ws         *workspace.Workspace
	extensions []*extension.ExtensionDescription
	cache      *store.CacheManager
	// disabled carries the one reason no lookup can be served at all, so every
	// candidate is reported unconsulted with an accurate cause instead of a
	// generic "no cache entry".
	disabled unconsultedCause

	once      sync.Once
	versions  cachekey.RunVersions
	entriesMu sync.Mutex
	entries   map[string]*store.TaskEntry

	// ctx is the run's context and remote resolves the run's remote cache at
	// lookup time, because a run that is not hosted loads its remote cache after
	// this source is built (readingRemote). A nil remote, or a nil answer, reads
	// the local store only.
	ctx    context.Context
	remote func() remoteTaskEntries
	// remoteLookup bounds one remote lookup and remoteBudget all of them.
	remoteLookup time.Duration
	remoteBudget time.Duration
	// remoteMu guards remoteEnd, the end of the budget, set by the first
	// remote lookup, and remoteMisses: each hash a remote lookup returned no
	// entry for, with why the remote cache could not answer, or the zero
	// cause for a miss. A hash is asked at most once per gate.
	remoteMu     sync.Mutex
	remoteEnd    time.Time
	remoteMisses map[string]unconsultedCause

	// planned is the run's own plan as the gate reads it, or nil when the
	// source does not serve a run (runObservationRecovery).
	planned *runPlanCoverage
	// answeredMu guards answered: each candidate's outcome, once a plan of this
	// gate derived it, or nil when the source keeps none. A candidate's answer
	// depends on the candidate alone, so a candidate several spec owners name
	// is planned and keyed once per gate.
	answeredMu sync.Mutex
	answered   map[string]*candidateOutcome
	// plans counts the plans RecoverReports derived: one per call that met a
	// candidate neither the run's plan nor an earlier call answered.
	plans atomic.Int64
}

// runPlanCoverage is what the run's own plan already answers for the
// recovery: the projects the run planned every verification command for, and
// how many of the run's tasks for each project can write its report. A
// selected project with none is consulted and silent, exactly as planning it
// again would find it.
type runPlanCoverage struct {
	selected  map[string]bool
	producers map[string]int
}

// unconsultedCause is why a candidate could not be consulted: prose for the
// unobserved-requirement warning, and the same cause from the record's closed
// set. The zero value, with no code, is no cause.
type unconsultedCause struct {
	prose string
	code  features.UnconsultedReason
}

// newCachedObservationRecovery builds the recovery source for one run, or nil
// when the run cannot carry one.
//
// Under --no-cache the source is built but permanently disabled: no cached byte
// may be served, so Part A yields nothing and every unresolved check falls to
// the non-blocking unobserved classification. That is the accepted outcome —
// `--no-cache` with a narrowed selection deliberately gets no second mechanism
// — and stating it as a reason is what keeps the resulting warning honest.
func newCachedObservationRecovery(req *Request, ws *workspace.Workspace,
	extensions []*extension.ExtensionDescription, cache *store.CacheManager) *cachedObservationRecovery {
	if req == nil || ws == nil {
		return nil
	}
	source := &cachedObservationRecovery{req: req, ws: ws, extensions: extensions, cache: cache}
	switch {
	case req.Global.NoCache:
		source.disabled = unconsultedCause{"--no-cache: no cached observation may be served", features.UnconsultedNoCache}
	case cache == nil:
		source.disabled = unconsultedCause{"this run has no cache store", features.UnconsultedNoCache}
	}
	return source
}

// readingRemote makes the source ask, on a local miss, the remote cache remote
// resolves to at lookup time, under the run's context ctx. A source without it
// reads the local store only: the read-only verification bridge
// (VerifyCachedVerificationReport) never calls it.
func (source *cachedObservationRecovery) readingRemote(ctx context.Context,
	remote func() remoteTaskEntries) *cachedObservationRecovery {
	if source == nil || ctx == nil || remote == nil {
		return source
	}
	source.ctx, source.remote = ctx, remote
	source.remoteLookup, source.remoteBudget = specGateRemoteLookupTimeout, specGateRemoteBudget
	return source
}

// runObservationRecovery is the recovery source of a run's spec gate: the
// run's cache manager, then, on a local miss, the remote cache the session
// records when its execution starts (Request.executionRemote), asked under the
// run's context ctx.
//
// It also answers, from the run's own plan, every candidate the run selected
// (selected) under every verification command, and plans again only the
// others. The run's plan (planned) holds the same tasks for such a candidate
// that a recovery plan would: a task's plan, like its key, depends on its
// project and its upstream, never on the projects planned beside it. The
// recovery plans every candidate when the run's plan was cut after planning,
// or planned from something the recovery does not have: a measured diff,
// which gates steps (planner.Plan's changedFiles) and narrows a project to
// its impacted tasks (narrowToTaskScopes); an executing dry run, which drops
// the jobs it cannot run (dropUndryableSideEffectJobs); and a lifecycle run,
// which plans without the workspace's disable lists the recovery applies. A
// step whose condition reads the run's other commands (WhenContext.Commands)
// could also differ; no verification producer of this repository has one.
func runObservationRecovery(ctx context.Context, req *Request, ws *workspace.Workspace,
	extensions []*extension.ExtensionDescription, cache *store.CacheManager,
	planned []*scheduler.ScheduledJob, selected []*workspace.Project) *cachedObservationRecovery {
	source := newCachedObservationRecovery(req, ws, extensions, cache).readingRemote(ctx, req.executionRemoteEntries)
	if source == nil {
		return source
	}
	// One gate's answers are kept for the gate: the run's store does not change
	// under it. The read-only verification bridge keeps none, since it reads an
	// entry again on every call.
	source.answered = make(map[string]*candidateOutcome)
	dryRun, _ := req.CommandParams["dry-run"].(bool)
	if req.WorkspaceLifecycle || req.selectionEvidence.changedPaths != nil ||
		len(req.selectionEvidence.taskScopes) > 0 || (req.ExecutesUnderDryRun && dryRun) {
		return source
	}
	runCommands := make(map[string]bool, len(req.Commands))
	for _, command := range req.Commands {
		runCommands[command] = true
	}
	for _, command := range scheduler.CommandsOfKind(extensions, extension.CommandKindVerification) {
		if !runCommands[command] {
			return source
		}
	}
	coverage := &runPlanCoverage{selected: make(map[string]bool, len(selected)), producers: map[string]int{}}
	for _, project := range selected {
		if project != nil {
			coverage.selected[project.ID] = true
		}
	}
	for _, job := range planned {
		if job != nil && job.Project != nil && scheduler.ProducesVerificationReport(job) {
			coverage.producers[job.Project.ID]++
		}
	}
	source.planned = coverage
	return source
}

// RecoverReports keys one plan of the loaded verification commands over the
// candidates the run's own plan does not already answer (runObservationRecovery), and
// reports where each candidate's captured verification report lives, or why it
// could not be consulted. Only the tasks that can
// write the report are asked (see jobs.ProducesVerificationReport); the other
// steps of those commands cannot answer for it.
func (source *cachedObservationRecovery) RecoverReports(candidates []*workspace.Project) specgate.Recovery {
	recovery := specgate.Recovery{}
	if source == nil || len(candidates) == 0 {
		return recovery
	}
	if source.disabled.code != "" {
		return source.unconsultedAll(candidates, source.disabled)
	}

	// ONE plan and ONE keying pass over the whole candidate set, rather than the
	// per-candidate pair. The two are equivalent — a cache key
	// mixes declared inputs and UPSTREAM KEYS, never the run's selection, so a
	// task keys the same whichever projects are planned beside it (that is the
	// property every warm hit already depends on) — and the batch pays one
	// topological pass and one activation-probe cache instead of N of each. The
	// path runs only for a spec group the session left unresolved, and only
	// over candidates whose report producer the session did not plan: a narrowed
	// run, or one whose tests failed or were canceled.
	//
	// Without a verification command no candidate has a task that can write the
	// report, so each one is consulted and silent.
	commands := scheduler.CommandsOfKind(source.extensions, extension.CommandKindVerification)
	if len(commands) == 0 {
		return recovery
	}
	// A candidate the run's own plan answers has no task that can write its
	// report: it is consulted and silent. A candidate an earlier call of this
	// gate answered keeps that answer, since it depends on the candidate alone.
	// Only the others are planned, once per gate however many spec owners
	// name them. A failure reports every candidate of the call.
	all := candidates
	source.answeredMu.Lock()
	unanswered := make([]*workspace.Project, 0, len(candidates))
	for _, candidate := range candidates {
		_, known := source.answered[candidate.ID]
		runAnswers := source.planned != nil && source.planned.selected[candidate.ID] && source.planned.producers[candidate.ID] == 0
		if !known && !runAnswers {
			unanswered = append(unanswered, candidate)
		}
	}
	source.answeredMu.Unlock()
	var outcomes map[string]*candidateOutcome
	if len(unanswered) > 0 {
		var disabledJobs, disabledExtensions []string
		if source.req.Config != nil && source.req.Config.Disable != nil {
			disabledJobs, disabledExtensions = source.req.Config.Disable.Jobs, source.req.Config.Disable.Extensions
		}
		source.plans.Add(1)
		planned, err := planner.Plan(source.ws, commands, unanswered, source.extensions,
			source.req.CommandParams, disabledJobs, disabledExtensions, nil)
		if err != nil {
			return source.unconsultedAll(all, unconsultedCause{
				fmt.Sprintf("their test plan could not be derived: %v", err), features.UnconsultedPlanFailed})
		}
		keys, err := cachekey.PrecomputeKeys(source.ws, planned, source.req.CommandParams,
			source.runVersions(), source.cache, cachekey.CacheBypass{}, cachekey.AtEntryKeys(scheduler.SourceBoundEntryKeys(source.ws)))
		if err != nil {
			return source.unconsultedAll(all, unconsultedCause{
				fmt.Sprintf("their cache keys could not be derived: %v", err), features.UnconsultedPlanFailed})
		}

		outcomes = make(map[string]*candidateOutcome, len(unanswered))
		for _, candidate := range unanswered {
			outcomes[candidate.ID] = &candidateOutcome{}
		}
		for _, job := range planned {
			// Only a task that can write the report can answer for it. The
			// test-environment steps are cache:false by contract and a generation
			// entry can be evicted on its own; counting either read a candidate whose
			// report entry was recorded EMPTY as unreachable, which excused its gap.
			if job == nil || job.Project == nil || !scheduler.ProducesVerificationReport(job) {
				continue
			}
			outcome := outcomes[job.Project.ID]
			if outcome == nil {
				continue // a prerequisite of a candidate, not a candidate itself
			}
			outcome.reportTasks++
			hash := keys[job.Key()]
			if hash == "" {
				continue // the task is not cacheable, so it can never be recovered
			}
			outcome.keyed++
			entry, failure := source.lookupTaskEntry(hash, job)
			if entry == nil {
				if outcome.remoteFailure.code == "" {
					outcome.remoteFailure = failure
				}
				continue
			}
			outcome.entries++
			if report := reportInEntry(source.ws.Root, job, entry); report != nil && outcome.report == nil {
				outcome.report = report
				source.entriesMu.Lock()
				if source.entries == nil {
					source.entries = make(map[string]*store.TaskEntry)
				}
				source.entries[report.Path] = entry
				source.entriesMu.Unlock()
			}
		}
	}
	source.answeredMu.Lock()
	defer source.answeredMu.Unlock()
	for _, candidate := range all {
		outcome, planned := outcomes[candidate.ID]
		if planned && source.answered != nil {
			source.answered[candidate.ID] = outcome
		} else if !planned {
			outcome = source.answered[candidate.ID]
		}
		if outcome == nil {
			continue // answered by the run's plan: consulted and silent
		}
		if outcome.report != nil {
			recovery.Reports = append(recovery.Reports, *outcome.report)
			continue
		}
		if cause, unconsulted := outcome.unconsultedReason(); unconsulted {
			recovery.Unconsulted = append(recovery.Unconsulted, specgate.UnconsultedProject{Project: candidate.ID, Reason: cause.prose, Code: cause.code})
		}
	}
	return recovery
}

// candidateOutcome is what the lookup learned about one candidate project's
// report tasks: the tasks that can write the verification report.
type candidateOutcome struct {
	report      *specgate.RecoveredReport
	reportTasks int
	keyed       int
	entries     int
	// remoteFailure says why the run's remote cache could not answer for a
	// keyed report task that has no entry, completing "not selected, and
	// ...", or is the zero cause when it answered or was not asked.
	remoteFailure unconsultedCause
}

// unconsultedReason decides whether a candidate that produced no report was
// CONSULTED-and-silent or never reachable at all. The distinction is the whole
// of Part B: only the second may excuse a still-missing check.
//
// Silence is an answer in two shapes, and both count as consulted:
//
//   - the project has no task that can write the verification report, so it
//     can never attest anything;
//   - its report task's entry exists and records the verification report
//     EMPTY, which is the explicit statement "this task ran for these inputs
//     and observed nothing". Reading that as "unreachable" would excuse every
//     genuine gap in a project that simply has no bound test yet, which is
//     exactly the regression the gate exists to catch.
//
// A report task whose remote lookup failed or ran out of time is unconsulted
// too, and its reason says so rather than claim the entry does not exist.
func (outcome *candidateOutcome) unconsultedReason() (unconsultedCause, bool) {
	switch {
	case outcome.reportTasks == 0:
		return unconsultedCause{}, false
	case outcome.keyed < outcome.reportTasks:
		return unconsultedCause{"not selected, and the test task that would write its verification report is not cacheable, " +
			"so no observation can be recovered", features.UnconsultedNotCacheable}, true
	case outcome.entries < outcome.keyed && outcome.remoteFailure.code != "":
		return unconsultedCause{"not selected, and " + outcome.remoteFailure.prose, outcome.remoteFailure.code}, true
	case outcome.entries < outcome.keyed:
		return unconsultedCause{"not selected, and the test task that writes its verification report has no cache entry for these inputs",
			features.UnconsultedNoEntry}, true
	default:
		return unconsultedCause{}, false
	}
}

// lookupTaskEntry returns the entry at hash of a candidate's report task: the
// local store's, else the one the run's remote cache publishes into the local
// store. failure says why the remote cache could not answer, and is the zero
// cause for an entry, a miss, and a run that reads no remote cache.
func (source *cachedObservationRecovery) lookupTaskEntry(hash string, job *scheduler.ScheduledJob) (*store.TaskEntry, unconsultedCause) {
	if entry, err := source.cache.LookupTaskEntry(hash); err == nil && entry != nil {
		return entry, unconsultedCause{}
	}
	if source.remote == nil {
		return nil, unconsultedCause{}
	}
	remote := source.remote()
	if remote == nil {
		return nil, unconsultedCause{}
	}
	source.remoteMu.Lock()
	failure, asked := source.remoteMisses[hash]
	if source.remoteEnd.IsZero() {
		source.remoteEnd = time.Now().Add(source.remoteBudget)
	}
	within := min(source.remoteLookup, time.Until(source.remoteEnd))
	source.remoteMu.Unlock()
	if asked {
		return nil, failure
	}

	var entry *store.TaskEntry
	var err error
	switch {
	case source.ctx.Err() != nil:
		err = source.ctx.Err()
	case within <= 0:
		err = context.DeadlineExceeded
	default:
		entry, err = remote.FetchTaskEntry(source.ctx, within, hash, job, source.cache)
	}
	if entry != nil {
		return entry, unconsultedCause{}
	}
	failure = remoteLookupFailure(err)
	source.remoteMu.Lock()
	if source.remoteMisses == nil {
		source.remoteMisses = make(map[string]unconsultedCause)
	}
	source.remoteMisses[hash] = failure
	source.remoteMu.Unlock()
	return nil, failure
}

// remoteLookupFailure states why a remote lookup returned no entry, its prose
// completing "not selected, and ...", or the zero cause for a miss.
func remoteLookupFailure(err error) unconsultedCause {
	switch {
	case err == nil:
		return unconsultedCause{}
	case errors.Is(err, context.DeadlineExceeded):
		return unconsultedCause{"the remote cache did not answer in time for the test task that writes its verification report",
			features.UnconsultedRemoteTimeout}
	default:
		return unconsultedCause{"the remote cache lookup for the test task that writes its verification report failed: " + err.Error(),
			features.UnconsultedRemoteUnreachable}
	}
}

func (source *cachedObservationRecovery) unconsultedAll(candidates []*workspace.Project, cause unconsultedCause) specgate.Recovery {
	recovery := specgate.Recovery{}
	for _, candidate := range candidates {
		recovery.Unconsulted = append(recovery.Unconsulted, specgate.UnconsultedProject{Project: candidate.ID, Reason: cause.prose, Code: cause.code})
	}
	return recovery
}

// reportInEntry resolves one cached report task's entry to the captured
// verification report inside it, or nil when the entry carries none.
//
// The DESCRIPTOR decides, exactly as restoreReservedDeclaredArtifactEvents does
// on a warm hit: the output must be a PRESENT command-output file at the
// protocol's reserved filename. An output recorded empty means the task ran and
// observed nothing — a fact, not a missing file — and must never be answered
// with whatever happens to sit at that address.
func reportInEntry(wsRoot string, job *scheduler.ScheduledJob, entry *store.TaskEntry) *specgate.RecoveredReport {
	if entry.FilesDir == "" {
		return nil
	}
	for _, output := range entry.Outputs {
		if !output.Present() || output.Kind != extensionproto.OutputKindFile ||
			output.Root != extensionproto.OutputRootCommandOutput ||
			output.Path != features.VerificationReportFilename {
			continue
		}
		path := filepath.Join(entry.FilesDir, output.ID)
		return &specgate.RecoveredReport{
			Project: job.Project.ID,
			Task:    job.Key(),
			Path:    path,
			Field:   relativeToWorkspace(wsRoot, path),
		}
	}
	return nil
}

// runVersions shares the versions already resolved by planning and execution.
// A recovery source assembled outside the engine resolves lazily on first use.
func (source *cachedObservationRecovery) runVersions() cachekey.RunVersions {
	source.once.Do(func() { source.versions = source.req.runVersions(source.ws) })
	return source.versions
}

// relativeToWorkspace renders a cache-store path the way the record's report
// references render a session artifact. A store outside the workspace stays
// absolute rather than becoming a ".."-walk nobody can act on.
func relativeToWorkspace(wsRoot, path string) string {
	relative, err := filepath.Rel(wsRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}
