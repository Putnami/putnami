package jobs

import (
	"time"

	runtimeproto "go.putnami.dev/protocol/runtime"
)

// JobResult holds the outcome of a job execution.
type JobResult struct {
	Status   string         `json:"status"` // "success", "failed", "canceled", "skipped"
	Data     map[string]any `json:"data,omitempty"`
	Error    *JobError      `json:"error,omitempty"`
	Events   []RawJobEvent  `json:"-"`
	Duration time.Duration  `json:"-"`
	// EmittedMeta preserves the runtime handshake independently of bounded
	// event retention. It is execution-control evidence (a job binary reached
	// its own logic), not user-facing detail, and must not change when an early
	// event flood exhausts the retained slice.
	EmittedMeta bool `json:"-"`
	// SourceMutated records that a successful, status-only source-writing task
	// changed one of its keyed source inputs. It is persisted in a cache-private
	// envelope so a pre-fix hit is executed again rather than leaving another
	// worktree unfixed.
	SourceMutated bool `json:"-"`
	// SpawnToFirstEvent is the wall latency from immediately before cmd.Start
	// until the first valid runtime JSONL event is received. FirstEventObserved
	// distinguishes an event arriving within the clock's zero bucket from a job
	// that emitted no valid events (including cache hits, which never spawn).
	SpawnToFirstEvent  time.Duration `json:"-"`
	FirstEventObserved bool          `json:"-"`
	// TaskWall is the scheduler-level wall time around executeJob. Unlike
	// Duration (the final subprocess attempt), it includes lookup/reuse,
	// hooks, every retry attempt, and result publication.
	TaskWall time.Duration `json:"-"`
	// CPUTime is the subprocess tree's consumed CPU time (user+system,
	// including waited children). Zero when unavailable. Feeds the task stats
	// history that drives learned CPU weights. For a batch member it is this
	// task's SHARE of Execution's measured total, not an independent
	// measurement — the physical number lives on Execution.
	CPUTime time.Duration `json:"-"`
	// Execution is the PHYSICAL subprocess this result came out of, shared by
	// every member of a batch and by every member of a plan-level shared node,
	// and nil for a result no subprocess in this run
	// produced (a cache hit, a lease-coalesced restore, a skipped or
	// failed-before-spawn task). A retried task carries its final attempt's, the
	// same rule Duration already follows. The ledger keys on its id, so a shared
	// subprocess is counted once however many logical rows name it.
	// See execution.go.
	Execution *Execution `json:"-"`
	ExitCode  int        `json:"-"`
	CacheHit  bool       `json:"-"` // true for an ordinary local or remote cache hit
	Coalesced bool       `json:"-"` // true when a sibling in this run did the work instead
	// CacheKey is the exact, already-computed key this run used for the task.
	// Cache entries and runtime events never carry it. The one place it leaves
	// the process is the canonical task record, as TaskResult.InputDigest, so a
	// session reader can tell why a task executed or reused a result.
	CacheKey string `json:"-"`
	// VerificationWrites is the workspace-relative file set whose bytes, mode,
	// or presence changed while this task's subprocess ran. It is populated only
	// by cache-verification runs, which execute singleton tasks serially so
	// another task cannot be mistaken for this writer.
	VerificationWrites []string `json:"-"`
	// VerificationError records an observation failure without changing the
	// task's own verdict. The cache verifier fails closed on this field.
	VerificationError string `json:"-"`
	// Reuse is the canonical provenance of a reused result, refining the two
	// booleans above into one mutually-exclusive value that also separates a
	// local hit from a remote one. MarkReuse is its only writer and sets all
	// three together, so the encodings cannot drift. It stays optional: a
	// JobResult assembled by hand (tests, older call sites) leaves it empty and
	// ReuseKind falls back to the booleans.
	Reuse ReuseKind `json:"-"`
	// TimedOut records that this result is a failure the task's own deadline
	// produced, not one its inputs produced. It is set at every site that turns
	// a context.DeadlineExceeded into a failed result, so a consumer never has
	// to string-match the error message. A timeout depends on host load rather
	// than on the cache key, which is why the failure cache refuses to record
	// one (internal/jobs/task_failure.go).
	TimedOut bool `json:"-"`
	// ReplayedFailure is set when this failed result was served from the local
	// failure cache instead of executed, because the task's cache key — and
	// therefore every declared input — is unchanged since it last failed.
	//
	// It is deliberately NOT MarkReuse: reuse turns Outcome() into "cached" and
	// folds the task into the cache-hit summary, while a replayed failure is a
	// failure in every respect — same Status, same DAG effect on dependents,
	// same non-zero exit code. Only the renderers read this field, to say where
	// the verdict came from.
	ReplayedFailure *ReplayedFailure `json:"-"`
	// Canonical carries the typed TaskResult when a producer had richer
	// structure than the event stream can express — today only a batch member,
	// whose records come from the typed batch wire and whose event stream is
	// PROJECTED from those records for the renderers and the cache entry
	// (scheduler_batch_exec.go). When set, the canonical model reads it and the
	// projection is never parsed back.
	Canonical *TaskResult `json:"-"`
}

// ReplayedFailure is the provenance of a failure served from the local failure
// cache: when the task first failed at this cache key, and how many times that
// same failure has been observed there (executed or replayed).
type ReplayedFailure struct {
	// FirstFailedAt is when the task first failed at this cache key. It is
	// preserved across every later attempt, so the age it yields is the age of
	// the failure, not of the last observation.
	FirstFailedAt time.Time
	// Attempts counts the observations of this failure at this key, including
	// the one being reported.
	Attempts int
}

const (
	JobOutcomeCached    = "cached"
	JobOutcomeCoalesced = "coalesced"
)

// Outcome returns the user-visible result classification. Status remains the
// subprocess outcome used by DAG scheduling, while cache reuse is reported as
// a mutually-exclusive first-class outcome for observability.
func (r *JobResult) Outcome() string {
	if r == nil {
		return ""
	}
	if r.Coalesced {
		return JobOutcomeCoalesced
	}
	if r.CacheHit {
		return JobOutcomeCached
	}
	return r.Status
}

// MarkReuse records that this result was reused rather than executed. It is the
// only writer of Reuse, CacheHit and Coalesced, so the canonical enum and the
// two legacy booleans always agree.
func (r *JobResult) MarkReuse(kind ReuseKind) {
	if r == nil {
		return
	}
	r.Reuse = kind
	r.CacheHit = kind.CacheHit()
	r.Coalesced = kind == ReuseCoalesced
}

// ReuseKind returns the canonical provenance of this result. When Reuse was set
// (every scheduler path) it is authoritative. Otherwise the booleans are read,
// and a bare CacheHit reports a local hit — that is all the boolean can say.
func (r *JobResult) ReuseKind() ReuseKind {
	if r == nil {
		return ReuseNone
	}
	if r.Reuse != ReuseNone {
		return r.Reuse
	}
	switch {
	case r.Coalesced:
		return ReuseCoalesced
	case r.CacheHit:
		return ReuseLocalCache
	default:
		return ReuseNone
	}
}

// JobError is the structured error from a failed job, as defined by the
// runtime event protocol.
type JobError = runtimeproto.JobError

// RawJobEvent is the CLI's rendering projection of a runtime protocol
// event (go.putnami.dev/protocol/runtime). Subprocess events carry
// type-specific fields flat at the top level; ParseRawEvent folds them
// into Data so renderers access every field uniformly. The wire format
// itself is owned by the protocol package.
type RawJobEvent struct {
	Version   int            `json:"v"`
	Type      string         `json:"type"`
	Time      string         `json:"time,omitempty"`
	Level     string         `json:"level,omitempty"`
	Message   string         `json:"message,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Timestamp int64          `json:"-"` // parsed from time field
}

// Event type constants, sourced from the runtime event protocol.
const (
	EventTypeLog        = string(runtimeproto.EventLog)
	EventTypeProgress   = string(runtimeproto.EventProgress)
	EventTypeArtifact   = string(runtimeproto.EventArtifact)
	EventTypeDiagnostic = string(runtimeproto.EventDiagnostic)
	EventTypeMetric     = string(runtimeproto.EventMetric)
	EventTypePhase      = string(runtimeproto.EventPhase)
	EventTypeSummary    = string(runtimeproto.EventSummary)
	EventTypeResult     = string(runtimeproto.EventResult)
	EventTypeMeta       = string(runtimeproto.EventMeta)
	// EventTypeReady is the typed readiness signal, admitted only by protocol
	// version 2. The CLI accepts and carries it from an earlier protocol bump on;
	// the watch loop arms on the typed ready event; the log probe is deleted.
	EventTypeReady = string(runtimeproto.EventReady)
)

// EventHandler receives parsed JSONL events in real-time as they arrive.
type EventHandler func(event RawJobEvent)

// SessionOutcome describes how the run ended, beyond the per-job results.
// Per-job status alone cannot distinguish "this work was never needed" from
// "this work was killed mid-flight", so the scheduler reports the session's
// own fate alongside them.
type SessionOutcome struct {
	// Aborted reports that a signal delivered to the CLI cut the run short
	// before its plan finished. Unfinished work carries no outcome, so an
	// aborted session is never a success.
	Aborted bool
	// AbortedBy names the abort source: AbortUser for an interactive Ctrl-C,
	// AbortSignal for a supervisor's SIGTERM. Empty unless Aborted is set.
	AbortedBy string
	// Duration is the scheduler-owned run wall supplied to terminal renderers so
	// JSON/JSONL includes setup work that occurs before Renderer.Start.
	Duration time.Duration
	// Cache is the settled cache attribution supplied only to terminal
	// renderers. Session reduction reads the same snapshot separately; keeping it
	// here lets Finish emit session:end without widening the renderer interface.
	Cache *CacheStatsSnapshot
}

// Renderer is the interface for rendering job execution output.
// Implementations live in the output package.
type Renderer interface {
	Start(planned []*ScheduledJob)
	JobStart(job *ScheduledJob)
	JobEvent(job *ScheduledJob, event RawJobEvent)
	JobComplete(job *ScheduledJob, result *JobResult)
	Finish(results map[string]*JobResult, outcome SessionOutcome)
}
