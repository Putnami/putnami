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
// as soon as exec.Cmd.Start returned, and its wait for a CPU is not measured.
func runningAtStart(*os.Process) func() processSample { return nil }

// neverRunning is a processProgress under which a process is never seen
// running, as if the host held it before its first instruction.
func neverRunning(*os.Process) func() processSample {
	return func() processSample { return processSample{held: true} }
}

// neverWaitingForCPU is a processProgress under which a process runs at once
// and never waits for a CPU, as one blocked in its own work does.
func neverWaitingForCPU(*os.Process) func() processSample {
	return func() processSample { return processSample{measured: true} }
}

// alwaysWaitingForCPU is a processProgress under which a process runs at once
// and then waits for a CPU all the time. Each sample reads a wait an hour
// longer than the last, which the deadline keeps to the time elapsed.
func alwaysWaitingForCPU(*os.Process) func() processSample {
	var waited time.Duration
	return func() processSample {
		waited += time.Hour
		return processSample{waitedForCPU: waited, measured: true}
	}
}

// countedProgress wraps the sampler of progress so that *calls counts its
// samples. The watch's goroutine is the only one that samples, and stop
// returns after it ended, so the count can be read once stop returned.
func countedProgress(progress processProgress, calls *int) processProgress {
	return func(process *os.Process) func() processSample {
		sample := progress(process)
		return func() processSample {
			*calls++
			return sample()
		}
	}
}

// TestProcessDeadlineArmsOnceTheProcessIsSeenRunning pins that the deadline
// counts nothing while the process is held. The deadline has elapsed by the
// time it arms, so it fires at once when it arms: it must not fire while the
// samples say the process is held, and must fire once one says it runs.
// The test answers each sample itself, so no case depends on how fast the
// host runs.
func TestProcessDeadlineArmsOnceTheProcessIsSeenRunning(t *testing.T) {
	t.Parallel()
	samples := make(chan chan processSample)
	progress := func(*os.Process) func() processSample {
		return func() processSample {
			answer := make(chan processSample)
			samples <- answer
			return <-answer
		}
	}
	expirations := 0
	watch := processDeadline{run: 0, uncounted: elapsesNever, progress: progress}.watch(nil, func() { expirations++ })
	const heldSamples = 4
	for range heldSamples {
		answer := <-samples
		select {
		case <-watch.expired:
			t.Error("the deadline fired while the process was held")
		default:
		}
		answer <- processSample{held: true}
	}
	answer := <-samples
	answer <- processSample{}
	<-watch.expired
	timing := watch.stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if timing.uncountedPastBound {
		t.Error("timing reports the bound on time not counted for a process that ran")
	}
	// The first sample comes at once, the next ones 1, 2, 4 and 8 ms apart,
	// and a timer never fires early, so the fifth sample comes at least 15 ms
	// after the watch armed.
	if minimum := 15 * processProgressFirstPoll; timing.held < minimum {
		t.Errorf("held = %s, want at least %s: the samples before the one that ran", timing.held, minimum)
	}
	if timing.ran < 0 {
		t.Errorf("ran = %s, want a duration from the first sample that ran", timing.ran)
	}
}

// TestProcessDeadlineBoundsTheHold pins that a process never seen running is
// stopped at the bound on time not counted, and that the watch says so.
func TestProcessDeadlineBoundsTheHold(t *testing.T) {
	t.Parallel()
	expirations := 0
	watch := processDeadline{run: elapsesNever, uncounted: 0, progress: neverRunning}.watch(nil, func() { expirations++ })
	<-watch.expired
	timing := watch.stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if !timing.uncountedPastBound || timing.ran != 0 || timing.waitedForCPU != 0 {
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
	progress := func(*os.Process) func() processSample {
		return func() processSample {
			if stopped.Load() {
				t.Error("the watch sampled the process after stop returned")
			}
			if sampled.Add(1) == 2 {
				close(twice)
			}
			return processSample{held: true}
		}
	}
	watch := processDeadline{run: elapsesNever, uncounted: elapsesNever, progress: progress}.watch(nil, func() {
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
	if timings[0].uncountedPastBound || timings[0].ran != 0 {
		t.Errorf("timing = %+v, want a hold that ended at stop", timings[0])
	}
	select {
	case <-watch.expired:
		t.Error("a stopped watch closed expired")
	default:
	}
}

// TestProcessDeadlineCountsAProcessRunningAtStartAtOnce pins the deadline of a
// platform that does not hold a started process and does not measure its wait
// for a CPU: it counts from the watch, all of the time.
func TestProcessDeadlineCountsAProcessRunningAtStartAtOnce(t *testing.T) {
	t.Parallel()
	watch := processDeadline{run: 0, uncounted: elapsesNever, progress: runningAtStart}.watch(nil, nil)
	<-watch.expired
	if timing := watch.stop(); timing.uncountedPastBound || timing.waitMeasured {
		t.Errorf("timing = %+v, want a run deadline and no measured wait", timing)
	}
}

// TestProcessDeadlineDoesNotCountTheWaitForACPU pins that a process waiting
// for a CPU all the time does not use its deadline. The deadline reads it
// every processDeadlineMinRearm, so three reads after the first are at least
// three times the deadline after it ran: a deadline that counted the wait
// would have fired by then.
func TestProcessDeadlineDoesNotCountTheWaitForACPU(t *testing.T) {
	t.Parallel()
	calls := 0
	reads := make(chan struct{})
	starved := func(process *os.Process) func() processSample {
		sample := alwaysWaitingForCPU(process)
		return func() processSample {
			calls++
			// The first call is the sample that sees the process run.
			if calls == 4 {
				close(reads)
			}
			return sample()
		}
	}
	deadline := processDeadline{run: processDeadlineMinRearm, uncounted: elapsesNever, progress: starved}
	watch := deadline.watch(nil, func() { t.Error("the deadline of a process waiting for a CPU fired") })
	<-reads
	timing := watch.stop()
	if timing.uncountedPastBound || !timing.waitMeasured {
		t.Errorf("timing = %+v, want a measured wait under the deadline", timing)
	}
	if minimum := 3 * processDeadlineMinRearm; timing.waitedForCPU < minimum {
		t.Errorf("waitedForCPU = %s, want at least %s: the time of three reads", timing.waitedForCPU, minimum)
	}
	if timing.ran < 0 {
		t.Errorf("ran = %s, want the time since the last read", timing.ran)
	}
}

// TestProcessDeadlineCountsABlockedProcess pins that a process that runs and
// never waits for a CPU, such as one blocked in its own work, is stopped at
// its deadline, and that the wait of zero is measured.
func TestProcessDeadlineCountsABlockedProcess(t *testing.T) {
	t.Parallel()
	const run = 3 * processDeadlineMinRearm
	expirations := 0
	watch := processDeadline{run: run, uncounted: elapsesNever, progress: neverWaitingForCPU}.watch(nil,
		func() { expirations++ })
	<-watch.expired
	timing := watch.stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if timing.uncountedPastBound || !timing.waitMeasured || timing.waitedForCPU != 0 {
		t.Errorf("timing = %+v, want a run deadline with a measured wait of zero", timing)
	}
	if timing.ran < run {
		t.Errorf("ran = %s, want at least the deadline %s", timing.ran, run)
	}
}

// TestProcessDeadlineCountsTheTimeAProcessDoesNotWait pins a process that
// waits for a CPU part of the time: each read finds it waited half the
// shortest time between two reads, so no read finds a wait longer than the
// time elapsed. The deadline fires once the time it did not wait reaches the
// deadline, and the wait it reports is the one it read.
func TestProcessDeadlineCountsTheTimeAProcessDoesNotWait(t *testing.T) {
	t.Parallel()
	const (
		run     = 3 * processDeadlineMinRearm
		perRead = processDeadlineMinRearm / 2
	)
	calls := 0
	progress := func(*os.Process) func() processSample {
		return func() processSample {
			calls++
			// The first call is the sample that sees the process run.
			return processSample{waitedForCPU: time.Duration(calls-1) * perRead, measured: true}
		}
	}
	watch := processDeadline{run: run, uncounted: elapsesNever, progress: progress}.watch(nil, nil)
	<-watch.expired
	timing := watch.stop()
	reads := calls - 1
	if reads < 1 {
		t.Fatalf("the deadline fired after %d reads, want at least one before it", reads)
	}
	if want := time.Duration(reads) * perRead; timing.waitedForCPU != want || !timing.waitMeasured {
		t.Errorf("timing = %+v, want a measured wait of %s from %d reads", timing, want, reads)
	}
	if timing.uncountedPastBound || timing.ran < run {
		t.Errorf("timing = %+v, want a run deadline that ran at least %s", timing, run)
	}
}

// TestProcessDeadlineKeepsTheWaitAlreadyRead pins that a read that fails, or
// that reads a lower wait, as a thread's exit does on Linux, does not throw
// away the wait already read: the deadline keeps it and counts only the time
// after it.
//
// The second sample reads a wait longer than any test, which the deadline
// keeps to the time elapsed, so it counts nothing at that read and always
// reads again, however late the host runs the watch. A wait the test measured
// on its own clock would miss a pause of the watch between the two clocks, and
// let the deadline fire at the read that measured it. The deadline then fires
// at the next read, so each kind of read gets its own case.
func TestProcessDeadlineKeepsTheWaitAlreadyRead(t *testing.T) {
	t.Parallel()
	const run = 3 * processDeadlineMinRearm
	cases := map[string]processSample{
		"a failed read": {},
		"a lower read":  {measured: true},
	}
	for name, next := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			progress := func(*os.Process) func() processSample {
				return func() processSample {
					calls++
					switch calls {
					case 1:
						// The sample that sees the process run.
						return processSample{measured: true}
					case 2:
						return processSample{waitedForCPU: time.Hour, measured: true}
					default:
						return next
					}
				}
			}
			watch := processDeadline{run: run, uncounted: elapsesNever, progress: progress}.watch(nil, nil)
			<-watch.expired
			timing := watch.stop()
			if calls < 3 {
				t.Fatalf("the deadline fired after %d samples, want a read past the one that measured a wait", calls)
			}
			// The second sample comes at least run after the process ran, and
			// its wait is all of that time.
			if timing.waitedForCPU < run || !timing.waitMeasured {
				t.Errorf("timing = %+v, want the wait of at least %s read before %s", timing, run, name)
			}
			if timing.uncountedPastBound || timing.ran < run {
				t.Errorf("timing = %+v, want a run deadline that ran at least %s", timing, run)
			}
		})
	}
}

// TestProcessDeadlineBoundsTheTimeNotCounted pins that a process that keeps
// waiting for a CPU is stopped once its hold and its wait reach the bound,
// and that the watch says so.
func TestProcessDeadlineBoundsTheTimeNotCounted(t *testing.T) {
	t.Parallel()
	const bound = 3 * processDeadlineMinRearm
	expirations := 0
	watch := processDeadline{run: elapsesNever, uncounted: bound, progress: alwaysWaitingForCPU}.watch(nil,
		func() { expirations++ })
	<-watch.expired
	timing := watch.stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if !timing.uncountedPastBound || !timing.waitMeasured || timing.held+timing.waitedForCPU < bound {
		t.Errorf("timing = %+v, want a hold and a wait past the bound %s", timing, bound)
	}
}

// TestProcessDeadlineCountsAnUnreadWaitInFull pins the fail-safe direction: a
// wait the deadline cannot read is no wait, so the deadline lasts no longer
// than for a process that never waits. A sampler whose first reading is not
// measured is not read again.
func TestProcessDeadlineCountsAnUnreadWaitInFull(t *testing.T) {
	t.Parallel()
	const run = 3 * processDeadlineMinRearm
	cases := map[string]struct {
		progress processProgress
		reads    func(calls int) bool
	}{
		"unread from the first reading": {
			progress: func(*os.Process) func() processSample {
				return func() processSample { return processSample{waitedForCPU: time.Hour} }
			},
			reads: func(calls int) bool { return calls == 1 },
		},
		"unread after the first reading": {
			progress: func(*os.Process) func() processSample {
				first := true
				return func() processSample {
					if first {
						first = false
						return processSample{waitedForCPU: time.Hour, measured: true}
					}
					return processSample{waitedForCPU: 2 * time.Hour}
				}
			},
			reads: func(calls int) bool { return calls > 1 },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			deadline := processDeadline{run: run, uncounted: elapsesNever, progress: countedProgress(tc.progress, &calls)}
			watch := deadline.watch(nil, nil)
			<-watch.expired
			timing := watch.stop()
			if !tc.reads(calls) {
				t.Errorf("the process was sampled %d times", calls)
			}
			if timing.uncountedPastBound || timing.waitMeasured || timing.waitedForCPU != 0 || timing.ran < run {
				t.Errorf("timing = %+v, want a run deadline that counted all of at least %s", timing, run)
			}
		})
	}
}

// TestCPUWaitBetweenKeepsTheWaitWithinTheTimeElapsed pins how two samples
// become a wait: only two measured samples give one, and it is never less
// than zero nor more than the time elapsed between them.
func TestCPUWaitBetweenKeepsTheWaitWithinTheTimeElapsed(t *testing.T) {
	t.Parallel()
	measured := func(waited time.Duration) processSample {
		return processSample{waitedForCPU: waited, measured: true}
	}
	cases := []struct {
		name              string
		baseline, current processSample
		want              time.Duration
		wantMeasured      bool
	}{
		{"a wait within the time elapsed", measured(time.Second), measured(4 * time.Second), 3 * time.Second, true},
		{"a wait that shrank", measured(4 * time.Second), measured(time.Second), 0, true},
		{"a wait longer than the time elapsed", measured(0), measured(time.Hour), 10 * time.Second, true},
		{"an unmeasured baseline", processSample{}, measured(time.Second), 0, false},
		{"an unmeasured sample", measured(0), processSample{waitedForCPU: time.Second}, 0, false},
	}
	for _, tc := range cases {
		got, gotMeasured := cpuWaitBetween(tc.baseline, tc.current, 10*time.Second)
		if got != tc.want || gotMeasured != tc.wantMeasured {
			t.Errorf("%s: cpuWaitBetween = %s, %t; want %s, %t", tc.name, got, gotMeasured, tc.want, tc.wantMeasured)
		}
	}
}

// TestProcessDeadlineCheckDecidesFromTheTimeCounted pins the decision a
// deadline takes at each read.
func TestProcessDeadlineCheckDecidesFromTheTimeCounted(t *testing.T) {
	t.Parallel()
	deadline := processDeadline{run: 10 * time.Second, uncounted: time.Minute}
	cases := []struct {
		name                  string
		held, elapsed, waited time.Duration
		wantNext              time.Duration
		wantOver, wantBound   bool
	}{
		{"nothing counted yet", 0, 0, 0, 10 * time.Second, false, false},
		{"part of the deadline counted", time.Second, 4 * time.Second, time.Second, 7 * time.Second, false, false},
		{"the deadline counted", time.Second, 12 * time.Second, 2 * time.Second, 0, true, false},
		{"a wait that keeps the deadline open", 0, 30 * time.Second, 25 * time.Second, 5 * time.Second, false, false},
		{"the bound nearer than the deadline", 50 * time.Second, 9 * time.Second, 8 * time.Second, 2 * time.Second, false, false},
		{"the bound reached", 30 * time.Second, 39 * time.Second, 30 * time.Second, 0, true, true},
		{"the deadline and the bound reached", 55 * time.Second, 20 * time.Second, 5 * time.Second, 0, true, false},
		{"a read due sooner than the floor", 0, 10*time.Second - time.Millisecond, 0, processDeadlineMinRearm, false, false},
	}
	for _, tc := range cases {
		next, over, pastBound := deadline.check(tc.held, tc.elapsed, tc.waited)
		if next != tc.wantNext || over != tc.wantOver || pastBound != tc.wantBound {
			t.Errorf("%s: check = %s, %t, %t; want %s, %t, %t",
				tc.name, next, over, pastBound, tc.wantNext, tc.wantOver, tc.wantBound)
		}
	}
}

// TestProcessTimingStatesTheTimeNotCounted pins the clause both timeout
// messages state the time their deadline did not count with.
func TestProcessTimingStatesTheTimeNotCounted(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		timing processTiming
		want   string
	}{
		{
			"a measured wait",
			processTiming{held: 1200 * time.Millisecond, waitedForCPU: 3 * time.Second, waitMeasured: true, ran: 5 * time.Second},
			"the host held it 1.2s before its first instruction and it waited 3s for a CPU, neither counted",
		},
		{
			"a wait not measured",
			processTiming{held: 1200 * time.Millisecond, ran: 5 * time.Second},
			"the host held it 1.2s before its first instruction, which is not counted, and the time it waited " +
				"for a CPU could not be read, so it is counted",
		},
		{
			"a hold past the bound",
			processTiming{held: 2 * time.Minute, uncountedPastBound: true},
			"the host held it 2m0s without letting it run, or it blocked before it was first sampled",
		},
		{
			"a wait past the bound",
			processTiming{held: time.Second, waitedForCPU: 119 * time.Second, waitMeasured: true, uncountedPastBound: true},
			"the host held it 1s before its first instruction and it waited 1m59s for a CPU, neither counted",
		},
	}
	for _, tc := range cases {
		if got := tc.timing.uncounted(); got != tc.want {
			t.Errorf("%s: uncounted() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
