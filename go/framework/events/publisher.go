package events

import (
	"context"
	"log/slog"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/logger"
)

// CodeEventsNoTransport is the error code for missing transport configuration.
const CodeEventsNoTransport errors.Code = "events.no_transport"

// Publisher publishes typed events to a transport.
type Publisher[T any] struct {
	topic     *Topic[T]
	transport Transport
	log       *logger.Logger
}

// NewPublisher creates a publisher for the given topic and transport.
func NewPublisher[T any](topic *Topic[T], transport Transport) *Publisher[T] {
	return &Publisher[T]{
		topic:     topic,
		transport: transport,
		log:       logger.Default().Named("events.publish"),
	}
}

// Publish sends a typed payload to the topic.
func (p *Publisher[T]) Publish(ctx context.Context, payload T, opts ...PublishOption) error {
	if p.transport == nil {
		return errors.New(CodeEventsNoTransport, "no transport configured")
	}
	if p.topic.Validate != nil {
		if err := p.topic.Validate(payload); err != nil {
			return errors.Wrap(err, CodeEventsInvalidPayload, errors.String("topic", p.topic.Name))
		}
	}
	options := &PublishOptions{}
	for _, opt := range opts {
		opt(options)
	}
	messageID := options.MessageID
	if messageID == "" {
		messageID = generateID()
	}

	env := Envelope{
		Protocol:     ProtocolVersion,
		ID:           messageID,
		Topic:        p.topic.Name,
		Channel:      p.topic.Channel,
		Payload:      payload,
		Key:          options.Key,
		DedupeKey:    options.DedupeKey,
		TopicVersion: p.topic.Version,
		Timestamp:    time.Now(),
		Attributes:   options.Attributes,
		// Attempt is the 1-based delivery count: the first delivery is attempt 1.
		Attempt: 1,
		TraceID: options.TraceID,
	}
	if err := p.transport.Publish(ctx, env); err != nil {
		return err
	}
	// Accumulate onto the CALLER's boundary (never a bag of our own): inside an
	// HTTP request the publishes summarize on that request's terminal record, and
	// the paired counter survives the logger's append cap. Both are no-ops when
	// the caller has no field bag, which is why the debug record below carries
	// this publish's own fields.
	logger.Append(ctx, "event.publishes", map[string]any{"topic": env.Topic, "messageId": env.ID})
	logger.Increment(ctx, "event.publishCount", 1)
	p.log.DebugCtx(ctx, "message published", slog.Any("event", map[string]any{
		"topic":     env.Topic,
		"messageId": env.ID,
	}))
	return nil
}

// PublishOption is a functional option for publish calls.
type PublishOption func(*PublishOptions)

// WithMessageID sets a stable message id.
func WithMessageID(messageID string) PublishOption {
	return func(o *PublishOptions) {
		o.MessageID = messageID
	}
}

// WithKey sets a routing or partition key.
func WithKey(key string) PublishOption {
	return func(o *PublishOptions) {
		o.Key = key
	}
}

// WithDedupeKey sets an idempotency key.
func WithDedupeKey(dedupeKey string) PublishOption {
	return func(o *PublishOptions) {
		o.DedupeKey = dedupeKey
	}
}

// WithAttributes sets custom routing attributes on the published message.
func WithAttributes(attrs map[string]string) PublishOption {
	return func(o *PublishOptions) {
		o.Attributes = attrs
	}
}

// WithTraceID sets an explicit trace ID on the published message.
func WithTraceID(traceID string) PublishOption {
	return func(o *PublishOptions) {
		o.TraceID = traceID
	}
}

// Publish is a convenience function that publishes a typed event through a transport.
func Publish[T any](ctx context.Context, transport Transport, topic *Topic[T], payload T, opts ...PublishOption) error {
	if transport == nil {
		return errors.New(CodeEventsNoTransport, "no transport configured")
	}
	pub := NewPublisher(topic, transport)
	return pub.Publish(ctx, payload, opts...)
}
