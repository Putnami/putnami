package events

import (
	"context"
	"reflect"
	"runtime"
	"time"
)

// Distribution controls how messages are delivered to handlers.
type Distribution string

const (
	// Competing distributes messages round-robin across handlers (only one receives each message).
	Competing Distribution = "competing"
	// Broadcast delivers each message to all matching handlers.
	Broadcast Distribution = "broadcast"
)

// AckMode controls acknowledgement semantics.
type AckMode string

const (
	// AutoAck maps nil handler error to ack and non-nil error to nack.
	AutoAck AckMode = "auto"
	// ManualAck is reserved for transports that expose explicit ack/nack APIs.
	ManualAck AckMode = "manual"
)

// Overflow controls behavior when a local handler queue is full.
type Overflow string

const (
	// OverflowThrow returns an error when a queue limit is reached.
	OverflowThrow Overflow = "throw"
	// OverflowDrop drops messages when a queue limit is reached.
	OverflowDrop Overflow = "drop"
)

// HandlerOptions configures handler behavior.
type HandlerOptions struct {
	// Group is a stable subscription or consumer group name for external transports.
	Group string
	// Distribution controls message delivery: Competing (one handler per message) or Broadcast (all handlers). Default: Competing.
	Distribution Distribution
	// MaxRetries is the maximum number of retry attempts for failed handlers. Default: 10.
	MaxRetries int
	// BaseBackoff is the initial backoff before the first retry. Subsequent
	// retries grow exponentially (BaseBackoff * 2^(attempt-1)) and are capped at
	// MaxBackoff. Default: 1s. Mirrors client.RetryConfig.BaseDelay so the two
	// retry subsystems are tunable on the same dimension; set it below a second
	// for fast-failing handlers that should retry sooner.
	BaseBackoff time.Duration
	// MaxBackoff caps the exponential backoff delay between retries. Default: 60s.
	MaxBackoff time.Duration
	// Timeout is the maximum duration for a single handler invocation. Default: 30s.
	Timeout time.Duration
	// Concurrency limits concurrent handler invocations. 0 means unlimited.
	Concurrency int
	// QueueLimit limits queued deliveries waiting for concurrency slots. 0 means unlimited.
	QueueLimit int
	// Overflow controls behavior when QueueLimit is reached.
	Overflow Overflow
	// DLQ enables dead-letter queue for messages that exhaust all retries. Default: true.
	DLQ bool
	// Ack controls acknowledgement semantics.
	Ack AckMode
}

// DefaultHandlerOptions returns handler options with sensible defaults.
func DefaultHandlerOptions() HandlerOptions {
	return HandlerOptions{
		Distribution: Competing,
		MaxRetries:   10,
		BaseBackoff:  time.Second,
		MaxBackoff:   60 * time.Second,
		Timeout:      30 * time.Second,
		Overflow:     OverflowThrow,
		DLQ:          true,
		Ack:          AutoAck,
	}
}

// HandlerFunc is the function signature for event handlers.
type HandlerFunc func(ctx context.Context, msg *Envelope) error

// HandlerDefinition describes a fully-configured handler subscription.
type HandlerDefinition struct {
	Topic        string
	Options      HandlerOptions
	Filter       map[string]string
	Handler      HandlerFunc
	designSymbol string
}

// HandlerOption is a functional option for configuring handlers.
type HandlerOption func(*HandlerDefinition)

// WithDistribution sets the handler distribution mode.
func WithDistribution(d Distribution) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.Distribution = d
	}
}

// WithGroup sets the stable subscription or consumer group name.
func WithGroup(group string) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.Group = group
	}
}

// WithMaxRetries sets the maximum number of retry attempts.
func WithMaxRetries(n int) HandlerOption {
	return func(h *HandlerDefinition) {
		if n < 1 {
			n = 1
		}
		h.Options.MaxRetries = n
	}
}

// WithBaseBackoff sets the initial backoff before the first retry. Subsequent
// retries grow exponentially from it and are capped at the max backoff.
func WithBaseBackoff(d time.Duration) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.BaseBackoff = d
	}
}

// WithMaxBackoff sets the maximum backoff duration between retries.
func WithMaxBackoff(d time.Duration) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.MaxBackoff = d
	}
}

// WithTimeout sets the handler execution timeout.
func WithTimeout(d time.Duration) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.Timeout = d
	}
}

// WithConcurrency sets the maximum concurrent handler invocations.
func WithConcurrency(n int) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.Concurrency = n
	}
}

// WithQueueLimit sets the maximum queued deliveries.
func WithQueueLimit(n int) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.QueueLimit = n
	}
}

// WithOverflow sets queue overflow behavior.
func WithOverflow(o Overflow) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.Overflow = o
	}
}

// WithDLQ enables or disables dead-letter queue for failed messages.
func WithDLQ(enabled bool) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.DLQ = enabled
	}
}

// WithAckMode sets acknowledgement mode.
func WithAckMode(mode AckMode) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Options.Ack = mode
	}
}

// WithFilter sets attribute filters for the handler.
func WithFilter(filter map[string]string) HandlerOption {
	return func(h *HandlerDefinition) {
		h.Filter = filter
	}
}

// Handle creates a HandlerDefinition for a typed topic.
func Handle[T any](topic *Topic[T], fn func(ctx context.Context, msg *Message[T]) error, opts ...HandlerOption) *HandlerDefinition {
	def := &HandlerDefinition{
		Topic:        topic.Name,
		Options:      DefaultHandlerOptions(),
		designSymbol: handlerSymbol(fn),
	}
	for _, opt := range opts {
		opt(def)
	}
	def.Handler = func(ctx context.Context, env *Envelope) error {
		payload, err := decodePayload[T](env.Payload)
		if err != nil {
			return err
		}
		ack := newAckState(def.Options.Ack)
		msg := &Message[T]{
			ID:           env.ID,
			Topic:        env.Topic,
			Channel:      env.Channel,
			Payload:      payload,
			Key:          env.Key,
			DedupeKey:    env.DedupeKey,
			TopicVersion: env.TopicVersion,
			Timestamp:    env.Timestamp,
			Attributes:   env.Attributes,
			Attempt:      env.Attempt,
			TraceID:      env.TraceID,
			ack:          ack,
		}
		if err := fn(ctx, msg); err != nil {
			return err
		}
		return ack.assert()
	}
	return def
}

func handlerSymbol(fn any) string {
	if fn == nil {
		return ""
	}
	if resolved := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()); resolved != nil {
		return resolved.Name()
	}
	return ""
}
