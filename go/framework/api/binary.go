package api

import (
	"fmt"
	"io"
	"mime"
	"reflect"
	"strconv"
	"strings"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
)

// BinarySchema declares a raw octet request body or success response: the
// endpoint carries bytes on the wire, not a JSON document.
//
// It is a declaration about the wire, so it names both facts a consumer cannot
// guess — the media type and the byte bound:
//
//	api.Endpoint("POST", "/blobs").
//	    Body(api.Binary("application/octet-stream", 64<<10)).
//	    ReturnsStatus(201, "Created", api.Type[Stored]()).
//	    Handle(store)
//
// Binary requires a bound for buffered payloads. BinaryStream explicitly opts
// into incremental consumption with a media type carried by each HTTP message.
type BinarySchema struct {
	mediaType string
	maxBytes  int64
	streamed  bool
}

// BinaryMeta is the discovered form of a [BinarySchema]: the facts every
// downstream consumer (OpenAPI projection, IR reader, emitters) needs.
type BinaryMeta struct {
	// MediaType is fixed for Binary and */* for BinaryStream.
	MediaType string
	// MaxBytes is strictly positive for Binary and BinaryStream.
	MaxBytes int64
	// Streamed selects an unbuffered body whose concrete media type is supplied
	// by the sender. Its MediaType is */* and its bound is enforced incrementally.
	Streamed bool
}

// Binary declares a raw octet payload of mediaType, bounded to maxBytes.
//
// It panics on a media type that is empty, unparseable or JSON-structured, and
// on a bound that is not strictly positive.
func Binary(mediaType string, maxBytes int64) BinarySchema {
	parsed, _, err := mime.ParseMediaType(mediaType)
	if err != nil || !strings.Contains(parsed, "/") || strings.Contains(parsed, "*") {
		panic(fmt.Sprintf("api.Binary: %q is not a media type: %v", mediaType, err))
	}
	if isJSONMediaType(parsed) {
		panic(fmt.Sprintf("api.Binary: %q is a JSON media type; declare a JSON body with api.Type instead", mediaType))
	}
	if maxBytes <= 0 {
		panic(fmt.Sprintf("api.Binary: %q needs a strictly positive byte bound, got %d", mediaType, maxBytes))
	}
	return BinarySchema{mediaType: parsed, maxBytes: maxBytes}
}

// BinaryStream declares one unframed HTTP octet stream. The sender supplies a
// concrete Content-Type; the framework does not buffer the body and enforces
// maxBytes incrementally while the handler consumes it.
// This is a unary HTTP operation, not a stream of JSON or WebSocket messages.
func BinaryStream(maxBytes int64) BinarySchema {
	if maxBytes <= 0 {
		panic(fmt.Sprintf("api.BinaryStream: needs a strictly positive byte bound, got %d", maxBytes))
	}
	return BinarySchema{mediaType: "*/*", maxBytes: maxBytes, streamed: true}
}

// MediaType returns the declared wire media type.
func (b BinarySchema) MediaType() string { return b.mediaType }

// MaxBytes returns the declared byte bound.
func (b BinarySchema) MaxBytes() int64 { return b.maxBytes }

func (b BinarySchema) meta() *BinaryMeta {
	return &BinaryMeta{MediaType: b.mediaType, MaxBytes: b.maxBytes, Streamed: b.streamed}
}

// isJSONMediaType reports the media types the JSON pipeline owns: application/
// json and every structured +json suffix.
func isJSONMediaType(mediaType string) bool {
	lower := strings.ToLower(mediaType)
	return lower == "application/json" || strings.HasSuffix(lower, "+json")
}

// binaryPayloadType is the Go type a binary payload takes in every metadata
// surface that still speaks reflect.Type. The bytes themselves never travel
// through the JSON schema generator: the projection short-circuits on
// BinaryMeta, and this type only keeps "a payload is declared" true for the
// route facts that predate binary bodies.
func binaryPayloadType() reflect.Type { return reflect.TypeFor[[]byte]() }

// BinaryBody returns the request bytes of an endpoint whose body was declared
// with [Binary]. The endpoint pipeline has already enforced the declared media
// type and the declared bound, so the handler receives bytes it may use
// directly.
//
// It fails on an endpoint that did not declare a binary body: reading JSON
// bytes through this accessor would skip the validation the declaration asked
// for.
func BinaryBody(ctx *phttp.EndpointContext) ([]byte, error) {
	if ctx == nil {
		return nil, perrors.New(perrors.CodeInternal, "api.BinaryBody: no endpoint context")
	}
	body, ok := ctx.DecodedBody.([]byte)
	if !ok {
		return nil, perrors.New(perrors.CodeInternal, "api.BinaryBody: endpoint did not declare a binary request body")
	}
	return body, nil
}

// BinaryResponse writes data verbatim under the declared media type. It is the
// response counterpart of [Binary]: no JSON encoding, no base64, no charset
// parameter appended to a binary media type.
func BinaryResponse(status int, mediaType string, data []byte) *phttp.Response {
	return phttp.Bytes(status, mediaType, data)
}

// BinaryStreamBody returns a declared stream without reading it. The request
// owns its lifetime; consume it within that request's lifetime. Use Request's
// Content-Type header when the label is part of the stored payload.
func BinaryStreamBody(ctx *phttp.EndpointContext) (io.Reader, error) {
	if ctx != nil {
		if body, ok := ctx.DecodedBody.(io.Reader); ok {
			return body, nil
		}
	}
	return nil, perrors.New(perrors.CodeInternal, "api.BinaryStreamBody: endpoint did not declare a streamed binary request body")
}

// BinaryStreamResponse transfers a source to the response writer without
// buffering. If it is also an io.Closer, the writer closes it after copying,
// including on failure. The concrete media type is preserved with parameters.
func BinaryStreamResponse(status int, mediaType string, body io.Reader) *phttp.Response {
	if !phttp.ConcreteMediaType(mediaType) {
		if closer, ok := body.(io.Closer); ok {
			closer.Close() //nolint:errcheck // The invalid content type is the primary refusal.
		}
		return phttp.ErrorResponse(perrors.New(perrors.CodeInternalServer, "Invalid streamed response content type"))
	}
	return phttp.Stream(status, mediaType, body)
}

// readDeclaredBinaryBody applies the declared media type and the declared bound
// to an incoming request, and returns the sanitized framework response when
// either is violated.
//
// The bound is checked twice on purpose. Content-Length is advisory, so a peer
// that announces a small body and sends a large one must still be stopped; and
// a peer that announces an oversized body must be refused before a single byte
// of it is read. Only the bounded read is authoritative.
func readDeclaredBinaryBody(ctx *phttp.Context, declared BinaryMeta) ([]byte, *phttp.Response) {
	if declared.MediaType != "" {
		if got := ctx.ContentType(); !strings.EqualFold(strings.TrimSpace(got), declared.MediaType) {
			return nil, phttp.ErrorResponse(perrors.New(perrors.CodeUnsupportedMediaType,
				"Unsupported request content type"))
		}
	}
	if announced := ctx.Request.Header.Get("Content-Length"); announced != "" {
		if length, err := strconv.ParseInt(announced, 10, 64); err == nil && length > declared.MaxBytes {
			return nil, phttp.ErrorResponse(perrors.New(perrors.CodePayloadTooLarge, "Request body exceeds the declared bound"))
		}
	}
	limited := io.LimitReader(ctx.Request.Body, declared.MaxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, phttp.ErrorResponse(perrors.BadRequest("Invalid request body"))
	}
	if int64(len(data)) > declared.MaxBytes {
		return nil, phttp.ErrorResponse(perrors.New(perrors.CodePayloadTooLarge, "Request body exceeds the declared bound"))
	}
	return data, nil
}

// cloneBinaryMeta defensively copies a discovered binary declaration so a
// consumer cannot mutate the endpoint's own facts.
func cloneBinaryMeta(meta *BinaryMeta) *BinaryMeta {
	if meta == nil {
		return nil
	}
	copied := *meta
	return &copied
}
