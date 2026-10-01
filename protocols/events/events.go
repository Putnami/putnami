// Package events provides the canonical Putnami events protocol.
//
// Framework implementations, Putnami Event Plane, self-hosted Event Servers,
// mobile clients, and broker adapters should use these names and shapes to
// avoid cross-language drift.
package events

import "encoding/json"

// Protocol is the canonical Putnami events protocol identifier.
const Protocol = "putnami.events.v1"

// FrameType enumerates WebSocket/mobile Event Server frame discriminator values.
type FrameType string

// Event Server frame types.
const (
	FrameAuth      FrameType = "auth"
	FrameSubscribe FrameType = "subscribe"
	FramePublish   FrameType = "publish"
	FrameEvent     FrameType = "event"
	FrameAck       FrameType = "ack"
	FrameNack      FrameType = "nack"
	FrameError     FrameType = "error"
	FrameComplete  FrameType = "complete"
)

// EventServerTransport enumerates Event Server endpoint profiles.
//
// Putnami Event Plane is the managed Event Server implementation: the simplest
// hosted path for SSE, WebSocket, HTTP publish, gRPC, replay, routing, and
// provider abstraction.
type EventServerTransport string

// Event Server endpoint profiles.
const (
	EventServerTransportSSE       EventServerTransport = "sse"
	EventServerTransportWebSocket EventServerTransport = "websocket"
	EventServerTransportHTTP      EventServerTransport = "http"
	EventServerTransportGRPC      EventServerTransport = "grpc"
)

// Well-known and default endpoint names used by compatible Event Servers.
const (
	EventServerDiscoveryPath        = "/.well-known/putnami/events"
	DefaultEventServerSSEPath       = "/events/stream"
	DefaultEventServerWebSocketPath = "/events/ws"
	DefaultEventServerPublishPath   = "/events/publish"
	DefaultEventServerHealthPath    = "/events/health"
	DefaultEventServerGRPCService   = "putnami.events.v1.EventServer"
	DefaultGRPCSubscribeRPC         = "Subscribe"
	DefaultGRPCPublishRPC           = "Publish"
	DefaultGRPCCapabilityRPC        = "Capabilities"
	DefaultGRPCHealthCheckRPC       = "Health"
)

// DeliveryProfile enumerates how a subscriber receives events.
//
// It is orthogonal to EventServerTransport (the server-hosted wire profiles
// sse|websocket|http|grpc): a delivery profile is the delivery *model* for a
// subscription, not a concrete server transport.
//
//   - pull:   the subscriber pulls or long-polls events from a transport.
//   - stream: the subscriber holds an open stream and receives events live.
//   - push:   the provider POSTs each event to a subscriber-hosted receiver
//     endpoint and an HTTP 2xx acknowledges delivery. Push is realized by the
//     provider (for example GCP Pub/Sub push subscriptions), not by an Event
//     Server transport, and carries the additional receiver contract specified
//     by schemas/push-delivery.json.
type DeliveryProfile string

// Delivery profile values.
const (
	DeliveryProfilePull   DeliveryProfile = "pull"
	DeliveryProfileStream DeliveryProfile = "stream"
	DeliveryProfilePush   DeliveryProfile = "push"
)

// DefaultDeliveryProfile is the delivery profile assumed when none is declared.
const DefaultDeliveryProfile = DeliveryProfilePull

// DefaultReceiverPath is the conventional route a workload exposes to receive
// provider push delivery. The trailing ":subscription" segment carries the
// provider subscription name. Both framework receivers register this path.
const DefaultReceiverPath = "/_putnami/events/:subscription"

// AuthScheme enumerates authentication schemes advertised by Event Servers.
type AuthScheme string

// Auth scheme values.
const (
	AuthNone             AuthScheme = "none"
	AuthBearer           AuthScheme = "bearer"
	AuthHeaders          AuthScheme = "headers"
	AuthCookie           AuthScheme = "cookie"
	AuthServiceAccount   AuthScheme = "serviceAccount"
	AuthWorkloadIdentity AuthScheme = "workloadIdentity"
)

// CursorMode enumerates replay cursor forms accepted by subscribe endpoints.
type CursorMode string

// Cursor mode values.
const (
	CursorLatest       CursorMode = "latest"
	CursorEarliest     CursorMode = "earliest"
	CursorMessageID    CursorMode = "message-id"
	CursorBrokerCursor CursorMode = "broker-cursor"
)

// PayloadEncoding enumerates payload encodings supported at protocol edges.
type PayloadEncoding string

// Payload encoding values.
const (
	PayloadJSON PayloadEncoding = "json"
)

// ErrorCode enumerates canonical Event Server error codes.
type ErrorCode string

// Event Server error code values.
const (
	ErrorUnauthorized        ErrorCode = "unauthorized"
	ErrorForbidden           ErrorCode = "forbidden"
	ErrorProtocolMismatch    ErrorCode = "protocol_mismatch"
	ErrorInvalidFrame        ErrorCode = "invalid_frame"
	ErrorInvalidEnvelope     ErrorCode = "invalid_envelope"
	ErrorInvalidTopic        ErrorCode = "invalid_topic"
	ErrorInvalidCursor       ErrorCode = "invalid_cursor"
	ErrorUnsupportedFeature  ErrorCode = "unsupported_feature"
	ErrorPayloadTooLarge     ErrorCode = "payload_too_large"
	ErrorRateLimited         ErrorCode = "rate_limited"
	ErrorUpstreamUnavailable ErrorCode = "upstream_unavailable"
	ErrorInternal            ErrorCode = "internal"
)

// Distribution controls how events are delivered to handler instances.
type Distribution string

// Distribution values.
const (
	DistributionCompeting Distribution = "competing"
	DistributionBroadcast Distribution = "broadcast"
)

// AckMode controls handler acknowledgement semantics.
type AckMode string

// AckMode values.
const (
	AckAuto   AckMode = "auto"
	AckManual AckMode = "manual"
)

// Overflow controls behavior when local handler queues are full.
type Overflow string

// Overflow values.
const (
	OverflowThrow Overflow = "throw"
	OverflowDrop  Overflow = "drop"
)

// Envelope is the canonical event message shape exchanged between
// publishers, transports, handlers, broker adapters, and cloud services.
type Envelope struct {
	// Protocol is the events protocol identifier (Protocol); optional on the wire
	// but stamped by producers.
	Protocol string `json:"protocol,omitempty"`
	// ID is the globally unique event identifier used for dedupe and acking.
	ID string `json:"id"`
	// Topic is the logical topic the event was published to.
	Topic string `json:"topic"`
	// Channel narrows delivery within a topic (e.g. a tenant partition).
	Channel string `json:"channel,omitempty"`
	// Payload is the application event body, carried opaquely as raw JSON.
	Payload json.RawMessage `json:"payload"`
	// Key is the partition/ordering key; events with the same Key preserve order.
	Key string `json:"key,omitempty"`
	// DedupeKey is the idempotency key brokers use to drop duplicate publishes.
	DedupeKey string `json:"dedupeKey,omitempty"`
	// TopicVersion is the schema version of the topic's payload, when versioned.
	TopicVersion string `json:"topicVersion,omitempty"`
	// Timestamp is the event's RFC 3339 production time.
	Timestamp string `json:"timestamp"`
	// Attributes are broker/transport metadata carried alongside the payload.
	Attributes map[string]string `json:"attributes,omitempty"`
	// Attempt is the 1-based delivery attempt count (increments on redelivery).
	Attempt int `json:"attempt"`
	// TraceID correlates the event with a distributed trace, when propagated.
	TraceID string `json:"traceId,omitempty"`
}

// EventServerFrame is the canonical WebSocket/mobile Event Server frame shape.
//
// The type field determines which fields are required:
//   - auth: token or headers may be present
//   - subscribe: topic is required
//   - publish: topic and payload are required
//   - event: id, topic, payload are required
//   - ack/nack: id is required
//   - error: message or error is required
//   - complete: no additional fields are required
type EventServerFrame struct {
	// Protocol is the events protocol identifier (Protocol).
	Protocol string `json:"protocol"`
	// Type is the frame discriminator that determines which fields are required.
	Type FrameType `json:"type"`

	// ID is the event/ack identifier (required on event and ack/nack frames).
	ID string `json:"id,omitempty"`
	// Topic is the frame's topic (required on subscribe/publish/event frames).
	Topic string `json:"topic,omitempty"`
	// Channel narrows delivery within a topic.
	Channel string `json:"channel,omitempty"`
	// From identifies the sender of an event frame, when set.
	From string `json:"from,omitempty"`
	// Payload is the event body (required on publish/event frames).
	Payload json.RawMessage `json:"payload,omitempty"`
	// Key is the partition/ordering key.
	Key string `json:"key,omitempty"`
	// DedupeKey is the publish idempotency key.
	DedupeKey string `json:"dedupeKey,omitempty"`
	// TopicVersion is the payload schema version, when versioned.
	TopicVersion string `json:"topicVersion,omitempty"`
	// Timestamp is the frame's RFC 3339 time.
	Timestamp string `json:"timestamp,omitempty"`
	// Attributes are transport metadata carried with the frame.
	Attributes map[string]string `json:"attributes,omitempty"`
	// TraceID correlates the frame with a distributed trace.
	TraceID string `json:"traceId,omitempty"`

	// Token is the bearer credential on an auth frame.
	Token string `json:"token,omitempty"`
	// Headers carry auth or transport headers on an auth frame.
	Headers map[string]string `json:"headers,omitempty"`
	// Message is the human-readable text on an error frame.
	Message string `json:"message,omitempty"`
	// Error is the machine error code on an error frame.
	Error string `json:"error,omitempty"`
}

// EventServerEndpoint describes one concrete Event Server endpoint.
type EventServerEndpoint struct {
	// Method is the HTTP method for an HTTP/SSE endpoint, when applicable.
	Method string `json:"method,omitempty"`
	// Path is the URL path for an HTTP/SSE/WebSocket endpoint.
	Path string `json:"path,omitempty"`
	// Service is the fully-qualified gRPC service name for a gRPC endpoint.
	Service string `json:"service,omitempty"`
}

// EventServerEndpoints lists endpoints exposed by a compatible Event Server.
type EventServerEndpoints struct {
	// SSE is the server-sent-events stream endpoint, when offered.
	SSE *EventServerEndpoint `json:"sse,omitempty"`
	// WebSocket is the bidirectional WebSocket endpoint, when offered.
	WebSocket *EventServerEndpoint `json:"websocket,omitempty"`
	// Publish is the HTTP publish endpoint, when offered.
	Publish *EventServerEndpoint `json:"publish,omitempty"`
	// Health is the health-check endpoint, when offered.
	Health *EventServerEndpoint `json:"health,omitempty"`
	// GRPC is the gRPC endpoint, when offered.
	GRPC *EventServerEndpoint `json:"grpc,omitempty"`
}

// EventServerFeatures describes behavioral capabilities exposed by an Event Server.
type EventServerFeatures struct {
	// Replay reports whether the server supports cursor-based replay.
	Replay bool `json:"replay"`
	// Publish reports whether the server accepts client publishes.
	Publish bool `json:"publish"`
	// Ack reports whether the server supports explicit acknowledgement.
	Ack bool `json:"ack"`
	// Auth lists the authentication schemes the server accepts.
	Auth []AuthScheme `json:"auth,omitempty"`
	// Cursor lists the replay cursor modes the server accepts.
	Cursor []CursorMode `json:"cursor,omitempty"`
	// Payload lists the payload encodings the server supports.
	Payload []PayloadEncoding `json:"payload,omitempty"`
}

// EventServerLimits describes optional Event Server limits.
type EventServerLimits struct {
	// MaxPayloadBytes is the largest single event payload the server accepts.
	MaxPayloadBytes int `json:"maxPayloadBytes,omitempty"`
	// MaxFrameBytes is the largest single frame the server accepts.
	MaxFrameBytes int `json:"maxFrameBytes,omitempty"`
	// MaxSubscriptions is the max concurrent subscriptions per connection.
	MaxSubscriptions int `json:"maxSubscriptions,omitempty"`
	// MaxTopicsPerSubscription is the max topics a single subscription may cover.
	MaxTopicsPerSubscription int `json:"maxTopicsPerSubscription,omitempty"`
}

// EventServerInfo identifies the Event Server implementation.
type EventServerInfo struct {
	// Name is the server implementation name.
	Name string `json:"name,omitempty"`
	// Version is the server implementation version.
	Version string `json:"version,omitempty"`
}

// EventServerCapabilities is returned by GET /.well-known/putnami/events.
type EventServerCapabilities struct {
	// Protocol is the events protocol identifier (Protocol).
	Protocol string `json:"protocol"`
	// Server identifies the implementation serving this discovery document.
	Server EventServerInfo `json:"server,omitempty"`
	// Transports lists the wire transports the server hosts.
	Transports []EventServerTransport `json:"transports"`
	// DeliveryProfiles lists the delivery models the server supports.
	DeliveryProfiles []DeliveryProfile `json:"deliveryProfiles,omitempty"`
	// Features describes the server's behavioral capabilities.
	Features EventServerFeatures `json:"features"`
	// Endpoints locates each transport's concrete endpoint.
	Endpoints EventServerEndpoints `json:"endpoints"`
	// Limits declares the server's operational limits, when advertised.
	Limits EventServerLimits `json:"limits,omitempty"`
}

// EventServerError is the canonical structured error payload for HTTP/gRPC
// endpoints and the canonical content behind WebSocket/SSE error frames.
type EventServerError struct {
	// Protocol is the events protocol identifier (Protocol).
	Protocol string `json:"protocol"`
	// Code is the canonical error code (see Error* values).
	Code ErrorCode `json:"code"`
	// Message is the human-readable error description.
	Message string `json:"message"`
	// Retryable reports whether the client may safely retry the request.
	Retryable bool `json:"retryable,omitempty"`
	// Details carries structured, error-specific context.
	Details map[string]string `json:"details,omitempty"`
}

// PushAckOutcome classifies how a push receiver's HTTP response status maps to
// at-least-once delivery acknowledgement under provider push delivery.
type PushAckOutcome string

// Push ack outcomes.
const (
	// PushAck acknowledges the message (HTTP 2xx); the provider drops it.
	PushAck PushAckOutcome = "ack"
	// PushDLQ permanently fails the message (HTTP 4xx); the provider routes it
	// to the dead-letter destination without further retries.
	PushDLQ PushAckOutcome = "dlq"
	// PushRetry transiently fails the message (HTTP 5xx, timeout, or any other
	// status); the provider redelivers with backoff.
	PushRetry PushAckOutcome = "retry"
)

// PushOutcomeForStatus maps an HTTP response status returned by a push receiver
// to its delivery acknowledgement outcome:
//
//	2xx        -> ack   (delivered; the provider drops the message)
//	4xx        -> dlq   (permanent failure; the provider dead-letters it)
//	otherwise  -> retry (transient; the provider redelivers with backoff)
//
// Push delivery is at-least-once, so receivers must be idempotent.
func PushOutcomeForStatus(status int) PushAckOutcome {
	switch {
	case status >= 200 && status < 300:
		return PushAck
	case status >= 400 && status < 500:
		return PushDLQ
	default:
		return PushRetry
	}
}

// PushMessage is the inner message of a provider push delivery. It mirrors the
// GCP Pub/Sub push message shape; Data is the base64-encoded canonical Envelope
// JSON (see DecodePushEnvelope).
type PushMessage struct {
	// Data is the base64-encoded canonical Envelope JSON (see DecodePushEnvelope).
	Data string `json:"data"`
	// Attributes are provider message attributes carried alongside Data.
	Attributes map[string]string `json:"attributes,omitempty"`
	// MessageID is the provider-assigned message identifier.
	MessageID string `json:"messageId,omitempty"`
	// PublishTime is the provider-assigned publish time (RFC 3339).
	PublishTime string `json:"publishTime,omitempty"`
	// OrderingKey is the provider ordering key, when ordered delivery is used.
	OrderingKey string `json:"orderingKey,omitempty"`
}

// PushEnvelope is the provider push wrapper POSTed to a subscriber's receiver
// endpoint (DefaultReceiverPath). It mirrors the GCP Pub/Sub push body:
//
//	{ "message": { "data": "<base64 Envelope JSON>", "attributes": {...} },
//	  "subscription": "projects/<p>/subscriptions/<s>" }
//
// The receiver authenticates the request out-of-band via an OIDC bearer token
// (audience-pinned, minted as the events-server service account; verified
// against the provider JWKS with an issuer + audience + service-account-email
// allowlist, fail-closed) — never via fields in this body. See
// doc/11-push-delivery.md.
type PushEnvelope struct {
	// Message is the provider push message wrapping the base64 Envelope JSON.
	Message PushMessage `json:"message"`
	// Subscription is the provider subscription resource name that delivered the
	// message (e.g. "projects/<p>/subscriptions/<s>").
	Subscription string `json:"subscription"`
}

// HandlerOptions is the canonical handler option vocabulary.
type HandlerOptions struct {
	// Group is the consumer group; handlers sharing a group compete for events
	// under DistributionCompeting.
	Group string `json:"group,omitempty"`
	// Distribution selects competing vs broadcast delivery across handler
	// instances (default DistributionCompeting).
	Distribution Distribution `json:"distribution,omitempty"`
	// MaxRetries bounds redelivery attempts before the event is dead-lettered.
	MaxRetries int `json:"maxRetries,omitempty"`
	// MaxBackoffMs caps the exponential redelivery backoff, in milliseconds.
	MaxBackoffMs int `json:"maxBackoffMs,omitempty"`
	// TimeoutMs bounds a single handler invocation, in milliseconds.
	TimeoutMs int `json:"timeoutMs,omitempty"`
	// Concurrency caps concurrent in-flight invocations (0 = framework default).
	Concurrency int `json:"concurrency,omitempty"`
	// QueueLimit caps the local pending-event queue (0 = framework default).
	QueueLimit int `json:"queueLimit,omitempty"`
	// Overflow selects behavior when the local queue is full (default
	// OverflowThrow).
	Overflow Overflow `json:"overflow,omitempty"`
	// DLQ enables dead-lettering of exhausted events; nil applies DefaultDLQ.
	DLQ *bool `json:"dlq,omitempty"`
	// Ack selects automatic vs manual acknowledgement (default AckAuto).
	Ack AckMode `json:"ack,omitempty"`
}

// Default handler option values.
const (
	DefaultDistribution = DistributionCompeting
	DefaultMaxRetries   = 10
	DefaultMaxBackoffMs = 60_000
	DefaultTimeoutMs    = 30_000
	DefaultConcurrency  = 0
	DefaultQueueLimit   = 0
	DefaultOverflow     = OverflowThrow
	DefaultAck          = AckAuto
)

// DefaultDLQ is the canonical default DLQ behavior.
const DefaultDLQ = true

// NormalizedHandlerOptions returns opts with canonical defaults applied.
func NormalizedHandlerOptions(opts HandlerOptions) HandlerOptions {
	if opts.Distribution == "" {
		opts.Distribution = DefaultDistribution
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = DefaultMaxRetries
	}
	if opts.MaxBackoffMs == 0 {
		opts.MaxBackoffMs = DefaultMaxBackoffMs
	}
	if opts.TimeoutMs == 0 {
		opts.TimeoutMs = DefaultTimeoutMs
	}
	if opts.Overflow == "" {
		opts.Overflow = DefaultOverflow
	}
	if opts.Ack == "" {
		opts.Ack = DefaultAck
	}
	if opts.DLQ == nil {
		v := DefaultDLQ
		opts.DLQ = &v
	}
	return opts
}
