package app

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// blockingStarter is a Starter+Stopper whose Start blocks until its ctx is
// canceled. It records (1) whether Start observed cancellation and (2) whether
// its Stop ever ran while Start was still in flight.
type blockingStarter struct {
	name string

	starting   atomic.Bool // true while Start has not yet returned
	sawCancel  atomic.Bool // Start unblocked via ctx cancellation
	startDone  chan struct{}
	stopRanMid atomic.Bool // Stop observed Start still in flight
	stopRan    atomic.Bool
}

func newBlockingStarter(name string) *blockingStarter {
	return &blockingStarter{name: name, startDone: make(chan struct{})}
}

func (p *blockingStarter) Name() string { return p.name }

func (p *blockingStarter) Start(ctx context.Context, _ *Module) error {
	p.starting.Store(true)
	defer func() {
		p.starting.Store(false)
		close(p.startDone)
	}()
	<-ctx.Done()
	p.sawCancel.Store(true)
	return ctx.Err()
}

func (p *blockingStarter) Stop(_ context.Context, _ *Module) error {
	p.stopRan.Store(true)
	if p.starting.Load() {
		p.stopRanMid.Store(true)
	}
	return nil
}

// A Start-phase timeout must (1) cancel the ctx handed to the blocked
// Start so it unblocks (no leaked goroutine), and (2) join the in-flight Start
// before running Stop, so Stop never races a still-running Start.
func TestStartPlugins_TimeoutCancelsStartAndJoinsBeforeStop(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "startup-failure", "start-timeout-cancels-and-joins")
	p := newBlockingStarter("blocker")

	a := New("start-timeout").WithStartTimeout(50 * time.Millisecond)
	a.Use(p)

	start := time.Now()
	err := a.Start(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected Start to fail when the start phase times out")
	}
	// Two truthful shapes exist for one outcome. The joiner usually wins the
	// race against the starter's own deadline and reports "timed out"; on a
	// starved scheduler the canceled Start can return first, wg drains, and
	// the aggregate carries the starter's deadline error instead. Both mean
	// the configured start timeout fired.
	if !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected a start-timeout error, got %q", err.Error())
	}

	// Start must have observed cancellation (it did not leak) — and it must have
	// already returned by the time Start() returns control to us.
	select {
	case <-p.startDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Start goroutine leaked: it never unblocked after the timeout cancel")
	}
	if !p.sawCancel.Load() {
		t.Error("blocked Start did not observe ctx cancellation on timeout")
	}
	if p.starting.Load() {
		t.Error("Start was still in flight after a.Start returned")
	}

	// Stop must have run as part of timeout cleanup, and it must not have raced
	// an in-flight Start.
	if !p.stopRan.Load() {
		t.Error("Stop did not run during start-timeout cleanup")
	}
	if p.stopRanMid.Load() {
		t.Error("Stop ran concurrently with a still-running Start")
	}

	// The phase must not block longer than the configured timeout plus the
	// bounded drain grace; a well-behaved Starter drains immediately.
	if elapsed > startDrainGrace+2*time.Second {
		t.Errorf("start phase took %s, expected it to return promptly after the timeout", elapsed)
	}
}
