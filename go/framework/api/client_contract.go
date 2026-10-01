package api

import (
	"strings"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// ClientServiceOptions declares the immutable service identity and credential
// profiles carried by every generated first-party client contract. Credential
// profiles describe where credentials come from; they never contain values.
type ClientServiceOptions struct {
	Service     clientcontract.Service
	Credentials map[string]clientcontract.CredentialProfile
	Defaults    *clientcontract.Defaults
}

// ClientProtobufProjection is the exact proto wire projection of one provider:
// the descriptor a Connect client decodes with, and the route binding that says
// which protobuf method each published route is reachable as.
type ClientProtobufProjection struct {
	// Descriptor is the deterministic client-useful proto descriptor.
	Descriptor *clientcontract.ProtobufDescriptor
	// RouteMethods maps "<METHOD> <path>" to the canonical protobuf method
	// identity ("/package.Service/Method") declared for that route.
	RouteMethods map[string]string
}

// ClientOperationOptions declares the client-facing policy that cannot be
// derived mechanically from the route itself. Stream mode, transport paths and
// declared errors are projected from the endpoint definition so those facts
// cannot disagree with the server actually bound by the framework.
type ClientOperationOptions struct {
	Security    clientcontract.Security
	Idempotency *clientcontract.Idempotency
	Resilience  *clientcontract.ResiliencePolicy
	// Transports declares this operation's wire preference order. The framework
	// still derives which transports the route's shape can carry; a declared
	// order reorders and narrows that derived list, and never invents a
	// transport the bound server does not serve. Absent, the framework order
	// applies. It is the single place a provider states how an operation
	// travels: a generated client dispatches in this order and never branches.
	Transports []clientcontract.TransportProtocol
	// ConnectEncodings declares this operation's Connect payload encoding
	// order. Connect is the one transport a provider serves in more than one
	// encoding, and the contract carries one entry per encoding, so this is
	// where a provider states which one a generated client dispatches first.
	// Like Transports, it reorders and narrows the encodings the mounted bridge
	// serves and never invents one. Absent, the bridge order applies.
	ConnectEncodings []clientcontract.Encoding
	// Resume declares that this operation's WebSocket transport can continue a
	// server stream after the last sequence the consumer completely delivered.
	// Only a safe server stream may declare it: a stream whose messages carry
	// side effects cannot be continued without either a gap or a duplicate.
	Resume bool
	// SSEContinuation declares how this operation's SSE transport continues
	// after its connection breaks (clientcontract ADR 0013). Build it with
	// SSECursorContinuation or SSEBestEffortContinuation. Only a safe server
	// stream may declare one; the projection publishes it on the SSE transport
	// and refuses every other shape with the route named. A first-party route
	// that declares one also speaks the negotiated SSE wire: a consumer that
	// asks for it is acknowledged and reads an explicit complete terminal.
	SSEContinuation *clientcontract.SSEContinuation
	// External declares that an external authority owns this operation's wire
	// contract, and names it: "OCI Distribution Specification v1.1", "npm
	// registry API". The route stays served and stays in the OpenAPI document,
	// where it carries x-putnami-external-contract instead of x-putnami-client.
	// It contributes no operation to the first-party contract, no protobuf
	// method, no Connect URL and no generated client method, and it is served
	// by the standard request pipeline, not the strict first-party one: the
	// standard, not Putnami, decides what its requests and errors look like.
	//
	// External stands alone. Combined with any other field of
	// ClientOperationOptions, left blank, or declared on an API without
	// WithClientService, it fails the provider at Configure.
	External string
}

// IsExternal reports whether the operation declares an external authority
// with External. A nil receiver is a first-party operation.
func (o *ClientOperationOptions) IsExternal() bool {
	return o != nil && o.External != ""
}

// ExternalAuthority returns the authority External names, or "" for a
// first-party operation. It fails when the declaration contradicts itself: a
// blank authority, or External combined with a first-party client policy that
// an operation outside the contract has nowhere to publish.
func (o *ClientOperationOptions) ExternalAuthority() (string, error) {
	if !o.IsExternal() {
		return "", nil
	}
	if strings.TrimSpace(o.External) == "" {
		return "", errors.New(CodeClientGenConfig,
			`declares External with a blank authority; name the specification that owns the route's wire contract, for example "OCI Distribution Specification v1.1"`)
	}
	var combined []string
	if len(o.Security.Alternatives) > 0 || o.Security.Authorization != nil {
		combined = append(combined, "Security")
	}
	if o.Idempotency != nil {
		combined = append(combined, "Idempotency")
	}
	if o.Resilience != nil {
		combined = append(combined, "Resilience")
	}
	if len(o.Transports) > 0 {
		combined = append(combined, "Transports")
	}
	if len(o.ConnectEncodings) > 0 {
		combined = append(combined, "ConnectEncodings")
	}
	if o.Resume {
		combined = append(combined, "Resume")
	}
	if o.SSEContinuation != nil {
		combined = append(combined, "SSEContinuation")
	}
	if len(combined) > 0 {
		return "", errors.Newf(CodeClientGenConfig,
			"declares External %q together with %s; an operation an external authority owns publishes no first-party client policy, so drop %s or drop External",
			o.External, strings.Join(combined, ", "), strings.Join(combined, ", "))
	}
	return o.External, nil
}

// SSECursorContinuation declares a cursor continuation. Every output message
// carries the provider's opaque position after it in outputField, a required
// plain-string property; a reopened connection sends the position of the last
// message the consumer received in queryParameter, a declared plain-string
// query parameter, and the provider continues exclusively after it, on any
// instance. The provider owns what a cursor means: its binding to the
// operation and selector, its authenticity and retention, and the typed
// refusal of a stale or forged one.
func SSECursorContinuation(outputField, queryParameter string) *clientcontract.SSEContinuation {
	return &clientcontract.SSEContinuation{
		Mode:   clientcontract.SSEContinuationCursor,
		Cursor: &clientcontract.SSECursor{OutputField: outputField, QueryParameter: queryParameter},
	}
}

// SSEBestEffortContinuation declares a best-effort continuation: a reopened
// connection sends the original query and no position. Messages produced while
// no connection was open may be missing, and the provider may repeat some.
func SSEBestEffortContinuation() *clientcontract.SSEContinuation {
	return &clientcontract.SSEContinuation{Mode: clientcontract.SSEContinuationBestEffort}
}

// WithClientService marks this API as a first-party generated-client provider.
// The marker is emitted into OpenAPI as x-putnami-client protocol version 1;
// first-party readers therefore validate it strictly and never degrade an
// unsupported semantic to an untyped value.
func WithClientService(opts ClientServiceOptions) Option {
	return func(p *Plugin) {
		p.clientService = &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         opts.Service,
			Credentials:     cloneCredentialProfiles(opts.Credentials),
			Defaults:        cloneClientDefaults(opts.Defaults),
		}
	}
}

func cloneCredentialProfiles(in map[string]clientcontract.CredentialProfile) map[string]clientcontract.CredentialProfile {
	out := make(map[string]clientcontract.CredentialProfile, len(in))
	for name, profile := range in {
		copy := profile
		copy.Scopes = append([]string(nil), profile.Scopes...)
		out[name] = copy
	}
	return out
}

func cloneRouteMethods(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for route, method := range in {
		out[route] = method
	}
	return out
}

func clonePointer[T any](in *T) *T {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneClientDefaults(in *clientcontract.Defaults) *clientcontract.Defaults {
	if in == nil {
		return nil
	}
	out := *in
	out.Resilience = cloneResiliencePolicy(in.Resilience)
	return &out
}

func cloneResiliencePolicy(in *clientcontract.ResiliencePolicy) *clientcontract.ResiliencePolicy {
	if in == nil {
		return nil
	}
	out := *in
	out.TimeoutMs = clonePointer(in.TimeoutMs)
	out.AttemptTimeoutMs = clonePointer(in.AttemptTimeoutMs)
	out.MaxResponseBytes = clonePointer(in.MaxResponseBytes)
	if in.Retry != nil {
		retry := *in.Retry
		retry.MaxAttempts = clonePointer(in.Retry.MaxAttempts)
		retry.Statuses = append([]int(nil), in.Retry.Statuses...)
		retry.Codes = append([]string(nil), in.Retry.Codes...)
		out.Retry = &retry
	}
	if in.Circuit != nil {
		circuit := *in.Circuit
		circuit.FailureThreshold = clonePointer(in.Circuit.FailureThreshold)
		circuit.ResetTimeoutMs = clonePointer(in.Circuit.ResetTimeoutMs)
		out.Circuit = &circuit
	}
	if in.Stream != nil {
		stream := *in.Stream
		stream.IdleTimeoutMs = clonePointer(in.Stream.IdleTimeoutMs)
		stream.HeartbeatMs = clonePointer(in.Stream.HeartbeatMs)
		stream.Reconnect = clonePointer(in.Stream.Reconnect)
		stream.MaxBufferedMessages = clonePointer(in.Stream.MaxBufferedMessages)
		out.Stream = &stream
	}
	out.Cache = cloneCachePolicy(in.Cache)
	return &out
}

// cloneCachePolicy copies a declared response cache so a provider mutating
// its options after registration cannot change the published contract.
func cloneCachePolicy(in *clientcontract.CachePolicy) *clientcontract.CachePolicy {
	if in == nil {
		return nil
	}
	out := *in
	out.StaleMs = clonePointer(in.StaleMs)
	out.MaxEntries = clonePointer(in.MaxEntries)
	if in.KeyFields != nil {
		// An explicitly empty list stays empty so validation can refuse it; it
		// must not collapse into the absent list that means the default key.
		out.KeyFields = append(make([]string, 0, len(in.KeyFields)), in.KeyFields...)
	}
	if in.InvalidationFields != nil {
		// The same holds for invalidation fields: empty stays empty and refused.
		out.InvalidationFields = append(make([]string, 0, len(in.InvalidationFields)), in.InvalidationFields...)
	}
	return &out
}

func cloneSecurity(in clientcontract.Security) clientcontract.Security {
	out := clientcontract.Security{
		Alternatives:  make([]clientcontract.SecurityAlternative, len(in.Alternatives)),
		Authorization: cloneAuthorization(in.Authorization),
	}
	for i, alternative := range in.Alternatives {
		out.Alternatives[i].AllOf = make([]clientcontract.SecurityRequirement, len(alternative.AllOf))
		for j, requirement := range alternative.AllOf {
			copy := requirement
			copy.Scopes = append([]string(nil), requirement.Scopes...)
			copy.Roles = append([]string(nil), requirement.Roles...)
			out.Alternatives[i].AllOf[j] = copy
		}
	}
	return out
}

func cloneAuthorization(in *clientcontract.Authorization) *clientcontract.Authorization {
	if in == nil {
		return nil
	}
	out := *in
	out.Issuers = append([]string(nil), in.Issuers...)
	out.Audiences = append([]string(nil), in.Audiences...)
	out.PrincipalKinds = append([]string(nil), in.PrincipalKinds...)
	out.Clients = append([]string(nil), in.Clients...)
	out.ScopesAll = append([]string(nil), in.ScopesAll...)
	out.ScopesAny = append([]string(nil), in.ScopesAny...)
	out.RolesAll = append([]string(nil), in.RolesAll...)
	out.RolesAny = append([]string(nil), in.RolesAny...)
	out.ScopeClaims = append([]string(nil), in.ScopeClaims...)
	out.RoleClaims = append([]string(nil), in.RoleClaims...)
	return &out
}

func cloneClientOperationOptions(in *ClientOperationOptions) *ClientOperationOptions {
	if in == nil {
		return nil
	}
	out := *in
	out.Security = cloneSecurity(in.Security)
	if in.Idempotency != nil {
		idempotency := *in.Idempotency
		out.Idempotency = &idempotency
	}
	out.Resilience = cloneResiliencePolicy(in.Resilience)
	out.Transports = append([]clientcontract.TransportProtocol(nil), in.Transports...)
	out.ConnectEncodings = append([]clientcontract.Encoding(nil), in.ConnectEncodings...)
	out.SSEContinuation = cloneSSEContinuation(in.SSEContinuation)
	return &out
}

// cloneSSEContinuation copies a declared continuation so a provider mutating
// its options after registration cannot change the published contract.
func cloneSSEContinuation(in *clientcontract.SSEContinuation) *clientcontract.SSEContinuation {
	if in == nil {
		return nil
	}
	out := *in
	out.Cursor = clonePointer(in.Cursor)
	return &out
}

func cloneClientDocument(in *clientcontract.DocumentV1) *clientcontract.DocumentV1 {
	if in == nil {
		return nil
	}
	out := *in
	out.Credentials = cloneCredentialProfiles(in.Credentials)
	out.Defaults = cloneClientDefaults(in.Defaults)
	out.Protobuf = cloneProtobufDescriptor(in.Protobuf)
	return &out
}

func cloneProtobufDescriptor(in *clientcontract.ProtobufDescriptor) *clientcontract.ProtobufDescriptor {
	if in == nil {
		return nil
	}
	out := *in
	out.Services = make([]clientcontract.ProtobufService, len(in.Services))
	for i, service := range in.Services {
		out.Services[i] = service
		// An empty list is a declaration (a service whose every route the
		// projection left out); only an absent one is nil. Folding the first
		// into the second made the copy fail validation as a missing field.
		if service.Methods != nil {
			out.Services[i].Methods = append([]clientcontract.ProtobufMethod{}, service.Methods...)
		}
	}
	out.Messages = make([]clientcontract.ProtobufMessage, len(in.Messages))
	for i, message := range in.Messages {
		out.Messages[i] = message
		out.Messages[i].OneOfs = append([]string(nil), message.OneOfs...)
		out.Messages[i].Fields = make([]clientcontract.ProtobufField, len(message.Fields))
		for j, field := range message.Fields {
			out.Messages[i].Fields[j] = field
			if field.Map != nil {
				value := *field.Map
				out.Messages[i].Fields[j].Map = &value
			}
		}
	}
	out.Enums = make([]clientcontract.ProtobufEnum, len(in.Enums))
	for i, enum := range in.Enums {
		out.Enums[i] = enum
		out.Enums[i].Values = append([]clientcontract.ProtobufEnumValue(nil), enum.Values...)
	}
	return &out
}
