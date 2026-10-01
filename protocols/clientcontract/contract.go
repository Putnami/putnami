package clientcontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	// ProtocolVersion is the only x-putnami-client protocol understood by this
	// package. Its presence at document level marks a first-party strict contract.
	ProtocolVersion = 1
	// ExtensionKey is used at both OpenAPI document and operation scope.
	ExtensionKey = "x-putnami-client"
	// SchemaURL identifies the shared schema containing document and operation defs.
	SchemaURL = "https://putnami.dev/schemas/x-putnami-client-v1.json"
	// WebSocketWireSchemaURL identifies the published first-party WebSocket frame
	// schema. It is the wire vocabulary a non-Go reader validates frames against.
	WebSocketWireSchemaURL = "https://putnami.dev/schemas/client-websocket-wire-v1.json"
	// WebSocketSubprotocolV1 is the public negotiation value recorded for a
	// first-party WebSocket transport. Credential values never appear in the
	// contract metadata or the subprotocol.
	WebSocketSubprotocolV1 = "putnami.service.v1"
	// OpaqueJSONKey is the schema keyword that declares an opaque JSON value.
	// It is an OpenAPI vendor extension, so a reader that does not know it
	// still sees a schema that accepts any value.
	OpaqueJSONKey = "x-putnami-json"
	// OpaqueJSONAny is the only value OpaqueJSONKey accepts: the value may be
	// any JSON document — null, a boolean, a number, a string, an array or an
	// object — and a generated client carries it without interpreting it.
	OpaqueJSONAny = "any"
)

// StreamMode identifies the request and response streaming cardinality.
type StreamMode string

// Supported operation streaming modes.
const (
	StreamUnary         StreamMode = "unary"
	StreamServer        StreamMode = "server"
	StreamClient        StreamMode = "client"
	StreamBidirectional StreamMode = "bidirectional"
)

// TransportProtocol identifies a generated client's wire transport.
type TransportProtocol string

// Supported generated client transport protocols.
const (
	TransportRESTJSON  TransportProtocol = "rest-json"
	TransportConnect   TransportProtocol = "connect"
	TransportSSE       TransportProtocol = "sse"
	TransportWebSocket TransportProtocol = "websocket"
)

// Encoding identifies the payload encoding used by a transport.
type Encoding string

// Supported generated client payload encodings.
const (
	EncodingJSON  Encoding = "json"
	EncodingProto Encoding = "proto"
	// EncodingBinary carries raw octets: each binary WebSocket message holds a
	// run of bytes that no schema describes. Only a provider-owned WebSocket
	// wire carries it; every other transport refuses it.
	EncodingBinary Encoding = "binary"
)

// WebSocketWire names who owns the vocabulary of a WebSocket transport's
// messages. The first-party conversation is the absent value, so a contract
// that declares only first-party transports keeps its exact bytes.
type WebSocketWire string

// Supported WebSocket wires beside the first-party conversation.
const (
	// WebSocketWireProvider is a wire the provider owns. Each message is either
	// one JSON value of the declared message types (encoding json, under the
	// subprotocol the provider declares) or a run of raw octets (encoding
	// binary). There is no conversation envelope, no in-band admission and no
	// framework resume: the upgrade request carries the declared credentials,
	// and the provider's own protocol owns everything after the handshake.
	WebSocketWireProvider WebSocketWire = "provider"
)

// WebSocketReservedSubprotocolPrefix is the namespace of the first-party
// conversation's negotiation tokens. A provider-owned wire never declares a
// token inside it, so a first-party runtime cannot mistake one for the other.
const WebSocketReservedSubprotocolPrefix = "putnami.service."

// CredentialKind identifies a credential profile supplied by a service binding.
type CredentialKind string

// Supported credential profile kinds.
const (
	CredentialServiceToken       CredentialKind = "service-token"
	CredentialForwardedUserToken CredentialKind = "forwarded-user-token"
	CredentialAPIKey             CredentialKind = "api-key"
	CredentialNamedHeader        CredentialKind = "named-header"
)

// IdempotencyKind controls which automatic retries are safe for an operation.
type IdempotencyKind string

// Supported operation idempotency classifications.
const (
	IdempotencySafe          IdempotencyKind = "safe"
	IdempotencyIdempotent    IdempotencyKind = "idempotent"
	IdempotencyNonIdempotent IdempotencyKind = "non-idempotent"
)

// DecodeContractService reads the provider identity an OpenAPI contract
// declares in its document-level x-putnami-client block, without parsing the
// paths, the schemas or the credential profiles.
//
// It is the counterpart of DecodeGeneratedClientReference: the two read the
// same service identity from the two ends of a generated client's relation to
// its provider, so a caller can pair a committed manifest with the contract it
// names. ok is false for a document with no first-party marker, for one whose
// marker does not parse, and for one that declares a blank service identity —
// none of the three identifies a provider.
func DecodeContractService(document []byte) (Service, bool) {
	var envelope struct {
		Client *struct {
			Service Service `json:"service"`
		} `json:"x-putnami-client"`
	}
	if err := json.Unmarshal(document, &envelope); err != nil || envelope.Client == nil {
		return Service{}, false
	}
	id := strings.TrimSpace(envelope.Client.Service.ID)
	if id == "" {
		return Service{}, false
	}
	return Service{ID: id, Audience: strings.TrimSpace(envelope.Client.Service.Audience)}, true
}

// DocumentV1 is the document-level x-putnami-client value. Credential entries
// are profiles only: generated artifacts never contain credential values.
type DocumentV1 struct {
	// ProtocolVersion selects the closed compatibility version parsed here.
	ProtocolVersion int `json:"protocolVersion"`
	// Service identifies the provider and its default credential audience.
	Service Service `json:"service"`
	// Credentials maps stable profile names to value-free credential metadata.
	Credentials map[string]CredentialProfile `json:"credentials"`
	// Defaults contains provider-wide client policy overrides.
	Defaults *Defaults `json:"defaults,omitempty"`
	// Protobuf preserves exact binary wire metadata when proto is advertised.
	Protobuf *ProtobufDescriptor `json:"protobuf,omitempty"`
}

// Service identifies the provider and the audience used for client credentials.
type Service struct {
	// ID is the stable provider service identity.
	ID string `json:"id"`
	// Audience is the provider's default service-token audience.
	Audience string `json:"audience"`
}

// CredentialProfile declares credential acquisition or injection metadata.
type CredentialProfile struct {
	// Kind selects how a future consumer binding supplies the credential.
	Kind CredentialKind `json:"kind"`
	// Audience overrides the service audience for service-token acquisition.
	Audience string `json:"audience,omitempty"`
	// Scopes lists provider-wide service-token scopes in authored order.
	Scopes []string `json:"scopes,omitempty"`
	// Header names the injection header for API-key and named-header profiles.
	Header string `json:"header,omitempty"`
}

// Defaults contains provider-wide generated client policies.
type Defaults struct {
	// Resilience contains provider-wide bounded resilience hints.
	Resilience *ResiliencePolicy `json:"resilience,omitempty"`
}

// OperationV1 is the operation-level x-putnami-client value. Transports are in
// preference/fallback order; security alternatives are OR and each AllOf is AND.
type OperationV1 struct {
	// Stream declares unary or request/response streaming cardinality.
	Stream StreamMode `json:"stream"`
	// Messages preserves logical stream input and output schemas.
	Messages *MessageShapes `json:"messages,omitempty"`
	// Transports lists wire alternatives in provider preference order.
	Transports []Transport `json:"transports"`
	// Security records ordered OR alternatives of AND requirements.
	Security Security `json:"security"`
	// Errors lists declared framework errors for typed client projection.
	Errors []DeclaredError `json:"errors"`
	// Idempotency states retry safety and any request identity header.
	Idempotency Idempotency `json:"idempotency"`
	// Resilience overrides document defaults for this operation.
	Resilience *ResiliencePolicy `json:"resilience,omitempty"`
}

// MessageShapes preserves logical stream messages when HTTP upgrade responses
// cannot carry request and response schemas.
type MessageShapes struct {
	// Input is the logical message sent on client-streaming operations.
	Input *Schema `json:"input,omitempty"`
	// Output is the logical message received on streaming operations.
	Output *Schema `json:"output,omitempty"`
}

// Transport declares one supported wire representation of an operation.
type Transport struct {
	// Protocol selects the wire transport family.
	Protocol TransportProtocol `json:"protocol"`
	// Path is the absolute same-authority request path.
	Path string `json:"path"`
	// Encoding selects JSON values or protobuf bytes.
	Encoding Encoding `json:"encoding"`
	// ProtobufMethod is the canonical /package.Service/Method identity.
	ProtobufMethod string `json:"protobufMethod,omitempty"`
	// WebSocket carries negotiation metadata only for WebSocket transports.
	WebSocket *WebSocketTransport `json:"websocket,omitempty"`
	// SSE carries continuation metadata only for SSE transports. It is absent
	// when the provider declares no continuation, so such a transport keeps its
	// exact bytes.
	SSE *SSETransport `json:"sse,omitempty"`
}

// SSETransport records how a first-party server-sent event stream continues
// after its connection breaks (ADR 0013).
type SSETransport struct {
	// Continuation is the provider half of the continuation agreement. The
	// consumer half is the effective resilience.stream.reconnect.
	Continuation *SSEContinuation `json:"continuation"`
}

// SSEContinuationMode names how a reopened connection continues a stream.
type SSEContinuationMode string

// Supported SSE continuation modes.
const (
	// SSEContinuationCursor reopens after the last message the consumer
	// received: the opaque position that message carries in a declared output
	// field travels back in a declared query parameter, and the provider
	// continues exclusively after it, on any instance.
	SSEContinuationCursor SSEContinuationMode = "cursor"
	// SSEContinuationBestEffort reopens the same operation with the original
	// selector and no position. Messages produced while no connection was open
	// may be missing, and the provider may repeat some: it is not lossless.
	SSEContinuationBestEffort SSEContinuationMode = "best-effort"
)

// SSEContinuation declares one continuation mode for an SSE transport.
type SSEContinuation struct {
	// Mode selects cursor or best-effort continuation.
	Mode SSEContinuationMode `json:"mode"`
	// Cursor names where the position travels. It is required in cursor mode
	// and absent in best-effort mode.
	Cursor *SSECursor `json:"cursor,omitempty"`
}

// SSECursor names the two carriers of a cursor-mode position. The runtime
// copies the value from one to the other and never parses, orders, increments
// or synthesizes it.
type SSECursor struct {
	// OutputField is the required string property of every output message
	// that carries the provider's position after that message.
	OutputField string `json:"outputField"`
	// QueryParameter is the declared string query parameter a reopened
	// connection carries the last delivered position in.
	QueryParameter string `json:"queryParameter"`
}

// Continuation returns the continuation an SSE transport declares, or nil when
// the transport is not SSE or declares none.
func (t Transport) Continuation() *SSEContinuation {
	if t.Protocol != TransportSSE || t.SSE == nil {
		return nil
	}
	return t.SSE.Continuation
}

// WebSocketTransport records public negotiation metadata, whether the provider
// declares gap-free resume for a safe server stream, and who owns the wire.
type WebSocketTransport struct {
	// Subprotocol is the public negotiation token. The first-party
	// conversation negotiates WebSocketSubprotocolV1; a provider-owned wire
	// negotiates the token its provider declares, and a provider-owned byte
	// stream may declare none. It never carries a credential.
	Subprotocol string `json:"subprotocol,omitempty"`
	// Resume states whether this transport can resume without gaps.
	Resume bool `json:"resume"`
	// Wire is WebSocketWireProvider for a provider-owned wire and absent for
	// the first-party conversation.
	Wire WebSocketWire `json:"wire,omitempty"`
}

// ProviderWire reports whether the transport is a provider-owned WebSocket
// wire rather than the first-party conversation.
func (t Transport) ProviderWire() bool {
	return t.Protocol == TransportWebSocket && t.WebSocket != nil && t.WebSocket.Wire == WebSocketWireProvider
}

// ByteStream reports whether the transport is a provider-owned WebSocket wire
// whose messages are raw octets.
func (t Transport) ByteStream() bool {
	return t.ProviderWire() && t.Encoding == EncodingBinary
}

// OperationProviderWire returns the operation's provider-owned WebSocket
// transport, or nil when every transport it declares is a first-party one. A
// valid operation declares at most one, and then nothing else.
func OperationProviderWire(operation *OperationV1) *Transport {
	if operation == nil {
		return nil
	}
	for i := range operation.Transports {
		if operation.Transports[i].ProviderWire() {
			return &operation.Transports[i]
		}
	}
	return nil
}

// ValidWebSocketSubprotocol reports whether value is a negotiable RFC 6455
// subprotocol token: a non-empty RFC 9110 token, which excludes separators,
// whitespace and control characters, so it cannot smuggle a second token or a
// header into the handshake.
func ValidWebSocketSubprotocol(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// Security declares credential alternatives and provider authorization checks.
type Security struct {
	// Alternatives lists OR choices in provider preference order.
	Alternatives []SecurityAlternative `json:"alternatives"`
	// Authorization preserves provider-side representable claim checks.
	Authorization *Authorization `json:"authorization,omitempty"`
}

// SecurityAlternative contains requirements that must all be satisfied together.
type SecurityAlternative struct {
	// AllOf lists credential requirements that must all be satisfied.
	AllOf []SecurityRequirement `json:"allOf"`
}

// SecurityRequirement selects a credential profile and its required claims.
type SecurityRequirement struct {
	// Profile refers to a document-level credential profile.
	Profile string `json:"profile"`
	// Scopes are the claims required from the selected credential.
	Scopes []string `json:"scopes,omitempty"`
	// Roles are the roles required from the selected credential.
	Roles []string `json:"roles,omitempty"`
}

// Authorization preserves representable declarative checks enforced by the
// provider. Custom verifier and guard functions are intentionally absent: a
// first-party provider using either must fail contract generation.
type Authorization struct {
	// Issuers lists accepted issuer claim values.
	Issuers []string `json:"issuers,omitempty"`
	// Audiences lists accepted audience claim values.
	Audiences []string `json:"audiences,omitempty"`
	// PrincipalKinds lists accepted provider principal classifications.
	PrincipalKinds []string `json:"principalKinds,omitempty"`
	// Clients lists accepted client identities.
	Clients []string `json:"clients,omitempty"`
	// ScopesAll lists scopes that must all be present.
	ScopesAll []string `json:"scopesAll,omitempty"`
	// ScopesAny lists scopes of which at least one must be present.
	ScopesAny []string `json:"scopesAny,omitempty"`
	// RolesAll lists roles that must all be present.
	RolesAll []string `json:"rolesAll,omitempty"`
	// RolesAny lists roles of which at least one must be present.
	RolesAny []string `json:"rolesAny,omitempty"`
	// ScopeClaims lists claim names from which scopes are read.
	ScopeClaims []string `json:"scopeClaims,omitempty"`
	// RoleClaims lists claim names from which roles are read.
	RoleClaims []string `json:"roleClaims,omitempty"`
}

// DeclaredError maps a provider error to a typed generated client error.
type DeclaredError struct {
	// Status is the HTTP status associated with the declared error.
	Status int `json:"status"`
	// Code is the stable framework error code.
	Code string `json:"code"`
	// GRPCCode is the optional canonical gRPC status code.
	GRPCCode *int `json:"grpcCode,omitempty"`
	// Schema describes safe structured error details when present.
	Schema *Schema `json:"schema,omitempty"`
	// Retryable is the provider's explicit retryability advisory.
	Retryable *bool `json:"retryable,omitempty"`
}

// Idempotency declares an operation's retry safety and optional request key.
type Idempotency struct {
	// Kind classifies automatic retry safety.
	Kind IdempotencyKind `json:"kind"`
	// KeyHeader carries the stable request identity for idempotent operations.
	KeyHeader string `json:"keyHeader,omitempty"`
}

// ResiliencePolicy declares bounded time, retry, circuit, and stream behavior.
type ResiliencePolicy struct {
	// TimeoutMs bounds the total operation in milliseconds.
	TimeoutMs *int `json:"timeoutMs,omitempty"`
	// AttemptTimeoutMs bounds each individual attempt in milliseconds.
	AttemptTimeoutMs *int `json:"attemptTimeoutMs,omitempty"`
	// MaxResponseBytes bounds unary response bodies.
	MaxResponseBytes *int64 `json:"maxResponseBytes,omitempty"`
	// Retry contains bounded retry policy hints.
	Retry *RetryPolicy `json:"retry,omitempty"`
	// Circuit contains circuit-breaker policy hints.
	Circuit *CircuitPolicy `json:"circuit,omitempty"`
	// Stream contains streaming lifetime and buffer policy hints.
	Stream *StreamPolicy `json:"stream,omitempty"`
	// Cache declares that a generated client may answer this operation from a
	// bounded in-process response cache. Only a unary operation whose
	// idempotency is safe or idempotent may declare it, and only at operation
	// scope: a document default would silently cache operations that never
	// asked for it.
	Cache *CachePolicy `json:"cache,omitempty"`
}

// CachePolicy declares how a generated client caches one unary operation's
// successful answers. Entries are always partitioned by the service binding
// and by the forwarded user identity carried by the call, whatever KeyFields
// says. That is the runtime's whole partition: a tenant carried any other way
// (a header set from the request context, a field left out of KeyFields) is
// the provider's to put in the key.
type CachePolicy struct {
	// FreshMs is the age, in milliseconds, below which a stored answer is
	// returned without calling the provider.
	FreshMs int `json:"freshMs"`
	// StaleMs is the age, in milliseconds, below which a stored answer is
	// returned when the provider call fails with a transport error, retry
	// exhaustion or an open circuit. Absent, a failure is never masked. It must
	// exceed FreshMs.
	StaleMs *int `json:"staleMs,omitempty"`
	// MaxEntries bounds the least-recently-used entries kept for this
	// operation per service binding. Absent, the runtime default applies.
	MaxEntries *int `json:"maxEntries,omitempty"`
	// KeyFields lists, in key order, the request fields that form the cache
	// key: "path.<name>", "query.<name>", "header.<name>", "body", or
	// "body.<property>" for one top-level body property. Absent, the key is
	// every path, query and header parameter plus the whole canonical body.
	KeyFields []string `json:"keyFields,omitempty"`
	// InvalidationFields lists top-level properties of the JSON success body
	// that tag a stored answer, so a consumer can drop every answer carrying a
	// value it learns about outside the request — a revoked principal the
	// request never named. Each names a string, integer or boolean property,
	// never a byte or binary string, which a TypeScript runtime holds as octets;
	// absent, an answer is dropped by key prefix only.
	InvalidationFields []string `json:"invalidationFields,omitempty"`
}

// Cache key field sections. A key field is "<section>.<name>", or "body"
// alone for the whole canonical request body.
const (
	CacheKeySectionPath   = "path"
	CacheKeySectionQuery  = "query"
	CacheKeySectionHeader = "header"
	CacheKeySectionBody   = "body"
)

// DefaultCacheMaxEntries is the per-operation entry bound both runtimes apply
// when a cache policy omits maxEntries.
const DefaultCacheMaxEntries = 1000

// RetryPolicy declares bounded retry attempts and retryable outcomes.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first.
	MaxAttempts *int `json:"maxAttempts,omitempty"`
	// Statuses lists retryable HTTP statuses.
	Statuses []int `json:"statuses,omitempty"`
	// Codes lists retryable framework error codes.
	Codes []string `json:"codes,omitempty"`
}

// CircuitPolicy declares circuit breaker thresholds and reset timing.
type CircuitPolicy struct {
	// FailureThreshold is the consecutive failure count that opens the circuit.
	FailureThreshold *int `json:"failureThreshold,omitempty"`
	// ResetTimeoutMs is the delay before a half-open probe.
	ResetTimeoutMs *int `json:"resetTimeoutMs,omitempty"`
}

// StreamPolicy declares time and buffering bounds for streaming operations.
type StreamPolicy struct {
	// HandshakeTimeoutMs bounds the connect-to-admission phase on its own budget,
	// independently of the declared operation duration. It defaults to
	// attemptTimeoutMs when absent.
	HandshakeTimeoutMs *int `json:"handshakeTimeoutMs,omitempty"`
	// IdleTimeoutMs bounds time without stream activity.
	IdleTimeoutMs *int `json:"idleTimeoutMs,omitempty"`
	// HeartbeatMs declares heartbeat cadence.
	HeartbeatMs *int `json:"heartbeatMs,omitempty"`
	// Reconnect permits automatic reconnect only when transport semantics allow it.
	Reconnect *bool `json:"reconnect,omitempty"`
	// MaxBufferedMessages bounds queued stream messages.
	MaxBufferedMessages *int `json:"maxBufferedMessages,omitempty"`
	// MaxFrameBytes bounds one encoded stream frame.
	MaxFrameBytes *int64 `json:"maxFrameBytes,omitempty"`
}

// ProtobufDescriptor is the deterministic client-useful projection of the
// provider's published Proto descriptor. It prevents OpenAPI-first generation
// from guessing field numbers, wire types, oneofs, maps, or enum numbers.
type ProtobufDescriptor struct {
	// Syntax is the protobuf source syntax; v1 accepts proto3 only.
	Syntax string `json:"syntax"`
	// Package is the descriptor's protobuf package name.
	Package string `json:"package"`
	// Services lists published service descriptors in canonical order.
	Services []ProtobufService `json:"services"`
	// Messages lists message descriptors used by client operations.
	Messages []ProtobufMessage `json:"messages"`
	// Enums lists enum descriptors with exact numeric values.
	Enums []ProtobufEnum `json:"enums"`
}

// ProtobufService preserves a published protobuf service and its methods.
type ProtobufService struct {
	// Name is the protobuf service name without its package.
	Name string `json:"name"`
	// Methods lists the service's RPC methods.
	Methods []ProtobufMethod `json:"methods"`
}

// ProtobufMethod preserves method message references and streaming cardinality.
type ProtobufMethod struct {
	// Name is the RPC method name.
	Name string `json:"name"`
	// Input is the request message type name.
	Input string `json:"input"`
	// Output is the response message type name.
	Output string `json:"output"`
	// ClientStreaming records request-streaming cardinality explicitly.
	ClientStreaming bool `json:"clientStreaming"`
	// ServerStreaming records response-streaming cardinality explicitly.
	ServerStreaming bool `json:"serverStreaming"`
}

// ProtobufMessage preserves a published protobuf message definition.
type ProtobufMessage struct {
	// Name is the protobuf message type name.
	Name string `json:"name"`
	// Fields lists fields in ascending protobuf field-number order.
	Fields []ProtobufField `json:"fields"`
	// OneOfs lists declared oneof group names.
	OneOfs []string `json:"oneofs,omitempty"`
}

// ProtobufField preserves a field's exact wire identity and shape.
type ProtobufField struct {
	// Name is the protobuf source field name.
	Name string `json:"name"`
	// JSONName is the field's protobuf JSON mapping name.
	JSONName string `json:"jsonName"`
	// Number is the exact protobuf wire field number.
	Number int `json:"number"`
	// TypeKind classifies Type as scalar, message, enum, or map.
	TypeKind string `json:"typeKind"`
	// Type is the scalar or referenced protobuf type name.
	Type string `json:"type"`
	// Repeated records repeated-field cardinality.
	Repeated bool `json:"repeated,omitempty"`
	// Optional records explicit proto3 presence.
	Optional bool `json:"optional,omitempty"`
	// OneOf names the containing declared oneof group.
	OneOf string `json:"oneof,omitempty"`
	// Map carries exact key and value metadata for map fields.
	Map *ProtobufMap `json:"map,omitempty"`
}

// ProtobufMap preserves protobuf map key and value types.
type ProtobufMap struct {
	// KeyType is the permitted protobuf scalar key type.
	KeyType string `json:"keyType"`
	// ValueKind classifies the map value as scalar, message, or enum.
	ValueKind string `json:"valueKind"`
	// ValueType is the scalar or referenced map value type.
	ValueType string `json:"valueType"`
}

// ProtobufEnum preserves a published protobuf enum definition.
type ProtobufEnum struct {
	// Name is the protobuf enum type name.
	Name string `json:"name"`
	// Values lists symbolic names and exact wire numbers.
	Values []ProtobufEnumValue `json:"values"`
}

// ProtobufEnumValue preserves an enum name and its numeric wire value.
type ProtobufEnumValue struct {
	// Name is the protobuf enum value name.
	Name string `json:"name"`
	// Number is the exact protobuf enum wire number.
	Number int `json:"number"`
}

// Schema is the strict neutral subset of OpenAPI Schema Object semantics used
// by generated clients. Pointer scalar fields preserve authored zero/false.
type Schema struct {
	// Type is the JSON value kind in the supported OpenAPI subset.
	Type string `json:"type,omitempty"`
	// Format refines the representation of the declared type.
	Format string `json:"format,omitempty"`
	// Nullable preserves an explicitly authored nullable flag.
	Nullable *bool `json:"nullable,omitempty"`
	// Ref is a local OpenAPI component-schema reference.
	Ref string `json:"$ref,omitempty"`
	// Properties maps object property names to their schemas.
	Properties map[string]Schema `json:"properties,omitempty"`
	// Required preserves the provider-authored required-property order.
	Required []string `json:"required,omitempty"`
	// Items is the element schema for arrays.
	Items *Schema `json:"items,omitempty"`
	// AdditionalProperties selects closed objects or a typed map value.
	AdditionalProperties *AdditionalProperties `json:"additionalProperties,omitempty"`
	// Enum preserves exact JSON enum values and numeric lexemes.
	Enum []json.RawMessage `json:"enum,omitempty"`
	// OneOf preserves ordered union alternatives.
	OneOf []Schema `json:"oneOf,omitempty"`
	// Discriminator records polymorphic dispatch metadata.
	Discriminator *Discriminator `json:"discriminator,omitempty"`
	// Title preserves the authored schema title.
	Title string `json:"title,omitempty"`
	// Description preserves the authored schema description.
	Description string `json:"description,omitempty"`
	// Default preserves an exact authored JSON default, including null.
	Default json.RawMessage `json:"default,omitempty"`
	// Minimum preserves the exact lower numeric bound.
	Minimum *json.Number `json:"minimum,omitempty"`
	// Maximum preserves the exact upper numeric bound.
	Maximum *json.Number `json:"maximum,omitempty"`
	// ExclusiveMinimum states whether Minimum is excluded.
	ExclusiveMinimum *bool `json:"exclusiveMinimum,omitempty"`
	// ExclusiveMaximum states whether Maximum is excluded.
	ExclusiveMaximum *bool `json:"exclusiveMaximum,omitempty"`
	// MinLength is the inclusive lower string-length bound.
	MinLength *int `json:"minLength,omitempty"`
	// MaxLength is the inclusive upper string-length bound.
	MaxLength *int `json:"maxLength,omitempty"`
	// Pattern is the required string regular expression.
	Pattern string `json:"pattern,omitempty"`
	// MinItems is the inclusive lower array-length bound.
	MinItems *int `json:"minItems,omitempty"`
	// MaxItems is the inclusive upper array-length bound.
	MaxItems *int `json:"maxItems,omitempty"`
	// UniqueItems states whether array elements must be unique.
	UniqueItems *bool `json:"uniqueItems,omitempty"`
	// ReadOnly marks a property as response-only.
	ReadOnly *bool `json:"readOnly,omitempty"`
	// WriteOnly marks a property as request-only.
	WriteOnly *bool `json:"writeOnly,omitempty"`
	// OpaqueJSON declares a value that may be any JSON document. Its only
	// value is OpaqueJSONAny, and it stands alone: title and description are
	// its only permitted siblings. A JSON object with free-form values is not
	// spelled with it — that is `type: object` with `additionalProperties:
	// true`.
	OpaqueJSON string `json:"x-putnami-json,omitempty"`
}

// IsOpaqueJSON reports whether the schema declares an opaque JSON value.
func (s Schema) IsOpaqueJSON() bool {
	return s.OpaqueJSON != ""
}

// IsFreeFormObject reports whether the schema declares a JSON object whose
// members are all opaque JSON values: `type: object`, `additionalProperties:
// true` and no named property.
func (s Schema) IsFreeFormObject() bool {
	return s.Type == "object" && len(s.Properties) == 0 && s.AdditionalProperties != nil &&
		s.AdditionalProperties.Schema == nil && s.AdditionalProperties.Allowed != nil && *s.AdditionalProperties.Allowed
}

// Discriminator preserves OpenAPI polymorphic dispatch metadata.
type Discriminator struct {
	// PropertyName is the object property used to select a union variant.
	PropertyName string `json:"propertyName"`
	// Mapping maps discriminator values to local component references.
	Mapping map[string]string `json:"mapping,omitempty"`
}

// AdditionalProperties represents either a boolean or a typed map value schema.
type AdditionalProperties struct {
	// Allowed holds the boolean additionalProperties representation.
	Allowed *bool
	// Schema holds the typed-map additionalProperties representation.
	Schema *Schema
}

// UnmarshalJSON accepts only a JSON boolean or a strict schema object.
func (a *AdditionalProperties) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("additionalProperties must not be null")
	}
	var allowed bool
	if err := json.Unmarshal(data, &allowed); err == nil {
		a.Allowed = &allowed
		a.Schema = nil
		return nil
	}
	var schema Schema
	if err := decodeStrict(data, &schema); err != nil {
		return fmt.Errorf("additionalProperties must be a boolean or schema: %w", err)
	}
	a.Allowed = nil
	a.Schema = &schema
	return nil
}

// MarshalJSON emits the single configured additional-properties representation.
func (a AdditionalProperties) MarshalJSON() ([]byte, error) {
	if a.Allowed != nil && a.Schema == nil {
		return json.Marshal(*a.Allowed)
	}
	if a.Schema != nil && a.Allowed == nil {
		return json.Marshal(a.Schema)
	}
	return nil, fmt.Errorf("additionalProperties must hold exactly one of boolean or schema")
}

func decodeStrict(data []byte, target any) error {
	if err := rejectDuplicateObjectKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateObjectKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := checkJSONValue(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func checkJSONValue(decoder *json.Decoder, field string) error {
	return checkJSONValueWithNullPayload(decoder, field, false)
}

func checkJSONValueWithNullPayload(decoder *json.Decoder, field string, nullPayload bool) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		if token == nil && !nullPayload {
			return fmt.Errorf("%s: null is not valid for this field", field)
		}
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s: object key is not a string", field)
			}
			if seen[key] {
				return fmt.Errorf("%s.%s: duplicate object key", field, key)
			}
			seen[key] = true
			allowNull := nullPayload || schemaRawValueAllowsNull(field, key)
			if err := checkJSONValueWithNullPayload(decoder, field+"."+key, allowNull); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		index := 0
		for decoder.More() {
			if err := checkJSONValueWithNullPayload(decoder, fmt.Sprintf("%s[%d]", field, index), nullPayload); err != nil {
				return err
			}
			index++
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("%s: unexpected JSON delimiter %q", field, delim)
	}
}

// schemaRawValueAllowsNull identifies the two Schema fields whose values are
// arbitrary JSON. Map keys named "default" or "enum" are ordinary names and
// must not weaken strict null handling for the schema stored under that key.
func schemaRawValueAllowsNull(parent, key string) bool {
	if key != "default" && key != "enum" {
		return false
	}
	for _, mapSuffix := range []string{".credentials", ".mapping", ".properties"} {
		if strings.HasSuffix(parent, mapSuffix) {
			return false
		}
	}
	return true
}
