package api

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	diag "go.putnami.dev/protocol/diagnostic"
)

// Admission bounds this provider applies when the operation declares none.
// They are framework floors, not permissive fallbacks: each one only exists
// because a socket with no bound at all is a resource leak.
const (
	defaultWebSocketHandshakeBudget = 10 * time.Second
	// webSocketAdmissionBackstopGrace is how much later than the admission budget
	// the transport idle timeout is armed, so the declared refusal wins the race.
	// Admit still ends the conversation at the declared budget.
	webSocketAdmissionBackstopGrace = time.Second
	defaultWebSocketQueueDepth      = 16
)

// webSocketService drives the published first-party WebSocket service protocol
// for one stream endpoint. It owns no phase rule of its own: every frame it
// accepts or emits passes through clientcontract.WebSocketConversationV1, which
// is the published form of the NextWebSocketStateV1 transition table.
type webSocketService struct {
	operationID string
	document    *clientcontract.DocumentV1
	transport   clientcontract.Transport
	stream      clientcontract.StreamMode
	budgets     webSocketBudgets
	before      phttp.Handler
	handle      func(*phttp.StreamContext) error
	log         *logger.Logger
	// resume holds the live continuation grants of this endpoint. It exists
	// whether or not the endpoint declares resume; an endpoint that does not
	// issues nothing, so the map stays empty.
	resume *webSocketResumeStore
}

// webSocketBudgets are the declared bounds of one admitted conversation. Each
// one is read from the operation's resilience policy, falling back to the
// document default and then to the framework floor.
type webSocketBudgets struct {
	Handshake           time.Duration
	Idle                time.Duration
	Heartbeat           time.Duration
	MaxFrameBytes       int64
	MaxBufferedMessages int
}

// webSocketService returns the first-party service protocol driver for this
// stream endpoint, or nil when the provider did not declare a first-party
// client contract. The route then keeps the raw transport stream.
func (d EndpointDefinition) webSocketService(document *clientcontract.DocumentV1, boundPath string,
	handler phttp.StreamHandler) *webSocketService {
	if document == nil || !d.IsStream() || handler.Handle == nil {
		return nil
	}
	stream := clientStreamMode(d.StreamMode())
	if stream == clientcontract.StreamUnary {
		return nil
	}
	builder := d.builder
	var policy *clientcontract.ResiliencePolicy
	if builder.clientOptions != nil {
		policy = builder.clientOptions.Resilience
	}
	var defaults *clientcontract.ResiliencePolicy
	if document.Defaults != nil {
		defaults = document.Defaults.Resilience
	}
	// The published transport is what this provider serves. Resume is declared
	// on the endpoint and only ever honored for a server stream: continuing a
	// duplex conversation would replay the caller's own messages.
	resume := builder.clientOptions != nil && builder.clientOptions.Resume && stream == clientcontract.StreamServer
	return &webSocketService{
		operationID: CanonicalOperationID(d.Method(), boundPath),
		document:    document,
		transport: clientcontract.Transport{
			Protocol: clientcontract.TransportWebSocket,
			Path:     boundPath,
			Encoding: clientcontract.EncodingJSON,
			WebSocket: &clientcontract.WebSocketTransport{
				Subprotocol: clientcontract.WebSocketSubprotocolV1,
				Resume:      resume,
			},
		},
		stream:  stream,
		budgets: resolveWebSocketBudgets(defaults, policy),
		before:  handler.Before,
		handle:  handler.Handle,
		log:     builder.logger(),
		resume:  newWebSocketResumeStore(),
	}
}

// clientStreamMode maps the transport stream mode onto the contract vocabulary.
func clientStreamMode(mode phttp.StreamMode) clientcontract.StreamMode {
	switch mode {
	case phttp.StreamModeServer:
		return clientcontract.StreamServer
	case phttp.StreamModeClient:
		return clientcontract.StreamClient
	case phttp.StreamModeBidirectional:
		return clientcontract.StreamBidirectional
	default:
		return clientcontract.StreamUnary
	}
}

// resolveWebSocketBudgets reads the declared stream policy, operation first and
// document default second, and applies the framework floor only where the
// contract is silent.
// webSocketAdmissionBackstop is the transport idle timeout a socket carries while
// admission runs. It MUST outlast the admission budget it backs: admit() owns the
// declared deadline and answers with the declared refusal, and a backstop armed
// at the same value turns that answer into a coin flip (see serve).
func webSocketAdmissionBackstop(handshake time.Duration) time.Duration {
	return handshake + webSocketAdmissionBackstopGrace
}

func resolveWebSocketBudgets(defaults, operation *clientcontract.ResiliencePolicy) webSocketBudgets {
	budgets := webSocketBudgets{
		Handshake:           defaultWebSocketHandshakeBudget,
		MaxBufferedMessages: defaultWebSocketQueueDepth,
		MaxFrameBytes:       phttp.DefaultMaxBodySize,
	}
	apply := func(value *clientcontract.ResiliencePolicy) {
		if value == nil {
			return
		}
		if value.AttemptTimeoutMs != nil && *value.AttemptTimeoutMs > 0 {
			budgets.Handshake = time.Duration(*value.AttemptTimeoutMs) * time.Millisecond
		}
		if value.Stream == nil {
			return
		}
		if value.Stream.HandshakeTimeoutMs != nil && *value.Stream.HandshakeTimeoutMs > 0 {
			budgets.Handshake = time.Duration(*value.Stream.HandshakeTimeoutMs) * time.Millisecond
		}
		if value.Stream.IdleTimeoutMs != nil && *value.Stream.IdleTimeoutMs > 0 {
			budgets.Idle = time.Duration(*value.Stream.IdleTimeoutMs) * time.Millisecond
		}
		if value.Stream.HeartbeatMs != nil && *value.Stream.HeartbeatMs > 0 {
			budgets.Heartbeat = time.Duration(*value.Stream.HeartbeatMs) * time.Millisecond
		}
		if value.Stream.MaxBufferedMessages != nil && *value.Stream.MaxBufferedMessages > 0 {
			budgets.MaxBufferedMessages = *value.Stream.MaxBufferedMessages
		}
		if value.Stream.MaxFrameBytes != nil && *value.Stream.MaxFrameBytes > 0 {
			budgets.MaxFrameBytes = *value.Stream.MaxFrameBytes
		}
	}
	apply(defaults)
	apply(operation)
	return budgets
}

// webSocketSession is one negotiated conversation. The reader goroutine owns
// the inbound half and the admission and handler goroutine owns the outbound
// half; the published conversation is shared between them under one mutex.
type webSocketSession struct {
	service *webSocketService
	conn    *phttp.WebSocketConn
	// write emits one encoded frame. It is the connection's own writer; a test
	// substitutes a recorder to observe the frames this provider would put on
	// the wire without standing up a socket.
	write  func([]byte) error
	cancel context.CancelFunc

	mu             sync.Mutex
	conversation   *clientcontract.WebSocketConversationV1
	serverSequence uint64
	terminal       bool

	init      chan *clientcontract.WebSocketInitFrameV1
	inbound   chan []byte
	inboundOn sync.Once
	readDone  chan struct{}

	// resumed records that this conversation continues an earlier one, and
	// resumedAfter the position it continues after. The handler reads that
	// position from its own context, so it produces what the consumer has not
	// received rather than the whole stream.
	resumed      bool
	resumedAfter uint64
	// resumeBudget is the number of continuations this stream still allows. It
	// is decremented by every redeemed grant, so a stream that keeps breaking
	// stops being resumable instead of living forever.
	resumeBudget int
	// grant is the live continuation grant of this session. Every message this
	// provider sends moves the position it buys.
	grant *webSocketResumeGrant

	failMu sync.Mutex
	failed error
}

// serve runs one first-party conversation from the negotiated socket to its
// single terminal frame.
func (s *webSocketService) serve(ctx *phttp.Context, conn *phttp.WebSocketConn) error {
	conn.SetMaxMessageBytes(s.budgets.MaxFrameBytes)
	// Admission has its own budget, and admit() is the one that OWNS it: it ends
	// a caller that never sent its init frame with the declared timeout refusal.
	// The transport idle timeout is only the BACKSTOP that unblocks the read pump
	// if that path never runs, so it is armed strictly later than the budget it
	// backs. Armed at the same value the two raced, and the loser decided the
	// terminal: the budget's typed CodeTimeout refusal reaches the client as an
	// error frame, while the transport timeout surfaces as a read failure that
	// webSocketReadFailure classifies CodeCancelled — a caller withdrawal, which
	// terminate answers with a bare normal close and NO error frame. A caller
	// that overran the declared handshake budget was told which of two equal
	// timers happened to fire first.
	conn.SetIdleTimeout(webSocketAdmissionBackstop(s.budgets.Handshake))

	sessionCtx, cancel := context.WithCancel(ctx.Context())
	defer cancel()
	session := &webSocketSession{
		service: s, conn: conn, cancel: cancel, resumeBudget: defaultWebSocketResumeBudget,
		conversation: clientcontract.NewWebSocketConversationV1(s.stream, s.transport.Encoding,
			s.transport.WebSocket != nil && s.transport.WebSocket.Resume),
		init:     make(chan *clientcontract.WebSocketInitFrameV1, 1),
		inbound:  make(chan []byte, s.budgets.MaxBufferedMessages),
		readDone: make(chan struct{}),
	}
	session.write = conn.WriteMessage
	go session.read(sessionCtx)
	defer func() {
		cancel()
		<-session.readDone
	}()

	admitted, err := session.admit(ctx, sessionCtx)
	if err != nil {
		return session.terminate(err)
	}
	return session.run(admitted, sessionCtx)
}

// admit waits for the mandatory init frame, reconstructs the request the
// endpoint's own security and validation chain expects, runs that chain, and
// only then tells the client the conversation is ready. No application message
// can reach a handler ahead of it: the published transition table refuses one.
func (s *webSocketSession) admit(ctx *phttp.Context, sessionCtx context.Context) (*phttp.Context, error) {
	budget := time.NewTimer(s.service.budgets.Handshake)
	defer budget.Stop()

	var frame *clientcontract.WebSocketInitFrameV1
	select {
	case frame = <-s.init:
	case <-budget.C:
		return nil, perrors.New(perrors.CodeTimeout, "websocket admission deadline exceeded")
	case <-s.conn.Draining():
		return nil, perrors.New(perrors.CodeUnavailable, "server is shutting down")
	case <-sessionCtx.Done():
		return nil, s.readFailure(sessionCtx.Err())
	}

	if frame.OperationID != s.service.operationID {
		return nil, contractRefusal(clientcontract.ErrorCodeInvalidTransport,
			"init operation does not match the upgraded route")
	}
	// The wire rule is consulted before this provider's own limit, so a refusal
	// names the contract when the contract has something to say. A resume is
	// redeemed before the security chain runs: a token that buys nothing must
	// not cost a credential check.
	resumeAfter, resumeErr := s.admitResume(frame)
	if resumeErr != nil {
		return nil, resumeErr
	}
	if s.service.transport.Encoding != clientcontract.EncodingJSON {
		// A provider that publishes an encoding it cannot serve would refuse at
		// the first message instead of at admission.
		return nil, contractRefusal(clientcontract.ErrorCodeInvalidTransport,
			"selected websocket transport encoding is not supported by this provider")
	}
	if frame.Request != nil && frame.Request.Encoding != clientcontract.EncodingJSON {
		return nil, contractRefusal(clientcontract.ErrorCodeInvalidTransport,
			"request encoding does not match the selected transport")
	}
	admissionCtx, err := s.reconstruct(ctx, frame)
	if err != nil {
		return nil, err
	}
	if s.service.before != nil {
		if response := s.service.before(admissionCtx); response != nil {
			return nil, responseRefusal(response)
		}
	}
	// Authentication may have been asynchronous. The conversation state is
	// re-read after it, so a client that canceled, a caller deadline that
	// elapsed, or a server that started draining during the check is never
	// answered with a ready frame.
	if err := s.recheck(frame, sessionCtx); err != nil {
		return nil, err
	}
	// A resumed stream continues the sequence it left: the next message the
	// handler sends carries resumeAfter+1, so a consumer sees neither a gap nor
	// a value it already read.
	resumed := frame.Resume != nil
	if resumed {
		s.mu.Lock()
		s.serverSequence = resumeAfter
		s.resumedAfter = resumeAfter
		s.resumed = true
		s.mu.Unlock()
	}
	// The token rotates on every ready frame: the one this connection presented
	// is spent, and only the new one can continue the stream after it.
	token, tokenErr := s.issueResumeGrant(frame.ClientID, resumeAfter)
	if tokenErr != nil {
		return nil, perrors.Wrap(tokenErr, perrors.CodeInternal)
	}
	if err := s.send(&clientcontract.WebSocketReadyFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameReady,
		Resumed: &resumed, ResumeToken: token,
	}); err != nil {
		return nil, err
	}
	// The conversation is admitted: the idle budget replaces the handshake one.
	s.conn.SetIdleTimeout(s.service.budgets.Idle)
	return admissionCtx, nil
}

// recheck re-reads the facts that an asynchronous credential check may have
// outlived, between the end of that check and the ready frame.
func (s *webSocketSession) recheck(frame *clientcontract.WebSocketInitFrameV1, sessionCtx context.Context) error {
	select {
	case <-s.conn.Draining():
		return perrors.New(perrors.CodeUnavailable, "server is shutting down")
	case <-sessionCtx.Done():
		return s.readFailure(sessionCtx.Err())
	default:
	}
	s.mu.Lock()
	state := s.conversation.State()
	s.mu.Unlock()
	if state != clientcontract.WebSocketAwaitReady {
		return contractRefusal(clientcontract.ErrorCodeInvalidTransport,
			"conversation left admission before the provider was ready")
	}
	if deadline, ok := webSocketDeadline(frame.DeadlineUnixMs); ok && !time.Now().Before(deadline) {
		return perrors.New(perrors.CodeTimeout, "caller deadline elapsed during admission")
	}
	return nil
}

// webSocketSequence reads one canonical uint64 decimal sequence value.
func webSocketSequence(value string) (uint64, bool) {
	sequence, err := strconv.ParseUint(value, 10, 64)
	return sequence, err == nil
}

// webSocketDeadline reads the caller's absolute deadline. "0" declares none.
func webSocketDeadline(value string) (time.Time, bool) {
	milliseconds, err := strconv.ParseUint(value, 10, 64)
	if err != nil || milliseconds == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(milliseconds)), true //nolint:gosec // the value is a validated uint64 millisecond instant
}

// reconstruct rebuilds the request the endpoint declared from the protected
// init frame: declared credential profiles become their injection headers,
// ordinary declared headers are replayed, and bounded propagation values are
// restored. Admission material never travels on the handshake, so this is the
// only place it enters the request.
func (s *webSocketSession) reconstruct(ctx *phttp.Context, frame *clientcontract.WebSocketInitFrameV1) (*phttp.Context, error) {
	header := make(http.Header, len(frame.Headers)+len(frame.Credentials)+4)
	credentialHeaders := map[string]bool{}
	for name, profile := range s.service.document.Credentials {
		_ = name
		if profile.Header != "" {
			credentialHeaders[strings.ToLower(profile.Header)] = true
		}
	}
	for _, ordinary := range frame.Headers {
		if credentialHeaders[strings.ToLower(ordinary.Name)] {
			return nil, contractRefusal(clientcontract.ErrorCodeInvalidSecurity,
				"ordinary headers cannot override a declared credential profile")
		}
		for _, value := range ordinary.Values {
			header.Add(ordinary.Name, value)
		}
	}
	for _, credential := range frame.Credentials {
		profile, declared := s.service.document.Credentials[credential.Profile]
		if !declared {
			return nil, contractRefusal(clientcontract.ErrorCodeUnknownProfile,
				"init references an undeclared credential profile")
		}
		name, err := webSocketCredentialHeader(profile)
		if err != nil {
			return nil, err
		}
		header.Set(name, credential.Value)
	}
	if frame.Context != nil {
		setWhenPresent(header, "Traceparent", frame.Context.Traceparent)
		setWhenPresent(header, "Tracestate", frame.Context.Tracestate)
		setWhenPresent(header, "Baggage", frame.Context.Baggage)
		setWhenPresent(header, "X-Request-Id", frame.Context.RequestID)
	}
	header.Set("X-Client-Id", frame.ClientID)

	request := ctx.Request.Clone(ctx.Context())
	request.Header = header
	admission := ctx.WithContext(ctx.Context())
	admission.Request = request
	// A forwarded user token arrives in band, so the request-scoped bearer that
	// NewContext derives from the Authorization header is restored here.
	if scheme, token, ok := strings.Cut(strings.TrimSpace(header.Get("Authorization")), " "); ok &&
		strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(token) != "" {
		admission.SetContext(phttp.ContextWithForwardedBearer(admission.Context(), token))
		admission.Request = request.WithContext(admission.Context())
	}
	return admission, nil
}

// webSocketCredentialHeader names the injection header of a declared profile.
// It is the exact inverse of the mapping a generated client applies when it
// moves the resolved credential into the init frame.
func webSocketCredentialHeader(profile clientcontract.CredentialProfile) (string, error) {
	switch profile.Kind {
	case clientcontract.CredentialServiceToken, clientcontract.CredentialForwardedUserToken:
		return "Authorization", nil
	case clientcontract.CredentialAPIKey, clientcontract.CredentialNamedHeader:
		if profile.Header == "" {
			return "", contractRefusal(clientcontract.ErrorCodeInvalidSecurity,
				"declared credential profile has no injection header")
		}
		return profile.Header, nil
	default:
		return "", contractRefusal(clientcontract.ErrorCodeUnknownProfile,
			"declared credential profile kind is unsupported")
	}
}

func setWhenPresent(header http.Header, name, value string) {
	if value != "" {
		header.Set(name, value)
	}
}

// run drives the admitted conversation: the endpoint handler reads the inbound
// queue and writes provider messages, and its return decides the single
// terminal frame.
func (s *webSocketSession) run(admitted *phttp.Context, sessionCtx context.Context) error {
	if s.service.budgets.Heartbeat > 0 {
		go s.heartbeat(sessionCtx)
	}
	go s.watchShutdown(sessionCtx)

	inbound := s.inbound
	if s.service.stream == clientcontract.StreamServer {
		// A server stream declares no client message direction, so the handler
		// is handed a closed queue rather than one that can never fill.
		closed := make(chan []byte)
		close(closed)
		inbound = closed
	}
	handlerCtx := sessionCtx
	s.mu.Lock()
	resumed, resumedAfter := s.resumed, s.resumedAfter
	s.mu.Unlock()
	if resumed {
		handlerCtx = withStreamResume(sessionCtx, resumedAfter)
	}
	stream := phttp.NewStreamContext(admitted.WithContext(handlerCtx), s.sendMessage, inbound)
	err := s.service.handle(stream)
	if err != nil {
		// The inbound half saw why the conversation ended before the handler saw
		// its symptom, so its reason is the one the peer is told.
		return s.terminate(s.readFailure(err))
	}
	if failure := s.readFailure(nil); failure != nil {
		return s.terminate(failure)
	}
	return s.complete(stream)
}

// complete emits the single successful terminal frame. A server stream has
// already delivered its values as messages, so its result carries no payload;
// a client stream must declare the value its caller waits for.
func (s *webSocketSession) complete(stream *phttp.StreamContext) error {
	frame := &clientcontract.WebSocketResultFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameResult,
	}
	if s.service.stream != clientcontract.StreamServer {
		value, declared := stream.Result()
		switch {
		case declared:
			encoded, err := json.Marshal(value)
			if err != nil {
				return s.terminate(perrors.Wrap(err, perrors.CodeInternal))
			}
			frame.Payload = &clientcontract.WebSocketEncodedPayloadV1{
				Encoding: clientcontract.EncodingJSON, Value: encoded,
			}
		case s.service.stream == clientcontract.StreamClient:
			return s.terminate(perrors.New(perrors.CodeInternal,
				"client stream handler completed without the declared result value"))
		}
	}
	if err := s.send(frame); err != nil {
		return err
	}
	return s.conn.CloseWith(phttp.WebSocketCloseNormal, "stream complete")
}

// terminate emits the single failed terminal frame and closes the socket with
// a code the peer can act on. Neither the frame nor the close reason carries
// credential, token or payload material.
func (s *webSocketSession) terminate(err error) error {
	if err == nil {
		return nil
	}
	if isWebSocketCallerWithdrawal(err) {
		// The client already reached its own terminal with a cancel frame, so a
		// second terminal frame would be a second delivery.
		return s.conn.CloseWith(phttp.WebSocketCloseNormal, "stream canceled")
	}
	frame := webSocketErrorFrame(err)
	if sendErr := s.send(frame); sendErr != nil {
		s.service.log.Debug(fmt.Sprintf("websocket terminal error frame was not delivered: %v", sendErr))
	}
	return s.conn.CloseWith(webSocketCloseCode(frame.Error.Status), "stream failed")
}

// webSocketErrorFrame projects a terminal failure into the published error
// envelope. A refusal this package shaped keeps its exact status, code and
// declared details; anything else goes through the framework's own sanitized
// HTTP envelope, so an internal cause never reaches the wire.
func webSocketErrorFrame(err error) *clientcontract.WebSocketErrorFrameV1 {
	frame := &clientcontract.WebSocketErrorFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameError,
	}
	var refusal *webSocketRefusal
	if stderrors.As(err, &refusal) {
		frame.Error = clientcontract.WebSocketRemoteErrorV1{
			Status: refusal.status, Code: refusal.code, Message: refusal.message,
			Details: refusal.details,
		}
	} else {
		status, body := perrors.HTTPErrorResponse(err)
		frame.Error = clientcontract.WebSocketRemoteErrorV1{
			Status: status, Code: body.Code, Message: body.Message,
		}
		if body.Details != nil {
			if details, marshalErr := json.Marshal(body.Details); marshalErr == nil {
				frame.Error.Details = details
			}
		}
	}
	if retryable := perrors.IsRetryable(err); retryable {
		frame.Error.Retryable = &retryable
	}
	if frame.Error.Status < 400 || frame.Error.Status > 599 {
		frame.Error.Status = http.StatusInternalServerError
	}
	if frame.Error.Code == "" {
		frame.Error.Code = string(perrors.CodeInternal)
	}
	if len(frame.Error.Message) > webSocketErrorMessageMax {
		frame.Error.Message = frame.Error.Message[:webSocketErrorMessageMax]
	}
	return frame
}

// webSocketCloseCode maps the typed error onto the RFC 6455 vocabulary. The
// code is the only thing a peer without the error frame can read, so it stays
// coarse and carries nothing about the operation.
func webSocketCloseCode(status int) int {
	switch {
	case status == http.StatusServiceUnavailable:
		return phttp.WebSocketCloseGoingAway
	case status >= 400 && status < 500:
		return phttp.WebSocketClosePolicyViolation
	default:
		return phttp.WebSocketCloseInternalError
	}
}

// isWebSocketCallerWithdrawal reports an end the caller asked for rather than a
// failure the provider must report.
func isWebSocketCallerWithdrawal(err error) bool {
	return perrors.GetCode(err) == perrors.CodeCancelled || stderrors.Is(err, context.Canceled)
}

// heartbeat keeps the declared cadence until the conversation ends. A provider
// that declares none sends none: a heartbeat nobody declared would hide an idle
// socket instead of revealing it.
func (s *webSocketSession) heartbeat(sessionCtx context.Context) {
	ticker := time.NewTicker(s.service.budgets.Heartbeat)
	defer ticker.Stop()
	nonce := uint64(0)
	for {
		select {
		case <-sessionCtx.Done():
			return
		case <-ticker.C:
			nonce++
			if err := s.send(&clientcontract.WebSocketHeartbeatFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePing,
				Nonce: "provider-" + strconv.FormatUint(nonce, 10),
			}); err != nil {
				return
			}
		}
	}
}

// watchShutdown ends the conversation deliberately when the server starts a
// graceful stop, so a client reads a typed error instead of a cut socket.
func (s *webSocketSession) watchShutdown(sessionCtx context.Context) {
	select {
	case <-sessionCtx.Done():
	case <-s.conn.Draining():
		s.recordFailure(perrors.New(perrors.CodeUnavailable, "server is shutting down"))
		s.cancel()
	}
}

// sendMessage emits one provider message under the declared sequence. The
// sequence is the conversation's, not this package's: an out-of-order value is
// refused by the published session rules before it reaches the wire.
func (s *webSocketSession) sendMessage(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return perrors.Wrap(err, perrors.CodeInternal)
	}
	s.mu.Lock()
	next := s.serverSequence + 1
	s.mu.Unlock()
	return s.send(&clientcontract.WebSocketMessageFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameMessage,
		Sequence: strconv.FormatUint(next, 10),
		Payload: clientcontract.WebSocketEncodedPayloadV1{
			Encoding: clientcontract.EncodingJSON, Value: encoded,
		},
	})
}

// send accepts one provider frame through the published conversation and, only
// if the conversation accepts it, writes it.
//
// Every frame, application messages included, is re-read by the contract's own
// strict parser before it reaches the conversation. A frame this provider
// composed wrongly is then a local fault carrying the contract's diagnostic
// code, and the handler that produced it learns so from Send, instead of a byte
// sequence a conforming client can only refuse. The wire is strict about more
// than shape — a JSON null anywhere inside a payload is refused — so a message
// payload is exactly where that second read earns its cost.
func (s *webSocketSession) send(frame any) error {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return perrors.Wrap(err, perrors.CodeInternal)
	}
	accepted, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(encoded)
	if diag.HasErrors(diagnostics) {
		return webSocketContractError(diagnostics[0])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		return perrors.New(perrors.CodeConflict, "stream already reached its terminal frame")
	}
	if diagnostics := s.conversation.Accept(accepted, clientcontract.WebSocketServerToClient); diag.HasErrors(diagnostics) {
		return webSocketContractError(diagnostics[0])
	}
	if message, ok := frame.(*clientcontract.WebSocketMessageFrameV1); ok {
		// The frame has just passed the contract's strict parser, which only
		// admits a canonical uint64 decimal, so this cannot fail.
		if sequence, parseErr := strconv.ParseUint(message.Sequence, 10, 64); parseErr == nil {
			s.serverSequence = sequence
			// The grant buys the position this provider has actually reached,
			// so a later continuation cannot claim more than was delivered.
			if s.grant != nil {
				s.grant.cursor.Store(sequence)
			}
		}
	}
	if s.conversation.State() == clientcontract.WebSocketTerminal {
		s.terminal = true
	}
	return s.write(encoded)
}

// read is the inbound half of the conversation. It runs from the first byte, so
// a heartbeat or a cancellation is answered while admission is still waiting on
// an asynchronous credential check.
func (s *webSocketSession) read(sessionCtx context.Context) {
	defer close(s.readDone)
	defer s.closeInbound()
	for {
		opcode, payload, err := s.conn.ReadMessage()
		if err != nil {
			s.recordFailure(webSocketReadFailure(err))
			s.cancel()
			return
		}
		if opcode != 0x1 {
			s.recordFailure(contractRefusal(clientcontract.ErrorCodeInvalidTransport,
				"first-party websocket frames are JSON text messages"))
			s.cancel()
			return
		}
		frame, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(payload)
		if diag.HasErrors(diagnostics) {
			s.recordFailure(webSocketContractError(diagnostics[0]))
			s.cancel()
			return
		}
		s.mu.Lock()
		diagnostics = s.conversation.Accept(frame, clientcontract.WebSocketClientToServer)
		s.mu.Unlock()
		if diag.HasErrors(diagnostics) {
			s.recordFailure(webSocketContractError(diagnostics[0]))
			s.cancel()
			return
		}
		if done := s.dispatch(sessionCtx, frame); done {
			return
		}
	}
}

// dispatch routes one accepted client frame. It reports whether the inbound
// half is finished.
func (s *webSocketSession) dispatch(sessionCtx context.Context, frame any) bool {
	switch value := frame.(type) {
	case *clientcontract.WebSocketInitFrameV1:
		s.init <- value
	case *clientcontract.WebSocketHeartbeatFrameV1:
		if value.Type == clientcontract.WebSocketFramePing {
			if err := s.send(&clientcontract.WebSocketHeartbeatFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePong, Nonce: value.Nonce,
			}); err != nil {
				s.recordFailure(err)
				s.cancel()
				return true
			}
		}
	case *clientcontract.WebSocketMessageFrameV1:
		// A full queue applies backpressure to the socket instead of dropping a
		// message; the declared idle budget still releases a stalled handler.
		select {
		case s.inbound <- value.Payload.Value:
		case <-sessionCtx.Done():
			return true
		}
	case *clientcontract.WebSocketHalfCloseFrameV1:
		s.closeInbound()
	case *clientcontract.WebSocketCancelFrameV1:
		s.recordFailure(perrors.New(perrors.CodeCancelled, "caller canceled the stream"))
		s.cancel()
		return true
	}
	return false
}

func (s *webSocketSession) closeInbound() {
	s.inboundOn.Do(func() { close(s.inbound) })
}

func (s *webSocketSession) recordFailure(err error) {
	if err == nil {
		return
	}
	s.failMu.Lock()
	if s.failed == nil {
		s.failed = err
	}
	s.failMu.Unlock()
}

// readFailure returns the inbound failure, preferring it over fallback so the
// conversation ends with the reason the wire gave rather than the symptom the
// handler saw.
func (s *webSocketSession) readFailure(fallback error) error {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if s.failed != nil {
		return s.failed
	}
	return fallback
}

// webSocketReadFailure projects a transport failure. A peer close and a
// canceled session are ordinary ends, not provider faults.
func webSocketReadFailure(err error) error {
	var closed *phttp.WebSocketCloseError
	if stderrors.As(err, &closed) {
		return perrors.New(perrors.CodeCancelled, "caller closed the stream")
	}
	var protocolErr *phttp.WebSocketProtocolError
	if stderrors.As(err, &protocolErr) {
		return &webSocketRefusal{
			status: http.StatusBadRequest, code: string(perrors.CodeInvalidArg), message: protocolErr.Error(),
		}
	}
	return perrors.New(perrors.CodeCancelled, "stream connection ended")
}

// webSocketRefusal is a terminal failure whose HTTP status, stable code and
// declared details are decided here rather than derived from the framework code
// table. The published error envelope carries all three, so a refusal names the
// rule it applied instead of collapsing into an opaque internal error.
type webSocketRefusal struct {
	status  int
	code    string
	message string
	details json.RawMessage
}

// Error implements error with the refusal's own client-safe text.
func (r *webSocketRefusal) Error() string { return r.code + ": " + r.message }

// webSocketErrorMessageMax is the published bound on a typed error message.
const webSocketErrorMessageMax = 4096

// contractRefusal turns a wire rule into the typed error the terminal frame
// carries. The code is the contract's own diagnostic code, so a generated
// client names the rule it broke instead of a code this package invented.
func contractRefusal(code, message string) error {
	return &webSocketRefusal{status: http.StatusBadRequest, code: code, message: message}
}

// webSocketContractError projects one refusal diagnostic.
func webSocketContractError(diagnostic diag.Diagnostic) error {
	message := diagnostic.Message
	if diagnostic.Field != "" {
		message = diagnostic.Field + ": " + diagnostic.Message
	}
	return contractRefusal(diagnostic.Code, message)
}

// responseRefusal projects a refusal from the endpoint's own security and
// validation chain into the typed terminal error, keeping the exact status and
// code the same endpoint would answer on a unary call.
func responseRefusal(response *phttp.Response) error {
	status := response.Status
	if status < 400 || status > 599 {
		status = http.StatusForbidden
	}
	body := struct {
		Code    string          `json:"code"`
		Error   string          `json:"error"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details,omitempty"`
	}{}
	if encoded, err := response.BodyBytes(); err == nil && len(encoded) > 0 {
		_ = json.Unmarshal(encoded, &body) //nolint:errcheck // a body this endpoint did not shape leaves the framework code below
	}
	if body.Code == "" {
		body.Code = string(perrors.CodeForbidden)
	}
	return &webSocketRefusal{status: status, code: body.Code, message: body.Message, details: body.Details}
}
