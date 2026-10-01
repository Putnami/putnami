package client

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	diag "go.putnami.dev/protocol/diagnostic"
)

// WebSocketContractError reports a first-party WebSocket frame the published
// wire contract refuses. Code is the contract diagnostic code, so a client and
// a provider name the same refusal with the same word, and Field is the frame
// member the refusal is about. Neither carries frame material.
type WebSocketContractError struct {
	Code  string
	Field string

	framework error
}

// Error implements error.
func (contractErr *WebSocketContractError) Error() string {
	if contractErr == nil {
		return ""
	}
	return "first-party websocket frame refused: " + contractErr.Code
}

// Unwrap exposes the framework error carrying the client error code.
func (contractErr *WebSocketContractError) Unwrap() error {
	if contractErr == nil {
		return nil
	}
	return contractErr.framework
}

// newWebSocketContractError projects the first refusal of the published
// contract. The diagnostic message is framework text, so it is safe to keep.
func newWebSocketContractError(diagnostics []diag.Diagnostic) error {
	first := diag.Diagnostic{Code: clientcontract.ErrorCodeInvalidTransport}
	for _, diagnostic := range diag.Errors(diagnostics) {
		first = diagnostic
		break
	}
	return &WebSocketContractError{
		Code:      first.Code,
		Field:     first.Field,
		framework: errors.New(CodeClientResponse, "service websocket frame does not match the published wire contract"),
	}
}

// webSocketOpenOptions carries what the three typed entry points share but the
// generated surface does not expose yet.
type webSocketOpenOptions struct {
	// resume asks the provider to continue a declared, resume-capable server
	// stream after the last completely delivered sequence. Stream resumption is
	// a provider-declared mechanism of its own, so v1 generated code never sets
	// this; it is the seam the resumption slice makes public.
	resume *clientcontract.WebSocketResumeRequestV1
}

// wsService is one admitted first-party WebSocket conversation. It owns the
// socket and the published conversation; every phase rule — admission, budgets,
// retry, breaker, terminal and the single call measurement — belongs to
// StreamSession, and every wire rule to WebSocketConversationV1.
type wsService struct {
	session      *StreamSession
	conn         *wsConn
	conversation *clientcontract.WebSocketConversationV1
	operation    Operation
	identity     callIdentity
	schemas      map[string]clientcontract.Schema
	secrets      []string
	encoding     clientcontract.Encoding
	// carryRemoteMessage is the consuming binding's
	// ServiceBinding.CarryRemoteMessage, carried next to the secrets it is
	// redacted against so every terminal of this session reads one value.
	carryRemoteMessage bool

	mu         sync.Mutex
	clientSeq  uint64
	sendClosed bool
	heartbeat  uint64
}

// accept validates one frame against the published conversation and advances
// it. The conversation is session state shared by the reader and the sender, so
// every acceptance runs under the same lock.
func (service *wsService) accept(frame any, direction clientcontract.WebSocketDirection) error {
	if diagnostics := service.conversation.Accept(frame, direction); diag.HasErrors(diagnostics) {
		return newWebSocketContractError(diagnostics)
	}
	return nil
}

// emit accepts one client frame and writes it. Accepting before writing means
// the client never puts a frame on the wire the contract would refuse.
func (service *wsService) emit(frame any) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.emitLocked(frame)
}

func (service *wsService) emitLocked(frame any) error {
	if err := service.accept(frame, clientcontract.WebSocketClientToServer); err != nil {
		return err
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return errors.Wrap(err, CodeClientRequest)
	}
	return service.conn.writeText(encoded)
}

// readServerFrame reads and accepts the next provider frame.
func (service *wsService) readServerFrame() (any, error) {
	opcode, payload, err := service.conn.readMessage()
	if err != nil {
		return nil, err
	}
	if opcode != wsOpcodeText {
		return nil, newWSProtocolError(wsCloseUnsupportedData, "first-party websocket frames are JSON text frames")
	}
	if !utf8.Valid(payload) {
		return nil, newWSProtocolError(wsCloseInvalidPayload, "websocket text frame is not valid UTF-8")
	}
	frame, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(payload)
	if diag.HasErrors(diagnostics) {
		return nil, newWebSocketContractError(diagnostics)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := service.accept(frame, clientcontract.WebSocketServerToClient); err != nil {
		return nil, err
	}
	return frame, nil
}

// answerHeartbeat replies to a provider ping with the nonce it sent.
func (service *wsService) answerHeartbeat(frame *clientcontract.WebSocketHeartbeatFrameV1) error {
	if frame.Type != clientcontract.WebSocketFramePing {
		return nil
	}
	return service.emit(&clientcontract.WebSocketHeartbeatFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePong, Nonce: frame.Nonce,
	})
}

// sendHeartbeat emits one client ping under the declared cadence.
func (service *wsService) sendHeartbeat() error {
	service.mu.Lock()
	service.heartbeat++
	nonce := "hb-" + strconv.FormatUint(service.heartbeat, 10)
	defer service.mu.Unlock()
	return service.emitLocked(&clientcontract.WebSocketHeartbeatFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePing, Nonce: nonce,
	})
}

// sendMessage validates one application message against the declared input
// schema and emits it with the next client sequence.
func (service *wsService) sendMessage(value any) error {
	if service.operation.Contract.Messages == nil || service.operation.Contract.Messages.Input == nil {
		return errors.New(CodeClientConfig, "operation declares no client message schema")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return errors.Wrap(err, CodeClientRequest)
	}
	projected, ok := projectJSON(encoded, service.operation.Contract.Messages.Input, service.schemas, nil, false)
	if !ok {
		return errors.New(CodeClientRequest, "service stream message does not match the generated contract")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.sendClosed {
		return errors.New(CodeClientRequest, "service stream message direction is already closed")
	}
	service.clientSeq++
	frame := &clientcontract.WebSocketMessageFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameMessage,
		Sequence: strconv.FormatUint(service.clientSeq, 10),
		Payload:  clientcontract.WebSocketEncodedPayloadV1{Encoding: service.encoding, Value: projected},
	}
	if err := service.emitLocked(frame); err != nil {
		service.clientSeq--
		return err
	}
	return nil
}

// closeSend ends the client message direction exactly once.
//
// The provider may end a conversation before it reads one client frame — a
// client stream that refuses an unknown resource does — and then close the
// socket, so a half-close can land after the terminal frame or after the socket
// is gone. Neither is a failure of the half-close: there is no client direction
// left to end. The reader records that terminal, and the caller reads it as the
// typed error from Result, Recv or Err, not as a contract or socket error here.
func (service *wsService) closeSend() error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.sendClosed {
		return nil
	}
	service.sendClosed = true
	frame := &clientcontract.WebSocketHalfCloseFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameHalfClose,
	}
	// The typed handles exist only for client and bidirectional streams after
	// admission, and sendClosed stops a second half-close, so the published
	// conversation refuses this one only once the conversation has ended.
	if diag.HasErrors(service.conversation.Accept(frame, clientcontract.WebSocketClientToServer)) {
		return nil
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return errors.Wrap(err, CodeClientRequest)
	}
	// A failed write means the socket ended under the half-close. The reader
	// sees the same end and records the terminal the caller reads.
	_ = service.conn.writeText(encoded) //nolint:errcheck // the reader owns the terminal of a socket that ended
	return nil
}

// requestCancel tells the provider the caller withdrew, then releases the
// socket. It never decides the terminal: the reader owns it, so the single call
// measurement carries the code the reader recorded.
func (service *wsService) requestCancel() {
	service.emitCancel()
	service.session.Cancel()
}

// emitCancel sends the single cancel frame, naming why the caller withdrew. The
// published conversation refuses a cancel once a terminal frame has been
// accepted, so a completed stream emits nothing.
func (service *wsService) emitCancel() {
	code := clientcontract.WebSocketCancelCodeCanceled
	if stderrors.Is(service.session.Context().Err(), context.DeadlineExceeded) {
		code = clientcontract.WebSocketCancelCodeDeadlineExceeded
	}
	_ = service.emit(&clientcontract.WebSocketCancelFrameV1{ //nolint:errcheck // a peer that is already gone needs no cancel frame
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameCancel, Code: code,
	})
}

// shutdown tells the peer why the conversation ended and closes the socket. The
// reason is the contract diagnostic code or framework text only: a close reason
// travels in clear text and is echoed in provider logs.
func (service *wsService) shutdown(err error) {
	code, reason := wsCloseNormal, ""
	var contractErr *WebSocketContractError
	var closeErr *wsCloseError
	switch {
	case err == nil:
	case stderrors.As(err, &contractErr):
		code, reason = wsClosePolicyViolation, contractErr.Code
		if contractErr.Code == clientcontract.ErrorCodeParseError {
			code = wsCloseInvalidPayload
		}
	case stderrors.As(err, &closeErr):
		code = wsCloseAbnormal
	case errors.Is(err, CodeClientCanceled) || errors.Is(err, CodeClientDeadline):
		code, reason = wsCloseGoingAway, "client_withdrew"
	default:
		code, reason = wsErrorCloseCode(err), "client_error"
	}
	if code != wsCloseAbnormal {
		_ = service.conn.setWriteDeadline(time.Now().Add(webSocketCloseWriteBudget)) //nolint:errcheck // a closing socket reports through the terminal error
		_ = service.conn.writeClose(code, reason)                                    //nolint:errcheck // the terminal error is authoritative
	}
	_ = service.conn.close() //nolint:errcheck // the terminal error is authoritative
}

// webSocketCloseWriteBudget bounds the courtesy close frame so a stalled peer
// cannot hold the reader goroutine after the terminal is decided.
const webSocketCloseWriteBudget = 2 * time.Second

// terminalError projects one read-side failure into the typed error the caller
// sees. Cancellation and deadlines are always reported as the session's own
// facts rather than as a socket failure.
func (service *wsService) terminalError(err error) error {
	sessionCtx := service.session.Context()
	if service.session.idleBudgetExpired() {
		return errors.New(CodeClientDeadline, "service stream idle timeout")
	}
	if sessionCtx.Err() != nil {
		return normalizedCallError(sessionCtx, err)
	}
	var contractErr *WebSocketContractError
	if stderrors.As(err, &contractErr) {
		return err
	}
	var protocolErr *wsProtocolError
	if stderrors.As(err, &protocolErr) {
		return errors.Newf(CodeClientResponse, "service websocket framing is invalid: %s", protocolErr.message)
	}
	var closeErr *wsCloseError
	if stderrors.As(err, &closeErr) {
		return errors.Newf(CodeClientResponse, "service closed the websocket before a terminal frame (code %d)", closeErr.Code)
	}
	return errors.New(CodeClientResponse, "service stream response failed")
}

// webSocketHandshakeExpired reports a read or write the handshake budget
// released. The socket deadline and the attempt context expire at the same
// instant, so the budget is reported as the typed deadline whichever fires
// first.
func webSocketHandshakeExpired(err error) bool {
	var netErr net.Error
	return stderrors.Is(err, os.ErrDeadlineExceeded) || (stderrors.As(err, &netErr) && netErr.Timeout())
}

// decodeWebSocketError projects a provider error frame onto the operation's
// declared errors. It is the same projection a unary non-2xx response gets, so
// a generated caller matches on the same typed error whatever the transport —
// including the frame's free-text message, which an in-band terminal carries
// under the same consumer opt-in and the same redaction as an HTTP envelope.
func decodeWebSocketError(identity callIdentity, frame *clientcontract.WebSocketErrorFrameV1,
	declared []clientcontract.DeclaredError, schemas map[string]clientcontract.Schema, secrets []string,
	carryMessage bool) error {
	wire := frame.Error
	selected := selectDeclaredError(wire.Status, wire.Code, declared)
	code := string(CodeClientRemote)
	if selected != nil {
		code = selected.Code
	}
	remote := &RemoteError{
		ServiceID:   identity.serviceID,
		OperationID: identity.operationID,
		StatusCode:  wire.Status,
		RemoteCode:  code,
		Message:     sanitizedErrorMessage(wire.Message, secrets, carryMessage),
	}
	if selected != nil {
		remote.GRPCCode = selected.GRPCCode
		remote.retryable = selected.Retryable != nil && *selected.Retryable
		if selected.Schema != nil && len(wire.Details) > 0 {
			var valid bool
			remote.Payload, valid = sanitizedErrorPayload(wire.Details, selected.Schema, schemas, secrets)
			if !valid {
				return errors.New(CodeClientResponse, "service websocket error does not match the generated contract")
			}
		}
	}
	remote.framework = errors.New(errors.Code(code), "remote service request failed").WithRetryable(remote.retryable)
	return remote
}

// resolveWebSocketBudgets projects the declared policy onto the four session
// budgets and then applies the stream handshake budget the contract declares on
// its own. The shared projection still defaults the handshake budget to the
// attempt timeout; reading the declared field here keeps that default and adds
// nothing when the provider stays silent.
func resolveWebSocketBudgets(document, operation *clientcontract.ResiliencePolicy) StreamBudgets {
	budgets := resolveStreamBudgets(document, operation)
	apply := func(value *clientcontract.ResiliencePolicy) {
		if value == nil || value.Stream == nil || value.Stream.HandshakeTimeoutMs == nil {
			return
		}
		budgets.Handshake = time.Duration(*value.Stream.HandshakeTimeoutMs) * time.Millisecond
	}
	apply(document)
	apply(operation)
	if budgets.Session > 0 && budgets.Handshake > budgets.Session {
		budgets.Handshake = budgets.Session
	}
	return budgets
}

// resolveWebSocketHeartbeat reads the declared heartbeat cadence. Absent, the
// client sends no heartbeat at all rather than inventing one.
func resolveWebSocketHeartbeat(document, operation *clientcontract.ResiliencePolicy) time.Duration {
	heartbeat := time.Duration(0)
	apply := func(value *clientcontract.ResiliencePolicy) {
		if value == nil || value.Stream == nil || value.Stream.HeartbeatMs == nil {
			return
		}
		heartbeat = time.Duration(*value.Stream.HeartbeatMs) * time.Millisecond
	}
	apply(document)
	apply(operation)
	return heartbeat
}

// selectWebSocketTransport picks the first provider-advertised WebSocket
// transport in declared order and refuses, before any network byte, every
// transport this runtime cannot honor exactly. Refusing an undeliverable
// transport belongs to strict generation; this is the runtime belt that makes a
// hand-written or stale descriptor fail closed instead of mid-stream.
func selectWebSocketTransport(operation Operation, mode clientcontract.StreamMode) (*clientcontract.Transport, error) {
	if operation.Contract.Stream != mode {
		return nil, errors.Newf(CodeClientConfig, "operation %q is not a typed %s stream", operation.ID, mode)
	}
	var selected *clientcontract.Transport
	for i := range operation.Contract.Transports {
		if operation.Contract.Transports[i].Protocol == clientcontract.TransportWebSocket {
			selected = &operation.Contract.Transports[i]
			break
		}
	}
	if selected == nil {
		return nil, errors.Newf(CodeClientConfig, "operation %q declares no websocket transport", operation.ID)
	}
	if selected.WebSocket == nil || selected.WebSocket.Subprotocol != clientcontract.WebSocketSubprotocolV1 {
		return nil, errors.Newf(CodeClientConfig,
			"operation %q declares a websocket transport without the %s subprotocol", operation.ID, clientcontract.WebSocketSubprotocolV1)
	}
	return selected, nil
}

// refuseUnsupportedWebSocketEncoding is the runtime belt for a transport this
// runtime cannot encode exactly. Refusing an undeliverable transport belongs to
// strict generation; this makes a hand-written or stale descriptor fail closed
// before any network byte instead of mid-stream.
func refuseUnsupportedWebSocketEncoding(operation Operation, transport *clientcontract.Transport) error {
	if transport.Encoding == clientcontract.EncodingJSON {
		return nil
	}
	return errors.Newf(CodeClientConfig,
		"operation %q declares a websocket transport with %q payloads, which this runtime cannot encode; regenerate the client once proto stream payloads are supported",
		operation.ID, transport.Encoding)
}

// preflightWebSocketResume checks the resume agreement against the published
// conversation before anything is acquired or dialed. The contract owns the
// rule, so the refusal carries the contract's own diagnostic code rather than a
// runtime code of this package's invention.
func preflightWebSocketResume(operation Operation, mode clientcontract.StreamMode,
	transport *clientcontract.Transport, resume *clientcontract.WebSocketResumeRequestV1) error {
	if resume == nil {
		return nil
	}
	probe := clientcontract.NewWebSocketConversationV1(mode, transport.Encoding,
		transport.WebSocket != nil && transport.WebSocket.Resume)
	shell := &clientcontract.WebSocketInitFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameInit,
		OperationID: operation.ID, DeadlineUnixMs: "0", BudgetMs: "0",
		Credentials: []clientcontract.WebSocketCredentialV1{},
		Headers:     []clientcontract.WebSocketHeaderV1{},
		Resume:      resume,
	}
	if diagnostics := probe.Accept(shell, clientcontract.WebSocketClientToServer); diag.HasErrors(diagnostics) {
		return newWebSocketContractError(diagnostics)
	}
	return nil
}

// webSocketURL builds the absolute upgrade URL from the bound transport and the
// generated request. Credentials never reach it: admission is in band.
func webSocketURL(transport *HTTPTransport, request *Request, declared *clientcontract.Transport) string {
	path := request.Path
	if path == "" {
		path = declared.Path
	}
	full := transport.baseURL + path
	if len(request.Query) > 0 || len(request.QueryValues) > 0 {
		params := cloneURLValues(request.QueryValues)
		if params == nil {
			params = make(url.Values, len(request.Query))
		}
		for key, value := range request.Query {
			params.Set(key, value)
		}
		full += "?" + params.Encode()
	}
	return full
}

// webSocketInitHeaders projects the caller's ordinary request headers onto the
// init frame. The contract's closed reserved list stays the single authority on
// what an ordinary header is, so nothing carrying identity, authentication,
// tracing or negotiation can be smuggled through this seam.
func webSocketInitHeaders(header http.Header) []clientcontract.WebSocketHeaderV1 {
	headers := make([]clientcontract.WebSocketHeaderV1, 0, len(header))
	for name, values := range header {
		if len(values) == 0 {
			continue
		}
		headers = append(headers, clientcontract.WebSocketHeaderV1{Name: name, Values: values})
	}
	slices.SortFunc(headers, func(left, right clientcontract.WebSocketHeaderV1) int {
		return strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
	})
	return headers
}

// webSocketMillis formats a millisecond count as the canonical uint64 decimal
// string the wire uses for wide values.
func webSocketMillis(value time.Duration) string {
	if value <= 0 {
		return "0"
	}
	return strconv.FormatInt(value.Milliseconds(), 10)
}

// webSocketPropagation reads the bounded propagation fields telemetry injected
// into a scratch header. Nothing else from that header reaches the wire.
func webSocketPropagation(ctx context.Context, injected http.Header) *clientcontract.WebSocketContextV1 {
	propagation := &clientcontract.WebSocketContextV1{
		Traceparent: injected.Get("Traceparent"),
		Tracestate:  injected.Get("Tracestate"),
		Baggage:     injected.Get("Baggage"),
		RequestID:   phttp.RequestIDFromContext(ctx),
	}
	if *propagation == (clientcontract.WebSocketContextV1{}) {
		return nil
	}
	return propagation
}
