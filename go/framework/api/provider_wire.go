package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// ByteStreamSchema marks a request body or return value as raw octets carried
// in binary WebSocket messages. A byte stream carries octets both ways, so an
// endpoint declares it on both sides:
//
//	api.Endpoint("GET", "/v1/databases/connect").
//	    Query(api.Type[ConnectQuery]()).
//	    Body(api.ByteStream()).
//	    Returns(api.ByteStream()).
//	    Secure(security.Options{Scopes: []string{"gateway:connect"}}).
//	    Handle(api.ByteTunnel(func(ctx *api.ByteStreamContext) error {
//	        return pipe(ctx, backend)
//	    }))
//
// It is a provider-owned wire: there is no first-party conversation envelope
// and no in-band admission. The upgrade request carries the declared
// credentials, the endpoint's security and validation chain runs on it, and
// the handler reads and writes octets through an io.ReadWriter.
type ByteStreamSchema struct{}

// ByteStream declares raw octets in binary WebSocket messages. Declare it on
// both Body and Returns.
func ByteStream() ByteStreamSchema { return ByteStreamSchema{} }

// ProviderWireMeta is the discovered form of a provider-owned WebSocket wire:
// the subprotocol the route negotiates, if any, and whether its messages are
// raw octets rather than JSON values of the declared message types.
type ProviderWireMeta struct {
	// Subprotocol is the declared negotiation token. It is empty only for a
	// byte stream that negotiates none.
	Subprotocol string
	// Bytes reports a byte stream: binary messages carrying raw octets.
	Bytes bool
}

// Subprotocol declares the WebSocket subprotocol this endpoint's
// provider-owned wire negotiates, for example "putnami.events.v1".
//
// On a bidirectional typed stream — Body(api.StreamOf[In]()) and
// Returns(api.StreamOf[Out]()) — it replaces the first-party conversation:
// each message is one JSON value of In or Out, with no envelope, and the
// provider's own protocol owns everything after the upgrade. On a byte stream
// it names the token the tunnel negotiates. The token must be a valid RFC 9110
// token outside the first-party "putnami.service." namespace.
func (b *EndpointBuilder) Subprotocol(token string) *EndpointBuilder {
	b.subprotocol = token
	b.subprotocolSet = true
	return b
}

// providerWire returns the endpoint's provider-owned wire, or nil when the
// endpoint is not one.
func (b *EndpointBuilder) providerWire() *ProviderWireMeta {
	switch {
	case b.bodyBytes || b.returnsBytes:
		return &ProviderWireMeta{Subprotocol: b.subprotocol, Bytes: true}
	case b.subprotocolSet:
		return &ProviderWireMeta{Subprotocol: b.subprotocol}
	default:
		return nil
	}
}

// validateProviderWire panics on a provider-owned wire the contract could not
// publish. Every rule here is one the client contract refuses, so an authoring
// mistake stops at registration instead of reaching a published document no
// reader accepts.
func (b *EndpointBuilder) validateProviderWire(finalizer string) {
	wire := b.providerWire()
	if wire == nil {
		return
	}
	if wire.Bytes && !(b.bodyBytes && b.returnsBytes) {
		panic(fmt.Sprintf("api.EndpointBuilder.%s: a byte stream carries octets both ways; declare Body(api.ByteStream()) and Returns(api.ByteStream())", finalizer))
	}
	if !b.bodyStream || !b.returnsStream {
		panic(fmt.Sprintf("api.EndpointBuilder.%s: .Subprotocol() declares a provider-owned wire, which carries a bidirectional stream; declare Body and Returns as streams", finalizer))
	}
	if b.subprotocolSet {
		if !clientcontract.ValidWebSocketSubprotocol(b.subprotocol) {
			panic(fmt.Sprintf("api.EndpointBuilder.%s: subprotocol %q is not a negotiable websocket token", finalizer, b.subprotocol))
		}
		if strings.HasPrefix(b.subprotocol, clientcontract.WebSocketReservedSubprotocolPrefix) {
			panic(fmt.Sprintf("api.EndpointBuilder.%s: subprotocol %q is in the first-party namespace %q", finalizer, b.subprotocol, clientcontract.WebSocketReservedSubprotocolPrefix))
		}
	}
	if b.clientOptions != nil && b.clientOptions.Resume {
		panic(fmt.Sprintf("api.EndpointBuilder.%s: a provider-owned wire resumes by its own protocol; ClientOperationOptions.Resume is the first-party conversation's", finalizer))
	}
	if b.clientOptions != nil && b.clientOptions.SSEContinuation != nil {
		panic(fmt.Sprintf("api.EndpointBuilder.%s: a provider-owned wire is a WebSocket and carries no SSE; ClientOperationOptions.SSEContinuation is the first-party SSE transport's", finalizer))
	}
}

// ProviderWire returns the endpoint's provider-owned WebSocket wire, or nil
// when the endpoint is unary or a first-party stream.
func (d EndpointDefinition) ProviderWire() *ProviderWireMeta { return d.builder.providerWire() }

// byteStreamPayloadType is the Go type a byte stream takes in every metadata
// surface that still speaks reflect.Type. No projection reads it as a JSON
// schema: each one short-circuits on ProviderWireMeta beside it.
func byteStreamPayloadType() reflect.Type { return reflect.TypeFor[[]byte]() }

// ByteStreamContext is the handler context of a byte stream. Read returns the
// octets the client sends and io.EOF once the client ends the stream normally;
// Write sends octets to the client. Read and Write may run in separate
// goroutines; neither is safe for concurrent use by several goroutines.
type ByteStreamContext struct {
	*phttp.Context

	stream  *phttp.StreamContext
	pending []byte
}

// Read reads octets the client sent. Message boundaries carry no meaning on a
// byte stream, so one binary message may be read in several calls.
func (c *ByteStreamContext) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(c.pending) == 0 {
		select {
		case chunk, open := <-c.stream.RawMessages():
			if !open {
				if err := c.streamErr(); err != nil {
					return 0, err
				}
				return 0, io.EOF
			}
			c.pending = chunk
		case <-c.done():
			return 0, c.streamErr()
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// Write sends octets to the client in binary messages, split under the
// declared frame bound.
func (c *ByteStreamContext) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := c.stream.Send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// done is the stream context's cancellation channel. It closes when the
// stream ends abnormally or the server stops, never on a normal client close.
func (c *ByteStreamContext) done() <-chan struct{} {
	if c.Context == nil || c.Context.Context() == nil {
		return nil
	}
	return c.Context.Context().Done()
}

// streamErr names why the stream stopped abnormally, or nil. The driver
// cancels the stream context with the cause, so the handler reads the reason
// the socket gave rather than a bare cancellation.
func (c *ByteStreamContext) streamErr() error {
	if err := c.stream.Err(); err != nil {
		return err
	}
	if c.Context != nil {
		return contextCause(c.Context.Context())
	}
	return nil
}

// ByteTunnel adapts a byte-stream handler for Endpoint.Handle. The endpoint
// must declare Body(api.ByteStream()) and Returns(api.ByteStream()). The
// handler's return ends the stream: nil closes it normally, an error closes it
// with the code its status maps to.
func ByteTunnel(fn func(*ByteStreamContext) error) StreamHandler {
	return byteTunnelHandler{fn: fn}
}

type byteTunnelHandler struct {
	fn func(*ByteStreamContext) error
}

func (h byteTunnelHandler) streamMode() phttp.StreamMode { return phttp.StreamModeBidirectional }

func (h byteTunnelHandler) carriesBytes() bool { return true }

func (h byteTunnelHandler) handleStream(ctx *phttp.StreamContext) error {
	if h.fn == nil {
		return errors.New("api: nil byte stream handler")
	}
	return h.fn(&ByteStreamContext{Context: ctx.Context, stream: ctx})
}

// byteCarrier is implemented by the stream handlers that read and write raw
// octets, so Handle can refuse a typed handler on a byte stream and the
// reverse.
type byteCarrier interface {
	carriesBytes() bool
}

func handlerCarriesBytes(handler StreamHandler) bool {
	carrier, ok := handler.(byteCarrier)
	return ok && carrier.carriesBytes()
}

// errByteStreamValue is returned when something other than octets is sent on
// a byte stream.
var errByteStreamValue = errors.New("api: a byte stream sends []byte values only")

// contextCause reports the cancellation cause of ctx, or nil.
func contextCause(ctx context.Context) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}
