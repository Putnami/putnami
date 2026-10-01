package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.putnami.dev/api"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	connectwire "go.putnami.dev/protocol/clientcontract/connect"
)

// defaultConnectMessageBytes bounds one decoded Connect message when the bridge
// is configured with no explicit budget. It is a declared bound, not a
// heuristic: a Connect payload is decompressed and decoded before any handler
// sees it, so the ceiling has to exist before the first byte is read.
const defaultConnectMessageBytes int64 = 4 << 20

// defaultConnectWriteTimeout bounds one write of a Connect server stream when
// the bridge is configured with no explicit bound. It is the same 30 seconds
// the HTTP server applies to an SSE write.
const defaultConnectWriteTimeout = 30 * time.Second

// connectEncoding is the payload encoding one Connect request uses.
type connectEncoding int

const (
	connectEncodingJSONUnary connectEncoding = iota
	connectEncodingProtoUnary
	connectEncodingJSONStream
	connectEncodingProtoStream
)

func (e connectEncoding) streamed() bool {
	return e == connectEncodingJSONStream || e == connectEncodingProtoStream
}

func (e connectEncoding) binary() bool {
	return e == connectEncodingProtoUnary || e == connectEncodingProtoStream
}

func (e connectEncoding) contentType() string {
	switch e {
	case connectEncodingProtoUnary:
		return connectwire.UnaryProtoContentType
	case connectEncodingJSONStream:
		return connectwire.StreamJSONContentType
	case connectEncodingProtoStream:
		return connectwire.StreamProtoType
	default:
		return connectwire.UnaryJSONContentType
	}
}

// connectEncodingFor classifies a request Content-Type. An absent header keeps
// the JSON unary shape the bridge has always served; an explicit type this
// bridge does not speak is refused rather than guessed at.
func connectEncodingFor(header string) (connectEncoding, bool) {
	switch connectwire.MediaType(header) {
	case "", connectwire.UnaryJSONContentType:
		return connectEncodingJSONUnary, true
	case connectwire.UnaryProtoContentType:
		return connectEncodingProtoUnary, true
	case connectwire.StreamJSONContentType:
		return connectEncodingJSONStream, true
	case connectwire.StreamProtoType:
		return connectEncodingProtoStream, true
	default:
		return 0, false
	}
}

// connectRoute is everything the Connect handler needs about one bridged route.
type connectRoute struct {
	definition api.EndpointDefinition
	path       string
	identity   string
	unary      phttp.Handler
	stream     phttp.StreamHandler
	serverSide bool
}

// connectHandler serves one bridged route over the Connect protocol. It is the
// single entry point for the four shapes the bridge speaks — JSON unary, proto
// unary, and both streamed codecs — because they share admission, the deadline
// header, the compression budget and the error envelope, and only differ in how
// a message is turned into bytes.
func (b *ApiBridge) connectHandler(route connectRoute) phttp.Handler {
	return func(ctx *phttp.Context) *phttp.Response {
		encoding, known := connectEncodingFor(ctx.Request.Header.Get("Content-Type"))
		if !known {
			return connectErrorResponse(http.StatusUnsupportedMediaType, connectwire.ErrorEnvelope{
				Code:    "unimplemented",
				Message: fmt.Sprintf("this Connect endpoint does not serve %q", connectwire.MediaType(ctx.Request.Header.Get("Content-Type"))),
			})
		}
		if encoding.binary() && b.codec == nil {
			return connectErrorResponse(http.StatusUnsupportedMediaType, connectwire.ErrorEnvelope{
				Code:    "unimplemented",
				Message: "this provider publishes no protobuf descriptor, so it serves no proto encoding",
			})
		}
		if encoding.streamed() != route.serverSide {
			return connectErrorResponse(http.StatusUnsupportedMediaType, connectwire.ErrorEnvelope{
				Code:    "unimplemented",
				Message: "the requested codec does not match the declared cardinality of this method",
			})
		}
		restore, err := b.applyConnectDeadline(ctx)
		if err != nil {
			return connectErrorResponse(http.StatusBadRequest, connectwire.ErrorEnvelope{Code: "invalid_argument", Message: err.Error()})
		}
		defer restore()

		payload, err := b.readConnectRequest(ctx, encoding)
		if err != nil {
			return connectErrorResponse(http.StatusBadRequest, connectwire.ErrorEnvelope{Code: "invalid_argument", Message: err.Error()})
		}
		envelope, err := b.decodeConnectRequest(route, encoding, payload)
		if err != nil {
			return connectErrorResponse(http.StatusBadRequest, connectwire.ErrorEnvelope{Code: "invalid_argument", Message: err.Error()})
		}
		if route.serverSide {
			return b.serveConnectServerStream(ctx, route, encoding, envelope)
		}
		return b.serveConnectUnary(ctx, route, encoding, envelope)
	}
}

// applyConnectDeadline honors Connect-Timeout-Ms. The caller's bound is the
// authority; a malformed header is refused because ignoring it would run a call
// the caller explicitly bounded.
func (b *ApiBridge) applyConnectDeadline(ctx *phttp.Context) (func(), error) {
	milliseconds, present, err := connectwire.ParseTimeout(ctx.Request.Header.Get(connectwire.TimeoutHeader))
	if err != nil {
		return func() {}, err
	}
	if !present {
		return func() {}, nil
	}
	parent := ctx.Context()
	bounded, cancel := context.WithTimeout(parent, time.Duration(milliseconds)*time.Millisecond)
	ctx.SetContext(bounded)
	ctx.Request = ctx.Request.WithContext(bounded)
	return func() {
		cancel()
		ctx.SetContext(parent)
	}, nil
}

// readConnectRequest reads and decompresses one request payload, bounded before
// allocation in both the compressed and the decompressed direction.
func (b *ApiBridge) readConnectRequest(ctx *phttp.Context, encoding connectEncoding) ([]byte, error) {
	budget := b.messageBytes()
	if ctx.Request.Body == nil {
		return nil, nil
	}
	if encoding.streamed() {
		return b.readConnectStreamRequest(ctx, budget)
	}
	raw, err := readBoundedBody(ctx.Request.Body, budget)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(ctx.Request.Header.Get("Content-Encoding")), connectwire.EncodingGzip) {
		return connectwire.DecompressGzip(raw, budget)
	}
	return raw, nil
}

// readConnectStreamRequest reads the single enveloped request message a server
// stream carries. A second message, or an end-stream flag from the caller, is a
// protocol violation: only the server closes a Connect stream.
func (b *ApiBridge) readConnectStreamRequest(ctx *phttp.Context, budget int64) ([]byte, error) {
	flags, payload, err := connectwire.ReadEnvelope(ctx.Request.Body, budget)
	if err != nil {
		return nil, fmt.Errorf("connect: the request carries no complete enveloped message (%w)", err)
	}
	if flags&connectwire.FlagEndStream != 0 {
		return nil, fmt.Errorf("connect: a caller may not send an EndStreamResponse; only the server ends a stream")
	}
	if flags&connectwire.FlagCompressed != 0 {
		if !strings.EqualFold(strings.TrimSpace(ctx.Request.Header.Get(connectwire.ContentEncoding)), connectwire.EncodingGzip) {
			return nil, fmt.Errorf("connect: an envelope is marked compressed but %s does not declare gzip", connectwire.ContentEncoding)
		}
		payload, err = connectwire.DecompressGzip(payload, budget)
		if err != nil {
			return nil, err
		}
	}
	trailing := make([]byte, 1)
	if _, err := io.ReadFull(ctx.Request.Body, trailing); err == nil {
		return nil, fmt.Errorf("connect: a server stream request carries more than one message")
	}
	return payload, nil
}

// decodeConnectRequest turns the request payload into the JSON envelope the
// bridged endpoint pipeline reads. The proto branch is a pure re-encoding: the
// descriptor's request message is exactly the {params, query, body} envelope,
// so one declaration drives both codecs.
func (b *ApiBridge) decodeConnectRequest(route connectRoute, encoding connectEncoding, payload []byte) ([]byte, error) {
	if !encoding.binary() {
		return payload, nil
	}
	method, declared := b.codec.Method(route.identity)
	if !declared {
		return nil, fmt.Errorf("connect: the published descriptor declares no method %s", route.identity)
	}
	return b.codec.Decode(method.Input, payload)
}

// serveConnectUnary runs the bridged endpoint and answers in the Connect unary
// shape: HTTP 200 with the reply, or the Connect error document.
func (b *ApiBridge) serveConnectUnary(ctx *phttp.Context, route connectRoute, encoding connectEncoding, envelope []byte) *phttp.Response {
	if len(envelope) == 0 {
		envelope = []byte("{}")
	}
	ctx.Request.Body = io.NopCloser(bytes.NewReader(envelope))
	ctx.Request.ContentLength = int64(len(envelope))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Del("Content-Encoding")
	response := route.unary(ctx)
	if response == nil {
		return connectErrorResponse(http.StatusInternalServerError, connectwire.ErrorEnvelope{Code: "internal", Message: "the bridged endpoint produced no response"})
	}
	body, err := response.BodyBytes()
	if err != nil {
		return connectErrorResponse(http.StatusInternalServerError, connectwire.ErrorEnvelope{Code: "internal", Message: "the bridged endpoint reply is not encodable"})
	}
	if response.Status < 200 || response.Status >= 300 {
		status, document := b.connectErrorDocument(response.Status, body)
		return connectErrorResponse(status, document)
	}
	// The Connect specification says a successful unary response is HTTP 200.
	// The endpoint's declared status is not lost: it is what the operation
	// contract declares, and a first-party client restores it from there.
	if !encoding.binary() {
		return connectRawResponse(http.StatusOK, connectwire.UnaryJSONContentType, body, ctx, false)
	}
	method, declared := b.codec.Method(route.identity)
	if !declared {
		return connectErrorResponse(http.StatusInternalServerError, connectwire.ErrorEnvelope{Code: "internal", Message: "the published descriptor declares no method for this URL"})
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	encoded, err := b.codec.Encode(method.Output, body)
	if err != nil {
		return connectErrorResponse(http.StatusInternalServerError, connectwire.ErrorEnvelope{Code: "internal", Message: err.Error()})
	}
	return connectRawResponse(http.StatusOK, connectwire.UnaryProtoContentType, encoded, ctx, true)
}

// serveConnectServerStream writes the enveloped message stream and its single
// terminating EndStreamResponse.
//
// A rejection raised before the response headers keeps its real HTTP status.
// The Connect specification puts every streamed error in the end-of-stream
// message, but that would make "the provider refused the call" indistinguishable
// from "the provider accepted it and then failed" — and the client's stream
// lifecycle (admission, retry, circuit) is built on that distinction. Once the
// headers are written, every error travels in EndStreamResponse as the
// specification requires.
func (b *ApiBridge) serveConnectServerStream(ctx *phttp.Context, route connectRoute, encoding connectEncoding, envelope []byte) *phttp.Response {
	if route.stream.Handle == nil {
		return connectErrorResponse(http.StatusInternalServerError, connectwire.ErrorEnvelope{Code: "internal", Message: "the bridged stream endpoint has no handler"})
	}
	if rejection := applyBridgeEnvelope(ctx, route.definition, route.path, envelope); rejection != nil {
		body, err := rejection.BodyBytes()
		if err != nil {
			return connectErrorResponse(http.StatusInternalServerError, connectwire.ErrorEnvelope{Code: "internal", Message: "the rejection is not encodable"})
		}
		status, document := b.connectErrorDocument(rejection.Status, body)
		return connectErrorResponse(status, document)
	}
	if route.stream.Before != nil {
		if rejection := route.stream.Before(ctx); rejection != nil {
			body, err := rejection.BodyBytes()
			if err != nil {
				return connectErrorResponse(http.StatusInternalServerError, connectwire.ErrorEnvelope{Code: "internal", Message: "the rejection is not encodable"})
			}
			status, document := b.connectErrorDocument(rejection.Status, body)
			return connectErrorResponse(status, document)
		}
	}
	flusher, ok := ctx.Writer.(http.Flusher)
	if !ok {
		return connectErrorResponse(http.StatusInternalServerError, connectwire.ErrorEnvelope{Code: "internal", Message: "this response writer cannot stream"})
	}
	// The server-wide write deadline is cleared for this stream's lifetime and a
	// bounded per-write deadline is re-armed before each flush.
	controller := http.NewResponseController(ctx.Writer)
	_ = controller.SetWriteDeadline(time.Time{}) //nolint:errcheck // a writer without deadlines simply has none to clear
	armWrite := b.streamWriteArmer(controller)
	armWrite()

	compress := acceptsConnectGzip(ctx.Request.Header.Get(connectwire.AcceptEncoding))
	header := ctx.Writer.Header()
	header.Set("Content-Type", encoding.contentType())
	if compress {
		header.Set(connectwire.ContentEncoding, connectwire.EncodingGzip)
	}
	ctx.Writer.WriteHeader(http.StatusOK)
	flusher.Flush()

	write := func(flags byte, payload []byte) error {
		armWrite()
		return b.writeConnectEnvelope(ctx.Writer, flusher, compress, flags, payload)
	}
	var method clientcontract.ProtobufMethod
	if encoding.binary() {
		declared := false
		if method, declared = b.codec.Method(route.identity); !declared {
			b.writeEndStream(write, &connectwire.ErrorEnvelope{Code: "internal", Message: "the published descriptor declares no method for this URL"})
			return nil
		}
	}
	streamContext := phttp.NewStreamContext(ctx, func(value any) error {
		document, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("connect: a stream message is not encodable")
		}
		if encoding.binary() {
			document, err = b.codec.Encode(method.Output, document)
			if err != nil {
				return err
			}
		}
		return write(0, document)
	}, nil)

	handlerErr := route.stream.Handle(streamContext)
	end := (*connectwire.ErrorEnvelope)(nil)
	if handlerErr != nil && !isContextCanceled(handlerErr) {
		status, body := perrors.HTTPErrorResponse(handlerErr)
		document, err := json.Marshal(body)
		if err != nil {
			document = []byte(`{}`)
		}
		_, envelopeDocument := b.connectErrorDocument(status, document)
		end = &envelopeDocument
	}
	b.writeEndStream(write, end)
	return nil
}

// streamWriteArmer re-arms the per-write deadline. A negative configured bound
// disables it explicitly; zero takes the default.
func (b *ApiBridge) streamWriteArmer(controller *http.ResponseController) func() {
	bound := b.config.StreamWriteTimeout
	if bound == 0 {
		bound = defaultConnectWriteTimeout
	}
	if bound < 0 {
		return func() {}
	}
	return func() {
		_ = controller.SetWriteDeadline(time.Now().Add(bound)) //nolint:errcheck // a writer without deadlines simply has none to arm
	}
}

func (b *ApiBridge) writeEndStream(write func(byte, []byte) error, failure *connectwire.ErrorEnvelope) {
	document, err := json.Marshal(connectwire.EndStreamResponse{Error: failure})
	if err != nil {
		document = []byte(`{}`)
	}
	_ = write(connectwire.FlagEndStream, document) //nolint:errcheck // the socket is already gone; there is no second channel to report on
}

func (b *ApiBridge) writeConnectEnvelope(writer http.ResponseWriter, flusher http.Flusher, compress bool, flags byte, payload []byte) error {
	if compress {
		compressed, err := connectwire.CompressGzip(payload)
		if err != nil {
			return err
		}
		if len(compressed) < len(payload) {
			payload, flags = compressed, flags|connectwire.FlagCompressed
		}
	}
	if _, err := writer.Write(connectwire.AppendEnvelope(nil, flags, payload)); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// connectErrorDocument projects the first-party error envelope onto the Connect
// error document.
//
// The Connect `code` is the protocol's category, chosen from the sixteen the
// specification defines — it says plainly that there are no user-defined codes.
// The finer first-party facts travel as a typed detail: the stable framework
// code, the exact HTTP status, and the declared `details` body verbatim. The
// detail message is the one the TypeScript provider publishes, so a Putnami
// consumer reads one encoding whichever provider answered, and a third-party
// Connect client reads a well-formed error with a code it knows and ignores a
// detail type it does not.
func (b *ApiBridge) connectErrorDocument(status int, body []byte) (int, connectwire.ErrorEnvelope) {
	var firstParty struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	_ = json.Unmarshal(body, &firstParty) //nolint:errcheck // a non-envelope body simply carries no stable code
	mapped := connectStatusForHTTP(status)
	document := connectwire.ErrorEnvelope{Code: mapped.Name, Message: firstParty.Message}
	envelope := connectwire.FrameworkError{Code: firstParty.Code, Status: status}
	if len(firstParty.Details) > 0 && !connectwire.IsJSONNull(firstParty.Details) {
		envelope.DetailsJSON = string(connectwire.TrimSpace(firstParty.Details))
	}
	debug, err := json.Marshal(map[string]any{"code": envelope.Code, "httpStatus": envelope.Status})
	if err != nil {
		debug = nil
	}
	document.Details = append(document.Details, connectwire.ErrorDetail{
		Type: connectwire.FrameworkErrorType,
		// `debug` is a readability affordance the specification lets a server
		// add and forbids a client from depending on; `value` is what a client
		// actually reads.
		Value: connectwire.DetailBase64.EncodeToString(connectwire.EncodeFrameworkError(envelope)),
		Debug: debug,
	})
	return mapped.Status, document
}

func (b *ApiBridge) messageBytes() int64 {
	if b.config.MaxMessageBytes > 0 {
		return b.config.MaxMessageBytes
	}
	return defaultConnectMessageBytes
}

// connectErrorResponse renders the Connect error document. Connect carries an
// error as JSON on every codec, so this shape does not depend on the request's
// content type.
func connectErrorResponse(status int, document connectwire.ErrorEnvelope) *phttp.Response {
	body, err := json.Marshal(document)
	if err != nil {
		body = []byte(`{"code":"internal"}`)
	}
	response := phttp.JSONBytes(body)
	response.Status = status
	return response
}

// connectRawResponse returns a pre-encoded body under an explicit content type,
// compressing it when the caller offered gzip and compression actually helps.
func connectRawResponse(status int, contentType string, body []byte, ctx *phttp.Context, allowCompression bool) *phttp.Response {
	if allowCompression && acceptsConnectGzip(ctx.Request.Header.Get("Accept-Encoding")) {
		if compressed, err := connectwire.CompressGzip(body); err == nil && len(compressed) < len(body) {
			response := phttp.JSONBytes(compressed)
			response.Status = status
			response.Headers.Set("Content-Type", contentType)
			response.Headers.Set("Content-Encoding", connectwire.EncodingGzip)
			return response
		}
	}
	response := phttp.JSONBytes(body)
	response.Status = status
	response.Headers.Set("Content-Type", contentType)
	return response
}

func readBoundedBody(body io.Reader, maxBytes int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("connect: the request body could not be read")
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("connect: the request body exceeds the %d-byte budget", maxBytes)
	}
	return raw, nil
}

func isContextCanceled(err error) bool {
	return stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded)
}

// connectStatusForHTTP picks the gRPC code a first-party HTTP status is
// published as. Several codes share one status, so the reverse direction is a
// choice, not a derivation: the table names the one code this provider means,
// and the exact status travels beside it in google.rpc.ErrorInfo so a
// first-party client restores it without inverting an ambiguous table.
func connectStatusForHTTP(status int) connectwire.Status {
	switch status {
	case http.StatusBadRequest:
		return connectwire.Status{Name: "invalid_argument", Number: 3, Status: 400}
	case http.StatusUnauthorized:
		return connectwire.Status{Name: "unauthenticated", Number: 16, Status: 401}
	case http.StatusForbidden:
		return connectwire.Status{Name: "permission_denied", Number: 7, Status: 403}
	case http.StatusNotFound:
		return connectwire.Status{Name: "not_found", Number: 5, Status: 404}
	case http.StatusConflict:
		return connectwire.Status{Name: "already_exists", Number: 6, Status: 409}
	case http.StatusRequestTimeout:
		return connectwire.Status{Name: "deadline_exceeded", Number: 4, Status: 504}
	case http.StatusTooManyRequests:
		return connectwire.Status{Name: "resource_exhausted", Number: 8, Status: 429}
	case http.StatusNotImplemented:
		return connectwire.Status{Name: "unimplemented", Number: 12, Status: 501}
	case http.StatusServiceUnavailable:
		return connectwire.Status{Name: "unavailable", Number: 14, Status: 503}
	case http.StatusGatewayTimeout:
		return connectwire.Status{Name: "deadline_exceeded", Number: 4, Status: 504}
	}
	if status >= 400 && status < 500 {
		return connectwire.Status{Name: "invalid_argument", Number: 3, Status: 400}
	}
	if status >= 500 {
		return connectwire.Status{Name: "internal", Number: 13, Status: 500}
	}
	return connectwire.Status{Name: "unknown", Number: 2, Status: 500}
}

// acceptsConnectGzip reports whether a comma-separated encoding list names gzip.
func acceptsConnectGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		token := strings.TrimSpace(strings.Split(part, ";")[0])
		if strings.EqualFold(token, connectwire.EncodingGzip) {
			return true
		}
	}
	return false
}
