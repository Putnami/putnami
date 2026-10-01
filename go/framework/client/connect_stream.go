package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	connectwire "go.putnami.dev/protocol/clientcontract/connect"
)

// openServerStreamConnect opens a declared server stream over the Connect
// protocol: one enveloped request message, then enveloped provider messages,
// then exactly one EndStreamResponse.
//
// It owns no lifecycle rule of its own. Admission, the four budgets, the single
// breaker fact, the credential re-check at the send point and the single call
// measurement all belong to StreamSession — the same session the SSE and
// WebSocket transports drive.
func openServerStreamConnect[T any](
	ctx context.Context,
	client *Client,
	call *OperationCall,
	operation Operation,
	transport clientcontract.Transport,
	errorMapper ...func(error) error,
) (*Stream[T], error) {
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
	connect, err := newConnectStreamCall(client, transport, operation)
	if err != nil {
		return nil, err
	}
	envelope, err := connectRequestEnvelope(call)
	if err != nil {
		return nil, err
	}

	var documentPolicy *clientcontract.ResiliencePolicy
	if runtime.descriptor.Contract.Defaults != nil {
		documentPolicy = runtime.descriptor.Contract.Defaults.Resilience
	}
	// resolveWebSocketBudgets is transport-neutral despite its name: it is
	// resolveStreamBudgets plus the declared `resilience.stream.handshakeTimeoutMs`
	// the shared projection does not read yet. Both stream transports call it so
	// neither holds a second opinion about the handshake budget; it disappears
	// when the shared projection reads the field.
	budgets := resolveWebSocketBudgets(documentPolicy, operation.Contract.Resilience)
	policy := resolvePolicy(documentPolicy, operation.Contract.Resilience, operation.Contract.Idempotency.Kind)

	baseRequest, err := runtime.requestWithBindingHeaders(call.Request, operation)
	if err != nil {
		return nil, err
	}
	baseRequest.Method = http.MethodPost
	baseRequest.Path = transport.Path
	baseRequest.Query = nil
	baseRequest.QueryValues = nil
	baseRequest.OperationID = operation.ID
	if baseRequest.Headers == nil {
		baseRequest.Headers = make(http.Header)
	}
	baseRequest.Headers.Set("Content-Type", connect.contentType)
	baseRequest.Headers.Set(connectwire.ProtocolVersionHeader, connectwire.ProtocolVersion)
	baseRequest.Headers.Set(connectwire.AcceptEncoding, connectwire.EncodingGzip)
	baseRequest.Headers.Set("X-Client-Id", runtime.binding.ClientID)
	if requestID := phttp.RequestIDFromContext(ctx); requestID != "" && baseRequest.Headers.Get("X-Request-ID") == "" {
		baseRequest.Headers.Set("X-Request-ID", requestID)
	}
	pristineHeaders := baseRequest.Headers.Clone()

	breaker := runtime.breaker(operation.ID, policy)
	telemetry := currentServiceTelemetry()
	session, streamCtx := newStreamSession(ctx, streamSessionConfig{
		ServiceID:   runtime.descriptor.Contract.Service.ID,
		OperationID: operation.ID,
		Protocol:    clientcontract.TransportConnect,
		Idempotency: operation.Contract.Idempotency.Kind,
		Budgets:     budgets,
		Breaker:     breaker,
		Telemetry:   telemetry,
		Credentials: func(resolveCtx context.Context) (appliedCredentials, error) {
			scratch := &Request{Headers: pristineHeaders.Clone()}
			applied, credErr := runtime.applyCredentials(resolveCtx, scratch, operation.Contract.Security)
			if credErr != nil {
				return appliedCredentials{}, credErr
			}
			baseRequest.Headers = scratch.Headers
			return applied, nil
		},
		Invalidate: func(applied appliedCredentials) {
			runtime.invalidateServiceCredentials(applied.serviceCredentials)
		},
		MapError: func(mapped error) error { return mapStreamError(mapped, errorMapper) },
	})
	var release func()
	fail := func(failure error) error {
		surfaced := session.Fail(failure)
		session.Close() //nolint:errcheck // Close always returns nil; the terminal error is the return value
		if release != nil {
			release()
		}
		return surfaced
	}

	if allowErr := breaker.AllowRequest(); allowErr != nil {
		return nil, fail(allowErr)
	}
	tracked, trackErr := runtime.registry.TrackStream(session)
	if trackErr != nil {
		return nil, fail(trackErr)
	}
	release = tracked

	attemptCtx, finishAttempt := session.beginAttempt()
	applied, authErr := session.Credentials(attemptCtx)
	if authErr != nil {
		finishAttempt(ServiceAttemptResult{Code: serviceErrorCode(authErr), AuthDuration: session.AuthDuration()})
		return nil, fail(authErr)
	}
	if telemetry != nil {
		telemetry.InjectServiceContext(attemptCtx, baseRequest.Headers)
	}
	payload, encodeErr := connect.encodeRequestMessage(envelope)
	if encodeErr != nil {
		finishAttempt(ServiceAttemptResult{Code: serviceErrorCode(encodeErr), AuthDuration: session.AuthDuration()})
		return nil, fail(encodeErr)
	}

	handshakeRemaining := remainingHandshake(session)
	var response *http.Response
	var handshake *sseHandshake
	var openErr error
	if handshakeRemaining <= 0 {
		openErr = context.DeadlineExceeded
	} else {
		session.Dispatch()
		response, handshake, openErr = client.openConnectStream(streamCtx, baseRequest, payload, handshakeRemaining)
	}
	finishHandshake := func(failure error) error {
		if handshake != nil {
			handshake.stop()
			if failure == nil && handshake.expired() {
				failure = errors.New(CodeClientDeadline, "service stream handshake deadline exceeded")
			}
		}
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		finishAttempt(ServiceAttemptResult{StatusCode: status, Code: serviceErrorCode(failure), AuthDuration: session.AuthDuration()})
		return failure
	}
	if openErr != nil {
		return nil, fail(finishHandshake(normalizedCallError(streamCtx, openErr)))
	}
	if response == nil {
		return nil, fail(finishHandshake(errors.New(CodeClientResponse, "service returned no stream response")))
	}
	if response.StatusCode != http.StatusOK {
		defer func() {
			_ = response.Body.Close() //nolint:errcheck // the returned error is authoritative
		}()
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, defaultMaxResponseSize+1))
		if readErr != nil || int64(len(raw)) > defaultMaxResponseSize {
			failure := errors.New(CodeClientResponse, "service stream response failed")
			if handshake != nil && handshake.expired() {
				failure = errors.New(CodeClientDeadline, "service stream handshake deadline exceeded")
			}
			return nil, fail(finishHandshake(failure))
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			session.invalidateCredentials()
		}
		return nil, fail(finishHandshake(connect.remoteError(runtime, operation, response.StatusCode, raw, applied.secrets)))
	}
	if media := connectwire.MediaType(response.Header.Get("Content-Type")); media != connect.contentType {
		_ = response.Body.Close() //nolint:errcheck // response validation error is authoritative
		return nil, fail(finishHandshake(errors.New(CodeClientResponse, "service stream content type is not declared")))
	}
	compression := strings.TrimSpace(response.Header.Get(connectwire.ContentEncoding))
	if compression != "" && !strings.EqualFold(compression, connectwire.EncodingGzip) && !strings.EqualFold(compression, connectwire.EncodingIdentity) {
		_ = response.Body.Close() //nolint:errcheck // response validation error is authoritative
		return nil, fail(finishHandshake(errors.New(CodeClientResponse, "service stream declares a compression this client did not offer")))
	}
	compressionNegotiated := strings.EqualFold(compression, connectwire.EncodingGzip)
	if err := finishHandshake(nil); err != nil {
		_ = response.Body.Close() //nolint:errcheck // the typed handshake deadline is authoritative
		return nil, fail(err)
	}

	// The provider answered 200 with the declared streamed content type: that is
	// the Connect admission. It records the single breaker success and starts
	// the idle budget; nothing after this point writes to the breaker.
	session.Admit()

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
		if terminalErr := readConnectMessages(streamCtx, response.Body, messages, connect, runtime, operation, session, applied.secrets, compressionNegotiated); terminalErr != nil {
			stream.setErr(session.Fail(terminalErr))
		} else {
			session.Complete()
		}
		session.Close() //nolint:errcheck // Close always returns nil; the terminal error reaches the caller through Stream.Err
	}()
	return stream, nil
}

// connectStreamCall is the streamed counterpart of connectCall: which codec the
// provider serves and which protobuf message its stream carries.
type connectStreamCall struct {
	codec       *connectwire.ProtoCodec
	method      clientcontract.ProtobufMethod
	contentType string
}

func newConnectStreamCall(client *Client, transport clientcontract.Transport, operation Operation) (*connectStreamCall, error) {
	if transport.Protocol != clientcontract.TransportConnect {
		return nil, errors.Newf(CodeClientConfig, "operation %s: %s is not a connect transport", operation.ID, transport.Protocol)
	}
	if transport.Path != transport.ProtobufMethod {
		return nil, errors.Newf(CodeClientConfig,
			"operation %s declares connect path %q for protobuf method %q; a Connect path is the method identity",
			operation.ID, transport.Path, transport.ProtobufMethod)
	}
	call := &connectStreamCall{contentType: connectwire.StreamJSONContentType}
	if transport.Encoding != clientcontract.EncodingProto {
		return call, nil
	}
	descriptor := connectDescriptorFor(client)
	if descriptor == nil {
		return nil, errors.Newf(CodeClientConfig,
			"operation %s declares a connect stream with proto encoding but the contract publishes no protobuf descriptor", operation.ID)
	}
	codec, err := connectwire.NewProtoCodec(descriptor)
	if err != nil {
		return nil, errors.Newf(CodeClientConfig, "operation %s: %v", operation.ID, err)
	}
	method, declared := codec.Method(transport.ProtobufMethod)
	if !declared {
		return nil, errors.Newf(CodeClientConfig,
			"operation %s names protobuf method %s, which the published descriptor does not declare", operation.ID, transport.ProtobufMethod)
	}
	if !method.ServerStreaming {
		return nil, errors.Newf(CodeClientConfig,
			"operation %s declares a server stream but protobuf method %s is unary", operation.ID, transport.ProtobufMethod)
	}
	call.codec, call.method, call.contentType = codec, method, connectwire.StreamProtoType
	return call, nil
}

func (c *connectStreamCall) encodeRequestMessage(envelope []byte) ([]byte, error) {
	if c.codec == nil {
		return envelope, nil
	}
	encoded, err := c.codec.Encode(c.method.Input, envelope)
	if err != nil {
		return nil, errors.Newf(CodeClientRequest, "service request does not match the generated protobuf contract: %v", err)
	}
	return encoded, nil
}

func (c *connectStreamCall) decodeMessage(payload []byte) ([]byte, error) {
	if c.codec == nil {
		return payload, nil
	}
	decoded, err := c.codec.Decode(c.method.Output, payload)
	if err != nil {
		return nil, errors.Newf(CodeClientResponse, "service stream message does not match the generated protobuf contract: %v", err)
	}
	return decoded, nil
}

// remoteError projects a Connect error document onto the same typed RemoteError
// a REST error produces, so one declared error reaches the caller identically
// whichever transport carried it.
func (c *connectStreamCall) remoteError(runtime *serviceRuntime, operation Operation, status int, raw []byte, secrets []string) error {
	mapped, envelope, err := connectFirstPartyEnvelope(status, raw)
	if err != nil {
		return err
	}
	return decodeRemoteError(runtime.identity(operation), &Response{StatusCode: mapped, Body: envelope}, operation.Contract.Errors, runtime.descriptor.Schemas, secrets, runtime.binding.CarryRemoteMessage)
}

// openConnectStream posts the single enveloped request message and returns the
// live response. The handshake budget is armed exactly as it is for SSE.
func (client *Client) openConnectStream(ctx context.Context, request *Request, payload []byte, handshakeTimeout time.Duration) (*http.Response, *sseHandshake, error) {
	transport, ok := client.transport.(*HTTPTransport)
	if !ok {
		return nil, nil, errors.New(CodeClientConfig, "service binding does not use the framework HTTP transport")
	}
	body := connectwire.AppendEnvelope(nil, 0, payload)
	requestCtx, requestCancel := context.WithCancel(ctx)
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, transport.baseURL+request.Path, bytes.NewReader(body))
	if err != nil {
		requestCancel()
		return nil, nil, err
	}
	httpRequest.Header = request.Headers.Clone()
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			requestCancel()
			return nil, nil, context.DeadlineExceeded
		}
		httpRequest.Header.Set(connectwire.TimeoutHeader, strconv.FormatInt(int64(remaining/time.Millisecond)+1, 10))
	}
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

// readConnectMessages reads enveloped messages until the single
// EndStreamResponse. A stream that ends without one, or that carries bytes after
// one, is a protocol violation: the specification says the terminal envelope is
// last and appears exactly once, and treating a truncated stream as a clean end
// would report a partial result as complete.
func readConnectMessages[T any](
	ctx context.Context,
	body io.Reader,
	messages chan<- T,
	connect *connectStreamCall,
	runtime *serviceRuntime,
	operation Operation,
	session *StreamSession,
	secrets []string,
	compressionNegotiated bool,
) error {
	budgets := session.config.Budgets
	reader := bufio.NewReaderSize(body, int(min(budgets.MaxFrameBytes, 64<<10)))
	idleExpired := session.idleExpired()
	for {
		flags, payload, err := connectwire.ReadEnvelope(reader, budgets.MaxFrameBytes)
		if err != nil {
			if stderrors.Is(err, io.EOF) {
				return errors.New(CodeClientResponse, "service stream ended without an EndStreamResponse")
			}
			if stderrors.Is(err, io.ErrUnexpectedEOF) {
				return errors.New(CodeClientResponse, "service stream ended inside an envelope")
			}
			if ctx.Err() != nil {
				return normalizedCallError(ctx, err)
			}
			return errors.New(CodeClientResponse, "service stream response failed")
		}
		session.touchIdle()
		if flags&connectwire.FlagCompressed != 0 {
			if !compressionNegotiated {
				return errors.New(CodeClientResponse, "service stream compressed an envelope this client did not accept")
			}
			payload, err = connectwire.DecompressGzip(payload, budgets.MaxFrameBytes)
			if err != nil {
				return errors.New(CodeClientResponse, "service stream compression is not readable")
			}
		}
		if flags&connectwire.FlagEndStream != 0 {
			terminal := readConnectEndStream(payload, connect, runtime, operation, secrets)
			trailing, _ := reader.Peek(1) //nolint:errcheck // io.EOF is the expected outcome; the byte count is the fact
			if len(trailing) > 0 {
				return errors.New(CodeClientResponse, "service stream carried data after its EndStreamResponse")
			}
			return terminal
		}
		document, decodeErr := connect.decodeMessage(payload)
		if decodeErr != nil {
			return decodeErr
		}
		projected, ok := projectResponseJSON(document, operation.Contract.Messages.Output, runtime.descriptor.Schemas, nil, false)
		if !ok {
			return errors.New(CodeClientResponse, "service stream message does not match the generated contract")
		}
		var message T
		if unmarshalErr := json.Unmarshal(projected, &message); unmarshalErr != nil {
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
		if session.idleBudgetExpired() {
			return errors.New(CodeClientDeadline, "service stream idle timeout")
		}
	}
}

// readConnectEndStream reads the terminal envelope. An absent `error` member is
// the clean end of the stream; a present one is projected onto the same typed
// error a REST call produces.
func readConnectEndStream(payload []byte, connect *connectStreamCall, runtime *serviceRuntime, operation Operation, secrets []string) error {
	var end connectwire.EndStreamResponse
	if err := json.Unmarshal(payload, &end); err != nil {
		return errors.New(CodeClientResponse, "service stream ended with a malformed EndStreamResponse")
	}
	if end.Error == nil {
		// A successful RPC omits the property entirely; metadata alone is still
		// a success.
		if bytes.Contains(payload, []byte(`"error"`)) {
			return errors.New(CodeClientResponse, "service stream ended with an EndStreamResponse whose error member is not an error")
		}
		return nil
	}
	mapped, listed := connectwire.StatusByName(end.Error.Code)
	if !listed {
		// `{"error": {}}` and `{"error": {"code": null}}` are invalid, and only
		// the sixteen codes are valid. A terminal that names none of them says
		// the stream failed without saying how, which is still a failure.
		return errors.New(CodeClientResponse, "service stream ended with an EndStreamResponse that declares no valid Connect code")
	}
	document, err := json.Marshal(end.Error)
	if err != nil {
		return errors.New(CodeClientResponse, "service stream ended with an unusable error")
	}
	return connect.remoteError(runtime, operation, mapped.Status, document, secrets)
}
