package sessionreporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

const StateFile = "reporting.json"
const FinalizationBudget = 30 * time.Second
const CanceledFinalizationBudget = 2 * time.Second
const operationTimeout = 5 * time.Second
const maxAttempts = 3

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
type state struct {
	Version   int                                `json:"version"`
	Provider  string                             `json:"provider"`
	SessionID string                             `json:"sessionId"`
	Events    cursor                             `json:"events"`
	Session   cursor                             `json:"session"`
	Pending   *protocolcli.SessionReportingChunk `json:"pending,omitempty"`
	Complete  bool                               `json:"complete"`
	Error     string                             `json:"error,omitempty"`
}

type LaunchSpec struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
}
type Resolve func(context.Context) (LaunchSpec, error)

// Run owns a single delivery worker of one capability, independent of
// scheduler cancellation and of every other capability's worker. Only Finish
// enables terminal transmission. The worker is a declared subscriber of the
// session's event stream: it reads events.jsonl by offset from disk, and
// acknowledges a position only after its checkpoint is durable. While the graph
// runs it batches: a live chunk leaves when a full frame is committed or when
// the capability's batch interval has passed, never once per append. The
// producer never waits for it, and no callback waits for a pipe or a remote ACK.
type Run struct {
	capability    Capability
	dir           string
	events        *sessionstream.Subscription
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
}

// Start declares the capability as a subscriber of events, resuming at its
// checkpoint. An unselected capability returns nil. When the provider cannot be
// prepared it still returns the Run with the error: the worker never starts,
// and Finish records that the subscriber acknowledged nothing new.
func Start(ctx context.Context, capability Capability, events *sessionstream.Log, sessionID string, resolve Resolve) (*Run, error) {
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
	setupCtx, cancel := context.WithTimeout(ctx, FinalizationBudget)
	launch, err := resolve(setupCtx)
	cancel()
	if err != nil {
		r.state.Error = "provider_unavailable"
		_ = r.save()
		r.release()
		close(r.done)
		return r, fmt.Errorf("%s unavailable; retained session %s for replay", capability.Label, sessionID)
	}
	launch.Env = capability.providerEnv(ctx, launch.Env)
	r.launch = launch
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
	r.state = state{Version: 1, Provider: provider, SessionID: sessionID}
	data, err := readBounded(filepath.Join(dir, capability.StateFile), 2*protocolcli.SessionReportingLineBytes)
	if err == nil {
		if json.Unmarshal(data, &r.state) != nil || r.state.Version != 1 || r.state.Provider != provider || r.state.SessionID != sessionID {
			r.release()
			return nil, fmt.Errorf("reporting checkpoint is invalid or belongs to a different provider/session")
		}
		if err := r.validate(); err != nil {
			r.release()
			return nil, err
		}
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

// subscribe declares the capability on the stream at its durable events cursor.
// A cursor the checkpoint already finalized acknowledges the final marker too.
func (r *Run) subscribe(events *sessionstream.Log) error {
	sub, err := events.Subscribe(r.capability.Name, r.state.Events.Offset)
	if err != nil {
		return fmt.Errorf("reporting artifact changed: %s", sessionstream.EventsFile)
	}
	if r.state.Events.Final {
		_ = sub.Ack(r.state.Events.Offset, true)
	}
	r.events = sub
	return nil
}

func (r *Run) validate() error {
	for _, c := range []cursor{r.state.Events, r.state.Session} {
		if c.Offset < 0 || c.Sequence < 0 {
			return fmt.Errorf("invalid reporting cursor")
		}
	}
	if !r.capability.sends(sessionFile) && r.state.Session != (cursor{}) {
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
		if c.Final || p.Offset != c.Offset || p.Sequence != c.Sequence {
			return fmt.Errorf("pending reporting frame disagrees with cursor")
		}
	}
	return nil
}

func (r *Run) cursor(artifact string) *cursor {
	if artifact == sessionFile {
		return &r.state.Session
	}
	return &r.state.Events
}

// complete reports whether the capability acknowledged the final marker of
// every artifact it transmits.
func (r *Run) complete() bool {
	return r.state.Events.Final && (r.state.Session.Final || !r.capability.sends(sessionFile))
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
	terminal := false
	// The batch interval starts with the worker, so a trickle's first chunk
	// waits one interval like every later one.
	lastEventsSend := r.clock.Now()
	for !r.state.Complete {
		if r.ctx.Err() != nil {
			return fmt.Errorf("reporting budget exhausted")
		}
		select {
		case <-r.finish:
			terminal = true
		default:
		}
		if r.state.Pending == nil {
			// Preserve the consumer ordering: session.json, when the capability
			// transmits it, closes before events.
			artifact := sessionstream.EventsFile
			if terminal && r.capability.sends(sessionFile) && !r.state.Session.Final {
				artifact = sessionFile
			}
			if !terminal {
				if wait := r.eventsBatchWait(lastEventsSend); wait > 0 {
					finished, err := r.awaitEvents(wait)
					if err != nil {
						return err
					}
					terminal = finished
					continue
				}
			}
			chunk, err := r.next(artifact, terminal)
			if err != nil {
				return err
			}
			if chunk == nil {
				// Nothing new is committed: wait for the stream to grow, for Finish,
				// or for the budget. A closed finish channel is not waited on again.
				finish := r.finish
				if terminal {
					finish = nil
				}
				select {
				case <-r.ctx.Done():
					return fmt.Errorf("reporting budget exhausted")
				case <-finish:
					terminal = true
				case <-r.events.Wake():
				}
				continue
			}
			if artifact == sessionstream.EventsFile {
				lastEventsSend = r.clock.Now()
			}
			r.state.Pending = chunk
			if err := r.save(); err != nil {
				return err
			}
		}
		if err := r.send(*r.state.Pending); err != nil {
			return err
		}
		p := r.state.Pending
		c := r.cursor(p.Artifact)
		c.Offset += int64(len(p.Data))
		c.Sequence++
		c.Final = p.Final
		r.state.Pending = nil
		r.state.Complete = r.complete()
		if err := r.save(); err != nil {
			return err
		}
		if p.Artifact == sessionstream.EventsFile {
			// Acknowledged only once the checkpoint is durable. A refused
			// acknowledgement can only make the evidence under-report delivery.
			_ = r.events.Ack(c.Offset, c.Final)
		}
	}
	return nil
}

// eventsBatchWait applies the live batching rule to the bytes committed past the
// events cursor, measured from the stream's extent without reading them. A full
// frame leaves at once; a shorter one once the interval since the last events
// send has passed. With nothing pending the worker waits a whole interval, or
// until an append wakes it. Zero means send now.
func (r *Run) eventsBatchWait(lastSend time.Time) time.Duration {
	end, _ := r.events.Extent()
	pending := end.Offset - r.state.Events.Offset
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
		return false, fmt.Errorf("reporting budget exhausted")
	case <-r.finish:
		return true, nil
	case <-r.events.Wake():
	case <-r.clock.After(wait):
	}
	return false, nil
}

// next frames the artifact bytes after its cursor. events.jsonl is read through
// the stream subscription, which never reads past what the producer committed;
// its final marker additionally waits for the stream's own final marker.
// session.json is not a stream: it is read once it exists.
func (r *Run) next(artifact string, terminal bool) (*protocolcli.SessionReportingChunk, error) {
	c := r.cursor(artifact)
	var data []byte
	if artifact == sessionstream.EventsFile {
		read, err := r.events.Read(c.Offset, protocolcli.SessionReportingChunkBytes)
		if err != nil {
			return nil, fmt.Errorf("reporting artifact changed: %s", artifact)
		}
		_, final := r.events.Extent()
		data, terminal = read, terminal && final
	} else {
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

func (r *Run) send(chunk protocolcli.SessionReportingChunk) error {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if r.ctx.Err() != nil {
			return fmt.Errorf("reporting budget exhausted")
		}
		if r.process == nil {
			p, err := spawn(r.launch)
			if err != nil {
				return fmt.Errorf("reporter process could not start")
			}
			r.process = p
		}
		opCtx, cancel := context.WithTimeout(r.ctx, r.opTimeout)
		ack, err := r.process.call(opCtx, chunk)
		cancel()
		if err == nil {
			if !ack.Matches(chunk) {
				return fmt.Errorf("reporter acknowledgement identity mismatch")
			}
			if ack.OK {
				return nil
			}
			if !ack.Retryable {
				return fmt.Errorf("reporter refused chunk")
			}
		} else {
			r.process.close()
			r.process = nil
		}
		if attempt+1 < maxAttempts {
			select {
			case <-r.ctx.Done():
				return fmt.Errorf("reporting budget exhausted")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return fmt.Errorf("reporter retry budget exhausted")
}

// Finish drains under one total budget even if the graph context was canceled.
// A failed worker is not restarted here: its exact pending frame is retained
// for an explicit replay, without re-running graph work. Once the worker has
// stopped, the reporter's delivery evidence is recorded beside the session.
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

func (r *Run) drain() error {
	r.once.Do(func() { close(r.finish) })
	deadline := time.Now().Add(FinalizationBudget)
	graphDone := r.graphCtx.Done()
	if r.graphCtx.Err() != nil {
		deadline = time.Now().Add(CanceledFinalizationBudget)
		graphDone = nil
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case <-r.done:
			return r.err
		case <-graphDone:
			cancelDeadline := time.Now().Add(CanceledFinalizationBudget)
			if cancelDeadline.Before(deadline) {
				deadline = cancelDeadline
				timer.Reset(time.Until(deadline))
			}
			graphDone = nil
		case <-timer.C:
			r.cancel()
			<-r.done
			return r.err
		}
	}
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
