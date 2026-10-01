package events

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

var validFrameTypes = map[FrameType]bool{
	FrameAuth:      true,
	FrameSubscribe: true,
	FramePublish:   true,
	FrameEvent:     true,
	FrameAck:       true,
	FrameNack:      true,
	FrameError:     true,
	FrameComplete:  true,
}

var validEventServerTransports = map[EventServerTransport]bool{
	EventServerTransportSSE:       true,
	EventServerTransportWebSocket: true,
	EventServerTransportHTTP:      true,
	EventServerTransportGRPC:      true,
}

var validDeliveryProfiles = map[DeliveryProfile]bool{
	DeliveryProfilePull:   true,
	DeliveryProfileStream: true,
	DeliveryProfilePush:   true,
}

// ValidateDeliveryProfile reports whether profile is a known delivery profile.
func ValidateDeliveryProfile(profile DeliveryProfile) bool {
	return validDeliveryProfiles[profile]
}

var validAuthSchemes = map[AuthScheme]bool{
	AuthNone:             true,
	AuthBearer:           true,
	AuthHeaders:          true,
	AuthCookie:           true,
	AuthServiceAccount:   true,
	AuthWorkloadIdentity: true,
}

var validCursorModes = map[CursorMode]bool{
	CursorLatest:       true,
	CursorEarliest:     true,
	CursorMessageID:    true,
	CursorBrokerCursor: true,
}

var validPayloadEncodings = map[PayloadEncoding]bool{
	PayloadJSON: true,
}

var validErrorCodes = map[ErrorCode]bool{
	ErrorUnauthorized:        true,
	ErrorForbidden:           true,
	ErrorProtocolMismatch:    true,
	ErrorInvalidFrame:        true,
	ErrorInvalidEnvelope:     true,
	ErrorInvalidTopic:        true,
	ErrorInvalidCursor:       true,
	ErrorUnsupportedFeature:  true,
	ErrorPayloadTooLarge:     true,
	ErrorRateLimited:         true,
	ErrorUpstreamUnavailable: true,
	ErrorInternal:            true,
}

// ParseEventServerFrameStrict decodes an Event Server frame and rejects unknown
// fields. It returns only decode diagnostics; use ParseAndValidateEventServerFrame
// (or call ValidateEventServerFrame separately) for structural validation.
func ParseEventServerFrameStrict(data []byte) (*EventServerFrame, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var frame EventServerFrame
	if err := dec.Decode(&frame); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse event server frame: %v", err),
		}
	}
	return &frame, nil
}

// ParseAndValidateEventServerFrame decodes an Event Server frame strictly and,
// when it decodes cleanly, validates its structural invariants.
func ParseAndValidateEventServerFrame(data []byte) (*EventServerFrame, []diag.Diagnostic) {
	frame, diags := ParseEventServerFrameStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return frame, append(diags, ValidateEventServerFrame(frame)...)
}

// ParseEventServerCapabilitiesStrict decodes Event Server capabilities and
// rejects unknown fields. It returns only decode diagnostics; use
// ParseAndValidateEventServerCapabilities for structural validation.
func ParseEventServerCapabilitiesStrict(data []byte) (*EventServerCapabilities, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var capabilities EventServerCapabilities
	if err := dec.Decode(&capabilities); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse event server capabilities: %v", err),
		}
	}
	return &capabilities, nil
}

// ParseAndValidateEventServerCapabilities decodes Event Server capabilities
// strictly and, when they decode cleanly, validates their invariants.
func ParseAndValidateEventServerCapabilities(data []byte) (*EventServerCapabilities, []diag.Diagnostic) {
	capabilities, diags := ParseEventServerCapabilitiesStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return capabilities, append(diags, ValidateEventServerCapabilities(capabilities)...)
}

// ParseEventServerErrorStrict decodes an Event Server error and rejects unknown
// fields. It returns only decode diagnostics; use ParseAndValidateEventServerError
// for structural validation.
func ParseEventServerErrorStrict(data []byte) (*EventServerError, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var eventServerError EventServerError
	if err := dec.Decode(&eventServerError); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse event server error: %v", err),
		}
	}
	return &eventServerError, nil
}

// ParseAndValidateEventServerError decodes an Event Server error strictly and,
// when it decodes cleanly, validates its invariants.
func ParseAndValidateEventServerError(data []byte) (*EventServerError, []diag.Diagnostic) {
	eventServerError, diags := ParseEventServerErrorStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return eventServerError, append(diags, ValidateEventServerError(eventServerError)...)
}

// ParseEnvelopeStrict decodes an envelope and rejects unknown fields. It returns
// only decode diagnostics; use ParseAndValidateEnvelope for structural validation.
func ParseEnvelopeStrict(data []byte) (*Envelope, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var envelope Envelope
	if err := dec.Decode(&envelope); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse envelope: %v", err),
		}
	}
	return &envelope, nil
}

// ParseAndValidateEnvelope decodes an envelope strictly and, when it decodes
// cleanly, validates its canonical invariants.
func ParseAndValidateEnvelope(data []byte) (*Envelope, []diag.Diagnostic) {
	envelope, diags := ParseEnvelopeStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return envelope, append(diags, ValidateEnvelope(envelope)...)
}

// ValidateEnvelope checks canonical event envelope invariants.
func ValidateEnvelope(envelope *Envelope) []diag.Diagnostic {
	if envelope == nil {
		return []diag.Diagnostic{diag.Errorf("nil-envelope", "", "envelope is nil")}
	}

	var diags []diag.Diagnostic
	if envelope.Protocol != "" && envelope.Protocol != Protocol {
		diags = append(diags, diag.Errorf("invalid-protocol", "protocol", "expected %q, got %q", Protocol, envelope.Protocol))
	}
	if envelope.ID == "" {
		diags = append(diags, diag.Errorf("required-field", "id", "envelope requires id"))
	}
	if envelope.Topic == "" {
		diags = append(diags, diag.Errorf("required-field", "topic", "envelope requires topic"))
	}
	if len(envelope.Payload) == 0 {
		diags = append(diags, diag.Errorf("required-field", "payload", "envelope requires payload"))
	}
	if envelope.Timestamp == "" {
		diags = append(diags, diag.Errorf("required-field", "timestamp", "envelope requires timestamp"))
	} else if _, err := time.Parse(time.RFC3339Nano, envelope.Timestamp); err != nil {
		diags = append(diags, diag.Errorf("invalid-timestamp", "timestamp", "timestamp must be RFC3339Nano: %v", err))
	}
	if envelope.Attempt < 1 {
		diags = append(diags, diag.Errorf("invalid-attempt", "attempt", "attempt must be >= 1"))
	}
	return diags
}

// ValidateEventServerFrame checks canonical WebSocket/mobile Event Server frame invariants.
func ValidateEventServerFrame(frame *EventServerFrame) []diag.Diagnostic {
	if frame == nil {
		return []diag.Diagnostic{diag.Errorf("nil-frame", "", "event server frame is nil")}
	}

	var diags []diag.Diagnostic
	if frame.Protocol != Protocol {
		diags = append(diags, diag.Errorf("invalid-protocol", "protocol", "expected %q, got %q", Protocol, frame.Protocol))
	}
	if !validFrameTypes[frame.Type] {
		diags = append(diags, diag.Errorf("invalid-frame-type", "type", "unknown frame type %q", frame.Type))
		return diags
	}

	switch frame.Type {
	case FrameSubscribe:
		if frame.Topic == "" {
			diags = append(diags, diag.Errorf("required-field", "topic", "subscribe frame requires topic"))
		}
	case FramePublish:
		if frame.Topic == "" {
			diags = append(diags, diag.Errorf("required-field", "topic", "publish frame requires topic"))
		}
		if len(frame.Payload) == 0 {
			diags = append(diags, diag.Errorf("required-field", "payload", "publish frame requires payload"))
		}
	case FrameEvent:
		if frame.ID == "" {
			diags = append(diags, diag.Errorf("required-field", "id", "event frame requires id"))
		}
		if frame.Topic == "" {
			diags = append(diags, diag.Errorf("required-field", "topic", "event frame requires topic"))
		}
		if len(frame.Payload) == 0 {
			diags = append(diags, diag.Errorf("required-field", "payload", "event frame requires payload"))
		}
		if frame.Timestamp != "" {
			if _, err := time.Parse(time.RFC3339Nano, frame.Timestamp); err != nil {
				diags = append(diags, diag.Errorf("invalid-timestamp", "timestamp", "timestamp must be RFC3339Nano: %v", err))
			}
		}
	case FrameAck, FrameNack:
		if frame.ID == "" {
			diags = append(diags, diag.Errorf("required-field", "id", "%s frame requires id", frame.Type))
		}
	case FrameError:
		if frame.Message == "" && frame.Error == "" {
			diags = append(diags, diag.Errorf("required-field", "message", "error frame requires message or error"))
		}
	}

	return diags
}

// ValidateEventServerCapabilities checks Event Server discovery invariants.
func ValidateEventServerCapabilities(capabilities *EventServerCapabilities) []diag.Diagnostic {
	if capabilities == nil {
		return []diag.Diagnostic{diag.Errorf("nil-capabilities", "", "event server capabilities is nil")}
	}

	var diags []diag.Diagnostic
	if capabilities.Protocol != Protocol {
		diags = append(diags, diag.Errorf("invalid-protocol", "protocol", "expected %q, got %q", Protocol, capabilities.Protocol))
	}
	if len(capabilities.Transports) == 0 {
		diags = append(diags, diag.Errorf("required-field", "transports", "event server capabilities require at least one transport"))
	}

	seenTransports := map[EventServerTransport]bool{}
	for i, transport := range capabilities.Transports {
		path := "transports"
		if !validEventServerTransports[transport] {
			diags = append(diags, diag.Errorf("invalid-enum", path, "unknown transport %q", transport))
			continue
		}
		if seenTransports[transport] {
			diags = append(diags, diag.Errorf("duplicate-transport", path, "duplicate transport %q at index %d", transport, i))
			continue
		}
		seenTransports[transport] = true
	}

	seenProfiles := map[DeliveryProfile]bool{}
	for i, profile := range capabilities.DeliveryProfiles {
		if !validDeliveryProfiles[profile] {
			diags = append(diags, diag.Errorf("invalid-enum", "deliveryProfiles", "unknown delivery profile %q", profile))
			continue
		}
		if seenProfiles[profile] {
			diags = append(diags, diag.Errorf("duplicate-delivery-profile", "deliveryProfiles", "duplicate delivery profile %q at index %d", profile, i))
			continue
		}
		seenProfiles[profile] = true
	}

	diags = append(diags, validateEventServerFeatureEnums(capabilities.Features)...)
	diags = append(diags, validateEventServerLimits(capabilities.Limits)...)
	diags = append(diags, validateEventServerEndpoints(capabilities, seenTransports)...)
	return diags
}

// ValidateEventServerError checks canonical Event Server error invariants.
func ValidateEventServerError(eventServerError *EventServerError) []diag.Diagnostic {
	if eventServerError == nil {
		return []diag.Diagnostic{diag.Errorf("nil-error", "", "event server error is nil")}
	}

	var diags []diag.Diagnostic
	if eventServerError.Protocol != Protocol {
		diags = append(diags, diag.Errorf("invalid-protocol", "protocol", "expected %q, got %q", Protocol, eventServerError.Protocol))
	}
	if !validErrorCodes[eventServerError.Code] {
		diags = append(diags, diag.Errorf("invalid-enum", "code", "unknown error code %q", eventServerError.Code))
	}
	if eventServerError.Message == "" {
		diags = append(diags, diag.Errorf("required-field", "message", "event server error requires message"))
	}
	return diags
}

// ValidateHandlerOptions checks handler option enum values and numeric bounds.
func ValidateHandlerOptions(opts HandlerOptions) []diag.Diagnostic {
	var diags []diag.Diagnostic

	switch opts.Distribution {
	case "", DistributionCompeting, DistributionBroadcast:
	default:
		diags = append(diags, diag.Errorf("invalid-enum", "distribution", "invalid distribution %q", opts.Distribution))
	}
	switch opts.Overflow {
	case "", OverflowThrow, OverflowDrop:
	default:
		diags = append(diags, diag.Errorf("invalid-enum", "overflow", "invalid overflow %q", opts.Overflow))
	}
	switch opts.Ack {
	case "", AckAuto, AckManual:
	default:
		diags = append(diags, diag.Errorf("invalid-enum", "ack", "invalid ack mode %q", opts.Ack))
	}
	if opts.MaxRetries < 0 {
		diags = append(diags, diag.Errorf("invalid-number", "maxRetries", "maxRetries must be >= 0"))
	}
	if opts.MaxBackoffMs < 0 {
		diags = append(diags, diag.Errorf("invalid-number", "maxBackoffMs", "maxBackoffMs must be >= 0"))
	}
	if opts.TimeoutMs < 0 {
		diags = append(diags, diag.Errorf("invalid-number", "timeoutMs", "timeoutMs must be >= 0"))
	}
	if opts.Concurrency < 0 {
		diags = append(diags, diag.Errorf("invalid-number", "concurrency", "concurrency must be >= 0"))
	}
	if opts.QueueLimit < 0 {
		diags = append(diags, diag.Errorf("invalid-number", "queueLimit", "queueLimit must be >= 0"))
	}
	return diags
}

// ParseHandlerOptions strict-parses a handler-options document, rejecting
// unknown fields. It is the parse entry point conformance suites feed the
// shared handler-options fixture corpus through.
func ParseHandlerOptions(data []byte) (*HandlerOptions, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var opts HandlerOptions
	if err := dec.Decode(&opts); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse handler options: %v", err),
		}
	}
	return &opts, nil
}

// ParseAndValidateHandlerOptions strict-parses then validates handler options.
func ParseAndValidateHandlerOptions(data []byte) (*HandlerOptions, []diag.Diagnostic) {
	opts, diags := ParseHandlerOptions(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return opts, append(diags, ValidateHandlerOptions(*opts)...)
}

func validateEventServerFeatureEnums(features EventServerFeatures) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seenAuth := map[AuthScheme]bool{}
	for i, scheme := range features.Auth {
		if !validAuthSchemes[scheme] {
			diags = append(diags, diag.Errorf("invalid-enum", "features.auth", "unknown auth scheme %q", scheme))
			continue
		}
		if seenAuth[scheme] {
			diags = append(diags, diag.Errorf("duplicate-auth", "features.auth", "duplicate auth scheme %q at index %d", scheme, i))
		}
		seenAuth[scheme] = true
	}
	if seenAuth[AuthNone] && len(features.Auth) > 1 {
		diags = append(diags, diag.Errorf("invalid-auth", "features.auth", "auth scheme %q must not be combined with other schemes", AuthNone))
	}

	seenCursors := map[CursorMode]bool{}
	for i, cursor := range features.Cursor {
		if !validCursorModes[cursor] {
			diags = append(diags, diag.Errorf("invalid-enum", "features.cursor", "unknown cursor mode %q", cursor))
			continue
		}
		if seenCursors[cursor] {
			diags = append(diags, diag.Errorf("duplicate-cursor", "features.cursor", "duplicate cursor mode %q at index %d", cursor, i))
		}
		seenCursors[cursor] = true
	}
	if features.Replay && !seenCursors[CursorMessageID] && !seenCursors[CursorBrokerCursor] {
		diags = append(diags, diag.Errorf("invalid-cursor", "features.cursor", "replay event servers must support message-id or broker-cursor resume"))
	}

	seenPayloads := map[PayloadEncoding]bool{}
	for i, payload := range features.Payload {
		if !validPayloadEncodings[payload] {
			diags = append(diags, diag.Errorf("invalid-enum", "features.payload", "unknown payload encoding %q", payload))
			continue
		}
		if seenPayloads[payload] {
			diags = append(diags, diag.Errorf("duplicate-payload", "features.payload", "duplicate payload encoding %q at index %d", payload, i))
		}
		seenPayloads[payload] = true
	}
	if !seenPayloads[PayloadJSON] {
		diags = append(diags, diag.Errorf("required-field", "features.payload", "event server capabilities must include json payload support"))
	}

	return diags
}

func validateEventServerLimits(limits EventServerLimits) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if limits.MaxPayloadBytes < 0 {
		diags = append(diags, diag.Errorf("invalid-number", "limits.maxPayloadBytes", "maxPayloadBytes must be >= 0"))
	}
	if limits.MaxFrameBytes < 0 {
		diags = append(diags, diag.Errorf("invalid-number", "limits.maxFrameBytes", "maxFrameBytes must be >= 0"))
	}
	if limits.MaxSubscriptions < 0 {
		diags = append(diags, diag.Errorf("invalid-number", "limits.maxSubscriptions", "maxSubscriptions must be >= 0"))
	}
	if limits.MaxTopicsPerSubscription < 0 {
		diags = append(diags, diag.Errorf("invalid-number", "limits.maxTopicsPerSubscription", "maxTopicsPerSubscription must be >= 0"))
	}
	return diags
}

func validateEventServerEndpoints(capabilities *EventServerCapabilities, transports map[EventServerTransport]bool) []diag.Diagnostic {
	var diags []diag.Diagnostic

	if transports[EventServerTransportSSE] {
		diags = append(diags, validatePathEndpoint("endpoints.sse", capabilities.Endpoints.SSE, "GET")...)
	}
	if transports[EventServerTransportWebSocket] {
		diags = append(diags, validatePathEndpoint("endpoints.websocket", capabilities.Endpoints.WebSocket, "GET")...)
	}
	if transports[EventServerTransportHTTP] {
		diags = append(diags, validatePathEndpoint("endpoints.publish", capabilities.Endpoints.Publish, "POST")...)
	}
	if transports[EventServerTransportGRPC] {
		diags = append(diags, validateServiceEndpoint("endpoints.grpc", capabilities.Endpoints.GRPC)...)
	}
	if capabilities.Features.Publish && capabilities.Endpoints.Publish == nil && !transports[EventServerTransportGRPC] {
		diags = append(diags, diag.Errorf("required-field", "endpoints.publish", "publish event servers require an HTTP publish endpoint or grpc transport"))
	}

	if capabilities.Endpoints.Health != nil {
		diags = append(diags, validatePathEndpoint("endpoints.health", capabilities.Endpoints.Health, "GET")...)
	}
	return diags
}

func validatePathEndpoint(path string, endpoint *EventServerEndpoint, method string) []diag.Diagnostic {
	if endpoint == nil {
		return []diag.Diagnostic{diag.Errorf("required-field", path, "%s is required", path)}
	}

	var diags []diag.Diagnostic
	if endpoint.Path == "" {
		diags = append(diags, diag.Errorf("required-field", path+".path", "%s requires path", path))
	}
	if endpoint.Method != method {
		diags = append(diags, diag.Errorf("invalid-method", path+".method", "%s method must be %s", path, method))
	}
	if endpoint.Service != "" {
		diags = append(diags, diag.Errorf("invalid-field", path+".service", "%s must not set service", path))
	}
	return diags
}

func validateServiceEndpoint(path string, endpoint *EventServerEndpoint) []diag.Diagnostic {
	if endpoint == nil {
		return []diag.Diagnostic{diag.Errorf("required-field", path, "%s is required", path)}
	}

	var diags []diag.Diagnostic
	if endpoint.Service == "" {
		diags = append(diags, diag.Errorf("required-field", path+".service", "%s requires service", path))
	}
	if endpoint.Path != "" {
		diags = append(diags, diag.Errorf("invalid-field", path+".path", "%s must not set path", path))
	}
	if endpoint.Method != "" {
		diags = append(diags, diag.Errorf("invalid-field", path+".method", "%s must not set method", path))
	}
	return diags
}

// ParsePushEnvelopeStrict decodes a provider push wrapper and rejects unknown
// fields. It returns only decode diagnostics; use ParseAndValidatePushEnvelope
// for structural validation (including the carried canonical envelope).
func ParsePushEnvelopeStrict(data []byte) (*PushEnvelope, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var push PushEnvelope
	if err := dec.Decode(&push); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse push envelope: %v", err),
		}
	}
	return &push, nil
}

// ParseAndValidatePushEnvelope decodes a provider push wrapper strictly and,
// when it decodes cleanly, validates the wrapper and the canonical envelope it
// carries.
func ParseAndValidatePushEnvelope(data []byte) (*PushEnvelope, []diag.Diagnostic) {
	push, diags := ParsePushEnvelopeStrict(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return push, append(diags, ValidatePushEnvelope(push)...)
}

// ValidatePushEnvelope checks provider push wrapper invariants and the canonical
// envelope carried, base64-encoded, in message.data.
func ValidatePushEnvelope(push *PushEnvelope) []diag.Diagnostic {
	if push == nil {
		return []diag.Diagnostic{diag.Errorf("nil-push", "", "push envelope is nil")}
	}

	var diags []diag.Diagnostic
	if push.Subscription == "" {
		diags = append(diags, diag.Errorf("required-field", "subscription", "push envelope requires subscription"))
	}
	if push.Message.Data == "" {
		diags = append(diags, diag.Errorf("required-field", "message.data", "push message requires data"))
		return diags
	}
	if push.Message.PublishTime != "" {
		if _, err := time.Parse(time.RFC3339Nano, push.Message.PublishTime); err != nil {
			diags = append(diags, diag.Errorf("invalid-timestamp", "message.publishTime", "publishTime must be RFC3339Nano: %v", err))
		}
	}

	_, decodeDiags := DecodePushEnvelope(push)
	diags = append(diags, decodeDiags...)
	return diags
}

// DecodePushEnvelope base64-decodes message.data and parses it as the canonical
// Envelope. It returns nil with diagnostics when message.data is not valid
// base64 or does not decode to a valid envelope.
func DecodePushEnvelope(push *PushEnvelope) (*Envelope, []diag.Diagnostic) {
	if push == nil {
		return nil, []diag.Diagnostic{diag.Errorf("nil-push", "", "push envelope is nil")}
	}
	raw, err := base64.StdEncoding.DecodeString(push.Message.Data)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf("invalid-base64", "message.data", "message.data must be base64-encoded: %v", err)}
	}
	envelope, diags := ParseAndValidateEnvelope(raw)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return envelope, diags
}
