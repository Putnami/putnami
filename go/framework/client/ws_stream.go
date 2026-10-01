package client

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	diag "go.putnami.dev/protocol/diagnostic"
)

// OpenServerStreamWS opens a provider-declared first-party WebSocket server
// stream. Admission travels in the first application frame, so no credential,
// identity or tracing header is placed on the HTTP upgrade and a browser client
// negotiates exactly the same way. Authentication, deadlines, telemetry,
// circuit state, typed terminal errors and schema validation are applied by the
// framework; the lifecycle belongs to StreamSession and the wire rules to the
// published WebSocketConversationV1.
func OpenServerStreamWS[T any](ctx context.Context, client *Client, request *Request, operation Operation,
	errorMapper ...func(error) error) (*Stream[T], error) {
	return openWebSocketServerStream[T](ctx, client, request, operation, errorMapper)
}

// RequestStream is a generated, typed client stream: the caller sends request
// messages, ends its own direction with CloseSend, and reads the single
// declared result the provider returns.
type RequestStream[TIn, TOut any] struct {
	service  *wsService
	delivery *webSocketDelivery[TOut]
	done     <-chan struct{}

	mu        sync.RWMutex
	err       error
	closeOnce sync.Once
}

// OpenClientStream opens a provider-declared first-party WebSocket client
// stream.
func OpenClientStream[TIn, TOut any](ctx context.Context, client *Client, request *Request, operation Operation,
	errorMapper ...func(error) error) (*RequestStream[TIn, TOut], error) {
	service, err := openWebSocketService(ctx, client, request, operation,
		clientcontract.StreamClient, webSocketOpenOptions{}, errorMapper)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stream := &RequestStream[TIn, TOut]{service: service, delivery: &webSocketDelivery[TOut]{}, done: done}
	go func() {
		defer close(done)
		stream.setErr(runWebSocketStream(service, stream.delivery))
	}()
	return stream, nil
}

// Send validates one message against the declared input schema and emits it.
func (stream *RequestStream[TIn, TOut]) Send(ctx context.Context, message TIn) error {
	return sendWebSocketMessage(ctx, stream.service, stream.done, stream.Err, message)
}

// CloseSend ends the client message direction. It is idempotent and leaves the
// provider direction open until the provider sends its terminal frame. A
// provider that already ended the conversation leaves no direction to end, so
// CloseSend returns nil and Result reports the terminal.
func (stream *RequestStream[TIn, TOut]) CloseSend() error {
	return stream.service.closeSend()
}

// Result waits for the provider's terminal frame and returns the declared
// result value, or the typed terminal error.
func (stream *RequestStream[TIn, TOut]) Result(ctx context.Context) (TOut, error) {
	var zero TOut
	select {
	case <-stream.done:
	case <-ctx.Done():
		return zero, normalizedCallError(ctx, ctx.Err())
	}
	if err := stream.Err(); err != nil {
		return zero, err
	}
	if !stream.delivery.hasResult {
		return zero, errors.New(CodeClientResponse, "service stream ended without the declared result value")
	}
	return stream.delivery.result, nil
}

// Done closes when the stream reaches its terminal frame, fails, or is closed.
func (stream *RequestStream[TIn, TOut]) Done() <-chan struct{} { return stream.done }

// Err returns the sanitized terminal error. Call it after Done.
func (stream *RequestStream[TIn, TOut]) Err() error {
	stream.mu.RLock()
	defer stream.mu.RUnlock()
	return stream.err
}

func (stream *RequestStream[TIn, TOut]) setErr(err error) {
	if err == nil {
		return
	}
	stream.mu.Lock()
	if stream.err == nil {
		stream.err = err
	}
	stream.mu.Unlock()
}

// Close cancels the stream. It is idempotent.
func (stream *RequestStream[TIn, TOut]) Close() error {
	stream.closeOnce.Do(stream.service.requestCancel)
	return nil
}

// BidiStream is a generated, typed bidirectional stream: both directions are
// live until the caller half-closes and the provider sends its terminal frame.
type BidiStream[TIn, TOut any] struct {
	service  *wsService
	delivery *webSocketDelivery[TOut]
	messages <-chan TOut
	done     <-chan struct{}

	mu         sync.RWMutex
	err        error
	resultOnce sync.Once
	closeOnce  sync.Once
}

// OpenBidiStream opens a provider-declared first-party WebSocket bidirectional
// stream.
func OpenBidiStream[TIn, TOut any](ctx context.Context, client *Client, request *Request, operation Operation,
	errorMapper ...func(error) error) (*BidiStream[TIn, TOut], error) {
	service, err := openWebSocketService(ctx, client, request, operation,
		clientcontract.StreamBidirectional, webSocketOpenOptions{}, errorMapper)
	if err != nil {
		return nil, err
	}
	messages := make(chan TOut, service.session.config.Budgets.MaxBufferedMessages)
	done := make(chan struct{})
	stream := &BidiStream[TIn, TOut]{
		service: service, delivery: &webSocketDelivery[TOut]{messages: messages},
		messages: messages, done: done,
	}
	go func() {
		defer close(done)
		defer close(messages)
		stream.setErr(runWebSocketStream(service, stream.delivery))
	}()
	return stream, nil
}

// Send validates one message against the declared input schema and emits it.
func (stream *BidiStream[TIn, TOut]) Send(ctx context.Context, message TIn) error {
	return sendWebSocketMessage(ctx, stream.service, stream.done, stream.Err, message)
}

// CloseSend ends the client message direction. It is idempotent. A provider
// that already ended the conversation leaves no direction to end, so CloseSend
// returns nil and Recv reports the terminal.
func (stream *BidiStream[TIn, TOut]) CloseSend() error { return stream.service.closeSend() }

// Recv returns the next provider message. A terminal result frame that carries
// a declared value delivers it as the last Recv value, so a bidirectional
// stream has exactly one delivery channel; Recv then returns io.EOF. A failed
// stream returns its typed terminal error.
func (stream *BidiStream[TIn, TOut]) Recv(ctx context.Context) (TOut, error) {
	var zero TOut
	select {
	case message, ok := <-stream.messages:
		if ok {
			return message, nil
		}
	case <-ctx.Done():
		return zero, normalizedCallError(ctx, ctx.Err())
	}
	if err := stream.Err(); err != nil {
		return zero, err
	}
	var final TOut
	delivered := false
	stream.resultOnce.Do(func() {
		if stream.delivery.hasResult {
			final, delivered = stream.delivery.result, true
		}
	})
	if delivered {
		return final, nil
	}
	return zero, io.EOF
}

// Done closes when the stream reaches its terminal frame, fails, or is closed.
func (stream *BidiStream[TIn, TOut]) Done() <-chan struct{} { return stream.done }

// Err returns the sanitized terminal error. Call it after Done.
func (stream *BidiStream[TIn, TOut]) Err() error {
	stream.mu.RLock()
	defer stream.mu.RUnlock()
	return stream.err
}

func (stream *BidiStream[TIn, TOut]) setErr(err error) {
	if err == nil {
		return
	}
	stream.mu.Lock()
	if stream.err == nil {
		stream.err = err
	}
	stream.mu.Unlock()
}

// Close cancels the stream. It is idempotent.
func (stream *BidiStream[TIn, TOut]) Close() error {
	stream.closeOnce.Do(stream.service.requestCancel)
	return nil
}

// sendWebSocketMessage is the send path shared by the two writable handles.
func sendWebSocketMessage[TIn any](ctx context.Context, service *wsService, done <-chan struct{},
	terminal func() error, message TIn) error {
	if err := ctx.Err(); err != nil {
		return normalizedCallError(ctx, err)
	}
	select {
	case <-done:
		if err := terminal(); err != nil {
			return err
		}
		return errors.New(CodeClientRequest, "service stream reached its terminal frame")
	default:
	}
	return service.sendMessage(message)
}

// webSocketDelivery is the typed side of one conversation: the bounded message
// queue and the single declared result value.
type webSocketDelivery[TOut any] struct {
	messages  chan TOut
	result    TOut
	hasResult bool
	// delivered is the sequence of the last provider message the caller
	// completely received. A message decoded but not handed over is not
	// counted: a resume that skipped it would drop a value nobody read.
	delivered uint64
}

// runWebSocketStream reads the conversation to its terminal, tells the peer why
// it ended, and records the single terminal and the single call measurement.
func runWebSocketStream[TOut any](service *wsService, delivery *webSocketDelivery[TOut]) error {
	err, _ := readWebSocketStream(service, delivery)
	service.shutdown(err)
	var surfaced error
	if err != nil {
		surfaced = service.session.Fail(err)
	} else {
		service.session.Complete()
	}
	service.session.Close() //nolint:errcheck // Close always returns nil; the terminal error reaches the caller through Err
	return surfaced
}

// readWebSocketStream drives the admitted conversation. It holds no phase rule
// of its own: every acceptance is WebSocketConversationV1's, and every
// lifecycle fact is StreamSession's.
//
// The second return value reports a socket that ended without a terminal frame
// while the session was still live: the only class a declared resume continues.
func readWebSocketStream[TOut any](service *wsService, delivery *webSocketDelivery[TOut]) (error, bool) {
	for {
		frame, err := service.readServerFrame()
		if err != nil {
			return service.terminalError(err), service.transportBreak(err)
		}
		switch value := frame.(type) {
		case *clientcontract.WebSocketHeartbeatFrameV1:
			if err := service.answerHeartbeat(value); err != nil {
				return service.terminalError(err), false
			}
		case *clientcontract.WebSocketMessageFrameV1:
			message, err := decodeWebSocketPayload[TOut](service, value.Payload)
			if err != nil {
				return err, false
			}
			service.session.Activate()
			if err := deliverWebSocketMessage(service, delivery.messages, message); err != nil {
				return err, false
			}
			// The wire's strict parser already proved this is a canonical
			// uint64 decimal, so it cannot fail here.
			if sequence, parseErr := strconv.ParseUint(value.Sequence, 10, 64); parseErr == nil {
				delivery.delivered = sequence
			}
		case *clientcontract.WebSocketResultFrameV1:
			if value.Payload == nil {
				return nil, false
			}
			message, err := decodeWebSocketPayload[TOut](service, *value.Payload)
			if err != nil {
				return err, false
			}
			delivery.result, delivery.hasResult = message, true
			service.session.Activate()
			return nil, false
		case *clientcontract.WebSocketErrorFrameV1:
			if value.Error.Status == http.StatusUnauthorized || value.Error.Status == http.StatusForbidden {
				service.session.invalidateCredentials()
			}
			return decodeWebSocketError(service.identity, value, service.operation.Contract.Errors,
				service.schemas, service.secrets, service.carryRemoteMessage), false
		default:
			return errors.New(CodeClientResponse, "service websocket frame is not deliverable to a generated caller"), false
		}
	}
}

// decodeWebSocketPayload validates one payload against the declared output
// schema before it becomes a typed value.
func decodeWebSocketPayload[TOut any](service *wsService, payload clientcontract.WebSocketEncodedPayloadV1) (TOut, error) {
	var zero TOut
	schema := service.operation.Contract.Messages.Output
	if schema == nil {
		return zero, errors.New(CodeClientConfig, "operation declares no provider message schema")
	}
	projected, ok := projectResponseJSON(payload.Value, schema, service.schemas, nil, false)
	if !ok {
		return zero, errors.New(CodeClientResponse, "service stream message does not match the generated contract")
	}
	var message TOut
	if err := json.Unmarshal(projected, &message); err != nil {
		return zero, errors.New(CodeClientResponse, "service stream message cannot be decoded")
	}
	return message, nil
}

// deliverWebSocketMessage hands one message to the caller under the declared
// queue budget. A full queue blocks the reader rather than dropping a message;
// the session's idle budget and cancellation still release it.
func deliverWebSocketMessage[TOut any](service *wsService, messages chan<- TOut, message TOut) error {
	if messages == nil {
		return errors.New(CodeClientResponse, "service stream delivered a message on a direction it does not declare")
	}
	select {
	case messages <- message:
		return nil
	case <-service.session.Context().Done():
		return service.terminalError(service.session.Context().Err())
	case <-service.session.idleExpired():
		return errors.New(CodeClientDeadline, "service stream idle timeout")
	}
}

// openWebSocketService runs everything up to and including admission: transport
// selection, budgets, credentials, the RFC 6455 handshake, the in-band init
// frame and the provider's ready frame. It opens one connection; a stream that
// continues over a second socket keeps the opener and calls connect again.
func openWebSocketService(ctx context.Context, client *Client, request *Request, operation Operation,
	mode clientcontract.StreamMode, options webSocketOpenOptions, errorMapper []func(error) error) (*wsService, error) {
	opener, err := newWSOpener(ctx, client, request, operation, mode, options.resume, errorMapper)
	if err != nil {
		return nil, err
	}
	if err := opener.session.config.Breaker.AllowRequest(); err != nil {
		return nil, opener.fail(err)
	}
	service, err := opener.connect(options.resume)
	if err != nil {
		return nil, opener.fail(err)
	}
	return service, nil
}

// wsOpener resolves, exactly once, everything a first-party WebSocket
// conversation needs, and opens one connection per call to connect.
//
// A resumed stream is the same session as the connection it continues: the same
// breaker verdict, the same call measurement, the same declared budgets, the
// same telemetry span. Only the socket, the credentials and the conversation
// are new. That is why the resolved material lives here and not in wsService.
type wsOpener struct {
	runtime       *serviceRuntime
	httpTransport *HTTPTransport
	request       *Request
	operation     Operation
	mode          clientcontract.StreamMode
	transport     *clientcontract.Transport
	session       *StreamSession
	sessionCtx    context.Context
	callerCtx     context.Context
	budgets       StreamBudgets
	heartbeat     time.Duration
	clientID      string
	headers       []clientcontract.WebSocketHeaderV1
	telemetry     ServiceCallTelemetry
	// credentials holds the frames the last credential resolution produced. The
	// session owns re-resolution, so a resumed connection carries a credential
	// that is valid now, never the one the first connection carried.
	credentials []clientcontract.WebSocketCredentialV1
	// resumeToken is the last token the provider issued in its ready frame. It
	// is provider material: it never reaches a URL, a subprotocol or a log.
	resumeToken string
	// unavailable records that the last connection failed because the provider
	// says this wire is not served at this path. It is what a declared
	// transport fallback reads, and nothing else acts on it.
	unavailable bool
}

// newWSOpener resolves the transport, the budgets and the session of one
// conversation. It refuses, before any network byte, every declared shape this
// runtime cannot honor exactly.
func newWSOpener(ctx context.Context, client *Client, request *Request, operation Operation,
	mode clientcontract.StreamMode, resume *clientcontract.WebSocketResumeRequestV1,
	errorMapper []func(error) error) (*wsOpener, error) {
	runtime, httpTransport, callCtx, err := webSocketRuntime(ctx, client)
	if err != nil {
		return nil, err
	}
	ctx = callCtx
	if diagnostics := clientcontract.ValidateOperationForID(operation.ID, &operation.Contract,
		&runtime.descriptor.Contract); len(diagnostics) > 0 {
		return nil, errors.Newf(CodeClientConfig, "invalid generated operation contract: %s", diagnostics[0].String())
	}
	transport, err := selectWebSocketTransport(operation, mode)
	if err != nil {
		return nil, err
	}
	// The published contract is consulted before this runtime's own limits, so a
	// refusal names the wire rule when there is one.
	if err := preflightWebSocketResume(operation, mode, transport, resume); err != nil {
		return nil, err
	}
	if err := refuseUnsupportedWebSocketEncoding(operation, transport); err != nil {
		return nil, err
	}
	if err := webSocketMessageShapes(operation, mode); err != nil {
		return nil, err
	}
	clientID := runtime.binding.ClientID
	if clientID == "" {
		return nil, errors.New(CodeClientConfig, "websocket admission requires a client identifier on the service binding")
	}
	request, err = runtime.requestWithBindingHeaders(request, operation)
	if err != nil {
		return nil, err
	}

	var documentPolicy *clientcontract.ResiliencePolicy
	if runtime.descriptor.Contract.Defaults != nil {
		documentPolicy = runtime.descriptor.Contract.Defaults.Resilience
	}
	opener := &wsOpener{
		runtime:       runtime,
		httpTransport: httpTransport,
		request:       request,
		operation:     operation,
		mode:          mode,
		transport:     transport,
		callerCtx:     ctx,
		budgets:       resolveWebSocketBudgets(documentPolicy, operation.Contract.Resilience),
		heartbeat:     resolveWebSocketHeartbeat(documentPolicy, operation.Contract.Resilience),
		clientID:      clientID,
		headers:       webSocketInitHeaders(request.Headers),
		telemetry:     currentServiceTelemetry(),
	}
	policy := resolvePolicy(documentPolicy, operation.Contract.Resilience, operation.Contract.Idempotency.Kind)
	opener.session, opener.sessionCtx = newStreamSession(ctx, streamSessionConfig{
		ServiceID:   runtime.descriptor.Contract.Service.ID,
		OperationID: operation.ID,
		Protocol:    clientcontract.TransportWebSocket,
		Idempotency: operation.Contract.Idempotency.Kind,
		Budgets:     opener.budgets,
		Breaker:     runtime.breaker(operation.ID, policy),
		Telemetry:   opener.telemetry,
		Credentials: func(resolveCtx context.Context) (appliedCredentials, error) {
			scratch := &Request{Headers: make(http.Header)}
			applied, err := runtime.applyCredentials(resolveCtx, scratch, operation.Contract.Security)
			if err != nil {
				return appliedCredentials{}, err
			}
			frames, err := webSocketInitCredentials(resolveCtx, runtime, operation.Contract.Security, scratch.Headers)
			if err != nil {
				return appliedCredentials{}, err
			}
			opener.credentials = frames
			return applied, nil
		},
		Invalidate: func(applied appliedCredentials) {
			runtime.invalidateServiceCredentials(applied.serviceCredentials)
		},
		MapError: func(err error) error { return mapStreamError(err, errorMapper) },
	})
	return opener, nil
}

// fail records the single terminal, applies the phase breaker rule and closes
// the session, which emits the single call measurement.
func (opener *wsOpener) fail(err error) error {
	surfaced := opener.session.Fail(err)
	opener.session.Close() //nolint:errcheck // Close always returns nil; the terminal error is the return value
	return surfaced
}

// connect opens one socket, admits it in band and returns the live
// conversation. resume is nil for a first connection and carries the provider's
// own token plus the last completely delivered sequence for a continuation.
//
// Credentials are re-resolved on every call, so a connection that continues a
// long stream carries a credential that is valid at the instant it dials rather
// than the one the first socket carried.
func (opener *wsOpener) connect(resume *clientcontract.WebSocketResumeRequestV1) (*wsService, error) {
	session := opener.session
	attemptCtx, finishAttempt := session.beginAttempt()
	resolved, authErr := session.Credentials(attemptCtx)
	if authErr != nil {
		finishAttempt(ServiceAttemptResult{Code: serviceErrorCode(authErr), AuthDuration: session.AuthDuration()})
		return nil, authErr
	}
	injected := make(http.Header)
	if opener.telemetry != nil {
		opener.telemetry.InjectServiceContext(attemptCtx, injected)
	}

	resumeDeclared := opener.transport.WebSocket != nil && opener.transport.WebSocket.Resume
	service := &wsService{
		session:            session,
		operation:          opener.operation,
		identity:           opener.runtime.identity(opener.operation),
		schemas:            opener.runtime.descriptor.Schemas,
		secrets:            resolved.secrets,
		encoding:           opener.transport.Encoding,
		carryRemoteMessage: opener.runtime.binding.CarryRemoteMessage,
		conversation: clientcontract.NewWebSocketConversationV1(opener.mode, opener.transport.Encoding,
			resumeDeclared),
	}
	initFrame := &clientcontract.WebSocketInitFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameInit,
		OperationID:    opener.operation.ID,
		ClientID:       opener.clientID,
		DeadlineUnixMs: webSocketDeadline(opener.sessionCtx),
		BudgetMs:       webSocketMillis(opener.budgets.Session),
		Credentials:    opener.credentials,
		Headers:        opener.headers,
		Context:        webSocketPropagation(opener.callerCtx, injected),
		Resume:         resume,
	}

	// The client's own first frame is checked against the published contract
	// before a byte reaches the network: an admission the wire would refuse is a
	// local configuration fault, and it is refused with the contract's own
	// diagnostic code rather than a runtime code of this package's invention.
	encoded, encodeErr := json.Marshal(initFrame)
	if encodeErr != nil {
		return nil, errors.Wrap(encodeErr, CodeClientRequest)
	}
	parsed, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(encoded)
	if diag.HasErrors(diagnostics) {
		return nil, newWebSocketContractError(diagnostics)
	}
	preflight := clientcontract.NewWebSocketConversationV1(opener.mode, opener.transport.Encoding, resumeDeclared)
	if diagnostics := preflight.Accept(parsed, clientcontract.WebSocketClientToServer); diag.HasErrors(diagnostics) {
		return nil, newWebSocketContractError(diagnostics)
	}

	session.Dispatch()
	conn, dialErr := dialWebSocket(attemptCtx, webSocketDialRequest{
		URL:             webSocketURL(opener.httpTransport, opener.request, opener.transport),
		Subprotocol:     clientcontract.WebSocketSubprotocolV1,
		Deadline:        session.handshakeDeadline(),
		MaxMessageBytes: opener.budgets.MaxFrameBytes,
	})
	if dialErr != nil {
		opener.markUnavailable(dialErr)
		return nil, webSocketHandshakeFailure(service, attemptCtx, finishAttempt, dialErr, resolved)
	}
	conn.onActivity = session.touchIdle
	service.conn = conn
	// The socket is released as soon as the session ends, so a caller that
	// withdraws during a slow admission is not held by a blocking read.
	startWebSocketRelease(service)

	ready, err := admitWebSocketService(service, attemptCtx, initFrame, session.handshakeDeadline(), opener.heartbeat)
	if err != nil {
		service.shutdown(err)
		finishAttempt(ServiceAttemptResult{Code: serviceErrorCode(err), AuthDuration: session.AuthDuration()})
		return nil, err
	}
	// A provider that answers a resume request with a fresh stream would deliver
	// the sequences the caller already consumed a second time. The wire allows
	// the answer; this runtime does not consume it silently.
	if resume != nil && (ready.Resumed == nil || !*ready.Resumed) {
		err := errors.New(CodeClientResponse, "provider refused to continue the stream and offered a fresh one")
		service.shutdown(err)
		finishAttempt(ServiceAttemptResult{Code: serviceErrorCode(err), AuthDuration: session.AuthDuration()})
		return nil, err
	}
	// Each ready frame rotates the token: the one this connection presented is
	// spent, and only the new one can continue the stream after it.
	opener.resumeToken = ready.ResumeToken
	finishAttempt(ServiceAttemptResult{StatusCode: http.StatusSwitchingProtocols, AuthDuration: session.AuthDuration()})
	session.Admit()
	if err := conn.setReadDeadline(time.Time{}); err != nil {
		service.shutdown(err)
		return nil, errors.Wrap(err, CodeClientResponse)
	}
	return service, nil
}

// admitWebSocketService sends the in-band init frame and waits for the ready
// frame under the handshake budget. A provider that refuses admission answers
// with a typed error frame, which stays decodable instead of collapsing into an
// opaque close code.
func admitWebSocketService(service *wsService, attemptCtx context.Context,
	initFrame *clientcontract.WebSocketInitFrameV1, deadline time.Time,
	heartbeat time.Duration) (*clientcontract.WebSocketReadyFrameV1, error) {
	if err := service.conn.setReadDeadline(deadline); err != nil {
		return nil, errors.Wrap(err, CodeClientRequest)
	}
	if err := service.emit(initFrame); err != nil {
		return nil, webSocketAdmissionError(service, attemptCtx, err)
	}
	// The heartbeat starts with the conversation, not with admission: an
	// asynchronous credential check on the provider side may hold the ready
	// frame long enough for a silent socket to look dead.
	startWebSocketHeartbeat(service, heartbeat)
	for {
		frame, err := service.readServerFrame()
		if err != nil {
			return nil, webSocketAdmissionError(service, attemptCtx, err)
		}
		switch value := frame.(type) {
		case *clientcontract.WebSocketReadyFrameV1:
			return value, nil
		case *clientcontract.WebSocketHeartbeatFrameV1:
			if err := service.answerHeartbeat(value); err != nil {
				return nil, webSocketAdmissionError(service, attemptCtx, err)
			}
		case *clientcontract.WebSocketErrorFrameV1:
			if value.Error.Status == http.StatusUnauthorized || value.Error.Status == http.StatusForbidden {
				service.session.invalidateCredentials()
			}
			return nil, decodeWebSocketError(service.identity, value, service.operation.Contract.Errors,
				service.schemas, service.secrets, service.carryRemoteMessage)
		default:
			return nil, errors.New(CodeClientResponse, "service websocket frame is not part of admission")
		}
	}
}

// webSocketAdmissionError reports a pre-admission failure. The handshake budget
// is its own bound, so its expiry is the typed deadline rather than a socket
// failure.
func webSocketAdmissionError(service *wsService, attemptCtx context.Context, err error) error {
	if webSocketHandshakeExpired(err) || attemptCtx.Err() != nil {
		if service.session.Context().Err() == nil {
			return errors.New(CodeClientDeadline, "service stream handshake deadline exceeded")
		}
	}
	return service.terminalError(err)
}

// webSocketHandshakeFailure projects a failed opening handshake. An ordinary
// HTTP refusal keeps the provider's declared typed error.
func webSocketHandshakeFailure(service *wsService, ctx context.Context, finishAttempt func(ServiceAttemptResult),
	dialErr error, resolved appliedCredentials) error {
	var rejection *webSocketHandshakeRejection
	if stderrors.As(dialErr, &rejection) {
		if rejection.StatusCode == http.StatusUnauthorized || rejection.StatusCode == http.StatusForbidden {
			service.session.invalidateCredentials()
		}
		remote := decodeRemoteError(service.identity,
			&Response{StatusCode: rejection.StatusCode, Headers: rejection.Header, Body: rejection.Body},
			service.operation.Contract.Errors, service.schemas, resolved.secrets, service.carryRemoteMessage)
		finishAttempt(ServiceAttemptResult{StatusCode: rejection.StatusCode, Code: serviceErrorCode(remote),
			AuthDuration: service.session.AuthDuration()})
		return remote
	}
	normalized := normalizedCallError(ctx, dialErr)
	if webSocketHandshakeExpired(dialErr) {
		normalized = errors.New(CodeClientDeadline, "service stream handshake deadline exceeded")
	}
	finishAttempt(ServiceAttemptResult{Code: serviceErrorCode(normalized), AuthDuration: service.session.AuthDuration()})
	return normalized
}

// startWebSocketRelease closes the socket when the session ends, telling the
// provider why when the conversation still allows it.
func startWebSocketRelease(service *wsService) {
	go func() {
		<-service.session.Context().Done()
		service.emitCancel()
		_ = service.conn.close() //nolint:errcheck // the terminal error is authoritative
	}()
}

// startWebSocketHeartbeat keeps the declared cadence until the session ends.
func startWebSocketHeartbeat(service *wsService, heartbeat time.Duration) {
	if heartbeat <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-service.session.Context().Done():
				return
			case <-ticker.C:
				if err := service.sendHeartbeat(); err != nil {
					return
				}
			}
		}
	}()
}

// webSocketRuntime resolves the bound runtime and the framework HTTP transport
// the upgrade shares an origin with.
func webSocketRuntime(ctx context.Context, client *Client) (*serviceRuntime, *HTTPTransport, context.Context, error) {
	var err error
	client, ctx, err = clientForEndpoint(ctx, client)
	if err != nil {
		return nil, nil, ctx, err
	}
	if client == nil || client.service == nil {
		return nil, nil, ctx, errors.New(CodeClientConfig, "generated websocket stream requires a service binding")
	}
	transport, ok := client.transport.(*HTTPTransport)
	if !ok {
		return nil, nil, ctx, errors.New(CodeClientConfig, "service binding does not use the framework HTTP transport")
	}
	return client.service, transport, ctx, nil
}

// webSocketMessageShapes refuses an operation whose declared message shapes
// cannot type the handle the caller asked for.
func webSocketMessageShapes(operation Operation, mode clientcontract.StreamMode) error {
	shapes := operation.Contract.Messages
	if shapes == nil {
		return errors.Newf(CodeClientConfig, "operation %q declares no stream message shapes", operation.ID)
	}
	if mode != clientcontract.StreamServer && shapes.Input == nil {
		return errors.Newf(CodeClientConfig, "operation %q declares no client message schema", operation.ID)
	}
	if shapes.Output == nil {
		return errors.Newf(CodeClientConfig, "operation %q declares no provider message schema", operation.ID)
	}
	return nil
}

// webSocketInitCredentials names the credential values the shared credential
// path applied, per declared profile. It reads the headers that path produced
// rather than acquiring anything itself, so acquisition, refresh and refusal
// stay in one place and admission stays in band.
func webSocketInitCredentials(ctx context.Context, runtime *serviceRuntime, security clientcontract.Security,
	applied http.Header) ([]clientcontract.WebSocketCredentialV1, error) {
	credentials := make([]clientcontract.WebSocketCredentialV1, 0, len(applied))
	for i := range security.Alternatives {
		alternative := &security.Alternatives[i]
		if !runtime.alternativeAvailable(ctx, alternative) {
			continue
		}
		for _, requirement := range alternative.AllOf {
			profile := runtime.descriptor.Contract.Credentials[requirement.Profile]
			var value string
			switch profile.Kind {
			case clientcontract.CredentialServiceToken, clientcontract.CredentialForwardedUserToken:
				value = applied.Get("Authorization")
			case clientcontract.CredentialAPIKey, clientcontract.CredentialNamedHeader:
				value = applied.Get(profile.Header)
			}
			if value == "" {
				return nil, errors.New(CodeClientCredential, "resolved credential is missing for a declared profile")
			}
			credentials = append(credentials, clientcontract.WebSocketCredentialV1{
				Profile: requirement.Profile, Value: value,
			})
		}
		return credentials, nil
	}
	return nil, errors.New(CodeClientCredential, "no configured credential alternative satisfies the service contract")
}

// webSocketDeadline formats the caller's absolute deadline for the init frame.
func webSocketDeadline(ctx context.Context) string {
	deadline, ok := ctx.Deadline()
	if !ok {
		return "0"
	}
	milliseconds := deadline.UnixMilli()
	if milliseconds <= 0 {
		return "0"
	}
	return webSocketMillis(time.Duration(milliseconds) * time.Millisecond)
}
