package events

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/logger"
)

// Error codes for event broker operations.
const (
	CodeEventsStopped      errors.Code = "events.stopped"
	CodeEventsDrainTimeout errors.Code = "events.drain_timeout"
	CodeEventsQueueFull    errors.Code = "events.queue_full"
)

// MemoryBrokerConfig configures the in-memory broker.
type MemoryBrokerConfig struct {
	// DrainTimeout is the maximum time to wait for in-flight messages during shutdown.
	DrainTimeout time.Duration
	// Observer receives broker lifecycle callbacks for metrics/logging bridges.
	Observer Observer
}

func (c MemoryBrokerConfig) withDefaults() MemoryBrokerConfig {
	if c.DrainTimeout == 0 {
		c.DrainTimeout = 10 * time.Second
	}
	return c
}

type subscription struct {
	def    *HandlerDefinition
	worker *subscriptionWorker
}

// Observer receives broker lifecycle callbacks. Implement this to bridge event
// activity into telemetry/logging without coupling this package to a concrete
// metrics backend.
type Observer interface {
	Published(ctx context.Context, env Envelope)
	Handled(ctx context.Context, env Envelope, duration time.Duration, err error)
	Retried(env Envelope, delay time.Duration, err error)
	DeadLettered(env Envelope, err error)
	Dropped(env Envelope, reason string)
}

type subscriptionWorker struct {
	b        *MemoryBroker
	sub      *subscription
	mu       sync.Mutex
	active   int
	queue    []Envelope
	stopping bool
}

// MemoryBroker is an in-memory event transport using goroutines and channels.
// It faithfully implements retry with exponential backoff, dead-letter queues,
// competing/broadcast distribution, and attribute-based filtering.
type MemoryBroker struct {
	config        MemoryBrokerConfig
	mu            sync.RWMutex
	subscriptions map[string][]*subscription // topic → handlers
	roundRobin    sync.Map                   // topic → *atomic.Uint64 (lock-free round-robin)
	inFlight      sync.WaitGroup
	stopped       bool
	// quit is closed by Stop to signal in-flight retry timers to abort promptly
	// instead of firing after shutdown. Recreated by Start so the broker can be
	// restarted.
	quit chan struct{}
	logs eventLoggers
}

// NewMemoryBroker creates a new in-memory event broker.
func NewMemoryBroker(config ...MemoryBrokerConfig) *MemoryBroker {
	cfg := MemoryBrokerConfig{}
	if len(config) > 0 {
		cfg = config[0]
	}
	return &MemoryBroker{
		config:        cfg.withDefaults(),
		subscriptions: make(map[string][]*subscription),
		quit:          make(chan struct{}),
		logs:          newEventLoggers(),
	}
}

// Publish sends an envelope to all matching subscribers.
func (b *MemoryBroker) Publish(ctx context.Context, env Envelope) error {
	b.mu.RLock()
	if b.stopped {
		b.mu.RUnlock()
		return errors.New(CodeEventsStopped, "broker is stopped")
	}
	subs := b.subscriptions[env.Topic]
	b.mu.RUnlock()

	b.observePublished(ctx, env)
	if len(subs) == 0 {
		return nil
	}

	targets := b.selectTargets(env.Topic, subs, env)
	for _, sub := range targets {
		if err := sub.worker.enqueue(ctx, env); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe registers a handler for a topic.
func (b *MemoryBroker) Subscribe(def *HandlerDefinition) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	sub := &subscription{
		def: def,
	}
	sub.worker = &subscriptionWorker{b: b, sub: sub}
	b.subscriptions[def.Topic] = append(b.subscriptions[def.Topic], sub)
	return nil
}

// Start initializes the broker.
func (b *MemoryBroker) Start(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = false
	b.quit = make(chan struct{})
	return nil
}

// Stop gracefully shuts down the broker, waiting for in-flight messages.
func (b *MemoryBroker) Stop(_ context.Context) error {
	b.mu.Lock()
	if !b.stopped {
		b.stopped = true
		if b.quit != nil {
			close(b.quit)
		}
	}
	for _, subs := range b.subscriptions {
		for _, sub := range subs {
			sub.worker.stop()
		}
	}
	b.mu.Unlock()

	done := make(chan struct{})
	go func() {
		b.inFlight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(b.config.DrainTimeout):
		return errors.New(CodeEventsDrainTimeout, "drain timeout", errors.Duration("timeout", b.config.DrainTimeout))
	}
}

// selectTargets filters subscriptions by attributes and applies distribution logic.
func (b *MemoryBroker) selectTargets(topic string, subs []*subscription, env Envelope) []*subscription {
	var broadcast []*subscription
	var competing []*subscription

	for _, sub := range subs {
		if !matchesFilter(sub.def.Filter, env.Attributes) {
			continue
		}
		switch sub.def.Options.Distribution {
		case Broadcast:
			broadcast = append(broadcast, sub)
		default:
			competing = append(competing, sub)
		}
	}

	var targets []*subscription
	targets = append(targets, broadcast...)

	if len(competing) > 0 {
		val, _ := b.roundRobin.LoadOrStore(topic, &atomic.Uint64{})
		counter := val.(*atomic.Uint64) //nolint:errcheck // type is guaranteed by LoadOrStore
		n := counter.Add(1) - 1
		idx := int(n % uint64(len(competing))) //nolint:gosec // len(competing) > 0 here
		targets = append(targets, competing[idx])
	}

	return targets
}

// matchesFilter checks if message attributes match the handler's filter.
func matchesFilter(filter, attrs map[string]string) bool {
	for k, v := range filter {
		if attrs[k] != v {
			return false
		}
	}
	return true
}

// dispatch registers and starts a handler goroutine for env. It does not itself
// re-check stopped: every caller is already ordered against Stop's drain — it
// either holds the worker lock (which Stop's worker.stop() barrier orders
// against) or runs inside an already-counted in-flight goroutine (the bounded-
// concurrency requeue and retry paths), so its inFlight.Add is never from zero.
// The unlimited-concurrency publish path, which has no such ordering, must use
// dispatchUnlessStopped instead.
func (b *MemoryBroker) dispatch(ctx context.Context, env Envelope, sub *subscription) {
	b.inFlight.Add(1)
	go b.runHandler(ctx, env, sub)
}

// dispatchUnlessStopped is dispatch for the unlimited-concurrency publish path,
// the only caller with no happens-before ordering against Stop's drain. It
// checks stopped and registers the in-flight dispatch atomically under the
// broker lock; Stop sets stopped under the same lock (write side) before it
// waits on inFlight, so the two are mutually exclusive. This closes the race
// where a publisher observed the broker as running, then registered and started
// a delivery after Stop had already drained and returned.
func (b *MemoryBroker) dispatchUnlessStopped(ctx context.Context, env Envelope, sub *subscription) {
	b.mu.RLock()
	if b.stopped {
		b.mu.RUnlock()
		return
	}
	b.inFlight.Add(1)
	b.mu.RUnlock()
	go b.runHandler(ctx, env, sub)
}

// runHandler is the body of a dispatch goroutine: execute the handler, then pull
// the next queued envelope for bounded-concurrency workers, then release the
// inFlight slot.
func (b *MemoryBroker) runHandler(ctx context.Context, env Envelope, sub *subscription) {
	defer b.inFlight.Done()
	defer sub.worker.complete(ctx)
	b.executeHandler(ctx, env, sub)
}

func (w *subscriptionWorker) enqueue(ctx context.Context, env Envelope) error {
	opts := w.sub.def.Options
	if opts.Concurrency <= 0 {
		w.b.dispatchUnlessStopped(ctx, env, w.sub)
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping {
		return nil
	}
	if w.active < opts.Concurrency {
		w.active++
		w.b.dispatch(ctx, env, w.sub)
		return nil
	}
	if opts.QueueLimit <= 0 || len(w.queue) < opts.QueueLimit {
		w.queue = append(w.queue, env)
		return nil
	}
	if opts.Overflow == OverflowDrop {
		// A queue-full drop loses the message, so it reports at ERROR like every
		// other drop. There is no handler error to attach — the delivery never
		// ran. ctx is the publisher's context, so the record still correlates
		// with the boundary that published the message.
		w.b.logs.broker.ErrorCtx(ctx, "message dropped", nil, eventAttr(env, map[string]any{
			"reason":     dropQueueFull,
			"queueLimit": opts.QueueLimit,
			"outcome":    outcomeFailure,
		}))
		w.b.observeDropped(env, dropQueueFull)
		return nil
	}
	return errors.New(CodeEventsQueueFull, "handler queue is full",
		errors.String("topic", env.Topic),
		errors.String("message_id", env.ID),
	)
}

func (w *subscriptionWorker) complete(ctx context.Context) {
	opts := w.sub.def.Options
	if opts.Concurrency <= 0 {
		return
	}

	var next *Envelope
	w.mu.Lock()
	w.active--
	if w.active < 0 {
		w.active = 0
	}
	if !w.stopping && len(w.queue) > 0 {
		env := w.queue[0]
		copy(w.queue, w.queue[1:])
		w.queue = w.queue[:len(w.queue)-1]
		w.active++
		next = &env
	}
	w.mu.Unlock()

	if next != nil {
		w.b.dispatch(ctx, *next, w.sub)
	}
}

func (w *subscriptionWorker) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopping = true
	w.queue = nil
}

// executeHandler runs the handler with timeout inside the shared per-delivery
// logging boundary (exactly one terminal record) and routes failures to
// retry/DLQ. The retry/DLQ records run on the boundary context returned by
// dispatchDelivery so they correlate with that terminal record.
func (b *MemoryBroker) executeHandler(ctx context.Context, env Envelope, sub *subscription) {
	deliveryCtx, duration, err := dispatchDelivery(ctx, b.logs, &env, func(deliveryCtx context.Context, msg *Envelope) error {
		handlerCtx, cancel := context.WithTimeout(deliveryCtx, handlerTimeout(sub.def.Options))
		defer cancel()
		return sub.def.Handler(handlerCtx, msg)
	})
	b.observeHandled(deliveryCtx, env, duration, err)
	if err != nil {
		b.handleFailure(deliveryCtx, err, env, sub)
	}
}

// handleFailure schedules a retry or sends to DLQ.
func (b *MemoryBroker) handleFailure(ctx context.Context, err error, env Envelope, sub *subscription) {
	opts := normalizeHandlerOptions(sub.def.Options)
	switch {
	case env.Attempt < opts.MaxRetries:
		b.scheduleRetry(ctx, env, sub, err)
	case opts.DLQ:
		b.sendToDLQ(ctx, env, err)
	default:
		// Retries are exhausted and no DLQ is configured, so the message is lost.
		// attempt is env.Attempt — the true final delivery count. It used to
		// report env.Attempt+1, which disagreed with the attempt recorded on the
		// dead-letter envelope for the very same failure.
		b.logs.broker.ErrorCtx(ctx, "message dropped", err, eventAttr(env, map[string]any{
			"reason":  dropRetriesExhausted,
			"outcome": outcomeFailure,
		}))
	}
}

// scheduleRetry re-delivers the envelope after exponential backoff with jitter,
// using the shared retry-backoff policy. The re-delivery itself uses a fresh
// context so retries are not canceled by the original request context, but it
// does abort promptly when the broker is stopped so pending retries neither delay
// shutdown nor fire a handler after Stop returns. ctx is used for logging only:
// it carries the failed delivery's trace id and event group.
func (b *MemoryBroker) scheduleRetry(ctx context.Context, env Envelope, sub *subscription, originalErr error) {
	opts := normalizeHandlerOptions(sub.def.Options)
	delay := retryBackoff(env.Attempt, opts.BaseBackoff, opts.MaxBackoff)
	b.observeRetried(env, delay, originalErr)
	// WARNING: the failure is recoverable, so it carries the structured error as
	// an attr rather than promoting the record to ERROR.
	b.logs.broker.WarnCtx(ctx, "message retry scheduled",
		retryEventAttr(env, delay, opts),
		logger.ErrorAttr(originalErr),
	)

	b.mu.RLock()
	quit := b.quit
	b.mu.RUnlock()

	b.inFlight.Add(1)
	go func() {
		defer b.inFlight.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-quit:
			// Broker is shutting down; abandon the retry instead of dispatching
			// after Stop.
			b.observeDropped(env, "broker_stopped")
			return
		}
		retryEnv := env
		retryEnv.Attempt++
		if err := sub.worker.enqueue(context.Background(), retryEnv); err != nil {
			// The re-delivery could not be queued, so the message is lost. ctx is
			// read for its trace id and bag only (never for cancellation), so using
			// the failed delivery's context here keeps the drop correlated.
			b.logs.broker.ErrorCtx(ctx, "message dropped", err, eventAttr(retryEnv, map[string]any{
				"reason":  dropRetryEnqueueFailed,
				"outcome": outcomeFailure,
			}))
			b.observeDropped(retryEnv, dropRetryEnqueueFailed)
		}
	}()
}

// sendToDLQ publishes the failed envelope to the dead-letter topic.
func (b *MemoryBroker) sendToDLQ(ctx context.Context, env Envelope, originalErr error) {
	dlqTopic := env.Topic + ".dlq"

	b.mu.RLock()
	subs := b.subscriptions[dlqTopic]
	b.mu.RUnlock()

	if len(subs) == 0 {
		// DLQ is enabled but no consumer is wired for the .dlq topic, so the
		// dead-letter vanishes: that is a DROP, not a dead-letter — nothing will
		// ever receive the message, and an operator must not be pointed at a DLQ
		// that never got it. It reports the true final attempt (env.Attempt), the
		// same value the dead-letter path records.
		b.logs.broker.ErrorCtx(ctx, "message dropped", originalErr, eventAttr(env, map[string]any{
			"reason":   dropDLQNoSubscriber,
			"dlqTopic": dlqTopic,
			"outcome":  outcomeFailure,
		}))
		b.observeDropped(env, dropDLQNoSubscriber)
		return
	}

	dlqEnv := env
	dlqEnv.Topic = dlqTopic
	dlqEnv.Attempt = 1
	if dlqEnv.Attributes == nil {
		dlqEnv.Attributes = make(map[string]string)
	}
	dlqEnv.Attributes["dlq.original_topic"] = env.Topic
	dlqEnv.Attributes["dlq.original_attempt"] = fmt.Sprintf("%d", env.Attempt)
	dlqEnv.Attributes["dlq.error"] = originalErr.Error()

	// Enqueue FIRST, then report the message-level outcome exactly once: a
	// dead-letter is only real once something has accepted it. Emitting the record
	// at the decision point (as this did before) let one message claim both
	// "message dead-lettered" and a dlq_enqueue_failed "message dropped", pointing
	// an operator at a DLQ the message never reached. The same
	// accepted-then-report ordering is implemented by redis_stream.go and by the
	// TypeScript twins (server/failure-router.ts, redis/stream-transport.ts).
	accepted := 0
	var enqueueErr error
	for _, sub := range subs {
		if err := sub.worker.enqueue(ctx, dlqEnv); err != nil {
			if enqueueErr == nil {
				enqueueErr = err
			}
			continue
		}
		accepted++
	}

	if accepted == 0 {
		// No subscriber took it, so the message is lost: a DROP, reported once for
		// the message (never once per refusing subscriber — the record describes the
		// message's fate, not each attempt to hand it over).
		b.logs.broker.ErrorCtx(ctx, "message dropped", enqueueErr, eventAttr(env, map[string]any{
			"reason":   dropDLQEnqueueFailed,
			"dlqTopic": dlqTopic,
			"outcome":  outcomeFailure,
		}))
		b.observeDropped(dlqEnv, dropDLQEnqueueFailed)
		return
	}

	// One dead-letter record per dead-letter (never one per .dlq subscriber), and
	// no drop record alongside it: at least one subscriber accepted, so the message
	// is not lost. The group describes the ORIGINAL delivery — its topic and its
	// true final attempt — plus the dlqTopic the message was routed to; dlqEnv's
	// own topic/attempt are the .dlq delivery's, reported by that delivery's own
	// terminal record.
	b.observeDeadLettered(dlqEnv, originalErr)
	b.logs.broker.ErrorCtx(ctx, "message dead-lettered", originalErr, eventAttr(env, map[string]any{
		"dlqTopic": dlqTopic,
		"outcome":  outcomeFailure,
	}))
}

func (b *MemoryBroker) observePublished(ctx context.Context, env Envelope) {
	if b.config.Observer != nil {
		b.config.Observer.Published(ctx, env)
	}
}

func (b *MemoryBroker) observeHandled(ctx context.Context, env Envelope, duration time.Duration, err error) {
	if b.config.Observer != nil {
		b.config.Observer.Handled(ctx, env, duration, err)
	}
}

func (b *MemoryBroker) observeRetried(env Envelope, delay time.Duration, err error) {
	if b.config.Observer != nil {
		b.config.Observer.Retried(env, delay, err)
	}
}

func (b *MemoryBroker) observeDeadLettered(env Envelope, err error) {
	if b.config.Observer != nil {
		b.config.Observer.DeadLettered(env, err)
	}
}

func (b *MemoryBroker) observeDropped(env Envelope, reason string) {
	if b.config.Observer != nil {
		b.config.Observer.Dropped(env, reason)
	}
}
