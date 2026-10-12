package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// uncountedTimeBound is how long a started process may spend, in total, in the
// time its deadline does not count: the time the host holds it before its
// first instruction, then the time it is ready to run and waits for a CPU. It
// is not a deadline on the process's own work, which a processDeadline counts
// apart. It stops a process the host never lets run, and one that keeps
// running on a machine too loaded to give it its deadline's worth of CPU.
//
// It also applies, in place of the deadline, to a process that blocked before
// its progress was first sampled and that the baseline cannot prove ran: the
// baseline sample follows exec.Cmd.Start, and on a loaded machine a program
// can start and block in between. On macOS that is a process blocked inside
// dyld before it allocated any memory. Such a process is stopped at this bound
// instead of at its deadline.
//
// Only the watched process is read, never its children. A process that waits
// on a child is blocked in its own work, so that time counts against its
// deadline even when the child itself waits for a CPU.
const uncountedTimeBound = 2 * time.Minute

// processProgressFirstPoll and processProgressMaxPoll space the samples of a
// process not yet seen running: the first one comes at once, the second one
// 1 ms later, each next one twice as late, up to 25 ms apart. The deadline
// therefore starts to count at most about 25 ms after the process starts to
// run, plus however long this goroutine waits for a CPU on a loaded machine.
const (
	processProgressFirstPoll = time.Millisecond
	processProgressMaxPoll   = 25 * time.Millisecond
)

// processDeadlineMinRearm is the shortest time a deadline waits before it
// reads a running process again. A deadline reads the process once each time
// the process could have used the time it has left. Without this floor, a
// process that waits for a CPU most of the time would be read ever more often
// as that time runs out.
const processDeadlineMinRearm = 10 * time.Millisecond

// errProcessDeadline is the cause a processWatch cancels its process's
// context with when the deadline fires.
var errProcessDeadline = errors.New("process deadline expired")

// processSample is one reading of a started process. The zero value reads a
// process that runs and whose wait for a CPU is not measured, so a deadline
// counts every moment of it.
type processSample struct {
	// held reports that the process has not run its own code since the
	// baseline.
	held bool
	// waitedForCPU is how long the process has been ready to run with no CPU
	// to run on, in total since it started. Only the difference between two
	// samples means something, and only when both have measured set.
	waitedForCPU time.Duration
	// measured reports that waitedForCPU was read.
	measured bool
}

// processProgress prepares the sampling of a started process and returns the
// function that samples it. It may read the process once first, as the
// baseline that tells a held process. A nil function means every sample is
// the zero processSample. Any error reading the process reads as that zero
// value too, so a deadline that cannot read the process counts every moment
// from the failed read: it never waits for uncountedTimeBound, and it never
// lasts longer than the deadline of a process it can read.
type processProgress func(process *os.Process) (sample func() processSample)

// processDeadline bounds a started process. run counts the time the process
// runs its own code or is blocked in its own work, from the moment it is first
// seen running. It counts neither the time the host holds the new process
// before its first instruction, nor the time the process is then ready to run
// and waits for a CPU: on a loaded machine both say how busy the host is, not
// how slow the process is. uncounted bounds the sum of both.
type processDeadline struct {
	run       time.Duration
	uncounted time.Duration
	progress  processProgress
}

// hostProcessDeadline is the deadline the CLI gives a process it started: run
// counts what the process does on this host from its first instruction, less
// the time it waits for a CPU, and uncountedTimeBound bounds the rest.
func hostProcessDeadline(run time.Duration) processDeadline {
	return processDeadline{run: run, uncounted: uncountedTimeBound, progress: hostProcessProgress}
}

// check decides the deadline of a process the host held for held, seen running
// elapsed ago, that waited for a CPU for waited of elapsed. The deadline counts
// the rest of elapsed. It is over when that rest reached run, and over past
// the bound when held and waited together reached uncounted. Otherwise next is
// how long the process must go on before either can happen, and never less
// than processDeadlineMinRearm.
func (d processDeadline) check(held, elapsed, waited time.Duration) (next time.Duration, over, pastBound bool) {
	counted, uncounted := elapsed-waited, held+waited
	switch {
	case counted >= d.run:
		return 0, true, false
	case uncounted >= d.uncounted:
		return 0, true, true
	}
	return max(min(d.run-counted, d.uncounted-uncounted), processDeadlineMinRearm), false, false
}

// watch arms the deadline of process, which exec.Cmd.Start has just started.
// It prepares the sampling of the process before it returns. onExpire, when
// not nil, runs once when the deadline fires, before expired is closed. The
// caller calls stop as soon as Wait returns.
func (d processDeadline) watch(process *os.Process, onExpire func()) *processWatch {
	w := &processWatch{
		armed:    time.Now(),
		expired:  make(chan struct{}),
		stopping: make(chan struct{}),
		done:     make(chan struct{}),
	}
	sample := d.progress(process)
	go w.follow(d, sample, onExpire)
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
	done               chan struct{}
	running            time.Time
	ended              time.Time
	waitedForCPU       time.Duration
	waitMeasured       bool
	uncountedPastBound bool
}

// processTiming is how a started process spent the time its deadline watched.
type processTiming struct {
	// held is how long the process was not seen running: until its first
	// sample that did not read it held, or until the watch ended when none
	// did. It is not counted.
	held time.Duration
	// waitedForCPU is how long the process, once seen running, was ready to
	// run and had no CPU: the most its deadline read. It is not counted. It
	// is zero unless waitMeasured is set.
	waitedForCPU time.Duration
	// waitMeasured reports that the deadline read waitedForCPU at least once
	// after the baseline. Without it, the time the process waited for a CPU
	// is part of ran.
	waitMeasured bool
	// ran is how long the process ran or was blocked in its own work under
	// its deadline: the time since it was seen running, less waitedForCPU.
	ran time.Duration
	// uncountedPastBound reports that the deadline fired because held and
	// waitedForCPU together reached the bound on uncounted time.
	uncountedPastBound bool
}

// uncounted states, for a timeout message, the time the deadline did not
// count: the hold, then the wait for a CPU when the deadline could read it.
// "it" is the process.
func (t processTiming) uncounted() string {
	held := t.held.Round(time.Millisecond)
	switch {
	case t.uncountedPastBound && t.waitedForCPU == 0:
		return fmt.Sprintf("the host held it %s without letting it run, or it blocked before it was first sampled", held)
	case !t.waitMeasured:
		return fmt.Sprintf("the host held it %s before its first instruction, which is not counted, and the time it "+
			"waited for a CPU could not be read, so it is counted", held)
	}
	return fmt.Sprintf("the host held it %s before its first instruction and it waited %s for a CPU, neither counted",
		held, t.waitedForCPU.Round(time.Millisecond))
}

// follow waits until the process is seen running, then reads it each time it
// could have used up its run deadline or its bound on uncounted time, until
// one of them is over.
func (w *processWatch) follow(d processDeadline, sample func() processSample, onExpire func()) {
	defer func() {
		w.ended = time.Now()
		close(w.done)
	}()
	var baseline processSample
	if sample == nil {
		w.running = time.Now()
	} else {
		var running bool
		if baseline, running = w.awaitRunning(sample, d.uncounted); !running {
			if w.uncountedPastBound {
				w.expire(onExpire)
			}
			return
		}
	}
	held := w.running.Sub(w.armed)
	var elapsed time.Duration
	for {
		next, over, pastBound := d.check(held, elapsed, w.waitedForCPU)
		if over {
			w.uncountedPastBound = pastBound
			w.expire(onExpire)
			return
		}
		timer := time.NewTimer(next)
		select {
		case <-w.stopping:
			timer.Stop()
			return
		case <-timer.C:
		}
		// The sample comes before the clock is read, so the wait it reports
		// is never longer than the time it is taken from.
		var current processSample
		if baseline.measured {
			current = sample()
		}
		elapsed = time.Since(w.running)
		// The wait only grows. A read that fails, or that a thread's exit
		// lowered, keeps the wait already read, so it counts the time after
		// that read and never the time already discounted.
		if wait, measured := cpuWaitBetween(baseline, current, elapsed); measured {
			w.waitedForCPU = max(w.waitedForCPU, wait)
			w.waitMeasured = true
		}
	}
}

// awaitRunning samples the process until one sample sees it running, and
// returns that sample: the baseline of the time the process waits for a CPU.
// It records as w.running the moment before that sample was taken, so the
// baseline misses no wait of the time the deadline counts from. It returns
// false when stop is called first, or when bound elapses first, which it
// records.
func (w *processWatch) awaitRunning(sample func() processSample, bound time.Duration) (processSample, bool) {
	limit := time.NewTimer(bound)
	defer limit.Stop()
	poll := time.NewTimer(0)
	defer poll.Stop()
	interval := processProgressFirstPoll
	for {
		select {
		case <-w.stopping:
			return processSample{}, false
		case <-limit.C:
			w.uncountedPastBound = true
			return processSample{}, false
		case <-poll.C:
			at := time.Now()
			if current := sample(); !current.held {
				w.running = at
				return current, true
			}
			poll.Reset(interval)
			interval = min(2*interval, processProgressMaxPoll)
		}
	}
}

// cpuWaitBetween returns how long of elapsed the process waited for a CPU
// between two samples, and whether both measured it. An unmeasured wait is no
// wait, so the deadline counts all of elapsed.
//
// The difference is kept between zero and elapsed, because a sample can read
// anything. A sample taken after the process ended reads no process, or
// another one that took its ID. The wait of a process is the sum of the waits
// of its threads: it shrinks when a thread exits, and it grows faster than the
// clock while several threads wait at once.
func cpuWaitBetween(baseline, current processSample, elapsed time.Duration) (time.Duration, bool) {
	if !baseline.measured || !current.measured {
		return 0, false
	}
	return min(max(current.waitedForCPU-baseline.waitedForCPU, 0), elapsed), true
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
// another one. No process reads as running with an unmeasured wait, and
// another one reads as anything, which cpuWaitBetween keeps between no wait
// and a wait for the whole time. Either way the deadline at most fires for a
// process that already ended, or is read again, which stop releases at once.
// A deadline that fires after Wait changes nothing: Wait already returned how
// the process ended, and a Kill after Wait signals nothing.
func (w *processWatch) stop() processTiming {
	w.stopOnce.Do(func() { close(w.stopping) })
	<-w.done
	if w.running.IsZero() {
		return processTiming{held: w.ended.Sub(w.armed), uncountedPastBound: w.uncountedPastBound}
	}
	return processTiming{
		held:               w.running.Sub(w.armed),
		waitedForCPU:       w.waitedForCPU,
		waitMeasured:       w.waitMeasured,
		ran:                w.ended.Sub(w.running) - w.waitedForCPU,
		uncountedPastBound: w.uncountedPastBound,
	}
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
