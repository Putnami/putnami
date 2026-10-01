package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

const (
	defaultStreamIdleTimeout = 30 * time.Second
	defaultStreamBuffer      = 64
	defaultStreamFrameBytes  = int64(1 << 20)
)

// Stream is a generated, typed server stream. Messages closes exactly once at
// the protocol terminal. Err returns the sanitized terminal error, if any.
// Close cancels the request and releases the underlying response body.
type Stream[T any] struct {
	messages <-chan T
	done     <-chan struct{}
	cancel   context.CancelFunc

	mu  sync.RWMutex
	err error

	closeOnce sync.Once
}

// Messages returns the bounded channel of validated provider messages.
func (stream *Stream[T]) Messages() <-chan T {
	if stream == nil || stream.messages == nil {
		closed := make(chan T)
		close(closed)
		return closed
	}
	return stream.messages
}

// Done closes when the stream reaches EOF, fails, or is closed.
func (stream *Stream[T]) Done() <-chan struct{} {
	if stream == nil || stream.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return stream.done
}

// Err returns the sanitized terminal error. Call it after Done or after the
// Messages channel closes.
func (stream *Stream[T]) Err() error {
	if stream == nil {
		return nil
	}
	stream.mu.RLock()
	defer stream.mu.RUnlock()
	return stream.err
}

// Close cancels the stream. It is idempotent.
func (stream *Stream[T]) Close() error {
	if stream == nil {
		return nil
	}
	stream.closeOnce.Do(stream.cancel)
	return nil
}

func (stream *Stream[T]) setErr(err error) {
	if err == nil {
		return
	}
	stream.mu.Lock()
	if stream.err == nil {
		stream.err = err
	}
	stream.mu.Unlock()
}

// resolveStreamBudgets projects the declared resilience policy onto the four
// session budgets. The handshake budget defaults to the declared attempt
// timeout: bounding connection opening through admission is exactly what the
// attempt timeout already meant for a stream, and the contract field that
// makes it declarable on its own (`resilience.stream.handshakeTimeoutMs`) is
// owned by the clientcontract slice. The declared operation duration bounds
// the whole session only when it is declared; absent, the session is unbounded
// in time and the handshake, idle and frame budgets still apply.
func resolveStreamBudgets(document, operation *clientcontract.ResiliencePolicy) StreamBudgets {
	budgets := StreamBudgets{
		Handshake:           defaultAttemptTimeout,
		Idle:                defaultStreamIdleTimeout,
		MaxFrameBytes:       defaultStreamFrameBytes,
		MaxBufferedMessages: defaultStreamBuffer,
	}
	apply := func(value *clientcontract.ResiliencePolicy) {
		if value == nil {
			return
		}
		if value.TimeoutMs != nil {
			budgets.Session = time.Duration(*value.TimeoutMs) * time.Millisecond
		}
		if value.AttemptTimeoutMs != nil {
			budgets.Handshake = time.Duration(*value.AttemptTimeoutMs) * time.Millisecond
		}
		if value.Stream == nil {
			return
		}
		if value.Stream.IdleTimeoutMs != nil {
			budgets.Idle = time.Duration(*value.Stream.IdleTimeoutMs) * time.Millisecond
		}
		if value.Stream.MaxBufferedMessages != nil {
			budgets.MaxBufferedMessages = *value.Stream.MaxBufferedMessages
		}
		if value.Stream.MaxFrameBytes != nil {
			budgets.MaxFrameBytes = *value.Stream.MaxFrameBytes
		}
	}
	apply(document)
	apply(operation)
	if budgets.Session > 0 && budgets.Handshake > budgets.Session {
		budgets.Handshake = budgets.Session
	}
	return budgets
}

// OpenServerStream opens the operation's declared SSE transport and returns a
// typed Stream[T].
//
// It is the SSE opener behind OpenOperationServerStream, which is what a
// generated method calls: the declared order, and any fallback it allows, are
// decided there. A caller with a reason to name the wire may call this directly.
//
// Authentication, client identity, deadlines, telemetry, circuit state, typed
// terminal errors and schema validation are applied by the framework. The
// lifecycle itself belongs to StreamSession: admission is the handshake, the
// four budgets are distinct from the declared operation duration, the breaker
// is written exactly once and never after admission, and the call measurement
// is emitted exactly once when the session closes.
//
// A transport that declares a continuation speaks the negotiated wire of
// clientcontract ADR 0013 on every connection, and, when the operation also
// declares reconnect, continues the same session over a new connection after
// an interruption (see openNegotiatedSSE).
func OpenServerStream[T any](ctx context.Context, client *Client, request *Request, operation Operation, errorMapper ...func(error) error) (*Stream[T], error) {
	var endpointErr error
	client, ctx, endpointErr = clientForEndpoint(ctx, client)
	if endpointErr != nil {
		return nil, endpointErr
	}
	if client == nil || client.service == nil {
		return nil, errors.New(CodeClientConfig, "generated server stream requires a service binding")
	}
	runtime := client.service
	if diagnostics := clientcontract.ValidateOperationForID(operation.ID, &operation.Contract, &runtime.descriptor.Contract); len(diagnostics) > 0 {
		return nil, errors.Newf(CodeClientConfig, "invalid generated operation contract: %s", diagnostics[0].String())
	}
	if operation.Contract.Stream != clientcontract.StreamServer || operation.Contract.Messages == nil || operation.Contract.Messages.Output == nil {
		return nil, errors.New(CodeClientConfig, "operation is not a typed server stream")
	}
	var transport *clientcontract.Transport
	for i := range operation.Contract.Transports {
		candidate := &operation.Contract.Transports[i]
		if candidate.Protocol == clientcontract.TransportSSE && candidate.Encoding == clientcontract.EncodingJSON {
			transport = candidate
			break
		}
	}
	if transport == nil {
		return nil, errors.New(CodeClientConfig, "server stream has no supported transport")
	}

	var documentPolicy *clientcontract.ResiliencePolicy
	if runtime.descriptor.Contract.Defaults != nil {
		documentPolicy = runtime.descriptor.Contract.Defaults.Resilience
	}
	budgets := resolveStreamBudgets(documentPolicy, operation.Contract.Resilience)
	policy := resolvePolicy(documentPolicy, operation.Contract.Resilience, operation.Contract.Idempotency.Kind)

	baseRequest, err := runtime.requestWithBindingHeaders(request, operation)
	if err != nil {
		return nil, err
	}
	baseRequest.Method = http.MethodGet
	if baseRequest.Path == "" {
		baseRequest.Path = transport.Path
	}
	baseRequest.OperationID = operation.ID
	if baseRequest.Headers == nil {
		baseRequest.Headers = make(http.Header)
	}
	baseRequest.Headers.Set("Accept", "text/event-stream")
	baseRequest.Headers.Set("X-Client-Id", runtime.binding.ClientID)
	if requestID := phttp.RequestIDFromContext(ctx); requestID != "" && baseRequest.Headers.Get("X-Request-ID") == "" {
		baseRequest.Headers.Set("X-Request-ID", requestID)
	}
	// A declared continuation asks for the negotiated wire on every opening,
	// and only a declared continuation does: every other stream keeps the
	// legacy framing, which an old provider and an old consumer both speak.
	continuation := transport.Continuation()
	if continuation != nil {
		baseRequest.Headers.Set(clientcontract.SSEWireHeader, clientcontract.SSEWireV1)
	}

	breaker := runtime.breaker(operation.ID, policy)
	telemetry := currentServiceTelemetry()
	opener := &sseOpener{
		client: client, runtime: runtime, operation: operation, continuation: continuation, telemetry: telemetry,
		path: baseRequest.Path,
		// The credential-free header set every resolution starts from.
		// Re-applying credentials to headers that already carry them is
		// refused by the credential rules, so the send-point re-check rebuilds
		// from here.
		pristine: baseRequest.Headers.Clone(),
	}
	session, streamCtx := newStreamSession(ctx, streamSessionConfig{
		ServiceID:   runtime.descriptor.Contract.Service.ID,
		OperationID: operation.ID,
		Protocol:    clientcontract.TransportSSE,
		Idempotency: operation.Contract.Idempotency.Kind,
		Budgets:     budgets,
		Breaker:     breaker,
		Telemetry:   telemetry,
		Credentials: opener.resolveCredentials,
		Invalidate: func(applied appliedCredentials) {
			runtime.invalidateServiceCredentials(applied.serviceCredentials)
		},
		MapError: func(err error) error { return mapStreamError(err, errorMapper) },
	})
	opener.session, opener.ctx = session, streamCtx
	// release detaches the session from the application registry. It is set
	// once the registry accepts the session and stays nil before that.
	var release func()
	// fail records the single terminal, applies the phase breaker rule, closes
	// the session — which emits the single call measurement — and detaches it
	// from the registry.
	fail := func(err error) error {
		surfaced := session.Fail(err)
		session.Close() //nolint:errcheck // Close always returns nil; the terminal error is the return value
		if release != nil {
			release()
		}
		return surfaced
	}

	if err := breaker.AllowRequest(); err != nil {
		return nil, fail(err)
	}

	// The application registry owns the session's lifetime: stopping the
	// application closes the streams the caller never closed. A registry that
	// is already closed refuses the session instead of opening a stream nothing
	// would ever stop.
	tracked, trackErr := runtime.registry.TrackStream(session)
	if trackErr != nil {
		return nil, fail(trackErr)
	}
	release = tracked

	query := sseRequestQuery(baseRequest)
	connection, err := opener.open(query, false)
	if err != nil {
		return nil, fail(err)
	}

	// The provider answered 2xx with the declared content type — and, on the
	// negotiated wire, its acknowledgment: that is the admission. It records
	// the single breaker success and starts the idle budget; nothing after this
	// point writes to the breaker, and no reopening admits the session again.
	session.Admit()

	if continuation != nil {
		reconnect := operation.Contract.Idempotency.Kind == clientcontract.IdempotencySafe &&
			streamReconnect(documentPolicy, operation.Contract.Resilience)
		return openNegotiatedSSE[T](opener, connection, query, reconnect, release), nil
	}

	response := connection.response
	messages := make(chan T, budgets.MaxBufferedMessages)
	done := make(chan struct{})
	stream := &Stream[T]{messages: messages, done: done, cancel: session.Cancel}
	go func() {
		defer close(done)
		defer close(messages)
		defer func() {
			_ = response.Body.Close() //nolint:errcheck // terminal stream error is reported separately
		}()
		defer release()
		if terminalErr := readSSEMessages(streamCtx, response.Body, messages, operation, runtime.descriptor.Schemas, session, connection.applied.secrets, runtime.binding.CarryRemoteMessage); terminalErr != nil {
			stream.setErr(session.Fail(terminalErr))
		} else {
			session.Complete()
		}
		session.Close() //nolint:errcheck // Close always returns nil; the terminal error reaches the caller through Stream.Err
	}()
	return stream, nil
}

// sseRequestQuery merges the query a caller set through either field of
// Request into the one query every opening of the session starts from, encoded
// exactly as openSSE encodes it.
func sseRequestQuery(request *Request) url.Values {
	query := cloneURLValues(request.QueryValues)
	if query == nil {
		query = make(url.Values, len(request.Query))
	}
	for key, value := range request.Query {
		query.Set(key, value)
	}
	return query
}

// sseOpener opens the connections of one SSE session. The first opening and
// every reopening run the same steps — an attempt, credentials resolved for
// that attempt, the handshake under its own budget, the admission checks — so
// a continuation is never a cheaper path into the provider than the stream it
// continues.
type sseOpener struct {
	client       *Client
	runtime      *serviceRuntime
	operation    Operation
	continuation *clientcontract.SSEContinuation
	telemetry    ServiceCallTelemetry
	session      *StreamSession
	ctx          context.Context
	path         string
	pristine     http.Header
	// headers is what the last credential resolution produced: the pristine
	// headers plus the credentials of the attempt about to be sent.
	headers http.Header
	// forwardedToken is a forwarded user credential the binding re-minted
	// after a reopening was refused with 401. It replaces the caller's for the
	// rest of the session.
	forwardedToken string
}

// sseConnection is one admitted SSE response and the credentials it carried.
type sseConnection struct {
	response *http.Response
	applied  appliedCredentials
}

// resolveCredentials is the session's credential hook: it applies the
// declared credentials to a fresh copy of the pristine headers.
func (opener *sseOpener) resolveCredentials(ctx context.Context) (appliedCredentials, error) {
	scratch := &Request{Headers: opener.pristine.Clone()}
	applied, err := opener.runtime.applyCredentials(ctx, scratch, opener.operation.Contract.Security)
	if err != nil {
		return appliedCredentials{}, err
	}
	opener.headers = scratch.Headers
	return applied, nil
}

// open runs one opening with query. A reopening the provider refuses with 401
// re-mints a forwarded user credential once, through the binding's Refresh,
// and opens again: the bounded refresh a unary call already has, and nothing
// more — no refresh on 403 and no second remint. The first opening never
// re-mints; its 401 ends the session as it always has.
func (opener *sseOpener) open(query url.Values, reopening bool) (*sseConnection, error) {
	connection, status, applied, err := opener.attempt(query)
	if err == nil || !reopening || status != http.StatusUnauthorized || applied.forwardedUserRefresh == nil ||
		opener.ctx.Err() != nil {
		return connection, err
	}
	fresh, refreshErr := applied.forwardedUserRefresh(opener.ctx)
	token := strings.TrimSpace(fresh)
	if refreshErr != nil || token == "" {
		return nil, err
	}
	opener.forwardedToken = token
	connection, _, _, err = opener.attempt(query)
	return connection, err
}

// attempt performs one opening: it begins an attempt, resolves credentials
// for it, sends the request under the handshake budget and checks what the
// provider answered. It returns the answer's status beside the error, so a
// reopening can tell a refused credential from every other failure.
func (opener *sseOpener) attempt(query url.Values) (*sseConnection, int, appliedCredentials, error) {
	session := opener.session
	attemptCtx, finishAttempt := session.beginAttempt()
	credentialCtx := attemptCtx
	if opener.forwardedToken != "" {
		credentialCtx = WithForwardedUserToken(attemptCtx, opener.forwardedToken)
	}
	applied, authErr := session.Credentials(credentialCtx)
	if authErr != nil {
		finishAttempt(ServiceAttemptResult{Code: serviceErrorCode(authErr), AuthDuration: session.AuthDuration()})
		return nil, 0, applied, authErr
	}
	request := &Request{Path: opener.path, QueryValues: query, Headers: opener.headers}
	if opener.telemetry != nil {
		opener.telemetry.InjectServiceContext(attemptCtx, request.Headers)
	}

	handshakeRemaining := remainingHandshake(session)
	var response *http.Response
	var handshake *sseHandshake
	var openErr error
	if handshakeRemaining <= 0 {
		openErr = context.DeadlineExceeded
	} else {
		session.Dispatch()
		response, handshake, openErr = opener.client.openSSE(opener.ctx, request, handshakeRemaining)
	}
	// finishHandshake reports the attempt exactly once and converts a handshake
	// budget expiry into the typed deadline error.
	finishHandshake := func(err error) error {
		if handshake != nil {
			handshake.stop()
			if err == nil && handshake.expired() {
				err = errors.New(CodeClientDeadline, "service stream handshake deadline exceeded")
			}
		}
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		finishAttempt(ServiceAttemptResult{StatusCode: status, Code: serviceErrorCode(err), AuthDuration: session.AuthDuration()})
		return err
	}
	if openErr != nil {
		return nil, 0, applied, finishHandshake(normalizedCallError(opener.ctx, openErr))
	}
	if response == nil {
		return nil, 0, applied, finishHandshake(errors.New(CodeClientResponse, "service returned no stream response"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer func() {
			_ = response.Body.Close() //nolint:errcheck // the returned error is authoritative
		}()
		body, readErr := io.ReadAll(io.LimitReader(response.Body, defaultMaxResponseSize+1))
		if readErr != nil || int64(len(body)) > defaultMaxResponseSize {
			var err error = errors.New(CodeClientResponse, "service stream response failed")
			if handshake != nil && handshake.expired() {
				err = errors.New(CodeClientDeadline, "service stream handshake deadline exceeded")
			}
			return nil, response.StatusCode, applied, finishHandshake(err)
		}
		remote := decodeRemoteError(opener.runtime.identity(opener.operation), &Response{StatusCode: response.StatusCode, Headers: response.Header, Body: body}, opener.operation.Contract.Errors, opener.runtime.descriptor.Schemas, applied.secrets, opener.runtime.binding.CarryRemoteMessage)
		// The stream lifecycle contract invalidates the resolved credential
		// exactly once on a rejected identity at admission, and never replays
		// the operation. 403 counts with 401: both mean the provider refused
		// the credential this session carried.
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			session.invalidateCredentials()
		}
		return nil, response.StatusCode, applied, finishHandshake(remote)
	}
	if media := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]); !strings.EqualFold(media, "text/event-stream") {
		_ = response.Body.Close() //nolint:errcheck // response validation error is authoritative
		return nil, response.StatusCode, applied, finishHandshake(errors.New(CodeClientResponse, "service stream content type is not declared"))
	}
	// A provider that did not acknowledge the negotiated wire predates the
	// declared continuation, or speaks another version of it. Its end of body
	// would mean nothing, so the exchange is refused before any message: no
	// legacy reading, no fallback, no retry — during a rolling deploy, a
	// reopening that lands on an older instance ends the same way.
	if opener.continuation != nil && !clientcontract.NegotiatesSSEWire(response.Header.Values(clientcontract.SSEWireHeader)) {
		_ = response.Body.Close() //nolint:errcheck // the contract error is authoritative
		return nil, response.StatusCode, applied, finishHandshake(errors.Newf(CodeClientResponse,
			"service stream provider did not acknowledge %s %s; it predates the declared continuation",
			clientcontract.SSEWireHeader, clientcontract.SSEWireV1))
	}
	if err := finishHandshake(nil); err != nil {
		_ = response.Body.Close() //nolint:errcheck // the typed handshake deadline is authoritative
		return nil, response.StatusCode, applied, err
	}
	return &sseConnection{response: response, applied: applied}, response.StatusCode, applied, nil
}

// remainingHandshake is the time left on the handshake budget. A session with
// no handshake deadline at all keeps the credential-acquisition default.
func remainingHandshake(session *StreamSession) time.Duration {
	deadline := session.handshakeDeadline()
	if deadline.IsZero() {
		return defaultCredentialTimeout
	}
	return time.Until(deadline)
}

func (client *Client) openSSE(ctx context.Context, request *Request, handshakeTimeout time.Duration) (*http.Response, *sseHandshake, error) {
	transport, ok := client.transport.(*HTTPTransport)
	if !ok {
		return nil, nil, errors.New(CodeClientConfig, "service binding does not use the framework HTTP transport")
	}
	fullURL := transport.baseURL + request.Path
	if len(request.Query) > 0 || len(request.QueryValues) > 0 {
		params := cloneURLValues(request.QueryValues)
		if params == nil {
			params = make(url.Values, len(request.Query))
		}
		for key, value := range request.Query {
			params.Set(key, value)
		}
		fullURL += "?" + params.Encode()
	}
	requestCtx, requestCancel := context.WithCancel(ctx)
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodGet, fullURL, nil)
	if err != nil {
		requestCancel()
		return nil, nil, err
	}
	httpRequest.Header = request.Headers.Clone()
	handshake := &sseHandshake{}
	if handshakeTimeout > 0 {
		handshake.done = make(chan struct{})
		handshake.timer = time.AfterFunc(handshakeTimeout, func() {
			handshake.timedOut.Store(true)
			requestCancel()
			close(handshake.done)
		})
	}
	response, err := transport.client.Do(httpRequest)
	if err != nil {
		handshake.stop()
		requestCancel()
		if handshake.expired() {
			return nil, nil, context.DeadlineExceeded
		}
		return nil, nil, err
	}
	response.Body = &cancelReadCloser{ReadCloser: response.Body, cancel: requestCancel}
	return response, handshake, nil
}

type sseHandshake struct {
	timer    *time.Timer
	timedOut atomic.Bool
	done     chan struct{}
}

func (handshake *sseHandshake) stop() {
	if handshake != nil && handshake.timer != nil {
		if !handshake.timer.Stop() {
			<-handshake.done
		}
	}
}

func (handshake *sseHandshake) expired() bool {
	return handshake != nil && handshake.timedOut.Load()
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (closer *cancelReadCloser) Close() error {
	err := closer.ReadCloser.Close()
	closer.once.Do(closer.cancel)
	return err
}

func readSSEMessages[T any](ctx context.Context, body io.Reader, messages chan<- T, operation Operation, schemas map[string]clientcontract.Schema, session *StreamSession, secrets []string, carryMessage bool) error {
	budgets := session.config.Budgets
	reader := bufio.NewReaderSize(body, int(min(budgets.MaxFrameBytes, 64<<10)))
	var data bytes.Buffer
	eventType := ""
	eventBytes := int64(0)
	idleExpired := session.idleExpired()
	for {
		line, err := readBoundedSSELine(reader, budgets.MaxFrameBytes-eventBytes, session.touchIdle)
		if stderrors.Is(err, errSSEFrameTooLarge) {
			return errors.New(CodeClientResponse, "service stream frame exceeds maximum size")
		}
		eventBytes += int64(len(line))
		if len(line) > 0 {
			trimmed := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			switch {
			case trimmed == "":
				if data.Len() > 0 {
					payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
					if eventType == "error" {
						return decodeSSEError(payload, operation, schemas, secrets, carryMessage)
					}
					projected, ok := projectResponseJSON(payload, operation.Contract.Messages.Output, schemas, nil, false)
					if !ok {
						return errors.New(CodeClientResponse, "service stream message does not match the generated contract")
					}
					var message T
					if decodeErr := json.Unmarshal(projected, &message); decodeErr != nil {
						return errors.New(CodeClientResponse, "service stream message cannot be decoded")
					}
					session.Activate()
					select {
					case messages <- message:
					case <-ctx.Done():
						return normalizedCallError(ctx, ctx.Err())
					case <-idleExpired:
						return errors.New(CodeClientDeadline, "service stream idle timeout")
					}
				}
				data.Reset()
				eventType = ""
				eventBytes = 0
			case strings.HasPrefix(trimmed, ":"):
			case strings.HasPrefix(trimmed, "event:"):
				eventType = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
				data.WriteByte('\n')
			}
		}
		if session.idleBudgetExpired() {
			return errors.New(CodeClientDeadline, "service stream idle timeout")
		}
		if err != nil {
			if stderrors.Is(err, io.EOF) {
				if data.Len() != 0 {
					return errors.New(CodeClientResponse, "service stream ended with an incomplete event")
				}
				return nil
			}
			if ctx.Err() != nil {
				return normalizedCallError(ctx, err)
			}
			return errors.New(CodeClientResponse, "service stream response failed")
		}
	}
}

var errSSEFrameTooLarge = stderrors.New("SSE frame exceeds maximum size")

func readBoundedSSELine(reader *bufio.Reader, remaining int64, touch func()) ([]byte, error) {
	if remaining <= 0 {
		return nil, errSSEFrameTooLarge
	}
	line := make([]byte, 0, min(int64(reader.Size()), remaining))
	for {
		fragment, err := reader.ReadSlice('\n')
		if int64(len(line))+int64(len(fragment)) > remaining {
			return nil, errSSEFrameTooLarge
		}
		line = append(line, fragment...)
		if len(fragment) > 0 {
			touch()
		}
		if stderrors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// decodeSSEError projects the SSE terminal frame — the first-party error
// envelope plus `status`, `{status, code, error, message, details?}`, written
// identically by both providers — onto the same typed RemoteError a unary
// non-2xx answer produces. The declared schema is validated against `details`
// alone (ADR 0006), and the free-text `message` is carried under the same
// consumer opt-in and the same redaction as decodeRemoteError; carryMessage is
// stated by the caller, never defaulted.
func decodeSSEError(body []byte, operation Operation, schemas map[string]clientcontract.Schema, secrets []string, carryMessage bool) error {
	var envelope struct {
		Code    string          `json:"code"`
		Status  int             `json:"status"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Status < 400 || envelope.Code == "" {
		return errors.New(CodeClientRemote, "remote service stream failed")
	}
	message := sanitizedErrorMessage(envelope.Message, secrets, carryMessage)
	selected := selectDeclaredError(envelope.Status, envelope.Code, operation.Contract.Errors)
	if selected == nil {
		return &RemoteError{
			StatusCode: envelope.Status, RemoteCode: string(CodeClientRemote), Message: message,
			framework: errors.New(CodeClientRemote, "remote service request failed"),
		}
	}
	remote := &RemoteError{
		StatusCode: envelope.Status, RemoteCode: selected.Code, GRPCCode: selected.GRPCCode,
		Message:   message,
		retryable: selected.Retryable != nil && *selected.Retryable,
	}
	if selected.Schema != nil && len(envelope.Details) > 0 {
		var valid bool
		remote.Payload, valid = sanitizedErrorPayload(envelope.Details, selected.Schema, schemas, secrets)
		if !valid {
			return errors.New(CodeClientResponse, "remote service stream error does not match the generated contract")
		}
	}
	remote.framework = errors.New(errors.Code(selected.Code), "remote service request failed").WithRetryable(remote.retryable)
	return remote
}

func mapStreamError(err error, mappers []func(error) error) error {
	if err == nil || len(mappers) == 0 || mappers[0] == nil {
		return err
	}
	return mappers[0](err)
}
