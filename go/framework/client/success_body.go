package client

import (
	"context"
	"sync"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// SuccessBody receives the success body of one generated call, byte for byte.
//
// Some answers are proven over their exact bytes rather than trusted as a
// decoded value: the provider derived an identifier or a digest from the byte
// sequence it sent. Marshaling the generated value again produces other bytes
// — another field order, other whitespace, omitted optional fields — so the
// proof cannot be rebuilt from it. A caller that owns such a check passes a
// SuccessBody with WithSuccessBody, and the ordinary generated call also hands
// over the body it accepted.
//
// The body is delivered only after every check the call applies: declared
// security and resilience, the declared status and media type, the declared
// schema and the typed decode. A cached answer delivers the bytes the provider
// sent when it was stored. The sink holds its own copy, so nothing a caller
// does with it reaches the response cache.
//
// A SuccessBody belongs to one call at a time: a second call that reaches it
// while the first is in flight is refused before it sends anything.
type SuccessBody struct {
	mu      sync.Mutex
	claimed bool
	body    []byte
}

// Bytes returns the success body the last call delivered. It is nil while a
// call holds the sink, after a call that failed, and before any call. An empty
// body a call accepted is empty and non-nil. The runtime keeps no reference to
// the returned bytes.
func (sink *SuccessBody) Bytes() []byte {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.body
}

type successBodyKey struct{}

// WithSuccessBody returns a context whose generated call also delivers its
// success body into sink.
//
// Only a generated call whose declared success body is JSON delivers it, and
// only on a transport that carries that body unchanged: rest-json, or Connect
// with the json encoding. Every other generated call made with the context —
// a void operation, raw octets, a stream, Connect with the proto encoding,
// whose body this runtime re-encodes from protobuf — fails with client.config
// before it sends anything, and empties sink: a refused call delivers no
// bytes, and never leaves an earlier call's body for the caller to mistake
// for its own.
//
// The call that takes the sink clears it first and fills it only when it
// succeeds. Credential providers, interceptors and other callbacks receive a
// context without the sink, so a nested generated call can neither fill it
// nor be refused because of it.
func WithSuccessBody(ctx context.Context, sink *SuccessBody) context.Context {
	return context.WithValue(ctx, successBodyKey{}, sink)
}

func successBodyFrom(ctx context.Context) *SuccessBody {
	sink, ok := ctx.Value(successBodyKey{}).(*SuccessBody)
	if !ok {
		return nil
	}
	return sink
}

// takeSuccessBody takes the sink ctx carries for one generated call whose
// declared success body is JSON. Everything that can refuse the sink is
// decided here, before it is touched and before anything is sent. The returned
// context no longer carries the sink.
func takeSuccessBody(ctx context.Context, operation Operation) (*SuccessBody, context.Context, error) {
	sink := successBodyFrom(ctx)
	if sink == nil {
		return nil, ctx, nil
	}
	if err := successBodyDeliverable(operation); err != nil {
		sink.reset()
		return nil, ctx, err
	}
	if err := sink.claim(); err != nil {
		return nil, ctx, err
	}
	return sink, context.WithValue(ctx, successBodyKey{}, (*SuccessBody)(nil)), nil
}

// successBodyDeliverable reports whether an operation's success body can reach
// the caller exactly as the provider sent it.
func successBodyDeliverable(operation Operation) error {
	declared := false
	for i := range operation.Successes {
		for j := range operation.Successes[i].Content {
			content := operation.Successes[i].Content[j]
			if content.IsBinary() || content.Streamed {
				return errors.Newf(CodeClientConfig,
					"operation %s declares a raw octet success payload; its generated method returns the octets unchanged",
					operation.ID)
			}
			declared = true
		}
	}
	if !declared {
		return errors.Newf(CodeClientConfig, "operation %s declares no success body to deliver", operation.ID)
	}
	transport, err := dispatchTransport(operation, supportedUnaryTransport)
	if err != nil {
		return err
	}
	if transport.Protocol == clientcontract.TransportConnect && transport.Encoding == clientcontract.EncodingProto {
		return errors.Newf(CodeClientConfig,
			"operation %s dispatches on connect with the proto encoding, whose success body this runtime re-encodes from protobuf; only rest-json and connect with the json encoding carry it unchanged",
			operation.ID)
	}
	return nil
}

// refuseUntakenSuccessBody refuses a generated call that reached the transport
// with a sink still in its context. The calls that deliver a success body take
// the sink before they get this far, so a sink left here belongs to a call that
// cannot deliver one; running it would leave the caller holding no body, or an
// earlier call's body, while it believes it has this one.
func refuseUntakenSuccessBody(ctx context.Context) error {
	sink := successBodyFrom(ctx)
	if sink == nil {
		return nil
	}
	sink.reset()
	return errors.New(CodeClientConfig,
		"only a generated call whose declared success body is JSON delivers it into a success body sink")
}

func (sink *SuccessBody) claim() error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.claimed {
		return errors.New(CodeClientConfig, "a success body sink receives one call at a time")
	}
	sink.claimed, sink.body = true, nil
	return nil
}

// reset empties a sink a call was refused on, so a refusal delivers no bytes.
// A sink another call holds is that call's to fill: the refusal of a second
// caller leaves it alone.
func (sink *SuccessBody) reset() {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.claimed {
		sink.body = nil
	}
}

// deliver ends the call that took the sink: a copy of the body it accepted, or
// nothing when it failed. A nil sink is a call that took none.
func (sink *SuccessBody) deliver(response *Response, err error) {
	if sink == nil {
		return
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.claimed = false
	if err == nil && response != nil {
		sink.body = append(make([]byte, 0, len(response.Body)), response.Body...)
	}
}
