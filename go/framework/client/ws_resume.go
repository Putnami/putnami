package client

import (
	"context"
	stderrors "errors"
	"strconv"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// maxStreamResumeAttempts bounds how many times one session continues over a
// new socket. It is a bound, never a behavior: a stream resumes only because
// the provider declared resume and the operation declared reconnect, and this
// caps how long a socket that keeps breaking may keep a session alive.
const maxStreamResumeAttempts = 5

// webSocketResumePolicy is the resolved answer to "may this stream continue
// over a second socket, and how many times?".
//
// Resume re-reads a position the caller already consumed up to, so it is only
// sound where re-reading has no effect: a server stream, declared safe, on a
// transport whose provider states it can continue without a gap, and only when
// the operation declares reconnect. All four are provider declarations; none is
// a consumer flag.
type webSocketResumePolicy struct {
	enabled bool
	limit   int
}

// resolveWebSocketResume reads the declared resume agreement. The provider
// half is the transport's own websocket.resume; the operation half is
// resilience.stream.reconnect, read operation-first and document-second exactly
// like every other bound.
func resolveWebSocketResume(operation Operation, mode clientcontract.StreamMode,
	transport *clientcontract.Transport, document *clientcontract.ResiliencePolicy) webSocketResumePolicy {
	if mode != clientcontract.StreamServer {
		return webSocketResumePolicy{}
	}
	if transport == nil || transport.WebSocket == nil || !transport.WebSocket.Resume {
		return webSocketResumePolicy{}
	}
	// A stream that is not declared safe cannot be continued: continuing is
	// re-reading, and re-reading an operation with effects repeats them.
	if operation.Contract.Idempotency.Kind != clientcontract.IdempotencySafe {
		return webSocketResumePolicy{}
	}
	if !streamReconnect(document, operation.Contract.Resilience) {
		return webSocketResumePolicy{}
	}
	return webSocketResumePolicy{enabled: true, limit: maxStreamResumeAttempts}
}

// streamReconnect resolves the consumer half of every continuation agreement,
// resilience.stream.reconnect, operation first and document second exactly like
// every other bound. The WebSocket resume and the SSE continuation read it the
// same way.
func streamReconnect(document, operation *clientcontract.ResiliencePolicy) bool {
	reconnect := false
	for _, value := range []*clientcontract.ResiliencePolicy{document, operation} {
		if value != nil && value.Stream != nil && value.Stream.Reconnect != nil {
			reconnect = *value.Stream.Reconnect
		}
	}
	return reconnect
}

// resumePolicy resolves this opener's declared resume agreement.
func (opener *wsOpener) resumePolicy() webSocketResumePolicy {
	var document *clientcontract.ResiliencePolicy
	if opener.runtime.descriptor.Contract.Defaults != nil {
		document = opener.runtime.descriptor.Contract.Defaults.Resilience
	}
	return resolveWebSocketResume(opener.operation, opener.mode, opener.transport, document)
}

// openWebSocketServerStream opens a declared server stream over the first-party
// WebSocket wire and, when the declaration allows it, continues it over a new
// socket after a transport break.
//
// A terminal whose cause was the provider saying this wire is not served at
// this path is tagged for the dispatcher, without changing the error the caller
// reads: an upgrade refused with 404, 405 or 426 already carries that in its
// status, and a handshake that completed without the first-party subprotocol
// carries it nowhere else.
func openWebSocketServerStream[T any](ctx context.Context, client *Client, request *Request, operation Operation,
	errorMapper []func(error) error) (*Stream[T], error) {
	opener, err := newWSOpener(ctx, client, request, operation, clientcontract.StreamServer, nil, errorMapper)
	if err != nil {
		return nil, err
	}
	if err := opener.session.config.Breaker.AllowRequest(); err != nil {
		return nil, opener.fail(err)
	}
	service, err := opener.connect(nil)
	if err != nil {
		return nil, markAbsentWire(opener.fail(err), opener.unavailable)
	}
	messages := make(chan T, opener.budgets.MaxBufferedMessages)
	done := make(chan struct{})
	stream := &Stream[T]{messages: messages, done: done, cancel: service.requestCancel}
	delivery := &webSocketDelivery[T]{messages: messages}
	go func() {
		defer close(done)
		defer close(messages)
		stream.setErr(runResumableServerStream(opener, service, delivery))
	}()
	return stream, nil
}

// runResumableServerStream reads the admitted conversation to its terminal and,
// on a transport break the declaration allows continuing, opens a new socket
// that continues after the last sequence the caller completely received.
//
// The session is the same across every socket: one breaker verdict, one call
// measurement, one set of budgets. Only the socket, the credential and the
// conversation are new — which is what makes an expired credential fail the
// resume rather than travel a second time.
func runResumableServerStream[TOut any](opener *wsOpener, service *wsService,
	delivery *webSocketDelivery[TOut]) error {
	policy := opener.resumePolicy()
	attempts := 0
	for {
		err, broken := readWebSocketStream(service, delivery)
		service.shutdown(err)
		if err == nil {
			opener.session.Complete()
			opener.session.Close() //nolint:errcheck // Close always returns nil; the caller reads the terminal from the stream
			return nil
		}
		resumable := policy.enabled && broken && attempts < policy.limit &&
			opener.resumeToken != "" && opener.session.Context().Err() == nil
		if !resumable {
			surfaced := opener.session.Fail(err)
			opener.session.Close() //nolint:errcheck // Close always returns nil; the terminal error is the return value
			return surfaced
		}
		attempts++
		next, connectErr := opener.connect(&clientcontract.WebSocketResumeRequestV1{
			Token: opener.resumeToken,
			// The last sequence the caller completely received. A message that
			// was decoded but never handed over is not counted: the provider
			// would then skip a value nobody read.
			AfterSequence: strconv.FormatUint(delivery.delivered, 10),
		})
		if connectErr != nil {
			surfaced := opener.session.Fail(connectErr)
			opener.session.Close() //nolint:errcheck // Close always returns nil; the terminal error is the return value
			return surfaced
		}
		service = next
	}
}

// transportBreak reports a socket that ended without a terminal frame while the
// session was still live.
//
// It is the only class a declared resume continues. A contract violation, a
// typed provider error, a budget expiry and a caller withdrawal are all facts
// about the stream: re-opening would repeat the same answer, or hide a defect.
func (service *wsService) transportBreak(err error) bool {
	if err == nil || service.session.Context().Err() != nil || service.session.idleBudgetExpired() {
		return false
	}
	var contractErr *WebSocketContractError
	if stderrors.As(err, &contractErr) {
		return false
	}
	var protocolErr *wsProtocolError
	if stderrors.As(err, &protocolErr) {
		return false
	}
	var closeErr *wsCloseError
	if stderrors.As(err, &closeErr) {
		// A normal close without a terminal frame is still a break: the wire
		// requires the provider to say how the stream ended.
		return true
	}
	// What is left is the socket itself failing: a reset, an EOF, a peer that
	// vanished. Those are the breaks a resume exists for.
	return !errors.Is(err, CodeClientDeadline) && !errors.Is(err, CodeClientCanceled)
}

// markUnavailable records a provider answer that says the first-party WebSocket
// wire is not served at this path, so the declared transport fallback can act
// on it. Every other answer is a fact about the call.
func (opener *wsOpener) markUnavailable(dialErr error) {
	if stderrors.Is(dialErr, errWebSocketNotNegotiated) {
		opener.unavailable = true
		return
	}
	var rejection *webSocketHandshakeRejection
	if stderrors.As(dialErr, &rejection) {
		opener.unavailable = streamTransportUnavailableStatus(rejection.StatusCode)
		return
	}
	var remote *RemoteError
	if stderrors.As(dialErr, &remote) {
		opener.unavailable = streamTransportUnavailableStatus(remote.StatusCode)
	}
}
