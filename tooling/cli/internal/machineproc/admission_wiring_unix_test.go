//go:build linux || darwin

package machineproc

import (
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

// connPair is two connected control channels.
func connPair(t *testing.T) (*conn, *conn) {
	t.Helper()
	fds, err := socketpair()
	if err != nil {
		t.Fatal(err)
	}
	ends := make([]*conn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "machine-test")
		fc, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		uc, err := unixConn(fc)
		if err != nil {
			t.Fatal(err)
		}
		ends[i] = &conn{c: uc}
		t.Cleanup(func() { _ = uc.Close() })
	}
	return ends[0], ends[1]
}

func TestAWorkerAsksForCPUOnlyUnderAFairShareAndASupervisorThatGrants(t *testing.T) {
	for _, tc := range []struct {
		supervisor int
		share      string
		asks       bool
	}{
		{supervisor: 0, share: CPUShareFair},
		{supervisor: 1, share: CPUShareFair},
		{supervisor: admissionProtocol, share: CPUShareOff},
		{supervisor: admissionProtocol, share: ""},
		{supervisor: admissionProtocol, share: CPUShareFair, asks: true},
	} {
		if got := asksForCPU(request{SupervisorProtocol: tc.supervisor, CPUShare: tc.share}); got != tc.asks {
			t.Errorf("supervisor %d, share %q: asks %v, want %v", tc.supervisor, tc.share, got, tc.asks)
		}
	}
}

func TestASupervisorStampsTheRunItForwardsWithItsProtocol(t *testing.T) {
	req := forwardedRun(request{Protocol: 1, Op: "run", CPUShare: CPUShareFair})
	if req.SupervisorProtocol != ProtocolVersion || req.Protocol != 1 || req.CPUShare != CPUShareFair {
		t.Fatalf("forwarded %+v", req)
	}
}

func TestTheWorkersControlReaderSeparatesGrantsFromRuns(t *testing.T) {
	supervisorEnd, workerEnd := connPair(t)
	sent := make(chan control, 4)
	admitter := newCPUAdmitter(func(msg control) error { sent <- msg; return nil })
	runs := readControl(workerEnd, admitter)

	granted := make(chan int, 1)
	go func() {
		_, share, _ := admitter.acquire(context.Background(), 1000, 4000)
		granted <- share
	}()
	acquire := <-sent
	if err := supervisorEnd.send(request{Protocol: ProtocolVersion, Op: "future-operation"}); err != nil {
		t.Fatal(err)
	}
	if err := supervisorEnd.send(request{Protocol: ProtocolVersion, Op: "grant", Grant: acquire.ID, ShareMilli: 2000}); err != nil {
		t.Fatal(err)
	}
	select {
	case share := <-granted:
		if share != 2000 {
			t.Fatalf("the grant carried a share of %d, want 2000", share)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the grant never reached the admitter")
	}
	if err := supervisorEnd.send(request{Protocol: ProtocolVersion, Op: "run", Args: []string{"build"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case next := <-runs:
		if next.req.Op != "run" || len(next.req.Args) != 1 {
			t.Fatalf("the reader answered %+v as a run", next.req)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the worker's loop")
	}
	_ = supervisorEnd.c.Close()
	if _, ok := <-runs; ok {
		t.Fatal("an operation of a later protocol reached the worker's loop as a run")
	}
	if _, _, ok := admitter.acquire(context.Background(), 1000, 4000); !ok {
		t.Fatal("once the channel closed, an acquire must be granted at once")
	}
}

func TestTheSupervisorAnswersAWorkersControlMessages(t *testing.T) {
	supervisorEnd, workerEnd := connPair(t)
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("no true command:", err)
	}
	s := &supervisor{dir: t.TempDir(), cpu: newCPULedger(), workers: map[string]*worker{}, pids: map[int]bool{}, stopped: make(chan struct{})}
	w := &worker{ctrl: supervisorEnd, cmd: cmd, busy: true, key: "k"}
	s.workers[w.key] = w

	s.control(w, control{Op: "acquire", ID: 7, CPUMilli: 1000, CapacityMilli: 4000})
	var grant request
	_ = workerEnd.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := workerEnd.receive(&grant); err != nil {
		t.Fatal(err)
	}
	if grant.Op != "grant" || grant.Grant != 7 || grant.ShareMilli != 4000 {
		t.Fatalf("answered %+v, want the grant of 7 with the whole capacity as a run alone", grant)
	}
	s.control(w, control{Op: "release", ID: 7})
	s.control(w, control{Op: "done", HeldBytes: 90 << 20})
	if w.busy || w.runs != 1 || w.heldBytes != 90<<20 {
		t.Fatalf("after done: busy %v, runs %d, held %d", w.busy, w.runs, w.heldBytes)
	}
	s.control(w, control{Op: "held", HeldBytes: 40 << 20})
	if w.heldBytes != 40<<20 {
		t.Fatalf("held %d after a held report of 40 MiB", w.heldBytes)
	}
	s.control(w, control{Op: "future-operation"})
}

func TestCPULedgerLetsARunBelowItsShareOvertake(t *testing.T) {
	l := newCPULedger()
	var g grants
	for i := uint64(1); i <= 4; i++ {
		l.acquire("a", i, 1000, 4000, g.fn("a"))
	}
	l.acquire("b", 1, 1000, 4000, g.fn("b1"))
	// a asks before b does, but a holds its share and b does not.
	l.acquire("a", 5, 1000, 4000, g.fn("a5"))
	l.acquire("b", 2, 1000, 4000, g.fn("b2"))
	l.release("a", 1)
	l.release("a", 2)
	if !g.has("b2") || g.has("a5") {
		t.Fatalf("granted %v: b, below its share, must go before a's earlier request", g.list())
	}
}
