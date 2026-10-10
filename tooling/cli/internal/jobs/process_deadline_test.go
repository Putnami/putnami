package jobs

import (
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// elapsesNever is a deadline or bound no test outlives.
const elapsesNever = time.Duration(math.MaxInt64)

// runningAtStart is a processProgress under which a process counts as running
// as soon as exec.Cmd.Start returned.
func runningAtStart(*os.Process) func() bool { return nil }

// neverRunning is a processProgress under which a process is never seen
// running, as if the host held it before its first instruction.
func neverRunning(*os.Process) func() bool { return func() bool { return false } }

// TestProcessDeadlineArmsOnceTheProcessIsSeenRunning pins that the deadline
// counts nothing while the process is held. The deadline has elapsed by the
// time it arms, so it fires at once when it arms: it must not fire while the
// samples say the process has not run, and must fire once one says it has.
// The test answers each sample itself, so no case depends on how fast the
// host runs.
func TestProcessDeadlineArmsOnceTheProcessIsSeenRunning(t *testing.T) {
	t.Parallel()
	samples := make(chan chan bool)
	progress := func(*os.Process) func() bool {
		return func() bool {
			answer := make(chan bool)
			samples <- answer
			return <-answer
		}
	}
	expirations := 0
	watch := processDeadline{run: 0, admission: elapsesNever, progress: progress}.watch(nil, func() { expirations++ })
	const heldSamples = 4
	for range heldSamples {
		answer := <-samples
		select {
		case <-watch.expired:
			t.Error("the deadline fired while the process was held")
		default:
		}
		answer <- false
	}
	answer := <-samples
	answer <- true
	<-watch.expired
	timing := watch.stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if timing.heldPastBound {
		t.Error("timing reports a hold past the bound for a process that ran")
	}
	// Samples are 1, 2, 4, 8 and 16 ms apart, and a timer never fires early,
	// so the fifth sample comes at least 31 ms after the watch armed.
	if minimum := 31 * processProgressFirstPoll; timing.held < minimum {
		t.Errorf("held = %s, want at least %s: the samples before the one that moved", timing.held, minimum)
	}
	if timing.ran < 0 {
		t.Errorf("ran = %s, want a duration from the first sample that moved", timing.ran)
	}
}

// TestProcessDeadlineBoundsTheHold pins that a process never seen running is
// stopped at the admission bound, and that the watch says so.
func TestProcessDeadlineBoundsTheHold(t *testing.T) {
	t.Parallel()
	expirations := 0
	watch := processDeadline{run: elapsesNever, admission: 0, progress: neverRunning}.watch(nil, func() { expirations++ })
	<-watch.expired
	timing := watch.stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if !timing.heldPastBound || timing.ran != 0 {
		t.Errorf("timing = %+v, want a hold past the bound and no run", timing)
	}
}

// TestProcessDeadlineStopEndsTheWatch pins that stop ends the sampling for
// good, may be called again and from several goroutines, and fires nothing.
func TestProcessDeadlineStopEndsTheWatch(t *testing.T) {
	t.Parallel()
	var sampled atomic.Int32
	var stopped atomic.Bool
	twice := make(chan struct{})
	progress := func(*os.Process) func() bool {
		return func() bool {
			if stopped.Load() {
				t.Error("the watch sampled the process after stop returned")
			}
			if sampled.Add(1) == 2 {
				close(twice)
			}
			return false
		}
	}
	watch := processDeadline{run: elapsesNever, admission: elapsesNever, progress: progress}.watch(nil, func() {
		t.Error("a stopped watch fired its deadline")
	})
	<-twice
	var wg sync.WaitGroup
	timings := make([]processTiming, 3)
	for i := range timings {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timings[i] = watch.stop()
		}()
	}
	wg.Wait()
	stopped.Store(true)
	// Several sample intervals: a watch still sampling would sample here.
	time.Sleep(4 * processProgressMaxPoll)
	for _, timing := range timings[1:] {
		if timing != timings[0] {
			t.Errorf("stop returned %+v and %+v, want one timing", timings[0], timing)
		}
	}
	if timings[0].heldPastBound || timings[0].ran != 0 {
		t.Errorf("timing = %+v, want a hold that ended at stop", timings[0])
	}
	select {
	case <-watch.expired:
		t.Error("a stopped watch closed expired")
	default:
	}
}

// TestProcessDeadlineCountsAProcessRunningAtStartAtOnce pins the deadline of a
// platform that does not hold a started process: it counts from the watch.
func TestProcessDeadlineCountsAProcessRunningAtStartAtOnce(t *testing.T) {
	t.Parallel()
	watch := processDeadline{run: 0, admission: elapsesNever, progress: runningAtStart}.watch(nil, nil)
	<-watch.expired
	if timing := watch.stop(); timing.heldPastBound {
		t.Errorf("timing = %+v, want a run deadline", timing)
	}
}
