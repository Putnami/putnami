package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/errors"
)

// --- Concurrent IsRunning / Context reads ---

func TestIsRunning_ConcurrentReads(t *testing.T) {
	a := New("concurrent-read")

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_ = a.IsRunning()
		})
	}
	for range 50 {
		wg.Go(func() {
			_ = a.Context()
		})
	}
	wg.Wait()
}

// --- Concurrent IsRunning during Start/Stop lifecycle ---

func TestIsRunning_ConcurrentWithLifecycle(t *testing.T) {
	a := New("lifecycle-concurrent")

	ctx, cancel := context.WithCancel(context.Background())

	// Start in background (it will block on <-ctx.Done()).
	started := make(chan struct{})
	go func() {
		// Use a runner that signals when started.
		a.Run(func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
		a.Start(ctx) //nolint:errcheck
	}()

	// Wait for start to complete.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for start")
	}

	// Concurrent reads while running.
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if !a.IsRunning() {
				t.Error("expected running=true")
			}
		})
	}
	wg.Wait()

	// Trigger shutdown.
	cancel()
	time.Sleep(50 * time.Millisecond) // let Stop() complete

	// Concurrent reads after stop.
	for range 50 {
		wg.Go(func() {
			_ = a.IsRunning()
		})
	}
	wg.Wait()
}

// --- Concurrent Stop calls (idempotent) ---

func TestStop_ConcurrentCalls(t *testing.T) {
	a := New("concurrent-stop")
	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	go func() {
		a.Run(func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
		a.Start(ctx) //nolint:errcheck
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for start")
	}

	cancel()
	time.Sleep(50 * time.Millisecond)

	// Multiple concurrent Stop calls should not panic or error.
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_ = a.Stop(context.Background())
		})
	}
	wg.Wait()
}

// --- Concurrent Start rejection (already running) ---

func TestStart_ConcurrentDoubleStart(t *testing.T) {
	a := New("double-start")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	go func() {
		a.Run(func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
		a.Start(ctx) //nolint:errcheck
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for start")
	}

	// Concurrent second Start calls should return "already running" error.
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			err := a.Start(ctx)
			if err == nil {
				t.Error("expected error for double start")
			}
		})
	}
	wg.Wait()

	cancel()
	a.Stop(context.Background()) //nolint:errcheck
}

// --- MigrationRegistry read concurrent with container construction ---
//
// buildContainer assigns a.migrationRegistry; MigrationRegistry() reads it.
// This test must stay clean under `-race`. We hammer MigrationRegistry() from many goroutines
// while Start (which calls buildContainer) runs concurrently. The reader may
// observe nil (before buildContainer) or the registry (after) — either is fine;
// the point is that the access must not race.
func TestMigrationRegistry_ConcurrentWithStart(t *testing.T) {
	a := New("migreg-race")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	a.Run(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return nil
	})

	var wg sync.WaitGroup
	// Reader goroutines run for the whole startup window, overlapping the
	// buildContainer write inside Start.
	for range 50 {
		wg.Go(func() {
			for range 100 {
				_ = a.MigrationRegistry()
			}
		})
	}
	// Drive the lifecycle concurrently with the readers.
	wg.Go(func() {
		a.Start(ctx) //nolint:errcheck
	})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for start")
	}

	// After a successful start the registry is always non-nil (the framework
	// registers a per-app *migration.Registry during buildContainer).
	if a.MigrationRegistry() == nil {
		t.Error("expected non-nil migration registry after start")
	}

	cancel()
	wg.Wait()
	a.Stop(context.Background()) //nolint:errcheck
}

// TestMigrationRegistry_ConcurrentWithBuildContainer exercises the same
// write/read race through Validate (which also calls buildContainer) on a fresh
// app, with no full Start lifecycle. This keeps the race window tight and
// purely around buildContainer.
func TestMigrationRegistry_ConcurrentWithBuildContainer(t *testing.T) {
	a := New("migreg-build-race")

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 100 {
				_ = a.MigrationRegistry()
			}
		})
	}
	wg.Go(func() {
		// Validate runs PreConfigure -> buildContainer -> ... and cleans up.
		_ = a.Validate()
	})
	wg.Wait()
}

// --- Concurrent Start on a fresh app permits exactly one caller ---
//
// The double-start guard must be atomic: of two concurrent Start calls on a
// brand-new app, EXACTLY ONE may proceed; the other must be rejected with the
// app.already_running error. We count outcomes to assert exactly one winner and
// one rejection.
func TestStart_ConcurrentFreshStart_ExactlyOneProceeds(t *testing.T) {
	a := New("fresh-double-start")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// entered counts how many goroutines actually reached the runner, i.e. how
	// many Start calls proceeded all the way through startup. The guard must
	// keep this at exactly 1.
	var entered atomic.Int32
	runnerEntered := make(chan struct{}, 2)
	a.Run(func(ctx context.Context) error {
		entered.Add(1)
		runnerEntered <- struct{}{}
		<-ctx.Done() // the winner blocks here until we cancel
		return nil
	})

	const n = 2
	results := make(chan error, n)
	for range n {
		go func() {
			results <- a.Start(ctx)
		}()
	}

	// The loser returns the already_running error promptly. Read exactly one
	// result before releasing the winner: it must be the rejection. If BOTH
	// goroutines had wrongly proceeded, both would block in the runner and this
	// read would time out — which is the bug we are guarding against.
	var loserErr error
	select {
	case loserErr = <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the rejected Start: both Start calls may have proceeded")
	}
	if loserErr == nil {
		t.Fatal("expected one Start to be rejected with already_running, got nil")
	}
	if !errors.Is(loserErr, CodeAlreadyRunning) {
		t.Fatalf("rejected Start error = %v, want code %q", loserErr, CodeAlreadyRunning)
	}

	// Exactly one goroutine must have reached the runner.
	select {
	case <-runnerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the winning Start to reach the runner")
	}

	// Release the winner and collect its result.
	cancel()
	var winnerErr error
	select {
	case winnerErr = <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the winning Start to return")
	}
	if winnerErr != nil {
		t.Errorf("winning Start returned error: %v", winnerErr)
	}

	if got := entered.Load(); got != 1 {
		t.Errorf("exactly one Start should proceed to the runner, got %d", got)
	}

	a.Stop(context.Background()) //nolint:errcheck
}

// --- Concurrent Context reads during lifecycle ---

func TestContext_ConcurrentReadsDuringLifecycle(t *testing.T) {
	a := New("ctx-reads")
	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	go func() {
		a.Run(func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
		a.Start(ctx) //nolint:errcheck
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for start")
	}

	// Concurrent reads of Context() while running.
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			// Context() may be nil (no DI providers), but should not race.
			_ = a.Context()
		})
	}
	wg.Wait()

	cancel()
	a.Stop(context.Background()) //nolint:errcheck
}
