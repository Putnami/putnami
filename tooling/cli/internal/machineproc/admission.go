package machineproc

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
)

// admissionProtocol is the first supervisor protocol that grants CPU to the
// runs of its workers (CLI ADR 0116 §5).
const admissionProtocol = 2

// AcquireCPU waits until the supervisor grants cpuMilli thousandths of a CPU
// to the run this worker serves, and answers the function that returns the
// grant and the run's share of the machine when it was granted, the most a
// subprocess of the run is granted, in thousandths of a CPU. capacityMilli is
// the CPU capacity the run's scheduler admits against. Outside a worker, or
// under a supervisor that grants no CPU, it grants at once with no share
// (zero). It answers false, holding nothing, when ctx ends first.
func AcquireCPU(ctx context.Context, cpuMilli, capacityMilli int) (func(), int, bool) {
	a := currentAdmitter.Load()
	if a == nil || cpuMilli <= 0 {
		return func() {}, 0, true
	}
	return a.acquire(ctx, cpuMilli, capacityMilli)
}

// asksForCPU reports whether the worker asks the supervisor for the CPU of
// the run req forwards: only when the client's setting is a fair share and the
// supervisor speaks admissionProtocol. Otherwise the run admits on its own and
// makes no round trip for its CPU.
func asksForCPU(req request) bool {
	return req.SupervisorProtocol >= admissionProtocol && req.CPUShare == CPUShareFair
}

// forwarded stamps req, a run a supervisor forwards to its worker, with the
// supervisor's protocol.
func forwardedRun(req request) request {
	req.SupervisorProtocol = ProtocolVersion
	return req
}

// currentAdmitter asks the supervisor for the run a worker serves, and is nil
// between runs and in every other process.
var currentAdmitter atomic.Pointer[cpuAdmitter]

// cpuAdmitter is the worker's end of the supervisor's CPU admission: it sends
// acquire and release over the control channel and waits for the grants the
// supervisor sends back.
type cpuAdmitter struct {
	send func(control) error

	mu      sync.Mutex
	next    uint64
	waiting map[uint64]chan int
	// closed says the control channel ended: every wait is granted, since a
	// run never depends on the machine process.
	closed bool
}

func newCPUAdmitter(send func(control) error) *cpuAdmitter {
	return &cpuAdmitter{send: send, waiting: map[uint64]chan int{}}
}

func (a *cpuAdmitter) acquire(ctx context.Context, cpuMilli, capacityMilli int) (func(), int, bool) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return func() {}, 0, true
	}
	a.next++
	id := a.next
	granted := make(chan int, 1)
	a.waiting[id] = granted
	a.mu.Unlock()
	if err := a.send(control{Op: "acquire", ID: id, CPUMilli: cpuMilli, CapacityMilli: capacityMilli}); err != nil {
		a.forget(id)
		return func() {}, 0, true
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = a.send(control{Op: "release", ID: id}) }) }
	select {
	case share := <-granted:
		return release, share, true
	case <-ctx.Done():
	}
	// The release withdraws a waiting request or returns a grant that came
	// meanwhile; the supervisor accepts either.
	a.forget(id)
	release()
	return nil, 0, false
}

func (a *cpuAdmitter) forget(id uint64) {
	a.mu.Lock()
	delete(a.waiting, id)
	a.mu.Unlock()
}

// granted ends the wait of request id, granted with the run's share.
func (a *cpuAdmitter) granted(id uint64, shareMilli int) {
	a.mu.Lock()
	ch, ok := a.waiting[id]
	delete(a.waiting, id)
	a.mu.Unlock()
	if ok {
		ch <- shareMilli
	}
}

// close grants every wait: the supervisor is gone.
func (a *cpuAdmitter) close() {
	a.mu.Lock()
	a.closed = true
	waiting := a.waiting
	a.waiting = map[uint64]chan int{}
	a.mu.Unlock()
	for _, ch := range waiting {
		ch <- 0
	}
}

// cpuLedger is the supervisor's CPU admission across the concurrent runs of
// its workers, in thousandths of a CPU (CLI ADR 0116 §5). A run alone is
// granted every request at once. With several runs that hold or wait, each
// run's share is the capacity divided by their number, and a request is
// granted, in the order its run sent them, when its run holds nothing, or when
// CPU is free and its run holds less than its share, or when CPU is free and
// no other run below its share waits. A grant carries the run's share at that
// moment, which caps the CPU a subprocess of the run is given.
type cpuLedger struct {
	mu       sync.Mutex
	capacity map[string]int
	used     map[string]int
	held     map[string]map[uint64]int
	waiters  []cpuWaiter
	// counts is what each run was granted, and how many of its grants had to
	// wait for another run.
	counts map[string]cpuCounts
}

// cpuCounts is what a run asked of the ledger.
type cpuCounts struct {
	Granted, Waited int
}

type cpuWaiter struct {
	run   string
	id    uint64
	milli int
	grant func(shareMilli int)
}

func newCPULedger() *cpuLedger {
	return &cpuLedger{capacity: map[string]int{}, used: map[string]int{}, held: map[string]map[uint64]int{}, counts: map[string]cpuCounts{}}
}

// acquire queues request id of run, and calls grant with the run's share once
// it is granted.
func (l *cpuLedger) acquire(run string, id uint64, milli, capacityMilli int, grant func(shareMilli int)) {
	l.mu.Lock()
	if capacityMilli > 0 {
		l.capacity[run] = capacityMilli
	}
	l.waiters = append(l.waiters, cpuWaiter{run: run, id: id, milli: max(milli, 0), grant: grant})
	grants := l.grantLocked()
	if !l.holdsLocked(run, id) {
		c := l.counts[run]
		c.Waited++
		l.counts[run] = c
	}
	l.mu.Unlock()
	call(grants)
}

// release returns request id of run, whether it was granted or still waits.
func (l *cpuLedger) release(run string, id uint64) {
	l.mu.Lock()
	if milli, ok := l.held[run][id]; ok {
		delete(l.held[run], id)
		l.used[run] -= milli
	} else {
		for i, w := range l.waiters {
			if w.run == run && w.id == id {
				l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
				break
			}
		}
	}
	l.forgetIdleLocked(run)
	grants := l.grantLocked()
	l.mu.Unlock()
	call(grants)
}

func (l *cpuLedger) holdsLocked(run string, id uint64) bool {
	_, ok := l.held[run][id]
	return ok
}

// end drops whatever run holds or waits for: its run or its worker ended.
// It answers what the run was granted.
func (l *cpuLedger) end(run string) cpuCounts {
	l.mu.Lock()
	counts := l.counts[run]
	delete(l.counts, run)
	kept := l.waiters[:0]
	for _, w := range l.waiters {
		if w.run != run {
			kept = append(kept, w)
		}
	}
	l.waiters = kept
	delete(l.held, run)
	delete(l.used, run)
	delete(l.capacity, run)
	grants := l.grantLocked()
	l.mu.Unlock()
	call(grants)
	return counts
}

func (l *cpuLedger) forgetIdleLocked(run string) {
	if len(l.held[run]) > 0 {
		return
	}
	for _, w := range l.waiters {
		if w.run == run {
			return
		}
	}
	delete(l.held, run)
	delete(l.used, run)
}

// grantLocked grants the waiters the rule admits and answers their grants,
// which the caller calls once it unlocked.
func (l *cpuLedger) grantLocked() []func() {
	var grants []func()
	for progressed := true; progressed; {
		progressed = false
		blocked := map[string]bool{}
		for i := 0; i < len(l.waiters); i++ {
			w := l.waiters[i]
			if blocked[w.run] {
				continue
			}
			if !l.admitsLocked(w) {
				blocked[w.run] = true // a run's requests are granted in order
				continue
			}
			l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
			i--
			if l.held[w.run] == nil {
				l.held[w.run] = map[uint64]int{}
			}
			l.held[w.run][w.id] = w.milli
			l.used[w.run] += w.milli
			c := l.counts[w.run]
			c.Granted++
			l.counts[w.run] = c
			active := l.activeLocked()
			share := l.capacityLocked(active) / max(len(active), 1)
			grant := w.grant
			grants = append(grants, func() { grant(share) })
			progressed = true
		}
	}
	return grants
}

func (l *cpuLedger) admitsLocked(w cpuWaiter) bool {
	active := l.activeLocked()
	if len(active) <= 1 || l.used[w.run] == 0 {
		return true
	}
	capacity := l.capacityLocked(active)
	total := 0
	for run := range active {
		total += l.used[run]
	}
	if total >= capacity {
		return false
	}
	share := capacity / len(active)
	if l.used[w.run] < share {
		return true
	}
	for _, other := range l.waiters {
		if other.run != w.run && l.used[other.run] < share {
			return false
		}
	}
	return true
}

// activeLocked is the set of runs that hold or wait.
func (l *cpuLedger) activeLocked() map[string]bool {
	active := map[string]bool{}
	for run, milli := range l.used {
		if milli > 0 || len(l.held[run]) > 0 {
			active[run] = true
		}
	}
	for _, w := range l.waiters {
		active[w.run] = true
	}
	return active
}

// capacityLocked is the largest capacity the active runs reported, or the
// machine's CPUs when none did.
func (l *cpuLedger) capacityLocked(active map[string]bool) int {
	capacity := 0
	for run := range active {
		capacity = max(capacity, l.capacity[run])
	}
	if capacity <= 0 {
		capacity = runtime.NumCPU() * 1000
	}
	return capacity
}

func call(grants []func()) {
	for _, grant := range grants {
		grant()
	}
}
