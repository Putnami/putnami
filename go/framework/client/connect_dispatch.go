package client

import (
	"context"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// OperationCall is one generated call, described once and dispatched onto
// whichever transport the provider declared first.
//
// It exists because a REST request and a Connect request are two projections of
// the same declaration, not two calls: REST puts a path parameter in the URL,
// Connect puts it in the request message. A generated method therefore hands the
// runtime both the ready-made REST request and the structured sections it was
// built from, and the runtime — not the generated code — decides which wire
// carries them.
type OperationCall struct {
	// Request is the REST projection: the substituted path, the query values,
	// the headers and the JSON body.
	Request *Request
	// PathParams is the same path parameters before substitution, keyed by their
	// declared names. A Connect envelope carries them as a message section.
	PathParams map[string]string
}

// dispatchTransport picks the transport a generated call uses. The declared
// order is the dispatch order: the provider states its preference in the
// contract, and the client follows it. There is no fallback here — a transport
// that fails does not silently become another one; that is a separate,
// declared mechanism.
func dispatchTransport(operation Operation, supported func(clientcontract.Transport) bool) (clientcontract.Transport, error) {
	for _, transport := range operation.Contract.Transports {
		if supported(transport) {
			return transport, nil
		}
	}
	return clientcontract.Transport{}, errors.Newf(CodeClientConfig,
		"operation %s declares no transport this runtime can honor", operation.ID)
}

func supportedUnaryTransport(transport clientcontract.Transport) bool {
	switch transport.Protocol {
	case clientcontract.TransportRESTJSON:
		return transport.Encoding == clientcontract.EncodingJSON
	case clientcontract.TransportConnect:
		return transport.Encoding == clientcontract.EncodingJSON || transport.Encoding == clientcontract.EncodingProto
	default:
		return false
	}
}

// prepareUnaryDispatch resolves the transport and returns the client and
// request that carry it. For rest-json both are the ones the caller built; for
// Connect the request becomes the method envelope and the client writes Connect
// bytes over the same bound HTTP transport.
func prepareUnaryDispatch(client *Client, call *OperationCall, operation Operation) (*Client, *Request, error) {
	if err := validateBinaryStreamOperation(operation); err != nil {
		return nil, nil, err
	}
	if call == nil || call.Request == nil {
		return nil, nil, errors.New(CodeClientConfig, "generated call has no request")
	}
	transport, err := dispatchTransport(operation, supportedUnaryTransport)
	if err != nil {
		return nil, nil, err
	}
	if transport.Protocol == clientcontract.TransportRESTJSON {
		if client != nil && client.service != nil {
			if path, ok := client.service.binding.OperationPaths[operation.ID]; ok {
				request := cloneRequest(call.Request)
				request.Path = path
				return client, request, nil
			}
		}
		return client, call.Request, nil
	}
	if client == nil {
		return nil, nil, errors.New(CodeClientConfig, "a connect transport requires a client")
	}
	connect, err := newConnectCall(client, transport, operation)
	if err != nil {
		return nil, nil, err
	}
	envelope, err := connectRequestEnvelope(call)
	if err != nil {
		return nil, nil, err
	}
	connectRequest := cloneRequest(call.Request)
	connectRequest.Method = "POST"
	connectRequest.Path = transport.Path
	connectRequest.Query = nil
	connectRequest.QueryValues = nil
	connectRequest.Body = envelope
	if connectRequest.Headers != nil {
		connectRequest.Headers.Del("Content-Type")
	}
	connectClient, err := connectClient(client, connect, operation)
	if err != nil {
		return nil, nil, err
	}
	return connectClient, connectRequest, nil
}

// CallOperation executes one generated unary operation on the first transport
// the provider declared, and decodes its declared success body into T.
//
// A context carrying WithSuccessBody also receives that body, byte for byte,
// once T decoded from it.
func CallOperation[T any](ctx context.Context, client *Client, call *OperationCall, operation Operation) (output T, err error) {
	sink, ctx, err := takeSuccessBody(ctx, operation)
	if err != nil {
		return output, err
	}
	var response *Response
	defer func() { sink.deliver(response, err) }()
	dispatched, request, err := prepareUnaryDispatch(client, call, operation)
	if err != nil {
		return output, err
	}
	response, err = doOperationCall(ctx, client, call, dispatched, request, operation)
	if err != nil {
		return output, err
	}
	return decodeCallResponse[T](dispatched, response, operation)
}

// CallOperationVoid executes one generated unary operation that declares no
// success body, on the first transport the provider declared.
func CallOperationVoid(ctx context.Context, client *Client, call *OperationCall, operation Operation) error {
	dispatched, request, err := prepareUnaryDispatch(client, call, operation)
	if err != nil {
		return err
	}
	response, err := doOperationCall(ctx, client, call, dispatched, request, operation)
	if err != nil {
		return err
	}
	return checkVoidResponse(response, operation)
}

// CallOperationResponse executes one generated unary operation that declares
// more than one success status, on the first transport the provider declared,
// and returns the response it answered.
//
// The status is one the operation declares and the body matches that status's
// own declared representation — an undeclared status, a body where the status
// declares none, or a body its schema refuses fails here, exactly as it fails
// CallOperation. The generated method reads the status and decodes the body
// with DecodeResponse, which selects the schema of the status the provider
// answered. A Connect transport is refused: its response carries one status.
//
// A context carrying WithSuccessBody also receives the body of the status the
// provider answered, byte for byte, once its declared schema accepted it.
func CallOperationResponse(ctx context.Context, client *Client, call *OperationCall, operation Operation) (response *Response, err error) {
	sink, ctx, err := takeSuccessBody(ctx, operation)
	if err != nil {
		return nil, err
	}
	defer func() { sink.deliver(response, err) }()
	dispatched, request, err := prepareUnaryDispatch(client, call, operation)
	if err != nil {
		return nil, err
	}
	answered, err := doOperationCall(ctx, client, call, dispatched, request, operation)
	if err != nil {
		return nil, err
	}
	if _, err := successContent(answered, operation.Successes); err != nil {
		return nil, err
	}
	// The generated method decodes the body after this returns, so bytes handed
	// over here must already be ones its schema accepts. A bound client checked
	// them on every attempt; an unbound one checks them only when it decodes.
	if sink != nil {
		if err := validateSuccessfulResponse(answered, operation.Successes, operationSchemas(dispatched)); err != nil {
			return nil, err
		}
	}
	return answered, nil
}

// doOperationCall runs one dispatched generated call through the operation's
// declared response cache, and straight to DoOperation when it declares none
// or the context carries WithoutResponseCache. The cache wraps the whole call
// — deadline, credentials, breaker, retries — so a fresh answer costs none of
// them and a stale one masks their failure.
func doOperationCall(ctx context.Context, client *Client, call *OperationCall, dispatched *Client, request *Request, operation Operation) (*Response, error) {
	var err error
	client, callCtx, err := clientForEndpoint(ctx, client)
	if err != nil {
		return nil, err
	}
	dispatched, _, err = clientForEndpoint(ctx, dispatched)
	if err != nil {
		return nil, err
	}
	ctx = callCtx
	next := func(ctx context.Context, request *Request) (*Response, error) {
		return dispatched.doOperation(ctx, request, operation)
	}
	if responseCacheBypassed(ctx) {
		return next(ctx, request)
	}
	if interceptor := responseCacheInterceptor(client, call, operation); interceptor != nil {
		return interceptor(ctx, request, next)
	}
	return next(ctx, request)
}

// OpenOperationServerStream opens a generated server stream over the provider's
// declared transports, in the provider's own order.
//
// The declared order is the dispatch order: the first transport this runtime
// can drive is opened first, and the caller never branches on which one carries
// the stream. When — and only when — the provider answers that this transport
// is not served at this path, the next declared transport is opened. The
// generated method is the same whichever transport carries it, which is
// precisely why the fallback belongs here and not in a second generated method.
//
// All four wires share one lifecycle: StreamSession owns admission, the four
// budgets, the single breaker fact and the single call measurement — one per
// attempt, because each attempt is its own opening of the operation.
func OpenOperationServerStream[T any](ctx context.Context, client *Client, call *OperationCall, operation Operation, errorMapper ...func(error) error) (*Stream[T], error) {
	if call == nil || call.Request == nil {
		return nil, errors.New(CodeClientConfig, "generated stream call has no request")
	}
	candidates := serverStreamCandidates(operation)
	if len(candidates) == 0 {
		return nil, errors.Newf(CodeClientConfig,
			"operation %s declares no transport this runtime can honor", operation.ID)
	}
	// A fallback re-opens the operation. That is only sound where re-opening
	// cannot repeat an effect, so an operation the provider did not declare
	// replayable gets exactly one attempt on its first declared transport.
	if !streamFallbackAllowed(operation) {
		candidates = candidates[:1]
	}
	var lastErr error
	for index, transport := range candidates {
		stream, err := openDeclaredServerStream[T](ctx, client, call, operation, transport, errorMapper)
		if err == nil {
			return stream, nil
		}
		lastErr = err
		// Nothing was delivered: the provider refused before admission, and its
		// refusal says this wire is not there. Anything else is a fact about the
		// call, and asking a second wire would only ask it again.
		if !streamTransportUnavailable(err) || index == len(candidates)-1 {
			return nil, err
		}
	}
	return nil, lastErr
}

// openDeclaredServerStream opens one declared transport of a server stream.
func openDeclaredServerStream[T any](ctx context.Context, client *Client, call *OperationCall,
	operation Operation, transport clientcontract.Transport, errorMapper []func(error) error) (*Stream[T], error) {
	switch transport.Protocol {
	case clientcontract.TransportSSE:
		return OpenServerStream[T](ctx, client, call.Request, operation, errorMapper...)
	case clientcontract.TransportWebSocket:
		return OpenServerStreamWS[T](ctx, client, call.Request, operation, errorMapper...)
	default:
		return openServerStreamConnect[T](ctx, client, call, operation, transport, errorMapper...)
	}
}

func supportedServerStreamTransport(transport clientcontract.Transport) bool {
	switch transport.Protocol {
	case clientcontract.TransportSSE:
		return transport.Encoding == clientcontract.EncodingJSON
	case clientcontract.TransportWebSocket:
		return transport.Encoding == clientcontract.EncodingJSON
	case clientcontract.TransportConnect:
		return transport.Encoding == clientcontract.EncodingJSON || transport.Encoding == clientcontract.EncodingProto
	default:
		return false
	}
}
