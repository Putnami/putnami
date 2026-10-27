package deliverycli

import (
	"fmt"
	"sync"
	"time"
)

// The run-end drain. An upload is queued and answered at once, so the entries
// of a run are stored in the background. When the run ends, the provider waits
// for them, but never without a bound: core gives each step of the end of a
// run a fixed time, and a storage endpoint that stops answering must not hold
// the run.
//
// Core's bounds (its cache provider session):
//
//   - summary: 60 s. Past it, core discards the whole summary, counts included.
//   - shutdown: 5 s for the acknowledgement, then core closes stdin, waits 5 s
//     for the process to exit, and kills it.
//
// The provider's bounds are variables so a test can shorten them. A session
// reads them once, when it is created; production never changes them.
var (
	// uploadDrainStall is how long the drain waits while no upload moves
	// forward: no byte of a blob read to be compressed or sent, no PUT
	// answered, no control exchange completed. It is the bound the object
	// cache's drain has (objectDrainTimeout). It is a bound on silence, not on
	// the total time, so an upload that takes longer and still sends bytes is
	// not cut by it.
	uploadDrainStall = 20 * time.Second
	// uploadDrainBudget is the longest a drain waits in all, at the summary, or
	// at the exit when stdin closes without a shutdown. At the summary it
	// includes the object cache's drain, which runs first. 50 s leaves 10 s of
	// core's 60 s for the answer to reach it, so the counts of what did store
	// are never discarded.
	uploadDrainBudget = 50 * time.Second
	// uploadDrainShutdown is the longest the shutdown waits before its
	// acknowledgement: 4 s of core's 5 s.
	uploadDrainShutdown = 4 * time.Second
)

// drainLimits are the drain bounds as a session read them at creation.
type drainLimits struct {
	stall, budget, shutdown time.Duration
}

func readDrainLimits() drainLimits {
	return drainLimits{stall: uploadDrainStall, budget: uploadDrainBudget, shutdown: uploadDrainShutdown}
}

// drainClock is the clock of the run-end drain: it dates the start of the
// drain, each step an upload moves forward and the drain's deadlines, and it
// times the drain's waits. Production reads the wall clock. A test moves the
// clock by hand, so whether the drain sees a stall or runs out of time does not
// depend on how fast the machine runs the uploads.
type drainClock interface {
	now() time.Time
	// after returns a channel that is closed once d has passed, and a
	// function that releases the wait when the caller stops waiting first.
	after(d time.Duration) (<-chan struct{}, func())
}

// wallClock is the drain clock of production.
type wallClock struct{}

func (wallClock) now() time.Time { return time.Now() }

func (wallClock) after(d time.Duration) (<-chan struct{}, func()) {
	fired := make(chan struct{})
	timer := time.AfterFunc(d, func() { close(fired) })
	return fired, func() { timer.Stop() }
}

// background counts the session's background work: the prefetch, the restores
// in flight and the uploader. It is a wait group whose wait can be given up: a
// sync.WaitGroup cannot be waited on with a bound without leaving a goroutine
// in Wait, and a later Add would then race it.
type background struct {
	mu sync.Mutex
	n  int
	// done is closed when n returns to zero. It is replaced each time work
	// starts from zero.
	done chan struct{}
}

// Add records delta more pieces of work.
func (b *background) Add(delta int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == 0 && delta > 0 {
		b.done = make(chan struct{})
	}
	b.n += delta
}

// Done records that one piece of work ended.
func (b *background) Done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n--
	if b.n == 0 {
		close(b.done)
	}
}

// idle returns a channel that is closed once no work is left. It is already
// closed when none runs.
func (b *background) idle() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == 0 {
		return closedSignal
	}
	return b.done
}

// Wait blocks until no work is left.
func (b *background) Wait() { <-b.idle() }

// closedSignal is the idle channel of a background with no work.
var closedSignal = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// drainOutcome is how a bounded wait for the background work ended.
type drainOutcome int

const (
	// drainDone: no background work is left.
	drainDone drainOutcome = iota
	// drainStalled: no upload moved forward for the stall bound.
	drainStalled
	// drainOutOfTime: uploads still moved, and the wait reached its deadline.
	drainOutOfTime
)

// awaitIdle waits for idle to close, until deadline. It gives up sooner when
// no upload moved forward for the stall bound, counted from the later of the
// start of the run-end drain and the last progress. A drain that already gave
// up on a stall therefore returns at once the next time, unless an upload
// moved in between.
func (s *providerSession) awaitIdle(idle <-chan struct{}, deadline time.Time) drainOutcome {
	w := &s.uploads
	w.mu.Lock()
	since := w.drainSince
	w.mu.Unlock()
	if since.IsZero() {
		since = s.clock.now()
	}
	for {
		moved := since
		if last := time.Unix(0, w.progress.Load()); last.After(moved) {
			moved = last
		}
		outcome := drainStalled
		until := moved.Add(s.limits.stall)
		if !deadline.After(until) {
			outcome, until = drainOutOfTime, deadline
		}
		wait := until.Sub(s.clock.now())
		if wait <= 0 {
			select {
			case <-idle:
				return drainDone
			default:
				return outcome
			}
		}
		fired, stop := s.clock.after(wait)
		select {
		case <-idle:
			stop()
			return drainDone
		case <-fired:
		}
	}
}

// drainBackground flushes the upload queue now and waits for all the
// background work until deadline: the uploader, the prefetch and the restores
// in flight. The summary and the shutdown use it. consequence says what happens
// to the entries it leaves.
func (s *providerSession) drainBackground(deadline time.Time, consequence string) {
	s.drainUploads()
	s.reportDrain(s.awaitIdle(s.bg.idle(), deadline), consequence)
}

// drainUploader flushes the upload queue now and waits for the uploader alone
// until deadline. The exit without a shutdown uses it: nobody reads the answer
// of a restore then, so only the entries core was told were accepted are worth
// the wait.
func (s *providerSession) drainUploader(deadline time.Time, consequence string) {
	s.drainUploads()
	s.reportDrain(s.awaitIdle(s.uploads.stoppedSignal(), deadline), consequence)
}

// reportDrain logs a drain that gave up, with the entries it leaves.
func (s *providerSession) reportDrain(outcome drainOutcome, consequence string) {
	if outcome == drainDone {
		return
	}
	w := &s.uploads
	w.mu.Lock()
	pending := len(w.queue) + w.inflight
	w.mu.Unlock()
	why := fmt.Sprintf("no upload moved forward for %s", s.limits.stall)
	if outcome == drainOutOfTime {
		why = "the uploads did not finish in the time the end of a run has"
	}
	if pending == 0 {
		s.log(fmt.Sprintf("drain: %s; a prefetch or a restore is still running", why))
		return
	}
	s.log(fmt.Sprintf("upload drain: %s; %d entries are not stored yet: %s", why, pending, consequence))
}
