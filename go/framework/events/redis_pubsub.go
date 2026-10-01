package events

import (
	"context"
	"encoding/json"
	"sync"

	"go.putnami.dev/errors"
)

// codeEventsRedisPubSub identifies Redis pub/sub transport failures.
const codeEventsRedisPubSub errors.Code = "events.redis_pubsub"

// RedisPubSubTransport is a live-only fanout transport. It is useful for
// realtime notifications and local fanout, but it does not provide replay,
// competing consumers, retries, or DLQ guarantees. Use RedisStreamTransport for
// reliable background handlers.
type RedisPubSubTransport struct {
	client    RedisPubSubClient
	keyPrefix string

	mu      sync.Mutex
	subs    []*redisPubSubSubscription
	running bool
	logs    eventLoggers
}

// RedisPubSubClient is the minimal client surface needed by RedisPubSubTransport.
type RedisPubSubClient interface {
	Publish(ctx context.Context, channel string, message string) error
	Subscribe(ctx context.Context, channel string, handler func(context.Context, string)) error
	Unsubscribe(ctx context.Context, channel string) error
}

// RedisPubSubTransportConfig configures a Redis pub/sub event transport.
type RedisPubSubTransportConfig struct {
	Client    RedisPubSubClient
	KeyPrefix string
}

type redisPubSubSubscription struct {
	def     *HandlerDefinition
	channel string
}

// NewRedisPubSubTransport creates a live-only Redis pub/sub event transport.
func NewRedisPubSubTransport(config RedisPubSubTransportConfig) *RedisPubSubTransport {
	return &RedisPubSubTransport{
		client:    config.Client,
		keyPrefix: config.KeyPrefix,
		logs:      newEventLoggers(),
	}
}

// Publish sends an envelope to a Redis pub/sub channel.
func (t *RedisPubSubTransport) Publish(ctx context.Context, env Envelope) error {
	if t.client == nil {
		return errors.New(codeEventsRedisPubSub, "redis pubsub client is required")
	}
	data, err := json.Marshal(env)
	if err != nil {
		return errors.Wrap(err, codeEventsRedisPubSub, errors.String("phase", "marshal"))
	}
	return t.client.Publish(ctx, t.channelFor(env.Topic), string(data))
}

// Subscribe registers a handler on a Redis pub/sub channel.
func (t *RedisPubSubTransport) Subscribe(def *HandlerDefinition) error {
	if t.client == nil {
		return errors.New(codeEventsRedisPubSub, "redis pubsub client is required")
	}
	sub := &redisPubSubSubscription{def: def, channel: t.channelFor(def.Topic)}
	t.mu.Lock()
	t.subs = append(t.subs, sub)
	running := t.running
	t.mu.Unlock()
	if running {
		return t.subscribe(context.Background(), sub)
	}
	return nil
}

// Start subscribes all registered handlers to Redis pub/sub channels.
func (t *RedisPubSubTransport) Start(ctx context.Context) error {
	t.mu.Lock()
	t.running = true
	subs := append([]*redisPubSubSubscription(nil), t.subs...)
	t.mu.Unlock()
	for _, sub := range subs {
		if err := t.subscribe(ctx, sub); err != nil {
			return err
		}
	}
	return nil
}

// Stop unsubscribes registered handlers and closes the client when supported.
func (t *RedisPubSubTransport) Stop(ctx context.Context) error {
	t.mu.Lock()
	t.running = false
	subs := append([]*redisPubSubSubscription(nil), t.subs...)
	t.mu.Unlock()
	var first error
	for _, sub := range subs {
		if err := t.client.Unsubscribe(ctx, sub.channel); err != nil && first == nil {
			first = err
		}
	}
	if closer, ok := t.client.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (t *RedisPubSubTransport) subscribe(ctx context.Context, sub *redisPubSubSubscription) error {
	return t.client.Subscribe(ctx, sub.channel, func(messageCtx context.Context, raw string) {
		var env Envelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			return
		}
		// Strip caller-supplied auth.* on ingress (anti-spoofing), matching push.
		stripAuthAttributes(env.Attributes)
		// The failure is reported by the delivery's single terminal record
		// ("message handling failed", ERROR, structured error). This live-only
		// transport has no retry or DLQ, so there is nothing else to report and no
		// second record to emit.
		_, _ = invokeTransportHandler(messageCtx, t.logs, sub.def, env) //nolint:errcheck // the terminal record is the report; pub/sub cannot nack
	})
}

func (t *RedisPubSubTransport) channelFor(topic string) string {
	return redisKey(t.keyPrefix, topic)
}
