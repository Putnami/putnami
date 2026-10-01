package clientcontract

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// WebSocketFrameType identifies a control or data frame in the first-party
// WebSocket protocol. Application payloads never share this namespace.
type WebSocketFrameType string

// Closed first-party WebSocket frame vocabulary.
const (
	WebSocketFrameInit      WebSocketFrameType = "init"
	WebSocketFrameReady     WebSocketFrameType = "ready"
	WebSocketFrameMessage   WebSocketFrameType = "message"
	WebSocketFrameHalfClose WebSocketFrameType = "half-close"
	WebSocketFrameResult    WebSocketFrameType = "result"
	WebSocketFrameError     WebSocketFrameType = "error"
	WebSocketFrameCancel    WebSocketFrameType = "cancel"
	WebSocketFramePing      WebSocketFrameType = "ping"
	WebSocketFramePong      WebSocketFrameType = "pong"
)

// WebSocketDirection is relative to the generated client.
type WebSocketDirection string

const (
	// WebSocketClientToServer identifies a client-originated frame.
	WebSocketClientToServer WebSocketDirection = "client-to-server"
	// WebSocketServerToClient identifies a provider-originated frame.
	WebSocketServerToClient WebSocketDirection = "server-to-client"
)

// WebSocketFrameHeaderV1 is decoded first to select a closed frame shape.
type WebSocketFrameHeaderV1 struct {
	V    int                `json:"v"`
	Type WebSocketFrameType `json:"type"`
}

// WebSocketInitFrameV1 is the mandatory first client frame. Credential and
// resume values are sensitive and must never be placed in URLs, subprotocols,
// diagnostics, telemetry attributes, close reasons, or generated source.
type WebSocketInitFrameV1 struct {
	V              int                        `json:"v"`
	Type           WebSocketFrameType         `json:"type"`
	OperationID    string                     `json:"operationId"`
	ClientID       string                     `json:"clientId"`
	DeadlineUnixMs string                     `json:"deadlineUnixMs"`
	BudgetMs       string                     `json:"budgetMs"`
	Credentials    []WebSocketCredentialV1    `json:"credentials"`
	Headers        []WebSocketHeaderV1        `json:"headers"`
	Context        *WebSocketContextV1        `json:"context,omitempty"`
	Resume         *WebSocketResumeRequestV1  `json:"resume,omitempty"`
	Request        *WebSocketEncodedPayloadV1 `json:"request,omitempty"`
}

// WebSocketCredentialV1 carries one credential selected by declared profile.
type WebSocketCredentialV1 struct {
	Profile string `json:"profile"`
	Value   string `json:"value"`
}

// WebSocketHeaderV1 carries a declared ordinary request header. Authentication,
// client identity, tracing, hop-by-hop, and WebSocket negotiation headers use
// dedicated framework fields and cannot be overridden here.
type WebSocketHeaderV1 struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// WebSocketContextV1 carries bounded propagation fields in the protected init
// frame rather than in the URL or WebSocket subprotocol list.
type WebSocketContextV1 struct {
	Traceparent string `json:"traceparent,omitempty"`
	Tracestate  string `json:"tracestate,omitempty"`
	Baggage     string `json:"baggage,omitempty"`
	RequestID   string `json:"requestId,omitempty"`
}

// WebSocketResumeRequestV1 resumes after the last completely delivered server
// sequence. The token is sensitive first-frame material.
type WebSocketResumeRequestV1 struct {
	Token         string `json:"token"`
	AfterSequence string `json:"afterSequence"`
}

// WebSocketReadyFrameV1 proves authentication and authorization completed
// before the provider handler sees an application message.
type WebSocketReadyFrameV1 struct {
	V           int                `json:"v"`
	Type        WebSocketFrameType `json:"type"`
	Resumed     *bool              `json:"resumed"`
	ResumeToken string             `json:"resumeToken,omitempty"`
}

// WebSocketMessageFrameV1 carries one ordered application message.
type WebSocketMessageFrameV1 struct {
	V        int                       `json:"v"`
	Type     WebSocketFrameType        `json:"type"`
	Sequence string                    `json:"sequence"`
	Payload  WebSocketEncodedPayloadV1 `json:"payload"`
}

// WebSocketHalfCloseFrameV1 ends the client message direction while leaving
// the server direction open.
type WebSocketHalfCloseFrameV1 struct {
	V    int                `json:"v"`
	Type WebSocketFrameType `json:"type"`
}

// WebSocketResultFrameV1 is the single successful terminal server frame.
type WebSocketResultFrameV1 struct {
	V       int                        `json:"v"`
	Type    WebSocketFrameType         `json:"type"`
	Payload *WebSocketEncodedPayloadV1 `json:"payload,omitempty"`
}

// WebSocketErrorFrameV1 is the single failed terminal server frame.
type WebSocketErrorFrameV1 struct {
	V     int                    `json:"v"`
	Type  WebSocketFrameType     `json:"type"`
	Error WebSocketRemoteErrorV1 `json:"error"`
}

// WebSocketRemoteErrorV1 contains only framework error fields. It cannot carry
// raw responses, headers, credential values, or arbitrary server causes.
type WebSocketRemoteErrorV1 struct {
	Status    int             `json:"status"`
	Code      string          `json:"code"`
	GRPCCode  *int            `json:"grpcCode,omitempty"`
	Retryable *bool           `json:"retryable,omitempty"`
	Message   string          `json:"message,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
}

// Closed first-party cancel codes. The spelling is the framework's own, with a
// single l (go/framework/errors spells the code "canceled"). The British
// doubled-l form is not tolerated anywhere on the wire, in either direction.
const (
	// WebSocketCancelCodeCanceled reports a caller-initiated cancellation.
	WebSocketCancelCodeCanceled = "canceled"
	// WebSocketCancelCodeDeadlineExceeded reports the caller's deadline elapsing.
	WebSocketCancelCodeDeadlineExceeded = "deadline_exceeded"
)

// WebSocketCancelFrameV1 propagates cancellation without an arbitrary reason.
type WebSocketCancelFrameV1 struct {
	V    int                `json:"v"`
	Type WebSocketFrameType `json:"type"`
	Code string             `json:"code"`
}

// WebSocketHeartbeatFrameV1 is used by both ping and pong.
type WebSocketHeartbeatFrameV1 struct {
	V     int                `json:"v"`
	Type  WebSocketFrameType `json:"type"`
	Nonce string             `json:"nonce"`
}

// WebSocketEncodedPayloadV1 makes JSON values and protobuf bytes unambiguous.
// Proto is canonical RFC 4648 base64; JSON preserves the authored JSON value.
type WebSocketEncodedPayloadV1 struct {
	Encoding Encoding        `json:"encoding"`
	Value    json.RawMessage `json:"value,omitempty"`
	Base64   *string         `json:"base64,omitempty"`
}

// ParseAndValidateWebSocketFrameV1 selects a closed frame type, rejects unknown
// fields and ambiguous nulls, and validates values that JSON Schema cannot
// express exactly (uint64 bounds and canonical base64).
func ParseAndValidateWebSocketFrameV1(data []byte) (any, []diag.Diagnostic) {
	var header WebSocketFrameHeaderV1
	if err := decodeStrict(data, &header); err != nil {
		// The header decoder intentionally sees other fields as unknown. Decode a
		// tiny raw header first, then apply strict decoding to the selected frame.
		var raw struct {
			V    int                `json:"v"`
			Type WebSocketFrameType `json:"type"`
		}
		if unmarshalErr := json.Unmarshal(data, &raw); unmarshalErr != nil {
			return nil, []diag.Diagnostic{webSocketFrameParseDiagnostic()}
		}
		header = WebSocketFrameHeaderV1(raw)
	}

	var frame any
	switch header.Type {
	case WebSocketFrameInit:
		frame = &WebSocketInitFrameV1{}
	case WebSocketFrameReady:
		frame = &WebSocketReadyFrameV1{}
	case WebSocketFrameMessage:
		frame = &WebSocketMessageFrameV1{}
	case WebSocketFrameHalfClose:
		frame = &WebSocketHalfCloseFrameV1{}
	case WebSocketFrameResult:
		frame = &WebSocketResultFrameV1{}
	case WebSocketFrameError:
		frame = &WebSocketErrorFrameV1{}
	case WebSocketFrameCancel:
		frame = &WebSocketCancelFrameV1{}
	case WebSocketFramePing, WebSocketFramePong:
		frame = &WebSocketHeartbeatFrameV1{}
	default:
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTransport, "type",
			"websocket frame type is unsupported")}
	}
	if err := decodeStrict(data, frame); err != nil {
		return nil, []diag.Diagnostic{webSocketFrameParseDiagnostic()}
	}
	return frame, validateWebSocketFrameV1(frame)
}

// ValidateWebSocketInitForOperation joins admission data to the exact generated
// operation and selected transport. Providers call it before creating a handler
// context or accepting an application message.
func ValidateWebSocketInitForOperation(
	frame *WebSocketInitFrameV1,
	operationID string,
	operation *OperationV1,
	document *DocumentV1,
	transport Transport,
) []diag.Diagnostic {
	if frame == nil || operation == nil || document == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "websocket admission metadata is incomplete")}
	}
	diags := validateWebSocketFrameV1(frame)
	if frame.OperationID != operationID {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "operationId", "init operation does not match the upgraded route"))
	}
	if transport.Protocol != TransportWebSocket || transport.WebSocket == nil || transport.ProviderWire() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "transport", "selected transport is not a first-party websocket transport"))
	} else {
		if frame.Request != nil && frame.Request.Encoding != transport.Encoding {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "request.encoding", "request encoding does not match the selected transport"))
		}
		if frame.Resume != nil && !transport.WebSocket.Resume {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidResilience, "resume", "selected websocket transport does not support resume"))
		}
	}

	provided := map[string]bool{}
	for _, credential := range frame.Credentials {
		provided[credential.Profile] = true
		if _, exists := document.Credentials[credential.Profile]; !exists {
			diags = append(diags, diag.Errorf(ErrorCodeUnknownProfile, "credentials", "init references an undeclared credential profile"))
		}
	}
	matched := false
	for _, alternative := range operation.Security.Alternatives {
		requiredProfiles := map[string]bool{}
		for _, requirement := range alternative.AllOf {
			requiredProfiles[requirement.Profile] = true
		}
		if sameStringSet(requiredProfiles, provided) {
			matched = true
			break
		}
	}
	if !matched {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSecurity, "credentials", "init credentials do not satisfy one declared security alternative"))
	}

	credentialHeaders := map[string]bool{}
	for _, profile := range document.Credentials {
		if profile.Header != "" {
			credentialHeaders[strings.ToLower(profile.Header)] = true
		}
	}
	for i, header := range frame.Headers {
		if credentialHeaders[strings.ToLower(header.Name)] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSecurity, fmt.Sprintf("headers[%d].name", i),
				"ordinary headers cannot override a declared credential profile"))
		}
	}
	if operation.Security.Authorization != nil && len(operation.Security.Authorization.Clients) > 0 &&
		!contains(operation.Security.Authorization.Clients, frame.ClientID) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSecurity, "clientId", "client identity is not authorized for the operation"))
	}
	return diags
}

func validateWebSocketFrameV1(frame any) []diag.Diagnostic {
	var diags []diag.Diagnostic
	versionAndType := func(v int, got, want WebSocketFrameType) {
		if v != ProtocolVersion {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidVersion, "v", "websocket frame version is unsupported"))
		}
		if got != want {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidEnum, "type", "websocket frame type does not match its shape"))
		}
	}
	switch value := frame.(type) {
	case *WebSocketInitFrameV1:
		versionAndType(value.V, value.Type, WebSocketFrameInit)
		if blank(value.OperationID) {
			diags = append(diags, required("operationId"))
		}
		if !validWebSocketToken(value.ClientID, 128) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "clientId", "clientId must be a bounded token"))
		}
		diags = append(diags, validateWebSocketSequence("deadlineUnixMs", value.DeadlineUnixMs, true)...)
		diags = append(diags, validateWebSocketSequence("budgetMs", value.BudgetMs, true)...)
		if value.Credentials == nil {
			diags = append(diags, required("credentials"))
		} else if len(value.Credentials) > 32 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "credentials", "at most 32 credential profiles are allowed"))
		}
		seen := map[string]bool{}
		for i, credential := range value.Credentials {
			field := fmt.Sprintf("credentials[%d]", i)
			if blank(credential.Profile) || blank(credential.Value) {
				diags = append(diags, required(field))
			}
			if len(credential.Value) > 65536 || strings.ContainsAny(credential.Value, "\r\n\x00") {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".value", "credential value exceeds its bound or contains a forbidden control character"))
			}
			if seen[credential.Profile] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicate, field+".profile", "credential profile appears more than once"))
			}
			seen[credential.Profile] = true
		}
		if value.Headers == nil {
			diags = append(diags, required("headers"))
		} else if len(value.Headers) > 64 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "headers", "at most 64 ordinary headers are allowed"))
		}
		headerNames := map[string]bool{}
		for i, header := range value.Headers {
			field := fmt.Sprintf("headers[%d]", i)
			name := strings.ToLower(header.Name)
			if !validHeaderName(header.Name) || reservedWebSocketInitHeader(name) {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".name", "ordinary header name is forbidden"))
			}
			if headerNames[name] {
				diags = append(diags, diag.Errorf(ErrorCodeDuplicate, field+".name", "ordinary header name appears more than once"))
			}
			headerNames[name] = true
			if len(header.Values) == 0 || len(header.Values) > 32 {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".values", "ordinary header requires between 1 and 32 values"))
			}
			for _, item := range header.Values {
				if len(item) > 8192 || strings.ContainsAny(item, "\r\n\x00") {
					diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, field+".values", "ordinary header value exceeds its bound or contains a forbidden control character"))
				}
			}
		}
		if value.Context != nil {
			for name, item := range map[string]string{
				"traceparent": value.Context.Traceparent, "tracestate": value.Context.Tracestate,
				"baggage": value.Context.Baggage, "requestId": value.Context.RequestID,
			} {
				if len(item) > 4096 || strings.ContainsAny(item, "\r\n\x00") {
					diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "context."+name,
						"propagation value is invalid or exceeds 4096 bytes"))
				}
			}
		}
		if value.Resume != nil {
			if blank(value.Resume.Token) {
				diags = append(diags, required("resume.token"))
			} else if len(value.Resume.Token) > 4096 {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "resume.token", "resume token exceeds 4096 bytes"))
			}
			diags = append(diags, validateWebSocketSequence("resume.afterSequence", value.Resume.AfterSequence, true)...)
		}
		if value.Request != nil {
			diags = append(diags, validateWebSocketPayload("request", value.Request)...)
		}
	case *WebSocketReadyFrameV1:
		versionAndType(value.V, value.Type, WebSocketFrameReady)
		if value.Resumed == nil {
			diags = append(diags, required("resumed"))
		} else if *value.Resumed && blank(value.ResumeToken) {
			diags = append(diags, required("resumeToken"))
		}
		if len(value.ResumeToken) > 4096 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "resumeToken", "resume token exceeds 4096 bytes"))
		}
	case *WebSocketMessageFrameV1:
		versionAndType(value.V, value.Type, WebSocketFrameMessage)
		diags = append(diags, validateWebSocketSequence("sequence", value.Sequence, false)...)
		diags = append(diags, validateWebSocketPayload("payload", &value.Payload)...)
	case *WebSocketHalfCloseFrameV1:
		versionAndType(value.V, value.Type, WebSocketFrameHalfClose)
	case *WebSocketResultFrameV1:
		versionAndType(value.V, value.Type, WebSocketFrameResult)
		if value.Payload != nil {
			diags = append(diags, validateWebSocketPayload("payload", value.Payload)...)
		}
	case *WebSocketErrorFrameV1:
		versionAndType(value.V, value.Type, WebSocketFrameError)
		if value.Error.Status < 400 || value.Error.Status > 599 || blank(value.Error.Code) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidError, "error", "typed error status and code are required"))
		}
		if value.Error.GRPCCode != nil && (*value.Error.GRPCCode < 1 || *value.Error.GRPCCode > 16) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidError, "error.grpcCode", "grpcCode must be between 1 and 16"))
		}
		if len(value.Error.Message) > 4096 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidError, "error.message", "typed error message exceeds 4096 bytes"))
		}
	case *WebSocketCancelFrameV1:
		versionAndType(value.V, value.Type, WebSocketFrameCancel)
		if !containsString(WebSocketCancelCodesV1(), value.Code) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidEnum, "code", "cancel code is unsupported"))
		}
	case *WebSocketHeartbeatFrameV1:
		versionAndType(value.V, value.Type, value.Type)
		if (value.Type != WebSocketFramePing && value.Type != WebSocketFramePong) || !validWebSocketToken(value.Nonce, 64) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTransport, "nonce", "heartbeat type or nonce is invalid"))
		}
	}
	return diags
}

func validateWebSocketPayload(field string, payload *WebSocketEncodedPayloadV1) []diag.Diagnostic {
	switch payload.Encoding {
	case EncodingJSON:
		if len(payload.Value) == 0 || payload.Base64 != nil {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTransport, field,
				"json payload requires value and forbids base64")}
		}
		if !json.Valid(payload.Value) {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTransport, field+".value", "value must be valid JSON")}
		}
	case EncodingProto:
		if payload.Base64 == nil {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTransport, field,
				"proto payload requires canonical RFC 4648 base64 and forbids value")}
		}
		decoded, err := base64.StdEncoding.DecodeString(*payload.Base64)
		if len(payload.Value) != 0 || err != nil || base64.StdEncoding.EncodeToString(decoded) != *payload.Base64 {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTransport, field,
				"proto payload requires canonical RFC 4648 base64 and forbids value")}
		}
	default:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidEnum, field+".encoding",
			"payload encoding is unsupported")}
	}
	return nil
}

func webSocketFrameParseDiagnostic() diag.Diagnostic {
	return diag.Errorf(ErrorCodeParseError, "", "websocket frame is malformed or contains unsupported fields")
}

func reservedWebSocketInitHeader(name string) bool {
	if strings.HasPrefix(name, "sec-websocket-") || strings.HasPrefix(name, "proxy-") {
		return true
	}
	switch name {
	case "authorization", "baggage", "connection", "content-length", "cookie", "host", "set-cookie",
		"traceparent", "tracestate", "upgrade", "x-client-id", "x-request-id":
		return true
	default:
		return false
	}
}

func validateWebSocketSequence(field, value string, allowZero bool) []diag.Diagnostic {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTransport, field, "sequence must be a canonical uint64 decimal string")}
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || (!allowZero && parsed == 0) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTransport, field, "sequence must be a canonical uint64 decimal string")}
	}
	return nil
}

func validWebSocketToken(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && !strings.ContainsRune("-._~", char) {
			return false
		}
	}
	return true
}

func sameStringSet(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}

// WebSocketState is the closed admission-and-delivery state of one first-party
// WebSocket conversation, as observed by either peer. It is the published form
// of the transition table every first-party runtime shares: a runtime consumes
// NextWebSocketStateV1 instead of restating the rules.
type WebSocketState string

// Closed first-party WebSocket conversation states.
const (
	// WebSocketAwaitInit precedes the mandatory client init frame. No operation
	// identity exists yet, so nothing can be canceled and nothing is delivered.
	WebSocketAwaitInit WebSocketState = "await-init"
	// WebSocketAwaitReady follows a well-formed init and precedes admission.
	// Heartbeats are already legal here: admission may wait on an asynchronous
	// credential check, and the idle budget must not fire during that wait.
	WebSocketAwaitReady WebSocketState = "await-ready"
	// WebSocketOpen is the admitted conversation with both directions live.
	WebSocketOpen WebSocketState = "open"
	// WebSocketHalfClosed is the admitted conversation after the client ended
	// its own message direction. The provider direction stays live.
	WebSocketHalfClosed WebSocketState = "half-closed"
	// WebSocketTerminal is reached by exactly one of result, error or cancel.
	WebSocketTerminal WebSocketState = "terminal"
)

// WebSocketStatesV1 returns the closed conversation vocabulary in transition
// order.
func WebSocketStatesV1() []WebSocketState {
	return []WebSocketState{
		WebSocketAwaitInit, WebSocketAwaitReady, WebSocketOpen,
		WebSocketHalfClosed, WebSocketTerminal,
	}
}

// WebSocketFrameTypesV1 returns the closed frame vocabulary this package parses.
// It is the exact set the published wire schema must enumerate.
func WebSocketFrameTypesV1() []WebSocketFrameType {
	return []WebSocketFrameType{
		WebSocketFrameInit, WebSocketFrameReady, WebSocketFrameMessage,
		WebSocketFrameHalfClose, WebSocketFrameResult, WebSocketFrameError,
		WebSocketFrameCancel, WebSocketFramePing, WebSocketFramePong,
	}
}

// WebSocketCancelCodesV1 returns the closed cancel-code vocabulary, in the
// framework's single-l spelling.
func WebSocketCancelCodesV1() []string {
	return []string{WebSocketCancelCodeCanceled, WebSocketCancelCodeDeadlineExceeded}
}

// WebSocketPayloadEncodingsV1 returns the closed payload encoding vocabulary.
// Proto stays in the contract even while a runtime cannot yet decode it: an
// undeliverable transport is a strict generation error, never a runtime refusal.
func WebSocketPayloadEncodingsV1() []Encoding {
	return []Encoding{EncodingJSON, EncodingProto}
}

// NextWebSocketStateV1 applies the published transition table to one frame and
// returns the state that follows it. An illegal frame leaves the state
// unchanged and returns the diagnostics explaining the refusal.
//
// The table is intentionally total over the closed frame vocabulary and takes
// no session memory beyond state, direction and the operation's declared stream
// mode. Sequence continuity, encoding agreement and resume agreement are
// session facts, so WebSocketConversationV1 carries them.
func NextWebSocketStateV1(
	state WebSocketState,
	frame any,
	direction WebSocketDirection,
	stream StreamMode,
) (WebSocketState, []diag.Diagnostic) {
	refuse := func(message string) (WebSocketState, []diag.Diagnostic) {
		return state, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTransport, "type", "%s", message)}
	}
	fromClient := direction == WebSocketClientToServer
	fromServer := direction == WebSocketServerToClient
	clientSends := stream == StreamClient || stream == StreamBidirectional
	serverSends := stream == StreamServer || stream == StreamBidirectional
	admitted := state == WebSocketOpen || state == WebSocketHalfClosed
	pending := state == WebSocketAwaitReady || admitted

	switch value := frame.(type) {
	case *WebSocketInitFrameV1:
		if state != WebSocketAwaitInit || !fromClient {
			return refuse("init must be the first client frame")
		}
		return WebSocketAwaitReady, nil
	case *WebSocketReadyFrameV1:
		if state != WebSocketAwaitReady || !fromServer {
			return refuse("ready is valid only once, from the provider, after init")
		}
		return WebSocketOpen, nil
	case *WebSocketMessageFrameV1:
		if !admitted {
			return refuse("application message is valid only after ready")
		}
		if fromClient && (state != WebSocketOpen || !clientSends) {
			return refuse("client message direction is invalid for this state or stream")
		}
		if fromServer && !serverSends {
			return refuse("server message direction is invalid for this stream")
		}
		return state, nil
	case *WebSocketHalfCloseFrameV1:
		if state != WebSocketOpen || !fromClient || !clientSends {
			return refuse("half-close is valid only from the client, from open, on a client or bidirectional stream")
		}
		return WebSocketHalfClosed, nil
	case *WebSocketResultFrameV1:
		if !admitted || !fromServer {
			return refuse("result is valid only from the provider, after ready")
		}
		if stream == StreamServer && value.Payload != nil {
			return refuse("a server stream delivers values in message frames, so result carries no payload")
		}
		return WebSocketTerminal, nil
	case *WebSocketErrorFrameV1:
		if state == WebSocketTerminal || !fromServer {
			return refuse("error is valid only from the provider, before a terminal frame")
		}
		return WebSocketTerminal, nil
	case *WebSocketCancelFrameV1:
		if !pending || !fromClient {
			return refuse("cancel is valid only from the client, from init onwards, before a terminal frame")
		}
		return WebSocketTerminal, nil
	case *WebSocketHeartbeatFrameV1:
		if !pending {
			return refuse("heartbeat is valid in both directions from await-ready onwards")
		}
		return state, nil
	default:
		return refuse("websocket frame type is unsupported")
	}
}

// WebSocketConversationV1 is the published session view of the wire: the state
// machine plus the session facts a single frame cannot carry — per-direction
// sequence continuity, the transport's declared encoding, and whether resume was
// requested and declared. Every first-party runtime drives one of these instead
// of reimplementing the rules.
type WebSocketConversationV1 struct {
	stream          StreamMode
	encoding        Encoding
	resumeDeclared  bool
	resumeRequested bool
	state           WebSocketState
	clientSequence  uint64
	serverSequence  uint64
	admission       *webSocketAdmission
}

// webSocketAdmission binds a provider-side conversation to the exact generated
// operation the upgraded route belongs to.
type webSocketAdmission struct {
	operationID string
	operation   *OperationV1
	document    *DocumentV1
	transport   Transport
}

// NewWebSocketConversationV1 starts a conversation for one selected transport.
// resumeDeclared mirrors the transport's x-putnami-client websocket.resume flag.
// A peer that also holds the provider document uses
// NewWebSocketConversationForOperationV1 so the init frame is admitted too.
func NewWebSocketConversationV1(stream StreamMode, encoding Encoding, resumeDeclared bool) *WebSocketConversationV1 {
	return &WebSocketConversationV1{
		stream:         stream,
		encoding:       encoding,
		resumeDeclared: resumeDeclared,
		state:          WebSocketAwaitInit,
	}
}

// NewWebSocketConversationForOperationV1 starts a conversation bound to the
// exact generated operation and selected transport. The init frame is validated
// against the declared identity, credential profiles and security alternatives
// before the conversation leaves await-init, so no application message can reach
// a handler ahead of admission.
func NewWebSocketConversationForOperationV1(
	operationID string,
	operation *OperationV1,
	document *DocumentV1,
	transport Transport,
) *WebSocketConversationV1 {
	conversation := &WebSocketConversationV1{state: WebSocketAwaitInit, encoding: transport.Encoding}
	if operation != nil {
		conversation.stream = operation.Stream
	}
	if transport.WebSocket != nil {
		conversation.resumeDeclared = transport.WebSocket.Resume
	}
	conversation.admission = &webSocketAdmission{
		operationID: operationID, operation: operation, document: document, transport: transport,
	}
	return conversation
}

// State reports the conversation state reached by the accepted frames.
func (c *WebSocketConversationV1) State() WebSocketState { return c.state }

// Accept validates one already-parsed frame against the transition table and
// the session facts, then advances the conversation. It returns the refusal
// diagnostics and leaves the state unchanged when the frame is illegal.
func (c *WebSocketConversationV1) Accept(frame any, direction WebSocketDirection) []diag.Diagnostic {
	if diags := c.checkSessionFacts(frame, direction); diag.HasErrors(diags) {
		return diags
	}
	next, diags := NextWebSocketStateV1(c.state, frame, direction, c.stream)
	if diag.HasErrors(diags) {
		return diags
	}
	c.recordSessionFacts(frame, direction)
	c.state = next
	return nil
}

// checkSessionFacts enforces the rules a single frame cannot carry: encoding
// agreement with the selected transport, per-direction sequence continuity, and
// resume agreement between the init request, the provider's ready frame and the
// transport declaration.
func (c *WebSocketConversationV1) checkSessionFacts(frame any, direction WebSocketDirection) []diag.Diagnostic {
	refuse := func(code, field, message string) []diag.Diagnostic {
		return []diag.Diagnostic{diag.Errorf(code, field, "%s", message)}
	}
	switch value := frame.(type) {
	case *WebSocketInitFrameV1:
		if c.admission != nil {
			if diags := ValidateWebSocketInitForOperation(value, c.admission.operationID,
				c.admission.operation, c.admission.document, c.admission.transport); diag.HasErrors(diags) {
				return diags
			}
		}
		if value.Request != nil && value.Request.Encoding != c.encoding {
			return refuse(ErrorCodeInvalidTransport, "request.encoding", "request encoding differs from the selected transport")
		}
		if value.Resume != nil && !c.resumeDeclared {
			return refuse(ErrorCodeInvalidResilience, "resume", "selected websocket transport does not support resume")
		}
	case *WebSocketReadyFrameV1:
		if value.Resumed != nil && *value.Resumed && (!c.resumeRequested || !c.resumeDeclared) {
			return refuse(ErrorCodeInvalidResilience, "resumed",
				"ready reports a resumed stream that the init frame did not request on a resume-capable transport")
		}
	case *WebSocketMessageFrameV1:
		if value.Payload.Encoding != c.encoding {
			return refuse(ErrorCodeInvalidTransport, "payload.encoding", "message encoding differs from the selected transport")
		}
		sequence, err := strconv.ParseUint(value.Sequence, 10, 64)
		if err != nil {
			return refuse(ErrorCodeInvalidTransport, "sequence", "sequence must be a canonical uint64 decimal string")
		}
		if sequence != c.nextSequence(direction) {
			return refuse(ErrorCodeInvalidTransport, "sequence", "message sequence is not the next one for its direction")
		}
	case *WebSocketResultFrameV1:
		if value.Payload != nil && value.Payload.Encoding != c.encoding {
			return refuse(ErrorCodeInvalidTransport, "payload.encoding", "result encoding differs from the selected transport")
		}
	}
	return nil
}

// recordSessionFacts commits the session memory of an accepted frame.
func (c *WebSocketConversationV1) recordSessionFacts(frame any, direction WebSocketDirection) {
	switch value := frame.(type) {
	case *WebSocketInitFrameV1:
		if value.Resume != nil {
			c.resumeRequested = true
			c.serverSequence, _ = strconv.ParseUint(value.Resume.AfterSequence, 10, 64)
		}
	case *WebSocketMessageFrameV1:
		sequence, _ := strconv.ParseUint(value.Sequence, 10, 64)
		if direction == WebSocketClientToServer {
			c.clientSequence = sequence
			return
		}
		c.serverSequence = sequence
	}
}

// nextSequence is the only sequence a direction may send next. Sequences are
// per-direction and gap-free; a resumed server stream continues after the
// requested afterSequence rather than restarting at one.
func (c *WebSocketConversationV1) nextSequence(direction WebSocketDirection) uint64 {
	if direction == WebSocketClientToServer {
		return c.clientSequence + 1
	}
	return c.serverSequence + 1
}

// containsString reports set membership for a closed string vocabulary.
func containsString(set []string, value string) bool {
	for _, item := range set {
		if item == value {
			return true
		}
	}
	return false
}
