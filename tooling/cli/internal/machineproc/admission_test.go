package machineproc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// grants records the order in which a ledger granted requests.
type grants struct {
	mu     sync.Mutex
	got    []string
	shares map[string]int
}

func (g *grants) fn(name string) func(int) {
	return func(share int) {
		g.mu.Lock()
		g.got = append(g.got, name)
		if g.shares == nil {
			g.shares = map[string]int{}
		}
		g.shares[name] = share
		g.mu.Unlock()
	}
}

func (g *grants) share(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.shares[name]
}

func (g *grants) list() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.got...)
}

func (g *grants) has(name string) bool {
	for _, got := range g.list() {
		if got == name {
			return true
		}
	}
	return false
}

func TestCPULedgerGrantsARunAloneEveryRequestAtOnce(t *testing.T) {
	l := newCPULedger()
	var g grants
	for i := uint64(1); i <= 8; i++ {
		l.acquire("a", i, 1000, 4000, g.fn("a"))
	}
	if got := len(g.list()); got != 8 {
		t.Fatalf("a run alone was granted %d of 8 requests, want all of them as without the machine process", got)
	}
}

func TestCPULedgerSharesTheCapacityPerRun(t *testing.T) {
	l := newCPULedger()
	var g grants
	// Run a holds the whole machine alone.
	for i := uint64(1); i <= 4; i++ {
		l.acquire("a", i, 1000, 4000, g.fn("a"))
	}
	// Run b arrives: its first request is granted, since it holds nothing.
	l.acquire("b", 1, 1000, 4000, g.fn("b1"))
	l.acquire("b", 2, 1000, 4000, g.fn("b2"))
	l.acquire("a", 5, 1000, 4000, g.fn("a5"))
	if !g.has("b1") {
		t.Fatal("a run that holds nothing must be granted its first request")
	}
	if g.has("b2") || g.has("a5") {
		t.Fatalf("granted %v with the machine full", g.list())
	}
	// b's first grant passed the capacity by one charge: a release that
	// leaves the machine full grants nothing.
	l.release("a", 1)
	if g.has("b2") || g.has("a5") {
		t.Fatalf("granted %v with the machine still full", g.list())
	}
	// A second release frees one CPU: b, below its share of 2, goes before
	// a's own wait, since a holds its share.
	l.release("a", 2)
	if !g.has("b2") || g.has("a5") {
		t.Fatalf("granted %v, want b2 and not a5", g.list())
	}
	// a returns one more: it holds 1 of its share of 2, so its wait is granted.
	l.release("a", 3)
	if !g.has("a5") {
		t.Fatalf("a below its share must be granted, got %v", g.list())
	}
}

func TestCPULedgerLeavesNoFreeCPUIdle(t *testing.T) {
	l := newCPULedger()
	var g grants
	l.acquire("a", 1, 1000, 4000, g.fn("a1"))
	l.acquire("b", 1, 1000, 4000, g.fn("b1"))
	// b asks for no more: a takes the CPU b does not ask for, past its share.
	for i := uint64(2); i <= 3; i++ {
		l.acquire("a", i, 1000, 4000, g.fn("a"))
	}
	if got := len(g.list()); got != 4 {
		t.Fatalf("granted %v, want a to use the CPU no other run asks for", g.list())
	}
}

func TestCPULedgerKeepsTheOrderOfARunsRequests(t *testing.T) {
	l := newCPULedger()
	var g grants
	l.acquire("a", 1, 2000, 4000, g.fn("a1"))
	l.acquire("b", 1, 2000, 4000, g.fn("b1"))
	l.acquire("b", 2, 2000, 4000, g.fn("b2"))
	l.acquire("b", 3, 500, 4000, g.fn("b3"))
	if g.has("b2") || g.has("b3") {
		t.Fatalf("granted %v: a later request of b overtook an earlier one", g.list())
	}
	l.release("a", 1)
	if got := g.list(); len(got) != 4 || got[2] != "b2" || got[3] != "b3" {
		t.Fatalf("granted %v, want b2 then b3", got)
	}
}

func TestCPULedgerEndDropsWhatARunHolds(t *testing.T) {
	l := newCPULedger()
	var g grants
	for i := uint64(1); i <= 4; i++ {
		l.acquire("a", i, 1000, 4000, g.fn("a"))
	}
	l.acquire("b", 1, 1000, 4000, g.fn("b1"))
	l.acquire("b", 2, 1000, 4000, g.fn("b2"))
	l.acquire("a", 5, 1000, 4000, g.fn("a5"))
	l.end("a")
	if !g.has("b2") || g.has("a5") {
		t.Fatalf("after a ended, granted %v, want b2 and none of a's", g.list())
	}
	// b is alone again: every request is granted at once.
	for i := uint64(3); i <= 9; i++ {
		l.acquire("b", i, 1000, 4000, g.fn("b"))
	}
	if got := len(g.list()); got != 4+2+7 {
		t.Fatalf("a run alone again was not granted every request: %v", g.list())
	}
}

func TestCPULedgerReleaseWithdrawsAWaitingRequest(t *testing.T) {
	l := newCPULedger()
	var g grants
	for i := uint64(1); i <= 4; i++ {
		l.acquire("a", i, 1000, 4000, g.fn("a"))
	}
	l.acquire("b", 1, 1000, 4000, g.fn("b1"))
	l.acquire("b", 2, 1000, 4000, g.fn("b2"))
	l.release("b", 2) // withdrawn before it was granted
	l.release("a", 1)
	if g.has("b2") {
		t.Fatal("a withdrawn request was granted")
	}
}

func TestCPUAdmitterWaitsForTheGrant(t *testing.T) {
	sent := make(chan control, 8)
	a := newCPUAdmitter(func(msg control) error { sent <- msg; return nil })
	done := make(chan bool, 1)
	var release func()
	var share int
	go func() {
		var ok bool
		release, share, ok = a.acquire(context.Background(), 1500, 4000)
		done <- ok
	}()
	msg := <-sent
	if msg.Op != "acquire" || msg.CPUMilli != 1500 || msg.CapacityMilli != 4000 {
		t.Fatalf("sent %+v", msg)
	}
	select {
	case <-done:
		t.Fatal("acquire returned before its grant")
	case <-time.After(20 * time.Millisecond):
	}
	a.granted(msg.ID, 2000)
	if !<-done {
		t.Fatal("a granted acquire answered false")
	}
	if share != 2000 {
		t.Fatalf("the grant carried a share of %d, want 2000", share)
	}
	release()
	release()
	if got := <-sent; got.Op != "release" || got.ID != msg.ID {
		t.Fatalf("release sent %+v", got)
	}
	select {
	case extra := <-sent:
		t.Fatalf("a second release sent %+v", extra)
	default:
	}
}

func TestCPUAdmitterWithdrawsOnCancel(t *testing.T) {
	sent := make(chan control, 8)
	a := newCPUAdmitter(func(msg control) error { sent <- msg; return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, _, ok := a.acquire(ctx, 1000, 4000)
		done <- ok
	}()
	acquire := <-sent
	cancel()
	if <-done {
		t.Fatal("a canceled acquire answered true")
	}
	if got := <-sent; got.Op != "release" || got.ID != acquire.ID {
		t.Fatalf("cancel sent %+v, want the release of %d", got, acquire.ID)
	}
}

func TestCPUAdmitterGrantsWhenTheSupervisorIsGone(t *testing.T) {
	sent := make(chan control, 8)
	a := newCPUAdmitter(func(msg control) error { sent <- msg; return nil })
	done := make(chan bool, 1)
	go func() {
		_, _, ok := a.acquire(context.Background(), 1000, 4000)
		done <- ok
	}()
	<-sent
	a.close()
	if !<-done {
		t.Fatal("a wait must be granted once the control channel closed")
	}
	if _, _, ok := a.acquire(context.Background(), 1000, 4000); !ok {
		t.Fatal("an acquire after the channel closed must be granted at once")
	}
	broken := newCPUAdmitter(func(control) error { return errors.New("closed") })
	if _, _, ok := broken.acquire(context.Background(), 1000, 4000); !ok {
		t.Fatal("an acquire that cannot be sent must be granted at once")
	}
}

func TestAcquireCPUGrantsAtOnceOutsideAWorker(t *testing.T) {
	if currentAdmitter.Load() != nil {
		t.Fatal("no admitter is set outside a worker")
	}
	release, share, ok := AcquireCPU(context.Background(), 4000, 4000)
	if !ok || release == nil || share != 0 {
		t.Fatal("outside a worker, CPU is granted at once")
	}
	release()
}

func TestCPULedgerCountsTheGrantsThatWaited(t *testing.T) {
	l := newCPULedger()
	var g grants
	for i := uint64(1); i <= 4; i++ {
		l.acquire("a", i, 1000, 4000, g.fn("a"))
	}
	l.acquire("b", 1, 1000, 4000, g.fn("b1"))
	l.acquire("b", 2, 1000, 4000, g.fn("b2"))
	l.release("a", 1)
	l.release("a", 2)
	if got := l.end("b"); got != (cpuCounts{Granted: 2, Waited: 1}) {
		t.Fatalf("b counted %+v, want 2 granted and 1 waited", got)
	}
	if got := l.end("a"); got != (cpuCounts{Granted: 4}) {
		t.Fatalf("a counted %+v, want 4 granted at once", got)
	}
}

func TestCPULedgerGrantsCarryTheRunsShare(t *testing.T) {
	l := newCPULedger()
	var g grants
	l.acquire("a", 1, 1000, 4000, g.fn("a-alone"))
	if got := g.share("a-alone"); got != 4000 {
		t.Fatalf("a run alone was given a share of %d, want the whole capacity", got)
	}
	l.acquire("b", 1, 1000, 4000, g.fn("b"))
	if got := g.share("b"); got != 2000 {
		t.Fatalf("one of two runs was given a share of %d, want half the capacity", got)
	}
}
