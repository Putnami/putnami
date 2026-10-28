package sessionreporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

const StateFile = "reporting.json"

// FinalizationProgressDeadline ends a capability's finalization once this long
// has passed without an acknowledged chunk of any artifact: a reporter that
// keeps acknowledging keeps delivering, a stuck one stops.
const FinalizationProgressDeadline = 30 * time.Second

// FinalizationCap ends a capability's finalization this long after it began,
// however steadily its reporter acknowledges.
const FinalizationCap = 5 * time.Minute

// CanceledFinalizationBudget caps what remains of a finalization once the
// graph is canceled.
const CanceledFinalizationBudget = 2 * time.Second

// providerSetupTimeout bounds the resolution of a reporter's launch.
const providerSetupTimeout = 30 * time.Second
const operationTimeout = 5 * time.Second
const maxAttempts = 3

// errBudgetExhausted is the worker's error once it is canceled. drain names
// the limit that canceled it.
var errBudgetExhausted = errors.New("reporting budget exhausted")

// errDocumentAbsent, errDocumentTooLarge and errDocumentUnreadable are why a
// whole document, plan.json or spec-verification.json, cannot be framed: it
// does not exist before its first chunk, it exceeds its bound, or it cannot be
// read at its cursor.
var (
	errDocumentAbsent     = errors.New("document absent")
	errDocumentTooLarge   = errors.New("document exceeds its bound")
	errDocumentUnreadable = errors.New("document unreadable")
)

// The reasons a checkpoint records in planOmitted. plan.json is best-effort:
// the worker closes it without its final marker, records why, and delivers
// the other artifacts.
const (
	// planAbsent: no plan.json existed before its first chunk.
	planAbsent = "absent"
	// planTooLarge: plan.json exceeds protocolcli.SessionReportingPlanBytes.
	planTooLarge = "too_large"
	// planUnreadable: plan.json cannot be read at its cursor: it is not a
	// regular file, a read fails, or it was removed or truncated after its
	// first chunk.
	planUnreadable = "unreadable"
	// planRefused: the reporter answered a plan.json chunk ok:false without
	// retry.
	planRefused = "refused"
	// planUndeliverable: the last attempt to deliver a plan.json chunk failed
	// in transport.
	planUndeliverable = "undeliverable"
	// planLate: events.jsonl or session.json progressed while plan.json was
	// open, as in a checkpoint without a plan cursor, so plan.json can no
	// longer precede them.
	planLate = "late"
)

var planOmissions = map[string]bool{planAbsent: true, planTooLarge: true, planUnreadable: true, planRefused: true, planUndeliverable: true, planLate: true}

// The reasons a checkpoint records in specVerificationOmitted.
// spec-verification.json is best-effort and leaves last: every failure to
// deliver it closes it without its final marker, records why, and leaves the
// delivery complete. The worker never restarts a reporter for it.
const (
	// specVerificationAbsent: no spec-verification.json existed before its
	// first chunk.
	specVerificationAbsent = "absent"
	// specVerificationTooLarge: spec-verification.json exceeds
	// protocolcli.SessionReportingSpecVerificationBytes.
	specVerificationTooLarge = "too_large"
	// specVerificationUnreadable: spec-verification.json cannot be read at its
	// cursor, as for planUnreadable.
	specVerificationUnreadable = "unreadable"
	// specVerificationRefused: the reporter answered a chunk ok:false, with
	// or without retry, or acknowledged another chunk.
	specVerificationRefused = "refused"
	// specVerificationQuit: the reporter exited, closed its pipe, stopped
	// answering, or could not start before it acknowledged a chunk.
	specVerificationQuit = "quit"
	// specVerificationLate: the finalization limits ended before it was
	// delivered, or the checkpoint completed without it, as one that a CLI
	// that does not send it wrote.
	specVerificationLate = "late"
)

var specVerificationOmissions = map[string]bool{specVerificationAbsent: true, specVerificationTooLarge: true, specVerificationUnreadable: true, specVerificationRefused: true, specVerificationQuit: true, specVerificationLate: true}

// EventsBatchInterval is the longest a live session reporter events.jsonl chunk
// shorter than one frame waits before it leaves: the chunk count, and the
// receiver's writes, stay bounded by run duration divided by this interval,
// whatever the run length. LogEventsBatchInterval is the log reporter's.
const EventsBatchInterval = 10 * time.Second

// clock is the delivery worker's time source. Only tests replace it.
type clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type batchIntervalKey struct{}

// WithEventsBatchInterval shortens every capability's live batching interval
// for a test that drives a whole engine run, which cannot reach the worker's
// fields. No production caller exists;
// TestOnlyTestsShortenTheEventsBatchInterval keeps it that way.
func WithEventsBatchInterval(ctx context.Context, interval time.Duration) context.Context {
	return context.WithValue(ctx, batchIntervalKey{}, interval)
}

func eventsBatchInterval(ctx context.Context, capability Capability) time.Duration {
	if interval, ok := ctx.Value(batchIntervalKey{}).(time.Duration); ok && interval > 0 {
		return interval
	}
	return capability.BatchInterval
}

type cursor struct {
	Offset   int64 `json:"offset"`
	Sequence int64 `json:"sequence"`
	Final    bool  `json:"final"`
}

// state retains at most one bounded pending frame, saved before it can leave
// the process. A crash after remote acceptance but before the local checkpoint
// therefore replays the exact same identity, including a short live chunk.
// plan.json is closed once its final marker is acknowledged or PlanOmitted
// records why it is not sent (planAbsent and the other reasons), and
// spec-verification.json likewise with SpecVerificationOmitted.
//
// At version 2 (viewCheckpointVersion), Events is the cursor of the view the
// events.jsonl frames are cut from, and EventsRaw the raw offset of
// events.jsonl the subscription acknowledged. At version 1, Events is a raw
// cursor and EventsRaw is zero, absent from the checkpoint.
type state struct {
	Version                 int                                `json:"version"`
	Provider                string                             `json:"provider"`
	SessionID               string                             `json:"sessionId"`
	Events                  cursor                             `json:"events"`
	EventsRaw               int64                              `json:"eventsRaw,omitempty"`
	Session                 cursor                             `json:"session"`
	Plan                    cursor                             `json:"plan"`
	PlanOmitted             string                             `json:"planOmitted,omitempty"`
	SpecVerification        cursor                             `json:"specVerification"`
	SpecVerificationOmitted string                             `json:"specVerificationOmitted,omitempty"`
	Pending                 *protocolcli.SessionReportingChunk `json:"pending,omitempty"`
	Complete                bool                               `json:"complete"`
	Error                   string                             `json:"error,omitempty"`
}

type LaunchSpec struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
	// Runtime is the runtime executable of the serving extension. A hosted
	// run hands the run credential only to a reporter whose Command is it
	// (runcredential.RequireNativeHolder).
	Runtime string
}
type Resolve func(context.Context) (LaunchSpec, error)

// Cause is why the engine cannot prepare a capability's provider, in the
// engine's own words, so the warning names it. The warning echoes no other
// error: one may carry provider output.
type Cause string

func (c Cause) Error() string { return string(c) }

// Run owns a single delivery worker of one capability, independent of
// scheduler cancellation and of every other capability's worker. Only Finish
// enables terminal transmission. The worker is a declared subscriber of the
// session's event stream: it reads events.jsonl by offset from disk, and
// acknowledges a position only after its checkpoint is durable. While the graph
// runs it batches: a live chunk leaves when a full frame is committed or when
// the capability's batch interval has passed, never once per append. The
// producer never waits for it, and no callback waits for a pipe or a remote ACK.
type Run struct {
	capability Capability
	dir        string
	events     *sessionstream.Subscription
	// view is the view events.jsonl frames are cut from at checkpoint version
	// 2, built from raw offset 0 at the run's first use (eventsView).
	view          *view
	state         state
	launch        LaunchSpec
	process       *process
	lock          *os.File
	ctx           context.Context
	graphCtx      context.Context
	cancel        context.CancelFunc
	finish        chan struct{}
	done          chan struct{}
	once          sync.Once
	err           error
	opTimeout     time.Duration
	clock         clock
	batchInterval time.Duration
	// plain: on a hosted run, the reporter cannot hold the run credential
	// (Holders.Start), so its process starts without one.
	plain bool
	// progressMu guards lastAck, the clock time of the latest chunk the
	// reporter accepted, which drain reads to apply the progress deadline.
	progressMu sync.Mutex
	lastAck    time.Time
}

// Start declares the capability as a subscriber of events, resuming at its
// checkpoint. An unselected capability returns nil. When the provider cannot be
// prepared it still returns the Run with the error: the worker never starts,
// and Finish records that the subscriber acknowledged nothing new.
func Start(ctx context.Context, capability Capability, events *sessionstream.Log, sessionID string, resolve Resolve) (*Run, error) {
	return startRun(ctx, capability, events, sessionID, resolve, nil)
}

// startRun is Start that adopts what holders started for the capability
// before repository code (Holders.take).
func startRun(ctx context.Context, capability Capability, events *sessionstream.Log, sessionID string, resolve Resolve, holders *Holders) (*Run, error) {
	provider := capability.Provider(ctx)
	if provider == "" {
		return nil, nil
	}
	r, err := openRun(ctx, capability, events.Dir(), sessionID, provider)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", capability.Label, err)
	}
	if err := r.subscribe(events); err != nil {
		r.release()
		return nil, fmt.Errorf("%s: %w", capability.Label, err)
	}
	setupCtx, cancel := context.WithTimeout(ctx, providerSetupTimeout)
	launch, err := resolve(setupCtx)
	cancel()
	if err != nil {
		r.state.Error = "provider_unavailable"
		_ = r.save()
		r.release()
		close(r.done)
		var cause Cause
		if errors.As(err, &cause) {
			return r, fmt.Errorf("%s unavailable: %w; retained session %s for replay", capability.Label, cause, sessionID)
		}
		return r, fmt.Errorf("%s unavailable; retained session %s for replay", capability.Label, sessionID)
	}
	launch.Env = capability.providerEnv(ctx, launch.Env)
	r.launch = launch
	r.process, r.plain = holders.take(capability, provider)
	go r.work()
	return r, nil
}

func openRun(ctx context.Context, capability Capability, dir, sessionID, provider string) (*Run, error) {
	lock, err := flock.OpenFile(filepath.Join(dir, capability.LockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open reporting lock")
	}
	if err := flock.LockFile(lock, true, true); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("already active")
	}
	workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &Run{capability: capability, dir: dir, lock: lock, ctx: workerCtx, graphCtx: ctx, cancel: cancel, finish: make(chan struct{}), done: make(chan struct{}), opTimeout: operationTimeout, clock: systemClock{}, batchInterval: eventsBatchInterval(ctx, capability)}
	r.state = state{Version: capability.checkpointVersion(), Provider: provider, SessionID: sessionID}
	data, err := readBounded(filepath.Join(dir, capability.StateFile), 2*protocolcli.SessionReportingLineBytes)
	if err == nil {
		// The members a checkpoint omits keep the values an earlier CLI
		// assumed for them: a checkpoint without a version is a version 1 one.
		saved := state{Version: rawCheckpointVersion, Provider: provider, SessionID: sessionID}
		if json.Unmarshal(data, &saved) != nil || !knownCheckpointVersion(saved.Version) || saved.Provider != provider || saved.SessionID != sessionID {
			r.release()
			return nil, fmt.Errorf("reporting checkpoint is invalid or belongs to a different provider/session")
		}
		r.state = saved
		r.omitLatePlan()
		r.omitLateSpecVerification()
		if err := r.validate(); err != nil {
			r.release()
			return nil, err
		}
		r.moveToView()
	} else if !os.IsNotExist(err) {
		r.release()
		return nil, fmt.Errorf("read reporting checkpoint")
	}
	r.state.Error = ""
	if err := r.save(); err != nil {
		r.release()
		return nil, err
	}
	return r, nil
}

func knownCheckpointVersion(version int) bool {
	return version == rawCheckpointVersion || version == viewCheckpointVersion
}

// moveToView moves a version 1 checkpoint of a View capability to version 2
// when it framed no events.jsonl chunk yet. One that did keeps raw framing for
// its session: its receiver holds raw offsets.
func (r *Run) moveToView() {
	s := &r.state
	if !r.capability.View || s.Version != rawCheckpointVersion || s.Events.Sequence > 0 || s.Pending != nil && s.Pending.Artifact == sessionstream.EventsFile {
		return
	}
	s.Version, s.EventsRaw = viewCheckpointVersion, 0
}

// viewFraming reports whether events.jsonl frames are cut from the view: a
// View capability at checkpoint version 2.
func (r *Run) viewFraming() bool {
	return r.capability.View && r.state.Version == viewCheckpointVersion
}

// eventsView is the run's view, built from raw offset 0 at its first use and
// positioned at the checkpoint's view cursor.
func (r *Run) eventsView() *view {
	if r.view == nil {
		r.view = newView(r.events, r.state.Events.Offset)
	}
	return r.view
}

// subscribe declares the capability on the stream at its durable raw position:
// the events cursor, or at version 2 the raw offset it acknowledged. A cursor
// the checkpoint already finalized acknowledges the final marker too.
func (r *Run) subscribe(events *sessionstream.Log) error {
	from := r.state.Events.Offset
	if r.viewFraming() {
		from = r.state.EventsRaw
	}
	sub, err := events.Subscribe(r.capability.Name, from)
	if err != nil {
		return fmt.Errorf("reporting artifact changed: %s", sessionstream.EventsFile)
	}
	if r.state.Events.Final {
		_ = sub.Ack(from, true)
	}
	r.events = sub
	return nil
}

// omitLatePlan closes an open plan.json as planLate once events.jsonl or
// session.json progressed, or holds the pending frame: plan.json precedes
// every other frame or is not sent. A checkpoint without a plan member holds
// an open plan cursor.
func (r *Run) omitLatePlan() {
	if r.capability.sends(planFile) && !r.planClosed() && r.otherArtifactProgressed() {
		r.state.PlanOmitted = planLate
	}
}

// otherArtifactProgressed reports whether an artifact other than plan.json
// framed a chunk.
func (r *Run) otherArtifactProgressed() bool {
	s := r.state
	return s.Events.Sequence > 0 || s.Session.Sequence > 0 || s.SpecVerification.Sequence > 0 || s.Pending != nil && s.Pending.Artifact != planFile
}

// planClosed reports whether plan.json acknowledged its final marker or was
// omitted.
func (r *Run) planClosed() bool {
	return r.state.Plan.Final || r.state.PlanOmitted != ""
}

// omitLateSpecVerification closes an open spec-verification.json as late in a
// checkpoint that completed without it: a CLI that does not send the record
// wrote it, and the evidence of its delivery already reads delivered, so no
// replay would send the record.
func (r *Run) omitLateSpecVerification() {
	if r.state.Complete && r.specVerificationOpen() {
		r.state.SpecVerificationOmitted = specVerificationLate
	}
}

// specVerificationOpen reports whether the capability sends
// spec-verification.json and has neither acknowledged its final marker nor
// omitted it.
func (r *Run) specVerificationOpen() bool {
	return r.capability.sends(specVerificationFile) && !r.state.SpecVerification.Final && r.state.SpecVerificationOmitted == ""
}

// specVerificationStarted reports whether spec-verification.json framed a
// chunk, has one pending, or was omitted.
func (r *Run) specVerificationStarted() bool {
	s := r.state
	return s.SpecVerification != (cursor{}) || s.SpecVerificationOmitted != "" || s.Pending != nil && s.Pending.Artifact == specVerificationFile
}

func (r *Run) validate() error {
	if !r.validCursors() {
		return fmt.Errorf("invalid reporting cursor")
	}
	if !r.capability.sends(sessionFile) && r.state.Session != (cursor{}) {
		return fmt.Errorf("invalid reporting cursor")
	}
	if !r.capability.sends(planFile) && (r.state.Plan != (cursor{}) || r.state.PlanOmitted != "") {
		return fmt.Errorf("invalid reporting cursor")
	}
	if r.state.PlanOmitted != "" && (r.state.Plan.Final || !planOmissions[r.state.PlanOmitted]) {
		return fmt.Errorf("invalid reporting cursor")
	}
	if r.capability.sends(planFile) && !r.planClosed() && r.otherArtifactProgressed() {
		return fmt.Errorf("invalid reporting cursor")
	}
	// spec-verification.json frames only once the events final marker is
	// acknowledged.
	if r.specVerificationStarted() && (!r.capability.sends(specVerificationFile) || !r.state.Events.Final) {
		return fmt.Errorf("invalid reporting cursor")
	}
	if r.state.SpecVerificationOmitted != "" && (r.state.SpecVerification.Final || !specVerificationOmissions[r.state.SpecVerificationOmitted]) {
		return fmt.Errorf("invalid reporting cursor")
	}
	if r.capability.sends(sessionFile) && r.state.Events.Final && !r.state.Session.Final || r.state.Complete != r.complete() {
		return fmt.Errorf("invalid reporting completion")
	}
	if p := r.state.Pending; p != nil {
		if p.Validate() != nil || p.SessionID != r.state.SessionID || !r.capability.sends(p.Artifact) {
			return fmt.Errorf("invalid pending reporting frame")
		}
		c := r.cursor(p.Artifact)
		if c.Final || p.Offset != c.Offset || p.Sequence != c.Sequence || p.Artifact == planFile && r.state.PlanOmitted != "" || p.Artifact == specVerificationFile && r.state.SpecVerificationOmitted != "" {
			return fmt.Errorf("pending reporting frame disagrees with cursor")
		}
	}
	return nil
}

// validCursors reports whether no cursor is negative and the checkpoint's
// version and raw offset agree: only a View capability frames the view, and
// only a view checkpoint records a raw offset beside its cursor.
func (r *Run) validCursors() bool {
	for _, c := range []cursor{r.state.Events, r.state.Session, r.state.Plan, r.state.SpecVerification} {
		if c.Offset < 0 || c.Sequence < 0 {
			return false
		}
	}
	if r.state.Version == viewCheckpointVersion {
		return r.capability.View && r.state.EventsRaw >= 0
	}
	return r.state.EventsRaw == 0
}

func (r *Run) cursor(artifact string) *cursor {
	switch artifact {
	case sessionFile:
		return &r.state.Session
	case planFile:
		return &r.state.Plan
	case specVerificationFile:
		return &r.state.SpecVerification
	}
	return &r.state.Events
}

// complete reports whether the capability closed every artifact it transmits:
// it acknowledged each final marker, or omitted plan.json or
// spec-verification.json.
func (r *Run) complete() bool {
	return r.state.Events.Final && (r.state.Session.Final || !r.capability.sends(sessionFile)) && (r.planClosed() || !r.capability.sends(planFile)) && !r.specVerificationOpen()
}

// save publishes the checkpoint through a staged file and a rename. The rename
// waits out a reader that holds the checkpoint open, which fails a rename on
// Windows (robustio): a scanner or a test reads the checkpoint without the lock.
func (r *Run) save() error {
	data, err := json.Marshal(r.state)
	if err != nil {
		return fmt.Errorf("encode reporting checkpoint")
	}
	f, err := os.CreateTemp(r.dir, ".reporting-*")
	if err != nil {
		return fmt.Errorf("create reporting checkpoint")
	}
	name := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(name) }()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write reporting checkpoint")
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync reporting checkpoint")
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close reporting checkpoint")
	}
	if err := robustio.Rename(name, filepath.Join(r.dir, r.capability.StateFile)); err != nil {
		return fmt.Errorf("publish reporting checkpoint")
	}
	if err := flock.SyncDir(r.dir); err != nil {
		return fmt.Errorf("sync reporting checkpoint directory")
	}
	return nil
}

func (r *Run) release() {
	r.cancel()
	if r.process != nil {
		r.process.close()
	}
	if r.lock != nil {
		_ = flock.UnlockFile(r.lock)
		_ = r.lock.Close()
	}
}

func (r *Run) work() {
	defer close(r.done)
	defer r.release()
	r.err = r.deliver()
	if r.err != nil {
		// Diagnostics contain core-owned reason strings only, never subprocess
		// stderr, request bytes, transport URLs, or arbitrary provider messages.
		r.state.Error = "delivery_incomplete"
		_ = r.save()
	}
}

func (r *Run) deliver() error {
	if err := r.verifyPendingView(); err != nil {
		return err
	}
	terminal := false
	// The batch interval starts with the worker, so a trickle's first events
	// chunk waits one interval like every later one.
	lastEventsSend := r.clock.Now()
	for !r.state.Complete {
		if r.ctx.Err() != nil {
			// A spec-verification.json that alone remains when the limits end
			// is omitted, and the delivery it would have ended is complete.
			if r.state.Events.Final && r.specVerificationOpen() {
				return r.omit(specVerificationFile, specVerificationLate)
			}
			return errBudgetExhausted
		}
		select {
		case <-r.finish:
			terminal = true
		default:
		}
		if r.state.Pending == nil {
			pending, finished, err := r.frame(terminal, &lastEventsSend)
			if err != nil {
				return err
			}
			terminal = terminal || finished
			if !pending {
				continue
			}
		}
		if err := r.send(*r.state.Pending); err != nil {
			artifact := r.state.Pending.Artifact
			if reason := omission(artifact, err); reason != "" {
				if err := r.omit(artifact, reason); err != nil {
					return err
				}
				continue
			}
			return err
		}
		p := r.state.Pending
		c := r.cursor(p.Artifact)
		c.Offset += int64(len(p.Data))
		c.Sequence++
		c.Final = p.Final
		raw := c.Offset
		if p.Artifact == sessionstream.EventsFile && r.viewFraming() {
			// The view cursor moved: the raw offset it acknowledges only
			// moves forward.
			r.state.EventsRaw = max(r.state.EventsRaw, r.eventsView().acknowledged(c.Offset))
			raw = r.state.EventsRaw
		}
		r.state.Pending = nil
		r.state.Complete = r.complete()
		if err := r.save(); err != nil {
			return err
		}
		r.acknowledged()
		if p.Artifact == sessionstream.EventsFile {
			// Acknowledged only once the checkpoint is durable. A refused
			// acknowledgement can only make the evidence under-report delivery.
			_ = r.events.Ack(raw, c.Final)
		}
	}
	return nil
}

// verifyPendingView fails delivery when the pending events.jsonl frame of a
// view checkpoint is not the bytes the rebuilt view holds at its offset, or a
// pending final marker does not end it: the stream changed since the frame
// was cut.
func (r *Run) verifyPendingView() error {
	p := r.state.Pending
	if p == nil || p.Artifact != sessionstream.EventsFile || !r.viewFraming() {
		return nil
	}
	v := r.eventsView()
	target := p.Offset + int64(len(p.Data))
	if p.Final {
		target = math.MaxInt64
	}
	err := v.fill(target)
	var data []byte
	if err == nil {
		data, err = v.read(p.Offset, len(p.Data))
	}
	if err != nil || !bytes.Equal(data, p.Data) || p.Final && (!v.done || v.end != p.Offset) {
		return fmt.Errorf("reporting artifact changed: %s", sessionstream.EventsFile)
	}
	return nil
}

// frame saves the next frame as pending and reports whether it did. When it
// did not, it waited, under the live batching rule or for a stream with
// nothing new, or it omitted plan.json or spec-verification.json, and the
// worker loops; finished reports that Finish was called while it waited.
// lastEventsSend is the time of the latest events frame, which frame updates.
func (r *Run) frame(terminal bool, lastEventsSend *time.Time) (pending, finished bool, err error) {
	// Preserve the consumer ordering: plan.json, when the capability transmits
	// it, closes before any other frame leaves, session.json closes before
	// events, and spec-verification.json leaves once the events final marker
	// is acknowledged.
	artifact := sessionstream.EventsFile
	switch {
	case r.capability.sends(planFile) && !r.planClosed():
		artifact = planFile
	case terminal && r.capability.sends(sessionFile) && !r.state.Session.Final:
		artifact = sessionFile
	case r.state.Events.Final && r.specVerificationOpen():
		artifact = specVerificationFile
	}
	if artifact == sessionstream.EventsFile && !terminal {
		if wait := r.eventsBatchWait(*lastEventsSend); wait > 0 {
			called, err := r.awaitEvents(wait)
			return false, called, err
		}
	}
	chunk, err := r.next(artifact, terminal)
	if reason := omission(artifact, err); reason != "" {
		return false, false, r.omit(artifact, reason)
	}
	if err != nil {
		return false, false, err
	}
	if chunk == nil {
		// Nothing new is committed: wait for the stream to grow, for Finish, or
		// for the budget. A closed finish channel is not waited on again.
		finish := r.finish
		if terminal {
			finish = nil
		}
		select {
		case <-r.ctx.Done():
			return false, false, errBudgetExhausted
		case <-finish:
			return false, true, nil
		case <-r.events.Wake():
			return false, false, nil
		}
	}
	if artifact == sessionstream.EventsFile {
		*lastEventsSend = r.clock.Now()
	}
	r.state.Pending = chunk
	return true, false, r.save()
}

// omission is the reason to omit artifact after err framed or sent one of its
// chunks, or "" when err is nil or stops the delivery. Only plan.json and
// spec-verification.json are ever omitted.
func omission(artifact string, err error) string {
	switch {
	case err == nil:
		return ""
	case artifact == planFile:
		return planOmission(err)
	case artifact == specVerificationFile:
		return specVerificationOmission(err)
	}
	return ""
}

// planOmission is the reason to omit plan.json after err, or "" when err stops
// the delivery: a budget, a reporter that cannot start, an acknowledgement of
// another identity, a refusal with retry on the last attempt (errRetryBudget),
// whose frame stays pending for replay.
func planOmission(err error) string {
	var failure *chunkFailure
	switch {
	case errors.Is(err, errDocumentAbsent):
		return planAbsent
	case errors.Is(err, errDocumentTooLarge):
		return planTooLarge
	case errors.Is(err, errDocumentUnreadable):
		return planUnreadable
	case errors.As(err, &failure) && failure.refused:
		return planRefused
	case errors.As(err, &failure):
		return planUndeliverable
	}
	return ""
}

// specVerificationOmission is the reason to omit spec-verification.json after
// err. Every error omits it: it leaves after the events final marker, whose
// acknowledgement already makes the evidence read delivered, so no replay
// would retry it.
func specVerificationOmission(err error) string {
	var failure *chunkFailure
	switch {
	case errors.Is(err, errDocumentAbsent):
		return specVerificationAbsent
	case errors.Is(err, errDocumentTooLarge):
		return specVerificationTooLarge
	case errors.Is(err, errDocumentUnreadable):
		return specVerificationUnreadable
	case errors.Is(err, errBudgetExhausted):
		return specVerificationLate
	case errors.As(err, &failure) && failure.refused, errors.Is(err, errRetryBudget), errors.Is(err, errAckMismatch):
		return specVerificationRefused
	}
	// A failure in transport, or a reporter that could not start.
	return specVerificationQuit
}

// omit closes artifact, plan.json or spec-verification.json, without its
// final marker, records reason, and drops its pending frame, durably, before
// another frame is framed.
func (r *Run) omit(artifact, reason string) error {
	if artifact == planFile {
		r.state.PlanOmitted = reason
	} else {
		r.state.SpecVerificationOmitted = reason
	}
	r.state.Pending = nil
	r.state.Complete = r.complete()
	return r.save()
}

// acknowledged records that the reporter accepted a chunk and that the
// checkpoint holding it is durable.
func (r *Run) acknowledged() {
	now := r.clock.Now()
	r.progressMu.Lock()
	r.lastAck = now
	r.progressMu.Unlock()
}

// lastProgress is the latest chunk acceptance at or after since, or since when
// the reporter accepted none.
func (r *Run) lastProgress(since time.Time) time.Time {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	if r.lastAck.After(since) {
		return r.lastAck
	}
	return since
}

// eventsBatchWait applies the live batching rule to the bytes ready past the
// events cursor: for raw framing, the bytes committed, measured from the
// stream's extent without reading them; for the view, the view bytes its
// records left as, scanned only as far as one frame needs. A full frame leaves
// at once; a shorter one once the interval since the last events send has
// passed. With nothing pending the worker waits a whole interval, or until an
// append wakes it. Zero means send now, as for a view that cannot be read,
// whose next frame then reports why.
func (r *Run) eventsBatchWait(lastSend time.Time) time.Duration {
	var pending int64
	if r.viewFraming() {
		available, err := r.eventsView().available(r.state.Events.Offset)
		if err != nil {
			return 0
		}
		pending = available
	} else {
		end, _ := r.events.Extent()
		pending = end.Offset - r.state.Events.Offset
	}
	switch {
	case pending >= protocolcli.SessionReportingChunkBytes:
		return 0
	case pending <= 0:
		return r.batchInterval
	}
	return max(lastSend.Add(r.batchInterval).Sub(r.clock.Now()), 0)
}

// awaitEvents blocks until the stream grows, the wait elapses, Finish is
// called, or the budget ends. It reports whether Finish was called. Nothing is
// framed or saved while it waits.
func (r *Run) awaitEvents(wait time.Duration) (bool, error) {
	select {
	case <-r.ctx.Done():
		return false, errBudgetExhausted
	case <-r.finish:
		return true, nil
	case <-r.events.Wake():
	case <-r.clock.After(wait):
	}
	return false, nil
}

// next frames the artifact bytes after its cursor. events.jsonl is read through
// the stream subscription, which never reads past what the producer committed;
// its final marker additionally waits for the stream's own final marker, and
// for the view to be done. session.json is not a stream: it is read once it
// exists. plan.json is whole before the worker starts, and
// spec-verification.json before Finish, so neither final marker waits for the
// graph.
func (r *Run) next(artifact string, terminal bool) (*protocolcli.SessionReportingChunk, error) {
	c := r.cursor(artifact)
	var data []byte
	switch artifact {
	case sessionstream.EventsFile:
		read, final, err := r.readEvents(c.Offset)
		if err != nil {
			return nil, fmt.Errorf("reporting artifact changed: %s", artifact)
		}
		data, terminal = read, terminal && final
	case planFile, specVerificationFile:
		read, err := readDocument(r.dir, artifact, *c)
		if err != nil {
			return nil, err
		}
		data, terminal = read, true
	default:
		read, err := readArtifact(r.dir, artifact, c.Offset)
		if err != nil {
			return nil, err
		}
		data = read
	}
	if len(data) == 0 && !terminal {
		return nil, nil
	}
	chunk := protocolcli.NewSessionReportingChunk(r.state.SessionID, artifact, c.Offset, c.Sequence, data, len(data) == 0)
	return &chunk, nil
}

// readEvents reads up to one chunk of events.jsonl frame bytes at offset, from
// the view or from the raw stream, and reports whether a final marker may
// follow them.
func (r *Run) readEvents(offset int64) ([]byte, bool, error) {
	if r.viewFraming() {
		return r.eventsView().frame(offset)
	}
	read, err := r.events.Read(offset, protocolcli.SessionReportingChunkBytes)
	_, final := r.events.Extent()
	return read, final, err
}

// documentBounds are the whole documents a capability may transmit, each with
// the protocol bound on its bytes.
var documentBounds = map[string]int64{
	planFile:             protocolcli.SessionReportingPlanBytes,
	specVerificationFile: protocolcli.SessionReportingSpecVerificationBytes,
}

// readDocument reads the bytes after c of artifact, a whole document of
// documentBounds. Every failure omits the document: errDocumentAbsent when it
// does not exist before its first chunk, errDocumentTooLarge when it exceeds
// its bound, and errDocumentUnreadable otherwise, for a document that is not a
// regular file, fails to read, or was removed or truncated after its first
// chunk.
func readDocument(dir, artifact string, c cursor) ([]byte, error) {
	bound := documentBounds[artifact]
	info, err := os.Stat(filepath.Join(dir, artifact))
	if errors.Is(err, fs.ErrNotExist) && c == (cursor{}) {
		return nil, errDocumentAbsent
	}
	if err == nil && info.Size() > bound {
		return nil, errDocumentTooLarge
	}
	data, err := readArtifact(dir, artifact, c.Offset)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDocumentUnreadable, err)
	}
	if c.Offset+int64(len(data)) > bound {
		return nil, errDocumentTooLarge
	}
	return data, nil
}

func readArtifact(dir, artifact string, offset int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(dir, artifact))
	if err != nil {
		return nil, fmt.Errorf("reporting artifact unavailable: %s", artifact)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < offset {
		return nil, fmt.Errorf("reporting artifact changed: %s", artifact)
	}
	data := make([]byte, protocolcli.SessionReportingChunkBytes)
	n, err := f.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read reporting artifact: %s", artifact)
	}
	return data[:n], nil
}

// chunkFailure is a chunk the reporter did not accept, for a reason that lets
// plan.json be omitted (planOmission): refused when it answered ok:false
// without retry, otherwise because its last attempt ended with a failure in
// transport (an exit, a closed pipe, a timeout, an unreadable
// acknowledgement). A chunk the reporter answered ok:false with retry on its
// last attempt is not a chunkFailure: its frame stays pending for replay, as
// for any artifact but spec-verification.json.
type chunkFailure struct {
	refused bool
	reason  string
}

func (f *chunkFailure) Error() string { return f.reason }

// errRetryBudget is a chunk the reporter answered ok:false with retry on its
// last attempt.
var errRetryBudget = errors.New("reporter retry budget exhausted")

// errAckMismatch is an acknowledgement of another chunk than the one sent.
var errAckMismatch = errors.New("reporter acknowledgement identity mismatch")

// send delivers chunk in at most maxAttempts attempts, starting the reporter
// when no process serves the capability. A spec-verification.json chunk never
// restarts it: its first failure in transport ends the attempts.
func (r *Run) send(chunk protocolcli.SessionReportingChunk) error {
	retryable := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if r.ctx.Err() != nil {
			return errBudgetExhausted
		}
		if r.process == nil {
			p, err := r.start()
			if err != nil {
				return err
			}
			r.process = p
		}
		opCtx, cancel := context.WithTimeout(r.ctx, r.opTimeout)
		ack, err := r.process.call(opCtx, chunk)
		cancel()
		if err == nil {
			if !ack.Matches(chunk) {
				return errAckMismatch
			}
			if ack.OK {
				return nil
			}
			if !ack.Retryable {
				return &chunkFailure{refused: true, reason: "reporter refused chunk"}
			}
			retryable = true
		} else {
			r.process.close()
			r.process = nil
			retryable = false
			if chunk.Artifact == specVerificationFile {
				if r.ctx.Err() != nil {
					return errBudgetExhausted
				}
				// A reporter may quit after the events final marker, as one
				// that does not receive the record does.
				return &chunkFailure{reason: "reporter quit"}
			}
		}
		if attempt+1 < maxAttempts {
			select {
			case <-r.ctx.Done():
				return errBudgetExhausted
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if retryable {
		return errRetryBudget
	}
	return &chunkFailure{reason: errRetryBudget.Error()}
}

// start starts the provider process. Without a run credential, or for a
// reporter that cannot hold it (plain), it starts without a credential and
// sends no handshake. Otherwise it starts as a holder (startHolder): custody
// refuses a reporter that starts, or restarts, after repository code ran, and
// the delivery then fails with that refusal, never with a reporter that holds
// no credential. Its errors are core-owned.
func (r *Run) start() (*process, error) {
	if !runcredential.Hosted() || r.plain {
		p, err := spawn(r.launch)
		if err != nil {
			return nil, errReporterStart
		}
		return p, nil
	}
	return startHolder(r.ctx, r.capability.holderName(r.state.Provider), r.launch, r.opTimeout)
}

// Finish drains under the finalization limits (drain) even if the graph
// context was canceled. A failed worker is not restarted here: its exact
// pending frame is retained for an explicit replay, without re-running graph
// work. Once the worker has stopped, the reporter's delivery evidence is
// recorded beside the session.
func (r *Run) Finish() error {
	if r == nil {
		return nil
	}
	return errors.Join(r.drain(), r.recordEvidence())
}

// recordEvidence states in subscribers.json how much of the event stream the
// reporter acknowledged, then releases its subscription.
func (r *Run) recordEvidence() error {
	if r.events == nil {
		return nil
	}
	defer func() { _ = r.events.Close() }()
	if err := r.events.RecordEvidence(); err != nil {
		return fmt.Errorf("delivery evidence not recorded")
	}
	return nil
}

// drain enables terminal transmission and waits for the worker. It cancels the
// worker FinalizationProgressDeadline after the latest acknowledged chunk, or
// after drain began when none was acknowledged since, and FinalizationCap after
// drain began, whichever comes first. A graph canceled before or during the
// drain caps what remains to CanceledFinalizationBudget. A worker that
// completes before a limit succeeds.
func (r *Run) drain() error {
	r.once.Do(func() { close(r.finish) })
	started := r.clock.Now()
	limit, limitReason := started.Add(FinalizationCap), fmt.Sprintf("finalization reached its %s cap", FinalizationCap)
	graphDone := r.graphCtx.Done()
	if r.graphCtx.Err() != nil {
		limit, limitReason = started.Add(CanceledFinalizationBudget), ""
		graphDone = nil
	}
	for {
		now := r.clock.Now()
		deadline, reason := limit, limitReason
		if stalled := r.lastProgress(started).Add(FinalizationProgressDeadline); stalled.Before(deadline) {
			deadline, reason = stalled, fmt.Sprintf("no chunk acknowledged for %s", FinalizationProgressDeadline)
		}
		if !now.Before(deadline) {
			return r.stop(reason)
		}
		select {
		case <-r.done:
			return r.err
		case <-graphDone:
			if canceled := r.clock.Now().Add(CanceledFinalizationBudget); canceled.Before(limit) {
				limit, limitReason = canceled, ""
			}
			graphDone = nil
		case <-r.clock.After(deadline.Sub(now)):
		}
	}
}

// stop cancels the worker at a finalization limit and waits for it. A worker
// the cancellation stopped fails naming reason; an empty reason keeps the
// worker's own error, as does a worker that failed or completed on its own.
func (r *Run) stop(reason string) error {
	r.cancel()
	<-r.done
	if reason != "" && errors.Is(r.err, errBudgetExhausted) {
		return fmt.Errorf("%w: %s", r.err, reason)
	}
	return r.err
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("reporting record exceeds limit or is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fmt.Errorf("could not read bounded reporting record")
	}
	return data, nil
}

func validateFinalizedSession(dir, sessionID string) error {
	// This is a validation bound, not a transport allocation: every transmitted
	// frame remains 64 KiB and the artifact itself remains the sole source.
	data, err := readBounded(filepath.Join(dir, "session.json"), 16*1024*1024)
	if err != nil {
		return fmt.Errorf("session has no readable finalized record")
	}
	var document protocolcli.SessionFile
	if json.Unmarshal(data, &document) != nil || document.SessionID != sessionID || document.EndTime == "" || len(protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data)) != 0 {
		return fmt.Errorf("session has no valid finalized record matching its id")
	}
	if document.Run.Outcome != protocolcli.RunOutcomeSuccess && document.Run.Outcome != protocolcli.RunOutcomeFailure && document.Run.Outcome != protocolcli.RunOutcomeAborted {
		return fmt.Errorf("session verdict is not terminal")
	}
	return nil
}
