package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	connectwire "go.putnami.dev/protocol/clientcontract/connect"
)

// connectMaxRequestBytes is the size past which a Connect request body is worth
// compressing. Below it the gzip header costs more than it saves.
const connectMaxRequestBytes = 1024

// connectCall carries everything one Connect attempt needs that the generic
// operation machinery does not know about: which codec the provider serves,
// which protobuf method the URL is, and which success status the operation
// declares.
type connectCall struct {
	transport     clientcontract.Transport
	codec         *connectwire.ProtoCodec
	method        clientcontract.ProtobufMethod
	successStatus int
	identity      callIdentity
}

// newConnectCall resolves one declared Connect transport against the embedded
// service descriptor. It refuses rather than degrades: a proto transport with no
// published descriptor, or a method identity the descriptor does not declare,
// has no encoding this runtime can honor, and falling back to JSON would send a
// payload the provider never agreed to read.
func newConnectCall(client *Client, transport clientcontract.Transport, operation Operation) (*connectCall, error) {
	if transport.Path != transport.ProtobufMethod {
		return nil, errors.Newf(CodeClientConfig,
			"operation %s declares connect path %q for protobuf method %q; a Connect path is the method identity",
			operation.ID, transport.Path, transport.ProtobufMethod)
	}
	if len(operation.Successes) != 1 {
		return nil, errors.Newf(CodeClientConfig,
			"operation %s declares %d success variants; a Connect response carries one status to project",
			operation.ID, len(operation.Successes))
	}
	call := &connectCall{
		transport:     transport,
		successStatus: operation.Successes[0].Status,
		identity:      descriptorIdentity(client, operation),
	}
	if client != nil && client.service != nil {
		call.identity = client.service.identity(operation)
	}
	if transport.Encoding != clientcontract.EncodingProto {
		return call, nil
	}
	descriptor := connectDescriptorFor(client)
	if descriptor == nil {
		return nil, errors.Newf(CodeClientConfig,
			"operation %s declares a connect transport with proto encoding but the contract publishes no protobuf descriptor",
			operation.ID)
	}
	codec, err := connectwire.NewProtoCodec(descriptor)
	if err != nil {
		return nil, errors.Newf(CodeClientConfig, "operation %s: %v", operation.ID, err)
	}
	method, declared := codec.Method(transport.ProtobufMethod)
	if !declared {
		return nil, errors.Newf(CodeClientConfig,
			"operation %s names protobuf method %s, which the published descriptor does not declare",
			operation.ID, transport.ProtobufMethod)
	}
	call.codec, call.method = codec, method
	return call, nil
}

func connectDescriptorFor(client *Client) *clientcontract.ProtobufDescriptor {
	if client == nil {
		return nil
	}
	if client.service != nil {
		return client.service.descriptor.Contract.Protobuf
	}
	if client.descriptor != nil {
		return client.descriptor.Contract.Protobuf
	}
	return nil
}

// connectTransportFor wraps the bound HTTP transport so one Connect attempt
// speaks the Connect protocol on the wire while the generic operation
// machinery — credentials, deadline, retry, circuit breaker, telemetry
// propagation, response and error validation — stays exactly the one the REST
// transport uses. Nothing about resilience is reimplemented here.
type connectRoundTripper struct {
	call    *connectCall
	inner   *HTTPTransport
	errors  []clientcontract.DeclaredError
	schemas map[string]clientcontract.Schema
}

// connectClient returns a client that shares this client's service runtime —
// and therefore its credential cache, circuit breakers and telemetry — but
// writes Connect bytes.
func connectClient(client *Client, call *connectCall, operation Operation) (*Client, error) {
	inner, ok := client.transport.(*HTTPTransport)
	if !ok {
		return nil, errors.New(CodeClientConfig, "a connect transport requires the framework HTTP transport")
	}
	var schemas map[string]clientcontract.Schema
	switch {
	case client.service != nil:
		schemas = client.service.descriptor.Schemas
	case client.descriptor != nil:
		schemas = client.descriptor.Schemas
	}
	return &Client{
		transport:  &connectRoundTripper{call: call, inner: inner, errors: operation.Contract.Errors, schemas: schemas},
		config:     client.config,
		service:    client.service,
		descriptor: client.descriptor,
	}, nil
}

// Do executes one Connect unary attempt and returns the first-party response
// shape: the declared success status with the schema's JSON body, or the
// first-party error envelope rebuilt from the Connect error document.
func (t *connectRoundTripper) Do(ctx context.Context, request *Request) (_ *Response, retErr error) {
	body, err := t.encodeRequest(request)
	if err != nil {
		return nil, err
	}
	url := t.inner.baseURL + t.call.transport.Path
	// safeURL is the target with no query values at all: a Connect URL is the
	// method identity, so there is nothing else in it to redact.
	safeURL := t.call.transport.Path
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body.payload))
	if err != nil {
		return nil, errors.Wrap(err, CodeClientRequest, errors.String("url", safeURL))
	}
	httpRequest.Header = request.Headers.Clone()
	if httpRequest.Header == nil {
		httpRequest.Header = make(http.Header)
	}
	httpRequest.Header.Set("Content-Type", body.contentType)
	httpRequest.Header.Set(connectwire.ProtocolVersionHeader, connectwire.ProtocolVersion)
	httpRequest.Header.Set("Accept-Encoding", connectwire.EncodingGzip)
	if body.compressed {
		httpRequest.Header.Set("Content-Encoding", connectwire.EncodingGzip)
	} else {
		httpRequest.Header.Del("Content-Encoding")
	}
	// The caller's deadline is the provider's deadline. Connect states it in a
	// header so the provider can stop work the caller has already given up on,
	// instead of finishing a computation nobody will read.
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errors.New(CodeClientDeadline, "service request deadline exceeded")
		}
		httpRequest.Header.Set(connectwire.TimeoutHeader, strconv.FormatInt(int64(remaining/time.Millisecond)+1, 10))
	}

	response, err := t.inner.client.Do(httpRequest)
	if err != nil {
		return nil, errors.Wrap(err, CodeClientRequest, errors.String("url", safeURL))
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && retErr == nil {
			retErr = errors.Wrap(closeErr, CodeClientResponse, errors.String("url", safeURL))
		}
	}()
	maxBytes := request.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = t.inner.maxResponseSize
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, errors.Wrap(err, CodeClientResponse, errors.String("url", safeURL))
	}
	if int64(len(raw)) > maxBytes {
		return nil, errors.New(CodeClientResponse, "response body exceeds maximum size", errors.String("url", safeURL))
	}
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("Content-Encoding")), connectwire.EncodingGzip) {
		raw, err = connectwire.DecompressGzip(raw, maxBytes)
		if err != nil {
			return nil, errors.New(CodeClientResponse, "service response compression is not readable", errors.String("url", safeURL))
		}
	}
	return t.decodeResponse(response, raw)
}

type connectRequestBody struct {
	payload     []byte
	contentType string
	compressed  bool
}

func (t *connectRoundTripper) encodeRequest(request *Request) (connectRequestBody, error) {
	payload := request.Body
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	result := connectRequestBody{payload: payload, contentType: connectwire.UnaryJSONContentType}
	if t.call.codec != nil {
		encoded, err := t.call.codec.Encode(t.call.method.Input, payload)
		if err != nil {
			return connectRequestBody{}, errors.Newf(CodeClientRequest, "service request does not match the generated protobuf contract: %v", err)
		}
		result.payload, result.contentType = encoded, connectwire.UnaryProtoContentType
	}
	if len(result.payload) >= connectMaxRequestBytes {
		if compressed, err := connectwire.CompressGzip(result.payload); err == nil && len(compressed) < len(result.payload) {
			result.payload, result.compressed = compressed, true
		}
	}
	return result, nil
}

// decodeResponse projects the Connect response onto the first-party shape the
// generic operation machinery validates. A success becomes the declared status
// with the schema's JSON; a failure becomes the first-party error envelope, so
// one declared error is decoded identically whichever transport carried it.
func (t *connectRoundTripper) decodeResponse(response *http.Response, raw []byte) (*Response, error) {
	if response.StatusCode == http.StatusOK {
		body := raw
		if t.call.codec != nil {
			decoded, err := t.call.codec.Decode(t.call.method.Output, raw)
			if err != nil {
				return nil, errors.Newf(CodeClientResponse, "service response does not match the generated protobuf contract: %v", err)
			}
			body = decoded
		}
		// A Connect reply for an operation that declares no body is the empty
		// message, and the empty message is `{}`. Handing that to a void
		// operation would look like an undeclared response body. Anything else
		// is passed through, so a provider that answered with content where the
		// contract declares none is still refused rather than quietly ignored.
		if !t.expectsBody() && connectEmptyMessage(body) {
			body = nil
		}
		return &Response{StatusCode: t.call.successStatus, Headers: connectResponseHeaders(response.Header), Body: body}, nil
	}
	status, envelope, err := connectFirstPartyEnvelope(response.StatusCode, raw)
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: status, Headers: connectResponseHeaders(response.Header), Body: envelope}, nil
}

// connectResponseHeaders restates what the projected body actually is. The
// Connect codec is a wire detail: what leaves this transport is the JSON the
// published schema declares, and the generic response validation matches the
// declared media type against this header. Leaving "application/proto" here
// would make a validated JSON body look like a representation the operation
// never declared. The transport's own framing headers go with it.
func connectResponseHeaders(header http.Header) http.Header {
	projected := header.Clone()
	if projected == nil {
		projected = make(http.Header)
	}
	projected.Set("Content-Type", "application/json")
	projected.Del("Content-Encoding")
	projected.Del("Content-Length")
	return projected
}

func (t *connectRoundTripper) expectsBody() bool {
	return t.call.successStatus != http.StatusNoContent
}

// connectEmptyMessage reports whether a decoded reply is the empty message.
func connectEmptyMessage(body []byte) bool {
	trimmed := string(connectwire.TrimSpace(body))
	return trimmed == "" || trimmed == "{}"
}

// connectFirstPartyEnvelope rebuilds `{code, error, message, details}` from the
// Connect error document.
//
// The Connect `code` is one of the specification's sixteen categories. A
// first-party provider states the finer facts — the stable framework code, the
// exact HTTP status and the declared details body — in a
// `putnami.client.v1.FrameworkError` detail, the same message the TypeScript
// provider publishes. A third-party Connect service sends none, so the code
// falls back to the Connect code name and the status to the specification's
// canonical mapping, and no declared error is selected on a code nobody
// declared.
func connectFirstPartyEnvelope(httpStatus int, raw []byte) (int, []byte, error) {
	var document connectwire.ErrorEnvelope
	status := httpStatus
	code := connectwire.InferredCode(httpStatus)
	if err := json.Unmarshal(raw, &document); err == nil {
		if mapped, listed := connectwire.StatusByName(document.Code); listed {
			// Only the sixteen codes are valid; there are no user-defined ones.
			// A body naming anything else is not a Connect error document, so
			// the code is inferred from the status instead of invented from it.
			status, code = mapped.Status, document.Code
		} else {
			document = connectwire.ErrorEnvelope{}
		}
	} else {
		document = connectwire.ErrorEnvelope{}
	}
	envelope := map[string]json.RawMessage{}
	for _, detail := range document.Details {
		if detail.Type != connectwire.FrameworkErrorType {
			continue
		}
		decoded, err := connectwire.DetailBase64.DecodeString(strings.TrimRight(detail.Value, "="))
		if err != nil {
			return 0, nil, errors.New(CodeClientResponse, "service returned a Connect error detail that is not base64")
		}
		firstParty, decodeErr := connectwire.DecodeFrameworkError(decoded)
		if decodeErr != nil {
			return 0, nil, errors.New(CodeClientResponse, "service returned a malformed first-party Connect error detail")
		}
		if firstParty.Code != "" {
			code = firstParty.Code
		}
		if firstParty.Status >= 100 && firstParty.Status < 600 {
			status = firstParty.Status
		}
		if firstParty.DetailsJSON != "" {
			if !json.Valid([]byte(firstParty.DetailsJSON)) {
				return 0, nil, errors.New(CodeClientResponse, "service returned a first-party error detail that is not JSON")
			}
			envelope["details"] = json.RawMessage(firstParty.DetailsJSON)
		}
	}
	encodedCode, err := json.Marshal(code)
	if err != nil {
		return 0, nil, errors.New(CodeClientResponse, "service returned an unusable error code")
	}
	envelope["code"] = encodedCode
	encodedError, err := json.Marshal(http.StatusText(status))
	if err != nil {
		return 0, nil, errors.New(CodeClientResponse, "service returned an unusable error status")
	}
	envelope["error"] = encodedError
	encodedMessage, err := json.Marshal(document.Message)
	if err != nil {
		return 0, nil, errors.New(CodeClientResponse, "service returned an unusable error message")
	}
	envelope["message"] = encodedMessage
	body, err := json.Marshal(envelope)
	if err != nil {
		return 0, nil, errors.New(CodeClientResponse, "service returned an unusable error document")
	}
	return status, body, nil
}

// connectRequestEnvelope builds the `{params, query, body}` document a bridged
// Connect method carries. The three sections stay namespaced so a path param
// and a body field of the same name never fight for one key.
func connectRequestEnvelope(call *OperationCall) ([]byte, error) {
	sections := map[string]json.RawMessage{}
	if len(call.PathParams) > 0 {
		encoded, err := json.Marshal(call.PathParams)
		if err != nil {
			return nil, errors.New(CodeClientRequest, "service request path parameters are not encodable")
		}
		sections["params"] = encoded
	}
	query := map[string]string{}
	for key, value := range call.Request.Query {
		query[key] = value
	}
	for key, values := range call.Request.QueryValues {
		if len(values) > 1 {
			return nil, errors.Newf(CodeClientRequest,
				"query parameter %q carries %d values; a Connect request envelope has one value per parameter", key, len(values))
		}
		if len(values) == 1 {
			query[key] = values[0]
		}
	}
	if len(query) > 0 {
		encoded, err := json.Marshal(query)
		if err != nil {
			return nil, errors.New(CodeClientRequest, "service request query parameters are not encodable")
		}
		sections["query"] = encoded
	}
	if len(call.Request.Body) > 0 {
		trimmed := connectwire.TrimSpace(json.RawMessage(call.Request.Body))
		if len(trimmed) > 0 && trimmed[0] != '{' {
			return nil, errors.New(CodeClientRequest,
				"a Connect request envelope carries the request body as an object; this operation declares a non-object body")
		}
		if len(trimmed) > 0 {
			sections["body"] = trimmed
		}
	}
	document, err := json.Marshal(sections)
	if err != nil {
		return nil, errors.New(CodeClientRequest, "service request is not encodable as a Connect envelope")
	}
	return document, nil
}
