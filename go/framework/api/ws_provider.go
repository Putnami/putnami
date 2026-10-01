package api

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// providerWireService drives one route whose WebSocket wire the provider owns:
// raw octets in binary messages, or one JSON value per text message under the
// provider's declared subprotocol. It owns the socket — bounds, heartbeat,
// shutdown and close codes — and nothing of the vocabulary. The upgrade
// request was the admission: the route's security and validation chain ran on
// it before this driver was handed the socket.
type providerWireService struct {
	subprotocol string
	bytes       bool
	budgets     webSocketBudgets
	handle      func(*phttp.StreamContext) error
	log         *logger.Logger
}

// providerWireService returns the driver of this endpoint's provider-owned
// wire, or nil when the endpoint declares none. The declared stream policy
// applies whether or not the API publishes a client contract; without one the
// framework floors apply.
func (d EndpointDefinition) providerWireService(document *clientcontract.DocumentV1,
	handler phttp.StreamHandler) *providerWireService {
	wire := d.ProviderWire()
	if wire == nil || handler.Handle == nil {
		return nil
	}
	var defaults, policy *clientcontract.ResiliencePolicy
	if document != nil && document.Defaults != nil {
		defaults = document.Defaults.Resilience
	}
	if d.builder.clientOptions != nil {
		policy = d.builder.clientOptions.Resilience
	}
	return &providerWireService{
		subprotocol: wire.Subprotocol,
		bytes:       wire.Bytes,
		budgets:     resolveWebSocketBudgets(defaults, policy),
		handle:      handler.Handle,
		log:         d.builder.logger(),
	}
}

// providerWireSession is one admitted socket. The reader goroutine owns the
// inbound half; the handler owns the outbound half through send.
type providerWireSession struct {
	service *providerWireService
	conn    *phttp.WebSocketConn
	cancel  context.CancelCauseFunc

	inbound  chan []byte
	readDone chan struct{}

	// peerClosed records that the client sent a close frame. Its direction is
	// over, and the socket is not worth a second close frame.
	peerClosed atomic.Bool

	failMu sync.Mutex
	failed *providerWireFailure
}

// providerWireFailure is why a provider-owned wire stopped abnormally, with the
// close code the peer is told. The reason is fixed framework text: a close
// reason travels in clear text and never carries payload material.
type providerWireFailure struct {
	closeCode int
	reason    string
	err       error
}

func (f *providerWireFailure) Error() string { return f.err.Error() }

func (f *providerWireFailure) Unwrap() error { return f.err }

// serve runs one admitted socket from the handler's start to its close.
func (s *providerWireService) serve(ctx *phttp.Context, conn *phttp.WebSocketConn) error {
	conn.SetMaxMessageBytes(s.budgets.MaxFrameBytes)
	// The declared idle bound replaces the server's own, exactly as it does on
	// the first-party conversation: an undeclared one does not bound the socket.
	conn.SetIdleTimeout(s.budgets.Idle)
	sessionCtx, cancel := context.WithCancelCause(ctx.Context())
	defer cancel(nil)
	session := &providerWireSession{
		service:  s,
		conn:     conn,
		cancel:   cancel,
		inbound:  make(chan []byte, s.budgets.MaxBufferedMessages),
		readDone: make(chan struct{}),
	}
	go session.read(sessionCtx)
	if s.budgets.Heartbeat > 0 {
		go session.heartbeat(sessionCtx)
	}
	go session.watchShutdown(sessionCtx)
	stream := phttp.NewStreamContext(ctx.WithContext(sessionCtx), session.send, session.inbound)
	// The outcome is told to the peer as a close code and logged here; the
	// transport has nothing more to report.
	session.finish(s.handle(stream))
	return nil
}

// read delivers the client's messages to the handler. A normal client close
// ends the inbound direction and nothing else, so a byte handler reads io.EOF;
// every other end cancels the session with its cause first.
func (s *providerWireSession) read(sessionCtx context.Context) {
	defer close(s.readDone)
	defer close(s.inbound)
	want, name := byte(0x1), "text"
	if s.service.bytes {
		want, name = 0x2, "binary"
	}
	for {
		opcode, payload, err := s.conn.ReadMessage()
		if err != nil {
			s.readFailure(err)
			return
		}
		if opcode != want {
			s.fail(&providerWireFailure{
				closeCode: phttp.WebSocketCloseUnsupportedData, reason: "unsupported message type",
				err: fmt.Errorf("api: provider-owned websocket wire carries %s messages only", name),
			})
			return
		}
		if !s.service.bytes && !json.Valid(payload) {
			// A typed wire carries one JSON value per message; anything else is
			// a payload the declared vocabulary cannot hold.
			s.fail(&providerWireFailure{
				closeCode: phttp.WebSocketCloseInvalidPayload, reason: "invalid frame",
				err: perrors.BadRequest("provider-owned websocket frame is not a JSON value"),
			})
			return
		}
		// A full queue applies backpressure to the socket instead of dropping a
		// message; the idle bound and cancellation still release it.
		select {
		case s.inbound <- payload:
		case <-sessionCtx.Done():
			return
		}
	}
}

// readFailure classifies why the inbound half ended.
func (s *providerWireSession) readFailure(err error) {
	var closed *phttp.WebSocketCloseError
	if stderrors.As(err, &closed) {
		s.peerClosed.Store(true)
		if closed.Code == phttp.WebSocketCloseNormal {
			return
		}
		s.fail(&providerWireFailure{
			closeCode: phttp.WebSocketCloseNormal, reason: "stream closed",
			err: fmt.Errorf("api: client closed the stream with code %d", closed.Code),
		})
		return
	}
	var protocolErr *phttp.WebSocketProtocolError
	if stderrors.As(err, &protocolErr) {
		s.fail(&providerWireFailure{closeCode: protocolErr.CloseCode, reason: "protocol error", err: err})
		return
	}
	var netErr net.Error
	if stderrors.As(err, &netErr) && netErr.Timeout() {
		s.fail(&providerWireFailure{
			closeCode: phttp.WebSocketClosePolicyViolation, reason: "idle timeout",
			err: perrors.New(perrors.CodeTimeout, "websocket stream was idle past its declared bound"),
		})
		return
	}
	s.peerClosed.Store(true)
	s.fail(&providerWireFailure{
		closeCode: phttp.WebSocketCloseNormal, reason: "stream closed",
		err: perrors.New(perrors.CodeCancelled, "websocket connection ended"),
	})
}

// send writes one handler value. A byte stream writes octets in binary messages
// no larger than the declared frame bound; a typed wire writes one JSON value
// per text message, and a value past the bound is refused, not split.
func (s *providerWireSession) send(value any) error {
	if !s.service.bytes {
		encoded, err := json.Marshal(value)
		if err != nil {
			return perrors.Wrap(err, perrors.CodeInternal)
		}
		return s.conn.WriteMessage(encoded)
	}
	chunk, ok := value.([]byte)
	if !ok {
		return errByteStreamValue
	}
	limit := int(s.conn.MaxMessageBytes())
	for len(chunk) > 0 {
		size := min(len(chunk), limit)
		if err := s.conn.WriteBinary(chunk[:size]); err != nil {
			return err
		}
		chunk = chunk[size:]
	}
	return nil
}

// heartbeat sends an RFC 6455 ping at the declared cadence. A provider that
// declares none sends none: a heartbeat nobody declared would hide an idle
// socket instead of revealing it.
func (s *providerWireSession) heartbeat(sessionCtx context.Context) {
	ticker := time.NewTicker(s.service.budgets.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-sessionCtx.Done():
			return
		case <-ticker.C:
			if err := s.conn.Ping(nil); err != nil {
				return
			}
		}
	}
}

// watchShutdown ends the socket with going-away when the server drains.
func (s *providerWireSession) watchShutdown(sessionCtx context.Context) {
	select {
	case <-sessionCtx.Done():
	case <-s.conn.Draining():
		s.fail(&providerWireFailure{
			closeCode: phttp.WebSocketCloseGoingAway, reason: "server shutting down",
			err: perrors.New(perrors.CodeUnavailable, "server is shutting down"),
		})
	}
}

// fail records the first abnormal end, cancels the session with it, and
// releases a reader blocked on the socket, so a handler waiting for the next
// client message learns the stream is over instead of waiting for a peer that
// will never write again.
func (s *providerWireSession) fail(failure *providerWireFailure) {
	s.failMu.Lock()
	if s.failed == nil {
		s.failed = failure
	}
	s.failMu.Unlock()
	s.cancel(failure)
	_ = s.conn.SetReadDeadline(time.Now()) //nolint:errcheck // a socket that is already gone has no reader to release
}

func (s *providerWireSession) failure() *providerWireFailure {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	return s.failed
}

// finish closes the socket with the code that says how it ended: the recorded
// abnormal end first, then the handler's own error, then a normal close.
func (s *providerWireSession) finish(handlerErr error) {
	code, reason := phttp.WebSocketCloseNormal, "stream complete"
	failure := s.failure()
	switch {
	case failure != nil:
		code, reason = failure.closeCode, failure.reason
	case handlerErr != nil && !isWebSocketCallerWithdrawal(handlerErr):
		status, _ := perrors.HTTPErrorResponse(handlerErr)
		code, reason = webSocketCloseCode(status), "stream failed"
		s.service.log.Error("provider websocket stream handler error", handlerErr)
	}
	var closeErr error
	if !s.peerClosed.Load() {
		closeErr = s.conn.CloseWith(code, reason)
	}
	s.cancel(nil)
	<-s.readDone
	if closeErr != nil {
		s.service.log.Debug(fmt.Sprintf("provider websocket close was not delivered: %v", closeErr))
	}
}
