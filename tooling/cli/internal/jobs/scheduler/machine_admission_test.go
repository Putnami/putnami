package scheduler

import (
	"context"
	"sync"
	"testing"
)

// fakeMachine stands for the machine process's CPU admission.
type fakeMachine struct {
	mu       sync.Mutex
	asked    []int
	capacity []int
	released int
	refuse   bool
	share    int
}

func (m *fakeMachine) acquire(ctx context.Context, cpuMilli, capacityMilli int) (func(), int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, cpuMilli)
	m.capacity = append(m.capacity, capacityMilli)
	if m.refuse {
		return nil, 0, false
	}
	return func() {
		m.mu.Lock()
		m.released++
		m.mu.Unlock()
	}, m.share, true
}

func useFakeMachine(t *testing.T) *fakeMachine {
	t.Helper()
	m := &fakeMachine{}
	previous := machineCPU
	machineCPU = m.acquire
	t.Cleanup(func() { machineCPU = previous })
	return m
}

func TestExecutionAsksTheMachineForTheCPUItCharges(t *testing.T) {
	m := useFakeMachine(t)
	pool := newResourcePool(4, 0, nil)
	hold, ok := pool.acquireExecution(context.Background(), resourceRequest{cpuMilli: 1500}, executionRank{key: "a"})
	if !ok {
		t.Fatal("acquireExecution refused")
	}
	if len(m.asked) != 1 || m.asked[0] != 1500 || m.capacity[0] != 4000 {
		t.Fatalf("asked the machine %v against %v, want 1500 against 4000", m.asked, m.capacity)
	}
	pool.release(hold)
	pool.release(hold)
	if m.released != 1 {
		t.Fatalf("returned the machine grant %d times, want once", m.released)
	}
}

func TestExecutionThatChargesNoCPUAsksTheMachineNothing(t *testing.T) {
	m := useFakeMachine(t)
	pool := newResourcePool(4, 0, nil)
	hold, ok := pool.acquireExecution(context.Background(), resourceRequest{}, executionRank{key: "serve"})
	if !ok {
		t.Fatal("acquireExecution refused")
	}
	pool.release(hold)
	if len(m.asked) != 0 {
		t.Fatalf("a serve step asked the machine for %v", m.asked)
	}
}

func TestResizeToNoCPUReturnsTheMachineGrant(t *testing.T) {
	m := useFakeMachine(t)
	pool := newResourcePool(4, 0, nil)
	hold, _ := pool.acquireExecution(context.Background(), resourceRequest{cpuMilli: 2000}, executionRank{key: "batch"})
	pool.resize(&hold, resourceRequest{cpuMilli: 1000})
	if m.released != 0 {
		t.Fatal("a resize that keeps CPU returned the machine grant")
	}
	pool.resize(&hold, resourceRequest{})
	if m.released != 1 {
		t.Fatalf("a resize to no CPU returned the machine grant %d times, want once", m.released)
	}
	pool.release(hold)
	if m.released != 1 {
		t.Fatalf("the release after it returned the grant again (%d)", m.released)
	}
}

func TestMachineRefusalReleasesTheRunsOwnReservation(t *testing.T) {
	m := useFakeMachine(t)
	m.refuse = true
	pool := newResourcePool(1, 0, nil)
	if _, ok := pool.acquireExecution(context.Background(), resourceRequest{cpuMilli: 1000}, executionRank{key: "a"}); ok {
		t.Fatal("acquireExecution answered true when the machine wait ended")
	}
	m.refuse = false
	if _, ok := pool.tryAcquire(resourceRequest{cpuMilli: 1000}); !ok {
		t.Fatal("the run's own CPU stayed held after the machine wait ended")
	}
}

func TestTheRunsShareCapsTheCeilingsItGrants(t *testing.T) {
	m := useFakeMachine(t)
	m.share = 2000
	pool := newResourcePool(4, 0, nil)
	if _, ok := pool.acquireExecution(context.Background(), resourceRequest{cpuMilli: 1000}, executionRank{key: "a"}); !ok {
		t.Fatal("acquireExecution refused")
	}
	if got := pool.grantCeiling(4, true); got != 2 {
		t.Fatalf("a cold task under a share of 2 CPUs was granted %d, want 2", got)
	}
	if got := pool.grantCeiling(1, false); got != 1 {
		t.Fatalf("a task with history was raised to %d with the share taken, want 1", got)
	}
}

func TestWithoutAShareTheCeilingsAreAsBefore(t *testing.T) {
	useFakeMachine(t)
	pool := newResourcePool(4, 0, nil)
	if _, ok := pool.acquireExecution(context.Background(), resourceRequest{cpuMilli: 1000}, executionRank{key: "a"}); !ok {
		t.Fatal("acquireExecution refused")
	}
	if got := pool.grantCeiling(4, true); got != 4 {
		t.Fatalf("a cold task with no share was granted %d, want the whole machine", got)
	}
	if got := pool.grantCeiling(1, false); got != 1 {
		t.Fatalf("a task with history was granted %d with every CPU taken, want 1", got)
	}
}
