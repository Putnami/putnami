// Package events provides a typed event system with pluggable transports.
//
// Topics define typed event contracts, handlers subscribe to topics,
// and publishers emit events through a configurable transport layer.
package events

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"go.putnami.dev/errors"
	protoevents "go.putnami.dev/protocol/events"
)

// Topic defines a typed event topic. Topics are pure data contracts
// that carry no transport logic and can be shared across services.
type Topic[T any] struct {
	Name     string
	Version  string
	Channel  string
	Metadata map[string]string
	Validate func(T) error
}

// TopicOption configures topic metadata.
type TopicOption[T any] func(*Topic[T])

// NewTopic creates a new typed topic definition.
func NewTopic[T any](name string, opts ...TopicOption[T]) *Topic[T] {
	t := &Topic[T]{Name: name}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// WithTopicVersion sets the topic contract version.
func WithTopicVersion[T any](version string) TopicOption[T] {
	return func(t *Topic[T]) {
		t.Version = version
	}
}

// WithTopicChannel sets the logical protocol channel used for routing.
func WithTopicChannel[T any](channel string) TopicOption[T] {
	return func(t *Topic[T]) {
		t.Channel = channel
	}
}

// WithTopicMetadata sets static topic metadata.
func WithTopicMetadata[T any](metadata map[string]string) TopicOption[T] {
	return func(t *Topic[T]) {
		t.Metadata = metadata
	}
}

// WithTopicValidator sets an optional runtime payload validator.
func WithTopicValidator[T any](validator func(T) error) TopicOption[T] {
	return func(t *Topic[T]) {
		t.Validate = validator
	}
}

// Message represents a delivered event message with typed payload.
type Message[T any] struct {
	ID           string
	Topic        string
	Channel      string
	Payload      T
	Key          string
	DedupeKey    string
	TopicVersion string
	Timestamp    time.Time
	Attributes   map[string]string
	Attempt      int
	TraceID      string
	ack          *ackState
}

// Ack acknowledges the message in manual ack mode. It is a no-op in auto mode.
func (m *Message[T]) Ack() {
	if m.ack != nil {
		m.ack.ack()
	}
}

// Nack negatively acknowledges the message in manual ack mode. It returns an
// error so handlers can `return msg.Nack("reason")` to trigger retry or DLQ.
func (m *Message[T]) Nack(reason string) error {
	if m.ack == nil {
		return nil
	}
	return m.ack.nack(reason)
}

// Envelope is the untyped internal representation used by transports.
type Envelope struct {
	Protocol     string            `json:"protocol,omitempty"`
	ID           string            `json:"id"`
	Topic        string            `json:"topic"`
	Channel      string            `json:"channel,omitempty"`
	Payload      any               `json:"payload"`
	Key          string            `json:"key,omitempty"`
	DedupeKey    string            `json:"dedupeKey,omitempty"`
	TopicVersion string            `json:"topicVersion,omitempty"`
	Timestamp    time.Time         `json:"timestamp"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	// Attempt is the 1-based delivery count of THIS delivery: a freshly published
	// message is attempt 1, its first retry is attempt 2, and so on. It is the
	// value every log record of the delivery reports as event.attempt
	// (protocols/logging/conformance, "Attempt semantics"): a retry record reports
	// the failed delivery's attempt plus nextAttempt = attempt + 1, and
	// exhausted-retry / dead-letter records report the true final attempt — never
	// attempt + 1. Transports normalize a missing or zero wire value to 1 on
	// ingress, and a dead-letter envelope restarts at 1 (the pre-DLQ count travels
	// in its dlq.original_attempt attribute). The JSON name stays "attempt".
	Attempt int    `json:"attempt"`
	TraceID string `json:"traceId,omitempty"`
}

// PublishOptions configures how a message is published.
type PublishOptions struct {
	// MessageID is a stable message id. Generated when omitted.
	MessageID string
	// Key is a routing or partition key.
	Key string
	// DedupeKey is an idempotency key used by handlers or transports.
	DedupeKey string
	// Attributes are key-value metadata attached to the message for filtering and routing.
	Attributes map[string]string
	// TraceID is an optional trace identifier for correlating events with distributed traces.
	TraceID string
}

// ProtocolVersion is the canonical events protocol identifier.
const ProtocolVersion = protoevents.Protocol

const (
	// CodeEventsInvalidPayload identifies payload encoding or decoding failures.
	CodeEventsInvalidPayload errors.Code = "events.invalid_payload"
	// CodeEventsNack identifies a manual negative acknowledgement.
	CodeEventsNack errors.Code = "events.nack"
	// CodeEventsMissingAck identifies a manual handler that returned without Ack/Nack.
	CodeEventsMissingAck errors.Code = "events.missing_ack"
)

func decodePayload[T any](payload any) (T, error) {
	if typed, ok := payload.(T); ok {
		return typed, nil
	}

	var zero T
	var data []byte
	switch v := payload.(type) {
	case json.RawMessage:
		data = v
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return zero, errors.Wrap(err, CodeEventsInvalidPayload, errors.String("phase", "marshal"))
		}
	}
	if len(data) == 0 {
		return zero, errors.New(CodeEventsInvalidPayload, "empty payload")
	}
	if err := json.Unmarshal(data, &zero); err != nil {
		return zero, errors.Wrap(err, CodeEventsInvalidPayload, errors.String("phase", "unmarshal"))
	}
	return zero, nil
}

type ackState struct {
	manual bool
	acked  bool
	nacked bool
	reason string
}

func newAckState(mode AckMode) *ackState {
	return &ackState{
		manual: mode == ManualAck,
		acked:  mode != ManualAck,
	}
}

func (s *ackState) ack() {
	if s.manual {
		s.acked = true
	}
}

func (s *ackState) nack(reason string) error {
	if !s.manual {
		return nil
	}
	s.nacked = true
	s.reason = reason
	if reason == "" {
		reason = "message negatively acknowledged"
	}
	return errors.New(CodeEventsNack, reason)
}

func (s *ackState) assert() error {
	if s == nil || !s.manual {
		return nil
	}
	if s.nacked {
		reason := s.reason
		if reason == "" {
			reason = "message negatively acknowledged"
		}
		return errors.New(CodeEventsNack, reason)
	}
	if !s.acked {
		return errors.New(CodeEventsMissingAck, "message was not acknowledged")
	}
	return nil
}

// generateID creates a unique message ID.
func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
