package jobs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

// hostAdmissionBound is how long the host may hold a started process before
// the process runs its first instruction. It bounds the hold only and is not a
// deadline on the process's own work: a processDeadline counts that work from
// the moment the process is seen running.
//
// It also applies, in place of the deadline, to a process that blocked in its
// own code before its progress was first sampled: the baseline sample follows
// exec.Cmd.Start, and on a loaded machine a fast program can run and block in
// between. Such a process is stopped at this bound instead of at its deadline.
const hostAdmissionBound = 2 * time.Minute

// processProgressFirstPoll and processProgressMaxPoll space the samples of a
// process not yet seen running: the first one 1 ms after the baseline, each
// next one twice as late, up to 25 ms apart. The deadline therefore arms
// about 25 ms after the process starts to run, plus however long this
// goroutine waits for a CPU on a loaded machine.
const (
	processProgressFirstPoll = time.Millisecond
	processProgressMaxPoll   = 25 * time.Millisecond
)

// errProcessDeadline is the cause a processWatch cancels its process's
// context with when the deadline fires.
var errProcessDeadline = errors.New("process deadline expired")

// processProgress samples a started process once, as its baseline, and
// returns a function that reports whether the process ran its own code since.
// A nil function means the process counts as running already. Any error
// reading the process counts as running, so a deadline that cannot read the
// process counts from the failed read and never waits for the admission bound.
type processProgress func(process *os.Process) (moved func() bool)

// processDeadline bounds a started process. run counts from the moment the
// process is first seen running its own code, not from the moment
// exec.Cmd.Start returned, so the time the host holds a new process before its
// first instruction is never charged to it. admission bounds that hold.
type processDeadline struct {
	run       time.Duration
	admission time.Duration
	progress  processProgress
}

// hostProcessDeadline is the deadline the CLI gives a process it started:
// run counts from the process's first instruction on this host, and
// hostAdmissionBound bounds the hold before it.
func hostProcessDeadline(run time.Duration) processDeadline {
	return processDeadline{run: run, admission: hostAdmissionBound, progress: hostProcessProgress}
}

// watch arms the deadline of process, which exec.Cmd.Start has just started.
// It samples the process's baseline before it returns. onExpire, when not nil,
// runs once when the deadline fires, before expired is closed. The caller
// calls stop as soon as Wait returns.
func (d processDeadline) watch(process *os.Process, onExpire func()) *processWatch {
	w := &processWatch{
		armed:    time.Now(),
		expired:  make(chan struct{}),
		stopping: make(chan struct{}),
		done:     make(chan struct{}),
	}
	moved := d.progress(process)
	go w.follow(d, moved, onExpire)
	return w
}

// processWatch is the armed deadline of one started process.
type processWatch struct {
	armed time.Time
	// expired is closed when the deadline fires.
	expired  chan struct{}
	stopping chan struct{}
	stopOnce sync.Once
	// done is closed when follow returns. follow writes the fields below before.
	done          chan struct{}
	running       time.Time
	ended         time.Time
	heldPastBound bool
}

// processTiming is how a started process spent the time its deadline watched.
type processTiming struct {
	// held is how long the process was not seen running: until its first
	// sample that moved, or until the watch ended when none did. It is not
	// counted.
	held time.Duration
	// ran is how long the process ran under its deadline.
	ran time.Duration
	// heldPastBound reports that the deadline fired because the process was
	// not seen running within the admission bound.
	heldPastBound bool
}

// follow waits until the process is seen running, then for the run deadline.
func (w *processWatch) follow(d processDeadline, moved func() bool, onExpire func()) {
	defer func() {
		w.ended = time.Now()
		close(w.done)
	}()
	if moved != nil && !w.awaitRunning(moved, d.admission) {
		if w.heldPastBound {
			w.expire(onExpire)
		}
		return
	}
	w.running = time.Now()
	timer := time.NewTimer(d.run)
	defer timer.Stop()
	select {
	case <-timer.C:
		w.expire(onExpire)
	case <-w.stopping:
	}
}

// awaitRunning samples the process until it moved, and reports whether it
// did. It returns false when stop is called first, or when admission elapses
// first, which it records.
func (w *processWatch) awaitRunning(moved func() bool, admission time.Duration) bool {
	bound := time.NewTimer(admission)
	defer bound.Stop()
	interval := processProgressFirstPoll
	poll := time.NewTimer(interval)
	defer poll.Stop()
	for {
		select {
		case <-w.stopping:
			return false
		case <-bound.C:
			w.heldPastBound = true
			return false
		case <-poll.C:
			if moved() {
				return true
			}
			interval = min(2*interval, processProgressMaxPoll)
			poll.Reset(interval)
		}
	}
}

func (w *processWatch) expire(onExpire func()) {
	if onExpire != nil {
		onExpire()
	}
	close(w.expired)
}

// stop ends the watch and returns how the process spent its time. It returns
// only once no sample of the process is in flight, and it is safe to call more
// than once and from several goroutines.
//
// Wait reaps the process, and the host may then give its ID to another
// process. A sample taken between the reap and stop reads no process or
// another one. Either counts as running, which only arms a deadline that stop
// releases at once, and a Kill after Wait signals nothing.
func (w *processWatch) stop() processTiming {
	w.stopOnce.Do(func() { close(w.stopping) })
	<-w.done
	if w.running.IsZero() {
		return processTiming{held: w.ended.Sub(w.armed), heldPastBound: w.heldPastBound}
	}
	return processTiming{held: w.running.Sub(w.armed), ran: w.ended.Sub(w.running)}
}

// waitUnderDeadline waits for cmd, which Start has just started, under
// deadline, and stops the watch as soon as Wait returns. cancel cancels the
// context cmd was built with: a deadline that fires ends the process through
// it, with errProcessDeadline as the cause.
func waitUnderDeadline(cmd *exec.Cmd, deadline processDeadline, cancel context.CancelCauseFunc) (processTiming, error) {
	watch := deadline.watch(cmd.Process, func() { cancel(errProcessDeadline) })
	err := cmd.Wait()
	return watch.stop(), err
}
