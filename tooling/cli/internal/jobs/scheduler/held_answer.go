package scheduler

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"
)

// An unchanged run that a machine worker serves is answered from memory (CLI
// ADR 0116 §6a). The worker decides whether a request is answerable, from its
// journal and the witnesses the journal does not see (internal/machineproc);
// this file is the scheduler's half. A plan whose every task was served from
// the local cache or skipped leaves the worker a record of its result, and a
// later run the worker decided answerable is answered from that record when it
// plans the same jobs: no prune, no key, no lookup, no restore, no worker and
// no task stats, only what a local-cache hit emits to the renderer, task by
// task. A process that is no worker holds nothing, and every plan runs.

// HeldRuns is the memory of the machine worker this process is. The worker
// installs it with ServeHeldRuns: the scheduler does not import the machine
// process, which implements this interface without naming it.
type HeldRuns interface {
	// Take returns the record the worker holds for the next plan the current
	// run executes in the workspace at root, or nil: for a run the worker did
	// not decide answerable, past the plans the held run executed, and once a
	// plan of the run was computed rather than answered.
	Take(root string) any
	// Keep hands the worker what the plan Take was called for leaves to hold,
	// nil when it leaves nothing a later run may be answered with, and
	// whether it was answered from the record Take returned.
	Keep(root string, record any, answered bool)
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

// servedHeldRuns is the memory ServeHeldRuns installed, or nil.
func servedHeldRuns() HeldRuns {
	heldRuns.Lock()
	defer heldRuns.Unlock()
	return heldRuns.memory
}

// runOrAnswer answers req from the record memory offers for it when that
// record holds the same plan, runs the plan otherwise, and hands memory what
// the plan leaves to hold. A nil memory, outside a worker, runs the plan.
func runOrAnswer(ctx context.Context, req RunRequest, memory HeldRuns, run func() *SchedulerResult) *SchedulerResult {
	if memory == nil || req.Workspace == nil {
		return run()
	}
	root := req.Workspace.Root
	if result, record := answerHeld(ctx, req, memory.Take(root)); result != nil {
		memory.Keep(root, record, true)
		return result
	}
	result := run()
	memory.Keep(root, holdRecord(req, result), false)
	return result
}

// heldPlan is what a plan leaves for the worker to hold: the jobs it planned,
// in plan order, and its result. Results and Session are copies, so what the
// engine does with the result it was handed never reaches a later answer.
type heldPlan struct {
	keys   []string
	deps   [][]string
	result SchedulerResult
}

// holdRecord is the record of a plan a later run may be answered with, or nil.
// Only a plan that executed nothing leaves one: every task served from the
// local cache or skipped, the run successful and not cut short. A plan the
// held path cannot replay as it ran leaves none either: a framework-owned
// job, a serve step, or a run that refuses the cache or verifies it.
//
// It returns an untyped nil, never a nil *heldPlan, so the worker can tell a
// plan that left nothing from one that left a record.
func holdRecord(req RunRequest, result *SchedulerResult) any {
	if !holdable(req) || result == nil || !result.Success || result.Outcome.Aborted ||
		result.Session == nil || result.Tuning == nil || len(result.Results) != len(req.Plan) {
		return nil
	}
	held := &heldPlan{
		keys:   make([]string, len(req.Plan)),
		deps:   make([][]string, len(req.Plan)),
		result: *result,
	}
	held.result.Results = make(map[string]*JobResult, len(result.Results))
	for i, job := range req.Plan {
		if job == nil {
			return nil
		}
		key := job.Key()
		r := result.Results[key]
		if r == nil {
			return nil
		}
		// A task that executed is answered by executing it: only a local hit,
		// or a task that never ran, replays as it ran.
		served := r.Reuse == ReuseLocalCache || (r.Reuse == ReuseNone && r.Status == string(TaskStatusSkipped))
		if !served {
			return nil
		}
		copied := *r
		held.result.Results[key] = &copied
		held.keys[i], held.deps[i] = key, slices.Clone(job.DependsOn)
	}
	session := *result.Session
	held.result.Session = &session
	return held
}

// holdable says whether a run of req may hold or be answered at all.
func holdable(req RunRequest) bool {
	cfg := req.Config
	return req.Workspace != nil && req.Cache != nil && !cfg.NoCache && !cfg.CacheVerification &&
		len(cfg.ServeSteps) == 0 && len(req.InternalJobs) == 0
}

// matches says whether planned is the plan the record holds: the same jobs,
// in the same order, with the same dependencies.
func (h *heldPlan) matches(planned []*ScheduledJob) bool {
	if len(planned) != len(h.keys) {
		return false
	}
	for i, job := range planned {
		if job == nil || job.Key() != h.keys[i] || !slices.Equal(job.DependsOn, h.deps[i]) {
			return false
		}
	}
	return true
}

// answerHeld answers req from offered, the record the worker holds for this
// plan, and returns the result with the record to keep holding; it returns a
// nil result, and the plan runs, when offered is no record, the run may not be
// answered, or the plan differs from the held one.
//
// The renderer and the session recorder see what a local-cache hit makes them
// see, task by task in plan order: the job's start, the held result's events
// replayed as a restore replays them, its completion and its job:end record,
// between the run's opening and its Finish. The session is therefore recorded
// by the same machinery as a run that looked every task up. The result is the
// held one, with the current times: this run's wall, its orchestration
// report and its preparation (CLI ADR 0116 §6a, "the held result with the
// current times").
func answerHeld(ctx context.Context, req RunRequest, offered any) (*SchedulerResult, any) {
	held, ok := offered.(*heldPlan)
	if !ok || held == nil || ctx.Err() != nil || !holdable(req) || !held.matches(req.Plan) {
		return nil, nil
	}
	start := time.Now()
	s := &Scheduler{
		planned:              req.Plan,
		cfg:                  req.Config,
		renderer:             req.Renderer,
		onSessionEvent:       req.SessionEvents,
		openingSessionEvents: req.OpeningSessionEvents,
	}
	decision := held.result.Tuning.Parallel
	if reporter, ok := s.renderer.(PublishConcurrencyReporter); ok {
		configuredCap := decision.Workers
		if s.cfg.MaxParallel > 0 {
			configuredCap = s.cfg.MaxParallel
		}
		reporter.SetPublishConcurrency(configuredCap)
	}
	s.resetSessionJobEnds()
	s.renderer.Start(s.planned)
	s.emitSchedulerSummary(decision)
	results := make(map[string]*JobResult, len(held.result.Results))
	for _, job := range s.planned {
		answer := *held.result.Results[job.Key()]
		result := &answer
		results[job.Key()] = result
		s.renderer.JobStart(job)
		// A task that never ran said nothing; a restored one replays what its
		// entry captured, as replayCacheEvents does.
		if result.Reuse != ReuseNone {
			s.replayResultEvents(job, result)
		}
		s.renderer.JobComplete(job, result)
		s.emitSessionJobEnd(job, result)
	}
	answered := maps.Clone(results)
	if s.cfg.FinalizeResults != nil {
		s.cfg.FinalizeResults(results)
	}
	outcome := held.result.Outcome
	outcome.Duration = time.Since(start)
	outcome.Orchestration = s.cfg.Orchestration.Snapshot()
	cut := resolveSessionOutcome(ctx)
	outcome.Aborted, outcome.AbortedBy = cut.Aborted, cut.AbortedBy
	s.renderer.Finish(results, outcome)

	session := *held.result.Session
	record := any(held)
	// A finalizer that amended a row, or a signal that cut the answer short,
	// makes this run's verdict differ from the held one: it is reduced from
	// what it holds now, as a run that computed would be, and holds nothing.
	if cut.Aborted || !maps.Equal(answered, results) {
		reduced := reduceSchedulerSession(s.planned, results, outcome, outcome.Duration, held.result.Cache)
		reduced.Executions, reduced.Environment = session.Executions, session.Environment
		session = *reduced
		record = nil
	}
	session.Duration = outcome.Duration
	session.Orchestration = outcome.Orchestration
	session.Preparation = s.cfg.Preparation.Snapshot()
	return &SchedulerResult{
		Results:  results,
		Success:  session.Success(),
		Outcome:  outcome,
		Duration: outcome.Duration,
		Tuning:   held.result.Tuning,
		Cache:    held.result.Cache,
		Session:  &session,
	}, record
}
