package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestTopicCreation(t *testing.T) {
	type UserPayload struct {
		ID   string
		Name string
	}

	topic := NewTopic[UserPayload](
		"user.created",
		WithTopicVersion[UserPayload]("v1"),
		WithTopicChannel[UserPayload]("identity"),
		WithTopicMetadata[UserPayload](map[string]string{"owner": "users"}),
	)
	if topic.Name != "user.created" {
		t.Fatalf("expected topic name 'user.created', got %q", topic.Name)
	}
	if topic.Version != "v1" {
		t.Fatalf("expected topic version 'v1', got %q", topic.Version)
	}
	if topic.Channel != "identity" {
		t.Fatalf("expected topic channel 'identity', got %q", topic.Channel)
	}
	if topic.Metadata["owner"] != "users" {
		t.Fatalf("expected topic owner metadata")
	}
}

func TestPublishAndHandle(t *testing.T) {
	type OrderEvent struct {
		OrderID string
		Amount  float64
	}

	topic := NewTopic[OrderEvent]("order.placed", WithTopicVersion[OrderEvent]("v1"), WithTopicChannel[OrderEvent]("orders"))
	broker := NewMemoryBroker()

	var received atomic.Value
	done := make(chan struct{})

	handler := Handle(topic, func(ctx context.Context, msg *Message[OrderEvent]) error {
		received.Store(msg)
		close(done)
		return nil
	})

	if err := broker.Subscribe(handler); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer broker.Stop(context.Background())

	pub := NewPublisher(topic, broker)
	err := pub.Publish(
		context.Background(),
		OrderEvent{OrderID: "123", Amount: 99.99},
		WithMessageID("message-123"),
		WithKey("order-123"),
		WithDedupeKey("order.placed:123"),
		WithTraceID("trace-abc"),
	)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for handler")
	}

	msg := received.Load().(*Message[OrderEvent])
	if msg.Topic != "order.placed" {
		t.Errorf("expected topic 'order.placed', got %q", msg.Topic)
	}
	if msg.TraceID != "trace-abc" {
		t.Errorf("expected traceID 'trace-abc', got %q", msg.TraceID)
	}
	if msg.ID != "message-123" {
		t.Errorf("expected message id 'message-123', got %q", msg.ID)
	}
	if msg.Channel != "orders" {
		t.Errorf("expected channel 'orders', got %q", msg.Channel)
	}
	if msg.TopicVersion != "v1" {
		t.Errorf("expected topic version 'v1', got %q", msg.TopicVersion)
	}
	if msg.Key != "order-123" {
		t.Errorf("expected key 'order-123', got %q", msg.Key)
	}
	if msg.DedupeKey != "order.placed:123" {
		t.Errorf("expected dedupe key 'order.placed:123', got %q", msg.DedupeKey)
	}
	if msg.Attempt != 1 {
		t.Errorf("expected attempt 1, got %d", msg.Attempt)
	}
}

func TestEnvelopeJSONOmitsEmptyAttributes(t *testing.T) {
	env := Envelope{
		Protocol:  ProtocolVersion,
		ID:        "message-123",
		Topic:     "order.placed",
		Payload:   "payload",
		Timestamp: time.Now(),
		Attempt:   1,
	}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["attributes"]; ok {
		t.Fatal("empty attributes should be omitted from canonical envelope JSON")
	}
}

func TestBroadcastDistribution(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "delivery-semantics", "broadcast-delivery-selects-every-consumer")
	topic := NewTopic[string]("notify")
	broker := NewMemoryBroker()

	var count atomic.Int32
	done := make(chan struct{})
	expected := int32(3)

	for range 3 {
		h := Handle(topic, func(ctx context.Context, msg *Message[string]) error {
			if count.Add(1) >= expected {
				close(done)
			}
			return nil
		}, WithDistribution(Broadcast))
		broker.Subscribe(h)
	}

	broker.Start(context.Background())
	defer broker.Stop(context.Background())

	Publish(context.Background(), broker, topic, "hello")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for broadcast")
	}

	if got := count.Load(); got != expected {
		t.Errorf("expected %d handlers called, got %d", expected, got)
	}
}

func TestCompetingDistribution(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "delivery-semantics", "competing-delivery-selects-one-consumer")
	topic := NewTopic[int]("work")
	broker := NewMemoryBroker()

	var counts [3]atomic.Int32
	var mu sync.Mutex
	allDone := make(chan struct{})
	totalExpected := int32(6)
	var totalReceived atomic.Int32

	for i := range 3 {
		idx := i
		h := Handle(topic, func(ctx context.Context, msg *Message[int]) error {
			counts[idx].Add(1)
			if totalReceived.Add(1) >= totalExpected {
				mu.Lock()
				defer mu.Unlock()
				select {
				case <-allDone:
				default:
					close(allDone)
				}
			}
			return nil
		}, WithDistribution(Competing))
		broker.Subscribe(h)
	}

	broker.Start(context.Background())
	defer broker.Stop(context.Background())

	// Publish 6 messages, should round-robin across 3 handlers.
	for i := range 6 {
		Publish(context.Background(), broker, topic, i)
	}

	select {
	case <-allDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for competing handlers")
	}

	// Each handler should receive exactly 2 messages (round-robin).
	for i := range counts {
		if got := counts[i].Load(); got != 2 {
			t.Errorf("handler %d: expected 2 messages, got %d", i, got)
		}
	}
}

func TestAttributeFiltering(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "delivery-semantics", "attribute-filter-selects-matching-consumers")
	topic := NewTopic[string]("filtered")
	broker := NewMemoryBroker()

	var receivedA, receivedB atomic.Int32
	done := make(chan struct{})

	hA := Handle(topic, func(ctx context.Context, msg *Message[string]) error {
		receivedA.Add(1)
		return nil
	}, WithFilter(map[string]string{"region": "us"}), WithDistribution(Broadcast))

	hB := Handle(topic, func(ctx context.Context, msg *Message[string]) error {
		receivedB.Add(1)
		if receivedB.Load() >= 1 {
			close(done)
		}
		return nil
	}, WithFilter(map[string]string{"region": "eu"}), WithDistribution(Broadcast))

	broker.Subscribe(hA)
	broker.Subscribe(hB)
	broker.Start(context.Background())
	defer broker.Stop(context.Background())

	// Publish to EU region only.
	Publish(context.Background(), broker, topic, "hello-eu", WithAttributes(map[string]string{"region": "eu"}))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for filtered handler")
	}

	// Allow a moment for any incorrect deliveries.
	time.Sleep(50 * time.Millisecond)

	if got := receivedA.Load(); got != 0 {
		t.Errorf("handler A (us) should not have received messages, got %d", got)
	}
	if got := receivedB.Load(); got != 1 {
		t.Errorf("handler B (eu) should have received 1 message, got %d", got)
	}
}

func TestRetryOnFailure(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "retry-boundary", "handler-retries-on-failure")
	topic := NewTopic[string]("retry-test")
	broker := NewMemoryBroker()

	var attempts atomic.Int32
	done := make(chan struct{})

	h := Handle(topic, func(ctx context.Context, msg *Message[string]) error {
		n := attempts.Add(1)
		if n < 3 {
			return context.DeadlineExceeded // simulate failure
		}
		close(done)
		return nil
	}, WithMaxRetries(5), WithMaxBackoff(100*time.Millisecond))

	broker.Subscribe(h)
	broker.Start(context.Background())
	defer broker.Stop(context.Background())

	Publish(context.Background(), broker, topic, "will-retry")

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for retry success")
	}

	if got := attempts.Load(); got < 3 {
		t.Errorf("expected at least 3 attempts, got %d", got)
	}
}

func TestDLQ(t *testing.T) {
	topic := NewTopic[string]("dlq-source")
	broker := NewMemoryBroker()

	dlqReceived := make(chan *Envelope, 1)

	// Handler that always fails.
	h := Handle(topic, func(ctx context.Context, msg *Message[string]) error {
		return context.DeadlineExceeded
	}, WithMaxRetries(1), WithDLQ(true), WithMaxBackoff(10*time.Millisecond))
	broker.Subscribe(h)

	// DLQ handler.
	dlqDef := &HandlerDefinition{
		Topic:   "dlq-source.dlq",
		Options: DefaultHandlerOptions(),
		Handler: func(ctx context.Context, env *Envelope) error {
			dlqReceived <- env
			return nil
		},
	}
	broker.Subscribe(dlqDef)
	broker.Start(context.Background())
	defer broker.Stop(context.Background())

	Publish(context.Background(), broker, topic, "will-fail")

	select {
	case env := <-dlqReceived:
		if env.Attributes["dlq.original_topic"] != "dlq-source" {
			t.Errorf("expected original topic 'dlq-source', got %q", env.Attributes["dlq.original_topic"])
		}
		if env.Attributes["dlq.original_attempt"] != "1" {
			t.Errorf("expected original attempt '1', got %q", env.Attributes["dlq.original_attempt"])
		}
		if env.Attempt != 1 {
			t.Errorf("expected DLQ attempt 1, got %d", env.Attempt)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for DLQ delivery")
	}
}

func TestBrokerStop(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "shutdown", "stop-rejects-new-publishes")
	broker := NewMemoryBroker()
	broker.Start(context.Background())

	if err := broker.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Publishing after stop should fail.
	err := broker.Publish(context.Background(), Envelope{Topic: "test"})
	if err == nil {
		t.Fatal("expected error publishing to stopped broker")
	}
}

func TestPublishConvenienceFunction(t *testing.T) {
	err := Publish(context.Background(), nil, NewTopic[string]("test"), "payload")
	if err == nil {
		t.Fatal("expected error with nil transport")
	}
}

func TestDrainTimeout(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "shutdown", "drain-honors-the-configured-deadline")
	topic := NewTopic[string]("slow")
	broker := NewMemoryBroker(MemoryBrokerConfig{DrainTimeout: 50 * time.Millisecond})

	started := make(chan struct{})
	h := Handle(topic, func(ctx context.Context, msg *Message[string]) error {
		close(started)
		time.Sleep(500 * time.Millisecond) // longer than drain timeout
		return nil
	})

	broker.Subscribe(h)
	broker.Start(context.Background())

	Publish(context.Background(), broker, topic, "block")

	// Wait for handler to start executing.
	<-started

	err := broker.Stop(context.Background())
	if err == nil {
		t.Fatal("expected drain timeout error")
	}
}

func TestEventDroppedAfterRetriesExhausted(t *testing.T) {
	topic := NewTopic[string]("drop-test")
	broker := NewMemoryBroker()

	var attempts atomic.Int32

	done := make(chan struct{})
	h := Handle(topic, func(ctx context.Context, msg *Message[string]) error {
		if attempts.Add(1) >= 2 {
			close(done)
		}
		return context.DeadlineExceeded
	}, WithMaxRetries(2), WithDLQ(false), WithMaxBackoff(10*time.Millisecond))

	broker.Subscribe(h)
	broker.Start(context.Background())
	defer broker.Stop(context.Background())

	Publish(context.Background(), broker, topic, "will-drop")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout: only got %d attempts", attempts.Load())
	}
}

// dropReasonObserver captures the reason passed to Observer.Dropped.
type dropReasonObserver struct {
	drops chan string
}

func (o *dropReasonObserver) Published(context.Context, Envelope)                     {}
func (o *dropReasonObserver) Handled(context.Context, Envelope, time.Duration, error) {}
func (o *dropReasonObserver) Retried(Envelope, time.Duration, error)                  {}
func (o *dropReasonObserver) DeadLettered(Envelope, error)                            {}
func (o *dropReasonObserver) Dropped(_ Envelope, reason string) {
	select {
	case o.drops <- reason:
	default:
	}
}

// A DLQ-enabled message with no .dlq subscriber must be observable, not
// silently dropped.
func TestDLQWithNoSubscriber(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "terminal-outcome", "unsubscribed-dlq-drops-the-message")
	topic := NewTopic[string]("no-dlq-sub")
	obs := &dropReasonObserver{drops: make(chan string, 4)}
	broker := NewMemoryBroker(MemoryBrokerConfig{Observer: obs})

	// Handler fails; DLQ enabled but no DLQ subscriber registered.
	h := Handle(topic, func(_ context.Context, _ *Message[string]) error {
		return context.DeadlineExceeded
	}, WithMaxRetries(0), WithDLQ(true), WithMaxBackoff(10*time.Millisecond))

	broker.Subscribe(h)
	broker.Start(context.Background())
	defer broker.Stop(context.Background())

	Publish(context.Background(), broker, topic, "fail-no-dlq")

	select {
	case reason := <-obs.drops:
		if reason != "dlq_no_subscriber" {
			t.Errorf("drop reason = %q, want dlq_no_subscriber", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: a DLQ drop with no subscriber must be observed, not silent")
	}
}

func TestPublishToTopicWithNoSubscribers(t *testing.T) {
	broker := NewMemoryBroker()
	broker.Start(context.Background())
	defer broker.Stop(context.Background())

	// Publishing to a topic with no subscribers should succeed silently.
	err := broker.Publish(context.Background(), Envelope{Topic: "nonexistent"})
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestGenerateIDUniqueness(t *testing.T) {
	ids := make(map[string]bool, 100)
	for range 100 {
		id := generateID()
		if ids[id] {
			t.Fatalf("duplicate ID: %s", id)
		}
		ids[id] = true
	}
}

func TestMemoryBrokerConfigDefaults(t *testing.T) {
	cfg := MemoryBrokerConfig{}.withDefaults()
	if cfg.DrainTimeout != 10*time.Second {
		t.Errorf("expected 10s drain timeout, got %v", cfg.DrainTimeout)
	}

	custom := MemoryBrokerConfig{DrainTimeout: 5 * time.Second}.withDefaults()
	if custom.DrainTimeout != 5*time.Second {
		t.Errorf("expected 5s drain timeout, got %v", custom.DrainTimeout)
	}
}

func TestHandlerOptions(t *testing.T) {
	topic := NewTopic[string]("opts-test")
	h := Handle(topic, func(ctx context.Context, msg *Message[string]) error {
		return nil
	},
		WithDistribution(Broadcast),
		WithMaxRetries(5),
		WithMaxBackoff(10*time.Second),
		WithTimeout(5*time.Second),
		WithGroup("workers"),
		WithConcurrency(2),
		WithQueueLimit(10),
		WithOverflow(OverflowDrop),
		WithDLQ(false),
		WithAckMode(AutoAck),
	)

	if h.Options.Distribution != Broadcast {
		t.Errorf("expected Broadcast, got %v", h.Options.Distribution)
	}
	if h.Options.MaxRetries != 5 {
		t.Errorf("expected MaxRetries 5, got %d", h.Options.MaxRetries)
	}
	if h.Options.MaxBackoff != 10*time.Second {
		t.Errorf("expected MaxBackoff 10s, got %v", h.Options.MaxBackoff)
	}
	if h.Options.Timeout != 5*time.Second {
		t.Errorf("expected Timeout 5s, got %v", h.Options.Timeout)
	}
	if h.Options.Group != "workers" {
		t.Errorf("expected group workers, got %q", h.Options.Group)
	}
	if h.Options.Concurrency != 2 {
		t.Errorf("expected concurrency 2, got %d", h.Options.Concurrency)
	}
	if h.Options.QueueLimit != 10 {
		t.Errorf("expected queue limit 10, got %d", h.Options.QueueLimit)
	}
	if h.Options.Overflow != OverflowDrop {
		t.Errorf("expected overflow drop, got %q", h.Options.Overflow)
	}
	if h.Options.DLQ != false {
		t.Errorf("expected DLQ false, got %v", h.Options.DLQ)
	}
	if h.Options.Ack != AutoAck {
		t.Errorf("expected ack auto, got %q", h.Options.Ack)
	}
}

// TestHandlerInvocationIsDeadlineBound asserts the configured handler timeout
// reaches the handler's own context. A handler that blocks on a wedged
// dependency must be cut loose so the delivery can fail and retry, rather than
// occupying its concurrency slot until the process ends.
func TestHandlerInvocationIsDeadlineBound(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "retry-boundary", "handler-invocation-is-deadline-bound")

	topic := NewTopic[string]("handler-deadline")
	broker := NewMemoryBroker()

	observed := make(chan error, 1)
	h := Handle(topic, func(ctx context.Context, _ *Message[string]) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			observed <- errors.New("handler context carried no deadline")
			return nil
		}
		if remaining := time.Until(deadline); remaining > time.Second {
			observed <- fmt.Errorf("handler deadline is %s out, want the configured 50ms bound", remaining)
			return nil
		}
		// Block past the configured bound; the handler context must be canceled.
		select {
		case <-ctx.Done():
			observed <- nil
		case <-time.After(5 * time.Second):
			observed <- errors.New("handler context was never canceled by its configured timeout")
		}
		return context.DeadlineExceeded
	}, WithTimeout(50*time.Millisecond), WithMaxRetries(0), WithDLQ(false))

	if err := broker.Subscribe(h); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer broker.Stop(context.Background())

	if err := Publish(context.Background(), broker, topic, "slow"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case err := <-observed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler was never invoked")
	}
}
