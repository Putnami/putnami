package events

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

func TestHandleDecodesJSONPayload(t *testing.T) {
	type Payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	topic := NewTopic[Payload]("user.created")
	done := make(chan struct{})
	var got Payload

	def := Handle(topic, func(_ context.Context, msg *Message[Payload]) error {
		got = msg.Payload
		close(done)
		return nil
	})

	env := Envelope{
		ID:         "message-1",
		Topic:      topic.Name,
		Payload:    map[string]any{"id": "u1", "name": "Ada"},
		Timestamp:  time.Now(),
		Attributes: map[string]string{},
		Attempt:    1,
	}
	if err := def.Handler(context.Background(), &env); err != nil {
		t.Fatalf("handler: %v", err)
	}
	<-done
	if got.ID != "u1" || got.Name != "Ada" {
		t.Fatalf("decoded payload = %+v", got)
	}
}

func TestPublisherValidatesPayload(t *testing.T) {
	topic := NewTopic[string]("validated", WithTopicValidator[string](func(value string) error {
		if value == "" {
			return stderrors.New("empty")
		}
		return nil
	}))
	broker := NewMemoryBroker()
	if err := broker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer broker.Stop(context.Background())

	err := NewPublisher(topic, broker).Publish(context.Background(), "")
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestManualAckRequiresAck(t *testing.T) {
	topic := NewTopic[string]("manual")
	def := Handle(topic, func(_ context.Context, _ *Message[string]) error {
		return nil
	}, WithAckMode(ManualAck))
	err := def.Handler(context.Background(), &Envelope{
		ID:         "m1",
		Topic:      topic.Name,
		Payload:    "hello",
		Timestamp:  time.Now(),
		Attributes: map[string]string{},
		Attempt:    1,
	})
	if err == nil {
		t.Fatal("expected missing ack error")
	}
}

func TestManualAckAllowsAck(t *testing.T) {
	topic := NewTopic[string]("manual")
	def := Handle(topic, func(_ context.Context, msg *Message[string]) error {
		msg.Ack()
		return nil
	}, WithAckMode(ManualAck))
	err := def.Handler(context.Background(), &Envelope{
		ID:         "m1",
		Topic:      topic.Name,
		Payload:    "hello",
		Timestamp:  time.Now(),
		Attributes: map[string]string{},
		Attempt:    1,
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
}

func TestManualAckNackReturnsNackError(t *testing.T) {
	topic := NewTopic[string]("manual")
	def := Handle(topic, func(_ context.Context, msg *Message[string]) error {
		return msg.Nack("boom")
	}, WithAckMode(ManualAck))
	err := def.Handler(context.Background(), &Envelope{
		ID:         "m1",
		Topic:      topic.Name,
		Payload:    "hello",
		Timestamp:  time.Now(),
		Attributes: map[string]string{},
		Attempt:    1,
	})
	if err == nil {
		t.Fatal("expected nack error")
	}
	if !errors.Is(err, CodeEventsNack) {
		t.Fatalf("error code = %v, want %s", err, CodeEventsNack)
	}
}

func TestManualAckNackTriggersRetryAndDLQ(t *testing.T) {
	topic := NewTopic[string]("manual-nack")
	broker := NewMemoryBroker()

	var attempts atomic.Int32
	h := Handle(topic, func(_ context.Context, msg *Message[string]) error {
		attempts.Add(1)
		return msg.Nack("boom")
	}, WithAckMode(ManualAck), WithMaxRetries(2), WithDLQ(true), WithMaxBackoff(10*time.Millisecond))
	broker.Subscribe(h)

	dlqReceived := make(chan *Envelope, 1)
	broker.Subscribe(&HandlerDefinition{
		Topic:   "manual-nack.dlq",
		Options: DefaultHandlerOptions(),
		Handler: func(_ context.Context, env *Envelope) error {
			dlqReceived <- env
			return nil
		},
	})

	broker.Start(context.Background())
	defer broker.Stop(context.Background())

	if err := Publish(context.Background(), broker, topic, "will-nack"); err != nil {
		t.Fatal(err)
	}

	select {
	case env := <-dlqReceived:
		if env.Attributes["dlq.original_topic"] != "manual-nack" {
			t.Errorf("dlq original topic = %q, want %q", env.Attributes["dlq.original_topic"], "manual-nack")
		}
		if !strings.Contains(env.Attributes["dlq.error"], string(CodeEventsNack)) {
			t.Errorf("dlq error = %q, want it to mention %s", env.Attributes["dlq.error"], CodeEventsNack)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for DLQ delivery after nack retries")
	}

	if got := attempts.Load(); got < 2 {
		t.Errorf("expected at least 2 handler attempts before DLQ, got %d", got)
	}
}

func TestMemoryBrokerStopCancelsPendingRetries(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "shutdown", "stop-cancels-pending-retries")
	topic := NewTopic[string]("retry-shutdown")
	broker := NewMemoryBroker(MemoryBrokerConfig{DrainTimeout: 2 * time.Second})

	var attempts atomic.Int32
	h := Handle(topic, func(_ context.Context, _ *Message[string]) error {
		attempts.Add(1)
		return context.DeadlineExceeded // always fails → schedules a retry
	}, WithMaxRetries(5), WithMaxBackoff(60*time.Second))
	broker.Subscribe(h)
	broker.Start(context.Background())

	if err := Publish(context.Background(), broker, topic, "boom"); err != nil {
		t.Fatal(err)
	}

	// Wait for the first invocation to fail and schedule a retry whose backoff
	// (~1s) far exceeds the time before we stop the broker.
	deadline := time.After(2 * time.Second)
	for attempts.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("handler was never invoked")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	pending := attempts.Load()

	start := time.Now()
	if err := broker.Stop(context.Background()); err != nil {
		t.Fatalf("stop returned %v with a retry in flight, want prompt nil shutdown", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stop took %v with a pending retry, want prompt shutdown", elapsed)
	}

	// Give any (incorrectly) surviving retry timer time to fire.
	time.Sleep(200 * time.Millisecond)
	if got := attempts.Load(); got != pending {
		t.Fatalf("handler fired after stop: attempts %d → %d", pending, got)
	}
}

func TestMemoryBrokerConcurrencyAndOverflowDrop(t *testing.T) {
	topic := NewTopic[int]("limited")
	broker := NewMemoryBroker()
	block := make(chan struct{})
	var handled atomic.Int32

	broker.Subscribe(Handle(topic, func(_ context.Context, msg *Message[int]) error {
		handled.Add(1)
		if msg.Payload == 1 {
			<-block
		}
		return nil
	}, WithConcurrency(1), WithQueueLimit(1), WithOverflow(OverflowDrop)))
	if err := broker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer broker.Stop(context.Background())

	for i := 1; i <= 3; i++ {
		if err := Publish(context.Background(), broker, topic, i); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	close(block)
	time.Sleep(100 * time.Millisecond)
	if got := handled.Load(); got != 2 {
		t.Fatalf("handled = %d, want 2", got)
	}
}

type recordingObserver struct {
	published atomic.Int32
	handled   atomic.Int32
	dropped   atomic.Int32
	// handledCh signals that Handled fired. Observer.Handled runs AFTER the
	// handler returns (the delivery's terminal record is emitted in between), so a
	// test that waits on something the handler itself closes has no ordering
	// guarantee against this callback: it must wait on the callback it asserts.
	// The send is non-blocking (and never a close) so a second delivery cannot
	// block or panic the broker's dispatch goroutine.
	handledCh chan struct{}
}

func (o *recordingObserver) Published(context.Context, Envelope) {
	o.published.Add(1)
}

func (o *recordingObserver) Handled(context.Context, Envelope, time.Duration, error) {
	o.handled.Add(1)
	select {
	case o.handledCh <- struct{}{}:
	default:
	}
}

func (o *recordingObserver) Retried(Envelope, time.Duration, error) {}
func (o *recordingObserver) DeadLettered(Envelope, error)           {}
func (o *recordingObserver) Dropped(Envelope, string) {
	o.dropped.Add(1)
}

func TestMemoryBrokerObserver(t *testing.T) {
	observer := &recordingObserver{handledCh: make(chan struct{}, 1)}
	topic := NewTopic[string]("observed")
	broker := NewMemoryBroker(MemoryBrokerConfig{Observer: observer})
	broker.Subscribe(Handle(topic, func(_ context.Context, _ *Message[string]) error {
		return nil
	}))
	if err := broker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer broker.Stop(context.Background())
	if err := Publish(context.Background(), broker, topic, "hello"); err != nil {
		t.Fatal(err)
	}
	// Wait on the callback under assertion, not on the handler: Handled firing
	// already implies the handler ran and returned.
	select {
	case <-observer.handledCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Observer.Handled")
	}
	if observer.published.Load() != 1 || observer.handled.Load() != 1 {
		t.Fatalf("observer counts: published=%d handled=%d", observer.published.Load(), observer.handled.Load())
	}
}

func TestLocalServerTransportRoundTrip(t *testing.T) {
	type Payload struct {
		ID string `json:"id"`
	}
	server := NewLocalServer(LocalServerConfig{Port: 0})
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())

	transport := NewLocalServerTransport("http://"+server.listener.Addr().String(), server.Token())
	topic := NewTopic[Payload]("remote.created")
	done := make(chan Payload, 1)
	if err := transport.Subscribe(Handle(topic, func(_ context.Context, msg *Message[Payload]) error {
		done <- msg.Payload
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer transport.Stop(context.Background())

	if err := NewPublisher(topic, transport).Publish(context.Background(), Payload{ID: "p1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.ID != "p1" {
			t.Fatalf("payload = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for local server delivery")
	}
}

func TestEnvelopeJSONCompatibleWithProtocol(t *testing.T) {
	env := Envelope{
		Protocol:   ProtocolVersion,
		ID:         "message-1",
		Topic:      "order.placed",
		Payload:    map[string]any{"id": "o1"},
		Timestamp:  time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
		Attributes: map[string]string{},
		Attempt:    1,
		TraceID:    "trace-1",
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"protocol", "id", "topic", "payload", "timestamp", "attempt", "traceId"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("missing key %s in %s", key, string(data))
		}
	}
}

func TestLocalServerHealth(t *testing.T) {
	server := NewLocalServer(LocalServerConfig{Port: 0})
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer server.Stop(context.Background())

	res, err := http.Get(fmt.Sprintf("http://%s/health", server.listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
}
