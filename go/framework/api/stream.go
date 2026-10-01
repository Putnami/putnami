package api

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"

	phttp "go.putnami.dev/http"
)

// StreamMode describes the transport shape for a streaming endpoint.
type StreamMode = phttp.StreamMode

// Re-export the StreamMode constants from go.putnami.dev/http so api callers
// can declare endpoints without importing the http package directly.
const (
	StreamModeServer        = phttp.StreamModeServer
	StreamModeClient        = phttp.StreamModeClient
	StreamModeBidirectional = phttp.StreamModeBidirectional
)

// StreamSchema marks a request body or return value as a stream of messages
// with the given schema type.
type StreamSchema struct {
	typ reflect.Type
}

// Stream marks schemaType as a stream payload schema. Prefer StreamOf[T]()
// when the type is known statically.
func Stream(schemaType reflect.Type) StreamSchema {
	return StreamSchema{typ: schemaType}
}

// StreamOf marks T as a stream payload schema.
func StreamOf[T any]() StreamSchema {
	return Stream(reflect.TypeFor[T]())
}

// Type returns the underlying message type.
func (s StreamSchema) Type() reflect.Type {
	return s.typ
}

// StreamHandler is returned by the typed stream handler adapters.
type StreamHandler interface {
	streamMode() phttp.StreamMode
	handleStream(*phttp.StreamContext) error
}

// ServerStreamContext is the handler context for server-to-client streams.
type ServerStreamContext[T any] struct {
	*phttp.StreamContext
}

// Send writes a typed message to the client.
func (c *ServerStreamContext[T]) Send(v T) error {
	return c.StreamContext.Send(v)
}

// ClientStreamContext is the handler context for client-to-server streams.
// TIn is the message type the client uploads; TOut is the single declared value
// the handler returns once the client has half-closed its direction.
type ClientStreamContext[TIn, TOut any] struct {
	*phttp.StreamContext

	once     sync.Once
	messages chan TIn
	errMu    sync.RWMutex
	err      error
}

// Messages returns decoded messages from the client.
func (c *ClientStreamContext[TIn, TOut]) Messages() <-chan TIn {
	c.once.Do(func() {
		c.messages = make(chan TIn)
		go decodeMessages(c.StreamContext, c.messages, c.setErr)
	})
	return c.messages
}

// Result declares the single value this stream returns to its caller. A client
// stream delivers nothing through messages, so a handler that completes without
// declaring one is a contract violation rather than an empty result.
func (c *ClientStreamContext[TIn, TOut]) Result(value TOut) {
	c.StreamContext.SetResult(value)
}

// Err returns the first decode or transport error observed by the stream.
func (c *ClientStreamContext[TIn, TOut]) Err() error {
	c.errMu.RLock()
	err := c.err
	c.errMu.RUnlock()
	if err != nil {
		return err
	}
	if c.StreamContext != nil {
		return c.StreamContext.Err()
	}
	return nil
}

func (c *ClientStreamContext[TIn, TOut]) setErr(err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
}

// BidiStreamContext is the handler context for bidirectional streams.
type BidiStreamContext[TIn, TOut any] struct {
	*phttp.StreamContext

	once     sync.Once
	messages chan TIn
	errMu    sync.RWMutex
	err      error
}

// Messages returns decoded messages from the client.
func (c *BidiStreamContext[TIn, TOut]) Messages() <-chan TIn {
	c.once.Do(func() {
		c.messages = make(chan TIn)
		go decodeMessages(c.StreamContext, c.messages, c.setErr)
	})
	return c.messages
}

// Send writes a typed message to the client.
func (c *BidiStreamContext[TIn, TOut]) Send(v TOut) error {
	return c.StreamContext.Send(v)
}

// Result declares the single terminal value this stream returns beside its
// messages. It is optional: a bidirectional stream that says everything through
// messages completes without one.
func (c *BidiStreamContext[TIn, TOut]) Result(value TOut) {
	c.StreamContext.SetResult(value)
}

// Err returns the first decode or transport error observed by the stream.
func (c *BidiStreamContext[TIn, TOut]) Err() error {
	c.errMu.RLock()
	err := c.err
	c.errMu.RUnlock()
	if err != nil {
		return err
	}
	if c.StreamContext != nil {
		return c.StreamContext.Err()
	}
	return nil
}

func (c *BidiStreamContext[TIn, TOut]) setErr(err error) {
	if err == nil {
		return
	}
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
}

// ServerStream adapts a typed server-stream handler for Endpoint.Handle.
func ServerStream[T any](fn func(*ServerStreamContext[T]) error) StreamHandler {
	return serverStreamHandler[T]{fn: fn}
}

// ClientStream adapts a typed client-stream handler for Endpoint.Handle. TIn is
// the uploaded message type declared by Body(api.StreamOf[TIn]()); TOut is the
// single response type declared by Returns.
func ClientStream[TIn, TOut any](fn func(*ClientStreamContext[TIn, TOut]) error) StreamHandler {
	return clientStreamHandler[TIn, TOut]{fn: fn}
}

// BidiStream adapts a typed bidirectional-stream handler for Endpoint.Handle.
func BidiStream[TIn, TOut any](fn func(*BidiStreamContext[TIn, TOut]) error) StreamHandler {
	return bidiStreamHandler[TIn, TOut]{fn: fn}
}

type serverStreamHandler[T any] struct {
	fn func(*ServerStreamContext[T]) error
}

func (h serverStreamHandler[T]) streamMode() phttp.StreamMode { return phttp.StreamModeServer }

func (h serverStreamHandler[T]) handleStream(ctx *phttp.StreamContext) error {
	if h.fn == nil {
		return errors.New("api: nil server stream handler")
	}
	return h.fn(&ServerStreamContext[T]{StreamContext: ctx})
}

type clientStreamHandler[TIn, TOut any] struct {
	fn func(*ClientStreamContext[TIn, TOut]) error
}

func (h clientStreamHandler[TIn, TOut]) streamMode() phttp.StreamMode { return phttp.StreamModeClient }

func (h clientStreamHandler[TIn, TOut]) handleStream(ctx *phttp.StreamContext) error {
	if h.fn == nil {
		return errors.New("api: nil client stream handler")
	}
	return h.fn(&ClientStreamContext[TIn, TOut]{StreamContext: ctx})
}

type bidiStreamHandler[TIn, TOut any] struct {
	fn func(*BidiStreamContext[TIn, TOut]) error
}

func (h bidiStreamHandler[TIn, TOut]) streamMode() phttp.StreamMode {
	return phttp.StreamModeBidirectional
}

func (h bidiStreamHandler[TIn, TOut]) handleStream(ctx *phttp.StreamContext) error {
	if h.fn == nil {
		return errors.New("api: nil bidirectional stream handler")
	}
	return h.fn(&BidiStreamContext[TIn, TOut]{StreamContext: ctx})
}

func decodeMessages[T any](base *phttp.StreamContext, out chan<- T, setErr func(error)) {
	defer close(out)
	if base == nil {
		return
	}
	done := streamDone(base)
	for raw := range base.RawMessages() {
		var msg T
		if err := json.Unmarshal(raw, &msg); err != nil {
			setErr(err)
			return
		}
		// Select on the stream context so a handler that returns without draining
		// Messages() does not park this decoder goroutine forever on a send with
		// no consumer.
		select {
		case out <- msg:
		case <-done:
			return
		}
	}
	if err := base.Err(); err != nil {
		setErr(err)
	}
}

// streamDone returns the cancellation channel of the stream's context, used to
// abort a blocked send when the handler stops draining. A nil channel (no
// context) blocks forever in a select, preserving the original send behavior.
func streamDone(base *phttp.StreamContext) <-chan struct{} {
	if base.Context == nil {
		return nil
	}
	if ctx := base.Context.Context(); ctx != nil {
		return ctx.Done()
	}
	return nil
}
