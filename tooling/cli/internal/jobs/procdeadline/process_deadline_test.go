package procdeadline

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

// runningAtStart is a ProcessProgress under which a process counts as running
// as soon as exec.Cmd.Start returned, and its wait for a CPU is not measured.
func runningAtStart(*os.Process) func() ProcessSample { return nil }

// neverRunning is a ProcessProgress under which a process is never seen
// running, as if the host held it before its first instruction.
func neverRunning(*os.Process) func() ProcessSample {
	return func() ProcessSample { return ProcessSample{Held: true} }
}

// neverWaitingForCPU is a ProcessProgress under which a process runs at once
// and never waits for a CPU, as one blocked in its own work does.
func neverWaitingForCPU(*os.Process) func() ProcessSample {
	return func() ProcessSample { return ProcessSample{Measured: true} }
}

// alwaysWaitingForCPU is a ProcessProgress under which a process runs at once
// and then waits for a CPU all the time. Each sample reads a wait an hour
// longer than the last, which the deadline keeps to the time elapsed.
func alwaysWaitingForCPU(*os.Process) func() ProcessSample {
	var waited time.Duration
	return func() ProcessSample {
		waited += time.Hour
		return ProcessSample{WaitedForCPU: waited, Measured: true}
	}
}

// countedProgress wraps the sampler of progress so that *calls counts its
// samples. The watch's goroutine is the only one that samples, and stop
// returns after it ended, so the count can be read once stop returned.
func countedProgress(progress ProcessProgress, calls *int) ProcessProgress {
	return func(process *os.Process) func() ProcessSample {
		sample := progress(process)
		return func() ProcessSample {
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
	samples := make(chan chan ProcessSample)
	progress := func(*os.Process) func() ProcessSample {
		return func() ProcessSample {
			answer := make(chan ProcessSample)
			samples <- answer
			return <-answer
		}
	}
	expirations := 0
	watch := ProcessDeadline{Run: 0, Uncounted: elapsesNever, Progress: progress}.Watch(nil, func() { expirations++ })
	const heldSamples = 4
	for range heldSamples {
		answer := <-samples
		select {
		case <-watch.expired:
			t.Error("the deadline fired while the process was held")
		default:
		}
		answer <- ProcessSample{Held: true}
	}
	answer := <-samples
	answer <- ProcessSample{}
	<-watch.expired
	timing := watch.Stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if timing.UncountedPastBound {
		t.Error("timing reports the bound on time not counted for a process that ran")
	}
	// The first sample comes at once, the next ones 1, 2, 4 and 8 ms apart,
	// and a timer never fires early, so the fifth sample comes at least 15 ms
	// after the watch armed.
	if minimum := 15 * processProgressFirstPoll; timing.Held < minimum {
		t.Errorf("held = %s, want at least %s: the samples before the one that ran", timing.Held, minimum)
	}
	if timing.Ran < 0 {
		t.Errorf("ran = %s, want a duration from the first sample that ran", timing.Ran)
	}
}

// TestProcessDeadlineBoundsTheHold pins that a process never seen running is
// stopped at the bound on time not counted, and that the watch says so.
func TestProcessDeadlineBoundsTheHold(t *testing.T) {
	t.Parallel()
	expirations := 0
	watch := ProcessDeadline{Run: elapsesNever, Uncounted: 0, Progress: neverRunning}.Watch(nil, func() { expirations++ })
	<-watch.expired
	timing := watch.Stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if !timing.UncountedPastBound || timing.Ran != 0 || timing.WaitedForCPU != 0 {
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
	progress := func(*os.Process) func() ProcessSample {
		return func() ProcessSample {
			if stopped.Load() {
				t.Error("the watch sampled the process after stop returned")
			}
			if sampled.Add(1) == 2 {
				close(twice)
			}
			return ProcessSample{Held: true}
		}
	}
	watch := ProcessDeadline{Run: elapsesNever, Uncounted: elapsesNever, Progress: progress}.Watch(nil, func() {
		t.Error("a stopped watch fired its deadline")
	})
	<-twice
	var wg sync.WaitGroup
	timings := make([]ProcessTiming, 3)
	for i := range timings {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timings[i] = watch.Stop()
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
	if timings[0].UncountedPastBound || timings[0].Ran != 0 {
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
	watch := ProcessDeadline{Run: 0, Uncounted: elapsesNever, Progress: runningAtStart}.Watch(nil, nil)
	<-watch.expired
	if timing := watch.Stop(); timing.UncountedPastBound || timing.WaitMeasured {
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
	starved := func(process *os.Process) func() ProcessSample {
		sample := alwaysWaitingForCPU(process)
		return func() ProcessSample {
			calls++
			// The first call is the sample that sees the process run.
			if calls == 4 {
				close(reads)
			}
			return sample()
		}
	}
	deadline := ProcessDeadline{Run: processDeadlineMinRearm, Uncounted: elapsesNever, Progress: starved}
	watch := deadline.Watch(nil, func() { t.Error("the deadline of a process waiting for a CPU fired") })
	<-reads
	timing := watch.Stop()
	if timing.UncountedPastBound || !timing.WaitMeasured {
		t.Errorf("timing = %+v, want a measured wait under the deadline", timing)
	}
	if minimum := 3 * processDeadlineMinRearm; timing.WaitedForCPU < minimum {
		t.Errorf("waitedForCPU = %s, want at least %s: the time of three reads", timing.WaitedForCPU, minimum)
	}
	if timing.Ran < 0 {
		t.Errorf("ran = %s, want the time since the last read", timing.Ran)
	}
}

// TestProcessDeadlineCountsABlockedProcess pins that a process that runs and
// never waits for a CPU, such as one blocked in its own work, is stopped at
// its deadline, and that the wait of zero is measured.
func TestProcessDeadlineCountsABlockedProcess(t *testing.T) {
	t.Parallel()
	const run = 3 * processDeadlineMinRearm
	expirations := 0
	watch := ProcessDeadline{Run: run, Uncounted: elapsesNever, Progress: neverWaitingForCPU}.Watch(nil,
		func() { expirations++ })
	<-watch.expired
	timing := watch.Stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if timing.UncountedPastBound || !timing.WaitMeasured || timing.WaitedForCPU != 0 {
		t.Errorf("timing = %+v, want a run deadline with a measured wait of zero", timing)
	}
	if timing.Ran < run {
		t.Errorf("ran = %s, want at least the deadline %s", timing.Ran, run)
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
	progress := func(*os.Process) func() ProcessSample {
		return func() ProcessSample {
			calls++
			// The first call is the sample that sees the process run.
			return ProcessSample{WaitedForCPU: time.Duration(calls-1) * perRead, Measured: true}
		}
	}
	watch := ProcessDeadline{Run: run, Uncounted: elapsesNever, Progress: progress}.Watch(nil, nil)
	<-watch.expired
	timing := watch.Stop()
	reads := calls - 1
	if reads < 1 {
		t.Fatalf("the deadline fired after %d reads, want at least one before it", reads)
	}
	if want := time.Duration(reads) * perRead; timing.WaitedForCPU != want || !timing.WaitMeasured {
		t.Errorf("timing = %+v, want a measured wait of %s from %d reads", timing, want, reads)
	}
	if timing.UncountedPastBound || timing.Ran < run {
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
	cases := map[string]ProcessSample{
		"a failed read": {},
		"a lower read":  {Measured: true},
	}
	for name, next := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			progress := func(*os.Process) func() ProcessSample {
				return func() ProcessSample {
					calls++
					switch calls {
					case 1:
						// The sample that sees the process run.
						return ProcessSample{Measured: true}
					case 2:
						return ProcessSample{WaitedForCPU: time.Hour, Measured: true}
					default:
						return next
					}
				}
			}
			watch := ProcessDeadline{Run: run, Uncounted: elapsesNever, Progress: progress}.Watch(nil, nil)
			<-watch.expired
			timing := watch.Stop()
			if calls < 3 {
				t.Fatalf("the deadline fired after %d samples, want a read past the one that measured a wait", calls)
			}
			// The second sample comes at least run after the process ran, and
			// its wait is all of that time.
			if timing.WaitedForCPU < run || !timing.WaitMeasured {
				t.Errorf("timing = %+v, want the wait of at least %s read before %s", timing, run, name)
			}
			if timing.UncountedPastBound || timing.Ran < run {
				t.Errorf("timing = %+v, want a run deadline that ran at least %s", timing, run)
			}
		})
	}
}

// TestProcessDeadlineBoundsTheTimeNotCounted pins that a process that keeps
// waiting for a CPU is stopped once its hold and its wait reach the bound,
// and that the watch says so.
//
// The hold lasts until the watch's goroutine takes its first sample, so a host
// that pauses that goroutine for the whole bound makes the hold reach the
// bound alone. The deadline then fires before it reads any wait, as it must,
// so the test expects a measured wait only when the hold stayed under the
// bound.
func TestProcessDeadlineBoundsTheTimeNotCounted(t *testing.T) {
	t.Parallel()
	const bound = 3 * processDeadlineMinRearm
	expirations := 0
	watch := ProcessDeadline{Run: elapsesNever, Uncounted: bound, Progress: alwaysWaitingForCPU}.Watch(nil,
		func() { expirations++ })
	<-watch.expired
	timing := watch.Stop()
	if expirations != 1 {
		t.Errorf("onExpire ran %d times, want once", expirations)
	}
	if !timing.UncountedPastBound {
		t.Errorf("timing = %+v, want the bound %s on time not counted", timing, bound)
	}
	if timing.Held >= bound {
		if timing.WaitMeasured || timing.WaitedForCPU != 0 {
			t.Errorf("timing = %+v, want no wait read after a hold that reached the bound %s", timing, bound)
		}
		return
	}
	if !timing.WaitMeasured || timing.Held+timing.WaitedForCPU < bound {
		t.Errorf("timing = %+v, want a hold and a measured wait past the bound %s", timing, bound)
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
		progress ProcessProgress
		reads    func(calls int) bool
	}{
		"unread from the first reading": {
			progress: func(*os.Process) func() ProcessSample {
				return func() ProcessSample { return ProcessSample{WaitedForCPU: time.Hour} }
			},
			reads: func(calls int) bool { return calls == 1 },
		},
		"unread after the first reading": {
			progress: func(*os.Process) func() ProcessSample {
				first := true
				return func() ProcessSample {
					if first {
						first = false
						return ProcessSample{WaitedForCPU: time.Hour, Measured: true}
					}
					return ProcessSample{WaitedForCPU: 2 * time.Hour}
				}
			},
			reads: func(calls int) bool { return calls > 1 },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			deadline := ProcessDeadline{Run: run, Uncounted: elapsesNever, Progress: countedProgress(tc.progress, &calls)}
			watch := deadline.Watch(nil, nil)
			<-watch.expired
			timing := watch.Stop()
			if !tc.reads(calls) {
				t.Errorf("the process was sampled %d times", calls)
			}
			if timing.UncountedPastBound || timing.WaitMeasured || timing.WaitedForCPU != 0 || timing.Ran < run {
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
	measured := func(waited time.Duration) ProcessSample {
		return ProcessSample{WaitedForCPU: waited, Measured: true}
	}
	cases := []struct {
		name              string
		baseline, current ProcessSample
		want              time.Duration
		wantMeasured      bool
	}{
		{"a wait within the time elapsed", measured(time.Second), measured(4 * time.Second), 3 * time.Second, true},
		{"a wait that shrank", measured(4 * time.Second), measured(time.Second), 0, true},
		{"a wait longer than the time elapsed", measured(0), measured(time.Hour), 10 * time.Second, true},
		{"an unmeasured baseline", ProcessSample{}, measured(time.Second), 0, false},
		{"an unmeasured sample", measured(0), ProcessSample{WaitedForCPU: time.Second}, 0, false},
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
	deadline := ProcessDeadline{Run: 10 * time.Second, Uncounted: time.Minute}
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
		timing ProcessTiming
		want   string
	}{
		{
			"a measured wait",
			ProcessTiming{Held: 1200 * time.Millisecond, WaitedForCPU: 3 * time.Second, WaitMeasured: true, Ran: 5 * time.Second},
			"the host held it 1.2s before its first instruction and it waited 3s for a CPU, neither counted",
		},
		{
			"a wait not measured",
			ProcessTiming{Held: 1200 * time.Millisecond, Ran: 5 * time.Second},
			"the host held it 1.2s before its first instruction, which is not counted, and the time it waited " +
				"for a CPU could not be read, so it is counted",
		},
		{
			"a hold past the bound",
			ProcessTiming{Held: 2 * time.Minute, UncountedPastBound: true},
			"the host held it 2m0s without letting it run, or it blocked before it was first sampled",
		},
		{
			"a wait past the bound",
			ProcessTiming{Held: time.Second, WaitedForCPU: 119 * time.Second, WaitMeasured: true, UncountedPastBound: true},
			"the host held it 1s before its first instruction and it waited 1m59s for a CPU, neither counted",
		},
	}
	for _, tc := range cases {
		if got := tc.timing.Uncounted(); got != tc.want {
			t.Errorf("%s: uncounted() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
