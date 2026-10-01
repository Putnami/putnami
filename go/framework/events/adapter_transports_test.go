package events

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

type fakeGoogleClient struct {
	topic *fakeGoogleTopic
	sub   *fakeGoogleSubscription
}

func (c *fakeGoogleClient) Topic(string) GooglePubSubTopic {
	if c.topic == nil {
		c.topic = &fakeGoogleTopic{}
	}
	return c.topic
}

func (c *fakeGoogleClient) Subscription(string) GooglePubSubSubscription {
	if c.sub == nil {
		c.sub = &fakeGoogleSubscription{ready: make(chan struct{})}
	}
	return c.sub
}

type fakeGoogleTopic struct {
	mu        sync.Mutex
	published []GooglePubSubPublishMessage
}

func (t *fakeGoogleTopic) Publish(_ context.Context, message GooglePubSubPublishMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.published = append(t.published, message)
	return nil
}

type fakeGoogleSubscription struct {
	mu      sync.Mutex
	ready   chan struct{}
	handler func(context.Context, GooglePubSubMessage)
}

func (s *fakeGoogleSubscription) Receive(ctx context.Context, handler func(context.Context, GooglePubSubMessage)) error {
	s.mu.Lock()
	s.handler = handler
	close(s.ready)
	s.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (s *fakeGoogleSubscription) deliver(msg *fakeGoogleMessage) {
	<-s.ready
	s.mu.Lock()
	handler := s.handler
	s.mu.Unlock()
	handler(context.Background(), msg)
}

type fakeGoogleMessage struct {
	id         string
	data       []byte
	attributes map[string]string
	acked      bool
	nacked     bool
}

func (m *fakeGoogleMessage) MessageID() string                    { return m.id }
func (m *fakeGoogleMessage) MessageData() []byte                  { return m.data }
func (m *fakeGoogleMessage) MessageAttributes() map[string]string { return m.attributes }
func (m *fakeGoogleMessage) Ack()                                 { m.acked = true }
func (m *fakeGoogleMessage) Nack()                                { m.nacked = true }

func TestDecodeGooglePubSubEnvelopeStripsAuthAttributes(t *testing.T) {
	spectest.Proves(t, "go/event-delivery", "ingress-authority", "google-pubsub-ingress-strips-auth-attributes")
	// The pull ingress paths must strip caller-supplied auth.* just like the
	// push receiver — an external publisher must not spoof a reserved auth.*.
	msg := &fakeGoogleMessage{
		id:   "m-1",
		data: []byte(`{"topic":"t","payload":{"a":1}}`),
		attributes: map[string]string{
			"auth.sub": "spoofed",
			"region":   "eu",
		},
	}
	env, err := decodeGooglePubSubEnvelope(msg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := env.Attributes["auth.sub"]; ok {
		t.Error("auth.* must be stripped on the pull path (anti-spoofing)")
	}
	if env.Attributes["region"] != "eu" {
		t.Error("non-auth attributes must be preserved")
	}
}

func TestGooglePubSubTransportPublishAndSubscribe(t *testing.T) {
	client := &fakeGoogleClient{sub: &fakeGoogleSubscription{ready: make(chan struct{})}}
	transport := NewGooglePubSubTransport(GooglePubSubTransportConfig{Client: client})
	topic := NewTopic[string]("google.topic")

	if err := NewPublisher(topic, transport).Publish(context.Background(), "hello", WithKey("k1"), WithAttributes(map[string]string{"region": "eu"})); err != nil {
		t.Fatal(err)
	}
	if len(client.topic.published) != 1 {
		t.Fatalf("published = %d, want 1", len(client.topic.published))
	}
	if client.topic.published[0].OrderingKey != "k1" || client.topic.published[0].Attributes["region"] != "eu" {
		t.Fatalf("publish metadata = %+v", client.topic.published[0])
	}

	done := make(chan string, 1)
	if err := transport.Subscribe(Handle(topic, func(_ context.Context, msg *Message[string]) error {
		done <- msg.Payload
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer transport.Stop(context.Background())

	env := Envelope{ID: "g1", Topic: topic.Name, Payload: "world", Timestamp: time.Now(), Attributes: map[string]string{}, Attempt: 1}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	msg := &fakeGoogleMessage{id: "g1", data: data}
	client.sub.deliver(msg)

	select {
	case got := <-done:
		if got != "world" {
			t.Fatalf("payload = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for google pubsub handler")
	}
	if !msg.acked || msg.nacked {
		t.Fatalf("ack state: acked=%v nacked=%v", msg.acked, msg.nacked)
	}
}

type redisCommand struct {
	name string
	args []string
}

type fakeRedisCommandClient struct {
	mu       sync.Mutex
	commands []redisCommand
	response chan any
}

func (c *fakeRedisCommandClient) Do(ctx context.Context, command string, args ...string) (any, error) {
	c.mu.Lock()
	c.commands = append(c.commands, redisCommand{name: command, args: append([]string(nil), args...)})
	c.mu.Unlock()
	if command != "XREADGROUP" {
		return "OK", nil
	}
	select {
	case response := <-c.response:
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRedisStreamTransportPublishAndSubscribe(t *testing.T) {
	client := &fakeRedisCommandClient{response: make(chan any, 1)}
	transport := NewRedisStreamTransport(RedisStreamTransportConfig{
		Client:       client,
		KeyPrefix:    "events",
		ConsumerName: "test-consumer",
		BlockTimeout: 10 * time.Millisecond,
		Count:        1,
	})
	topic := NewTopic[string]("redis.topic")

	if err := NewPublisher(topic, transport).Publish(context.Background(), "publish"); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	if client.commands[0].name != "XADD" || client.commands[0].args[0] != "events:redis.topic" {
		t.Fatalf("publish command = %+v", client.commands[0])
	}
	client.mu.Unlock()

	done := make(chan string, 1)
	if err := transport.Subscribe(Handle(topic, func(_ context.Context, msg *Message[string]) error {
		done <- msg.Payload
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer transport.Stop(context.Background())

	env := Envelope{ID: "r1", Topic: topic.Name, Payload: "consume", Timestamp: time.Now(), Attributes: map[string]string{}, Attempt: 1}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	client.response <- []any{
		[]any{"events:redis.topic", []any{
			[]any{"1-0", []any{"message", string(data)}},
		}},
	}

	select {
	case got := <-done:
		if got != "consume" {
			t.Fatalf("payload = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for redis stream handler")
	}
}

type fakeRedisPubSubClient struct {
	mu       sync.Mutex
	handlers map[string]func(context.Context, string)
}

func (c *fakeRedisPubSubClient) Publish(ctx context.Context, channel string, message string) error {
	c.mu.Lock()
	handler := c.handlers[channel]
	c.mu.Unlock()
	if handler != nil {
		handler(ctx, message)
	}
	return nil
}

func (c *fakeRedisPubSubClient) Subscribe(_ context.Context, channel string, handler func(context.Context, string)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handlers == nil {
		c.handlers = map[string]func(context.Context, string){}
	}
	c.handlers[channel] = handler
	return nil
}

func (c *fakeRedisPubSubClient) Unsubscribe(_ context.Context, channel string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.handlers, channel)
	return nil
}

func TestRedisPubSubTransportFanout(t *testing.T) {
	client := &fakeRedisPubSubClient{}
	transport := NewRedisPubSubTransport(RedisPubSubTransportConfig{Client: client, KeyPrefix: "events"})
	topic := NewTopic[string]("pubsub.topic")
	done := make(chan string, 1)
	if err := transport.Subscribe(Handle(topic, func(_ context.Context, msg *Message[string]) error {
		done <- msg.Payload
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := transport.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer transport.Stop(context.Background())

	if err := NewPublisher(topic, transport).Publish(context.Background(), "live"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != "live" {
			t.Fatalf("payload = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for redis pubsub handler")
	}
}
