package engine

import (
	"bytes"
	"context"
	"io"
	"os"
	"reflect"
	"slices"
	"sync"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	doctor "go.putnami.dev/protocol/doctor"
	features "go.putnami.dev/protocol/features"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/inputresolve"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// An unchanged run that a machine worker serves is answered from memory (CLI
// ADR 0116 §6a). The worker decides whether a request is answerable, from its
// journal and the witnesses the journal does not see (internal/machineproc),
// and the scheduler answers a plan from the record the held run's plan left
// (scheduler.HeldRuns). This file is the engine's half. A run that may be
// held keeps what its phases 1 to 3 computed: the loaded workspace, the
// discovered extensions, the selection, the plan, and the flags and
// parameters they resolved. A later run of the same request, decided
// answerable, takes them instead of computing them: it opens no file index,
// input record or release record, and runs no workspace load, runtime
// synchronization, probe, selection or plan. Phase 4 runs as in any run, on
// the held values: the session is recorded by the usual machinery, and the
// scheduler answers the plan from its own record. A process that is no worker
// holds nothing, and every run computes.

// HeldRuns is the memory of the machine worker this process is, as the engine
// reads it. The worker installs it with ServeHeldRuns: the engine does not
// import the machine process, which implements this interface without naming
// it.
type HeldRuns interface {
	// TakeRun returns what the held run's engine run at the same position
	// computed, for the next engine run in the workspace at root, or nil: for
	// a run the worker did not decide answerable, once anything the decision
	// witnessed moved, and once anything of the run was computed.
	TakeRun(root string) any
	// KeepRun hands the worker what the engine run TakeRun was called for
	// computed, nil when it leaves nothing a later run may be answered with,
	// and whether it was answered from what TakeRun returned.
	KeepRun(root string, record any, skipped bool)
	// PeekRun returns what TakeRun would return now, under the same check of
	// what the decision witnessed, without taking the engine run's position.
	PeekRun(root string) any
	// Reused tells the worker whether the engine run in root took step from
	// what the held run computed (held) or computed it: "versions", the
	// version snapshot, and "finalizers", what the finalizers did.
	Reused(root, step string, held bool)
}

// versionSnapshot is the version snapshot Engine.Run would read from Git now.
// In a machine worker, an engine run of a request the worker decided
// answerable takes the one the held run's engine run at the same position
// read, when that run read it itself: HEAD, the refs, the index's entries,
// Git's configuration and the worktree it reads are what the decision
// witnessed, and the worker checks them again before it offers it (CLI ADR
// 0116 §6a). Anything missing, and every run outside a worker, reads Git.
func versionSnapshot(req *Request) (*git.VersionInfo, error) {
	heldRuns.Lock()
	memory := heldRuns.memory
	heldRuns.Unlock()
	if memory == nil || req.WorkspaceRoot == "" || !req.mayHold() {
		return captureVersionSnapshot(req.WorkspaceRoot)
	}
	if held, ok := memory.PeekRun(req.WorkspaceRoot).(*heldRun); ok && held.complete && held.captured &&
		held.snapshot != nil && held.root == req.WorkspaceRoot {
		memory.Reused(req.WorkspaceRoot, "versions", true)
		snapshot := *held.snapshot
		return &snapshot, nil
	}
	memory.Reused(req.WorkspaceRoot, "versions", false)
	return captureVersionSnapshot(req.WorkspaceRoot)
}

// heldRuns is the memory ServeHeldRuns installed, nil outside a worker.
var heldRuns struct {
	sync.Mutex
	memory HeldRuns
}

// ServeHeldRuns installs the memory of the machine worker this process is.
// Only a worker calls it, before it serves its first run: a run in its own
// process, in CI or with the machine process off is never answered from
// memory (CLI ADR 0116 §6a).
func ServeHeldRuns(memory HeldRuns) {
	heldRuns.Lock()
	heldRuns.memory = memory
	heldRuns.Unlock()
}

// heldRun is what an engine run leaves a worker to hold.
//
// Its first half is the request as the adapter built it, before the run
// resolved anything, and the version snapshot the run took before its hooks:
// a later run is answered from the second half only when it asks the same.
// The worker's decision witnessed everything else phases 1 to 3 read: the
// request's environment, the worktree, Git and the Putnami home (holdPlanned
// drops a run that reads anything more).
//
// The second half is what phases 1 to 3 computed, and phase 4 reads. The
// worker holds it only from a run whose plan the scheduler answered from a
// record, so no task execution ever touched these values; phase 4 reads
// them, and a run in a worker is the only one that uses them.
type heldRun struct {
	root         string
	config       *wsproto.Config
	commands     []string
	params       extension.ParamMap
	markerParams extension.ParamMap
	flags        GlobalFlags
	hooks        *wsproto.HooksConfig
	detached     bool
	ephemeral    bool
	snapshot     *git.VersionInfo
	// captured says Engine.Run read the snapshot itself rather than take the
	// adapter's: only such a snapshot is reused in place of reading Git.
	captured bool

	run             *plannedRun
	resolved        GlobalFlags
	renamed         extension.ParamMap
	selection       *protocoljob.Selection
	evidence        selectionEvidence
	noCacheProjects map[string]bool
	versions        cachekey.RunVersions
	versionsErr     error
	// finalizers is what phase 4's finalizers did, which an answer replays,
	// or nil when it cannot (holdFinalizers).
	finalizers *heldFinalizers
	// stdout and stderr are what phases 1 to 3 printed that the answer prints
	// again: the engine's human notices, the probe's diagnostics and the
	// impacted selection's fallback explanation.
	stdout, stderr bytes.Buffer
	// complete says phase 4 took what phases 1 to 3 computed (holdPlanned).
	complete bool
}

// run is the run between the before and after hooks. In a machine worker, an
// engine run that the worker offers what the held run's engine run at the
// same position computed, and that asks what that run asked, is answered from
// it (answerHeld); any other run runs its lifecycle, and leaves the worker
// what it computed when a later run may be answered with it. Outside a
// worker, it is the lifecycle.
func (e *Engine) run(ctx context.Context, req *Request, sink EventSink) (SessionResult, error) {
	heldRuns.Lock()
	memory := heldRuns.memory
	heldRuns.Unlock()
	if memory == nil || req.WorkspaceRoot == "" {
		return e.lifecycle(ctx, req, sink)
	}
	root := req.WorkspaceRoot
	var asked *heldRun
	if req.mayHold() {
		asked = &heldRun{
			root: root, config: req.Config, commands: req.Commands, params: req.CommandParams,
			markerParams: req.RunMarkerParams, flags: req.Global, hooks: req.Hooks,
			detached: req.DetachedReporting, ephemeral: req.EphemeralSession, snapshot: req.VersionSnapshot,
			captured: req.snapshotCaptured,
		}
	}
	// Every engine run takes and keeps, so the worker pairs the held run's
	// engine runs with this one's by position.
	if held, ok := memory.TakeRun(root).(*heldRun); ok && asked != nil && held.complete && held.asks(asked) {
		result, err := e.answerHeld(ctx, req, held, sink)
		var record any
		if err == nil && result.ExitCode == ExitSuccess {
			record = held
		}
		memory.Reused(root, "finalizers", req.finalizersReplayed)
		memory.KeepRun(root, record, true)
		return result, err
	}
	req.hold = asked
	result, err := e.lifecycle(ctx, req, sink)
	var record any
	if req.hold != nil && req.hold.complete && err == nil && result.ExitCode == ExitSuccess {
		record = req.hold
	}
	req.hold = nil
	if asked != nil {
		memory.Reused(root, "finalizers", false)
	}
	memory.KeepRun(root, record, false)
	return result, err
}

// heldFinalizers is what the finalizers of a held run did: in a run that may
// be held, the spec gate is the only finalizer (holdPlanned drops a run with
// a publication guard, a release set or a publish), and gate says it was
// attached, with the record it collected (nil when it had nothing to
// evaluate). Its warnings and sanctions are functions of that record and the
// results, which an answer holds equal.
type heldFinalizers struct {
	gate   bool
	record *features.SpecVerificationRecord
}

// holdFinalizers is what a run that may be held holds of what its finalizers
// did, or nil when an answer could not replay it: a finalizer other than the
// spec gate, a gate whose collection failed or never ran, one whose verdicts
// read the clock (a rolling window goes stale with time), and one whose
// recovery read a cache entry, which the store or the remote cache answered
// and no witness of the decision covers. Everything else the gate read is
// the held run's workspace, plan and results (CLI ADR 0116 §6a).
func holdFinalizers(onlyGate, gate bool, outcome *specGateOutcome, recovery *cachedObservationRecovery) *heldFinalizers {
	switch {
	case !onlyGate:
		return nil
	case !gate:
		return &heldFinalizers{}
	case outcome == nil || !outcome.collected || outcome.clock || (recovery != nil && recovery.plans.Load() > 0):
		return nil
	}
	return &heldFinalizers{gate: true, record: outcome.record}
}

// replay is what an answered run's finalizers do when the scheduler answers
// its plan from the held record: the spec gate's held record, with this
// run's time, its warnings and sanctions applied to the answered results as
// the gate applied them, and the record written beside the session as any
// gate's is.
func (h *heldFinalizers) replay(req *Request) func(map[string]*scheduler.JobResult) {
	return func(results map[string]*scheduler.JobResult) {
		req.finalizersReplayed = true
		if !h.gate {
			return
		}
		var record *features.SpecVerificationRecord
		if h.record != nil {
			copied := *h.record
			copied.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
			record = &copied
		}
		req.specGate.record, req.specGate.collected = record, true
		sanctionSpecGate(record, results)
	}
}

// mayHold says whether a run of req's shape may be held at all: one that
// executes its plan, once, in this process, with the cache on and no debug
// output that an answer would not print again.
func (req *Request) mayHold() bool {
	g := req.Global
	return !req.WorkspaceLifecycle && req.CacheVerification == nil && req.Portable == nil &&
		req.PlanExtension == nil && req.PlanExtensions == nil && !req.watchIteration &&
		len(req.ServeProcessEnv) == 0 && !req.ExecutesUnderDryRun && !runcredential.Hosted() &&
		(g.Where == "" || g.Where == "local") && !g.Plan && !g.DryRun && !g.Watch && !g.Debug && !g.NoCache && g.ServeEnv == ""
}

// asks says whether asked, the request of a later run, asks what h was asked:
// the same workspace, configuration, commands, parameters, flags and hooks,
// with the same version snapshot, read the same way.
func (h *heldRun) asks(asked *heldRun) bool {
	return h.root == asked.root && slices.Equal(h.commands, asked.commands) &&
		h.detached == asked.detached && h.ephemeral == asked.ephemeral && h.captured == asked.captured &&
		reflect.DeepEqual(h.flags, asked.flags) && reflect.DeepEqual(h.params, asked.params) &&
		reflect.DeepEqual(h.markerParams, asked.markerParams) && reflect.DeepEqual(h.hooks, asked.hooks) &&
		reflect.DeepEqual(h.config, asked.config) && reflect.DeepEqual(h.snapshot, asked.snapshot)
}

// holdPlanned completes the run's hold with what phases 1 to 3 computed, or
// drops it. It drops a run whose phases read what the worker's decision does
// not witness: a runner provider's placement, a release set's channels, the
// successful-run markers a bare selection reads, a production preflight. It
// drops one whose phase 4 an answer would not replay as it ran: a
// framework-owned job, a capability authorization, the publication guard's
// failure, a workload that streams or watches. And it drops one that prints
// debug output or refuses the cache.
func (req *Request) holdPlanned(run *plannedRun) {
	h := req.hold
	if h == nil {
		return
	}
	serve, once := workloadModes(run.discovered.Extensions, req.Commands)
	options := run.options
	if req.runnerProvider != nil || run.releaseSet != nil || options.Publishes || options.PlannedPublish ||
		options.ConsumesReleaseSet || len(run.unpublished) > 0 || len(run.unannounced) > 0 ||
		len(req.internalJobs) > 0 || req.processCapabilityAuthorization != nil || req.confirmsReleaseSetHead ||
		req.Global.AutoSelected || req.Global.Debug || req.Global.NoCache || serve || once ||
		doctor.Profile(req.Global.EnvProfile) == doctor.ProfileProduction {
		req.hold = nil
		return
	}
	h.run, h.resolved, h.renamed = run, req.Global, req.CommandParams
	h.selection, h.evidence, h.noCacheProjects = req.selection, req.selectionEvidence, req.noCacheProjects
	h.versions, h.versionsErr = req.versions, req.versionsErr
	h.complete = true
}

// notices returns w, which also records what is written to it into the hold
// while phases 1 to 3 run: into its stderr when stderr is set, its stdout
// otherwise. A nil or complete hold records nothing.
func (h *heldRun) notices(w io.Writer, stderr bool) io.Writer {
	if h == nil || h.complete {
		return w
	}
	if stderr {
		return io.MultiWriter(w, &h.stderr)
	}
	return io.MultiWriter(w, &h.stdout)
}

// answerHeld runs req from what held computed. It opens no file index, input
// record or release record, which an answer reads nothing through and so
// would not save, and runs no workspace load, runtime synchronization, probe,
// selection or plan. It prints what a run of its own prints before phase 4
// (the workspace's warnings, the command-scoped flag validation, the held
// notices), sets on req what phases 1 to 3 resolved, and runs phase 4 on the
// held values (CLI ADR 0116 §6a).
func (e *Engine) answerHeld(ctx context.Context, req *Request, held *heldRun, sink EventSink) (SessionResult, error) {
	run := held.run
	ApplyEnvOverrides(&req.Global, req.Config)
	reportWorkspaceWarnings(req, run.ws)
	defer req.workspaceWarnings.Flush()
	if req.ValidateCommandFlags != nil {
		if err := req.ValidateCommandFlags(run.discovered, req.Global); err != nil {
			return SessionResult{ExitCode: protocolcli.ExitCodeForError(err)}, err
		}
	}
	req.Global, req.CommandParams = held.resolved, held.renamed
	req.selection, req.selectionEvidence, req.noCacheProjects = held.selection, held.evidence, held.noCacheProjects
	req.versions, req.versionsErr = held.versions, held.versionsErr
	req.heldFinalizers = held.finalizers
	// The answer prepares nothing before phase 4, and what phase 4 prepares
	// it reports. Its input selectors resolve under this run's context.
	req.preparation, req.synchronizedExtensions = &scheduler.PreparationReport{}, run.extensions
	req.inputResolver = inputresolve.New(ctx, run.ws, append(slices.Clone(run.extensions), run.discovered.Extensions...), req.preparation)
	iox.Fprint(os.Stderr, held.stderr.String())
	iox.Fprint(req.stdout(), held.stdout.String())
	return e.executePlanned(ctx, req, run, newRunCacheManager(req, run.ws), sink), nil
}
