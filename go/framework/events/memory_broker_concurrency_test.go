package events

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Memory broker competing/broadcast dispatch is race-free and neither
// loses nor duplicates messages under concurrent publish from many goroutines ---

// TestMemoryBroker_ConcurrentCompetingAndBroadcast publishes a large count of
// messages from many goroutines into a topic that has both a competing handler
// set and a broadcast handler set. It asserts (under `go test -race`):
//   - the competing handlers, summed, observe exactly N deliveries;
//   - no message ID is delivered to the competing set more than once;
//   - every broadcast handler observes all N message IDs (no loss, no dup).
//
// This is the multi-goroutine proof that the goroutine-per-dispatch broker, the
// shared per-topic round-robin atomic, and the inFlight WaitGroup neither lose
// nor duplicate messages while many publishers race the same topic.
func TestMemoryBroker_ConcurrentCompetingAndBroadcast(t *testing.T) {
	t.Parallel()

	topic := NewTopic[int]("race.dispatch")
	broker := NewMemoryBroker()

	const (
		competingHandlers = 4
		broadcastHandlers = 3
		publishers        = 16
		perPublisher      = 250
		total             = publishers * perPublisher
	)

	// Per-competing-handler counters plus a shared map proving exactly-once.
	var competingCounts [competingHandlers]atomic.Int64
	var competingTotal atomic.Int64
	var competingSeen sync.Map // message ID -> struct{}; LoadOrStore detects dups
	var competingDups atomic.Int64

	for i := range competingHandlers {
		idx := i
		h := Handle(topic, func(_ context.Context, msg *Message[int]) error {
			competingCounts[idx].Add(1)
			competingTotal.Add(1)
			if _, loaded := competingSeen.LoadOrStore(msg.ID, struct{}{}); loaded {
				competingDups.Add(1)
			}
			return nil
		}, WithDistribution(Competing))
		if err := broker.Subscribe(h); err != nil {
			t.Fatalf("subscribe competing: %v", err)
		}
	}

	// Each broadcast handler must independently observe every published ID.
	broadcastSeen := make([]*sync.Map, broadcastHandlers)
	broadcastDups := make([]*atomic.Int64, broadcastHandlers)
	broadcastTotal := make([]*atomic.Int64, broadcastHandlers)
	for i := range broadcastHandlers {
		broadcastSeen[i] = &sync.Map{}
		broadcastDups[i] = &atomic.Int64{}
		broadcastTotal[i] = &atomic.Int64{}
		idx := i
		h := Handle(topic, func(_ context.Context, msg *Message[int]) error {
			broadcastTotal[idx].Add(1)
			if _, loaded := broadcastSeen[idx].LoadOrStore(msg.ID, struct{}{}); loaded {
				broadcastDups[idx].Add(1)
			}
			return nil
		}, WithDistribution(Broadcast))
		if err := broker.Subscribe(h); err != nil {
			t.Fatalf("subscribe broadcast: %v", err)
		}
	}

	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	var wg sync.WaitGroup
	for p := range publishers {
		base := p
		wg.Go(func() {
			for j := range perPublisher {
				id := fmt.Sprintf("msg-%d-%d", base, j)
				if err := Publish(context.Background(), broker, topic, base*perPublisher+j, WithMessageID(id)); err != nil {
					t.Errorf("publish %s: %v", id, err)
					return
				}
			}
		})
	}
	wg.Wait()

	// Stop drains all in-flight dispatch goroutines; after it returns every
	// delivery has completed, so the assertions observe the final tallies.
	if err := broker.Stop(context.Background()); err != nil {
		t.Fatalf("stop/drain: %v", err)
	}

	if got := competingTotal.Load(); got != total {
		t.Errorf("competing total deliveries = %d, want exactly %d (message loss or duplication)", got, total)
	}
	if got := competingDups.Load(); got != 0 {
		t.Errorf("competing set saw %d duplicate message IDs, want 0 (exactly-once violated)", got)
	}
	var competingSum int64
	for i := range competingCounts {
		competingSum += competingCounts[i].Load()
	}
	if competingSum != total {
		t.Errorf("competing per-handler sum = %d, want %d", competingSum, total)
	}

	for i := range broadcastHandlers {
		if got := broadcastTotal[i].Load(); got != total {
			t.Errorf("broadcast handler %d total = %d, want %d (broadcast handler missed messages)", i, got, total)
		}
		if got := broadcastDups[i].Load(); got != 0 {
			t.Errorf("broadcast handler %d saw %d duplicate IDs, want 0", i, got)
		}
	}
}

// TestMemoryBroker_ConcurrentStopDuringPublish calls Stop while many publishers
// are still racing the broker. It asserts there is no panic, that Stop drains
// cleanly within the drain timeout, and that the broker never delivers a message
// after Stop returns (no leaked in-flight dispatch). This exercises the
// unlimited-concurrency enqueue path (isStopped check + dispatch + inFlight.Add)
// racing Stop's inFlight.Wait().
func TestMemoryBroker_ConcurrentStopDuringPublish(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "shutdown", "stop-during-publish-is-race-free")
	t.Parallel()

	topic := NewTopic[int]("race.stop")
	broker := NewMemoryBroker(MemoryBrokerConfig{DrainTimeout: 5 * time.Second})

	dispatchStarted := make(chan struct{})
	var dispatchStartedOnce sync.Once
	var stopReturned atomic.Bool
	var afterStop atomic.Int64 // deliveries observed after Stop returned
	// outstanding counts deliveries published but not yet handled. The unlimited-
	// concurrency path spawns one goroutine per delivery, so unbounded publishers
	// on a starved CPU queue a backlog whose drain alone outlasts DrainTimeout.
	// Capping it keeps Stop racing live dispatch without measuring throughput.
	var outstanding atomic.Int64
	const maxOutstanding = 256

	const handlers = 3
	for range handlers {
		h := Handle(topic, func(_ context.Context, _ *Message[int]) error {
			defer outstanding.Add(-1)
			dispatchStartedOnce.Do(func() { close(dispatchStarted) })
			if stopReturned.Load() {
				afterStop.Add(1)
			}
			return nil
		}, WithDistribution(Broadcast))
		if err := broker.Subscribe(h); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
	}

	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	const publishers = 12
	var wg sync.WaitGroup
	for p := range publishers {
		base := p
		wg.Go(func() {
			for j := 0; ; j++ {
				for outstanding.Load() >= maxOutstanding {
					if stopReturned.Load() {
						return
					}
					runtime.Gosched()
				}
				outstanding.Add(handlers)
				// Publish returns an error once the broker is stopped; treat that
				// as the signal to stop this publisher. A nil return after stop is
				// also valid (the message is silently skipped by the enqueue path).
				if err := Publish(context.Background(), broker, topic, base*1_000_000+j, WithMessageID(fmt.Sprintf("s-%d-%d", base, j))); err != nil {
					return
				}
				if stopReturned.Load() {
					return
				}
			}
		})
	}

	// Observe a handler starting so Stop genuinely races in-flight dispatch. A
	// timeout here is a distinct precondition failure, not evidence about Stop.
	select {
	case <-dispatchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dispatch to start; test did not establish the race window")
	}

	if err := broker.Stop(context.Background()); err != nil {
		t.Fatalf("stop/drain returned error (no clean drain): %v", err)
	}
	stopReturned.Store(true)

	wg.Wait()
	// Once all publishers have returned, no more dispatches can be registered.
	// A second wait deterministically completes any dispatch that incorrectly
	// escaped Stop's drain instead of relying on scheduler timing.
	broker.inFlight.Wait()

	if got := afterStop.Load(); got != 0 {
		t.Errorf("%d deliveries occurred after Stop returned; drain did not wait for in-flight dispatch", got)
	}

	// The broker must reject publishes once stopped.
	if err := Publish(context.Background(), broker, topic, -1, WithMessageID("post-stop")); err == nil {
		t.Error("expected publish after Stop to return an error")
	}
}
