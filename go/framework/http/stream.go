package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	perrors "go.putnami.dev/errors"
)

// StreamMode describes how a streaming endpoint moves messages.
type StreamMode string

const (
	// StreamModeServer is server-to-client streaming. HTTP serves it as SSE
	// and also accepts WebSocket upgrades.
	StreamModeServer StreamMode = "server"
	// StreamModeClient is client-to-server streaming over WebSocket.
	StreamModeClient StreamMode = "client"
	// StreamModeBidirectional is bidirectional streaming over WebSocket.
	StreamModeBidirectional StreamMode = "bidirectional"
)

// defaultStreamWriteTimeout bounds each SSE write when
// ServerConfig.StreamWriteTimeout is left unset.
const defaultStreamWriteTimeout = 30 * time.Second

// StreamHandler is the transport-level handler for API stream endpoints.
type StreamHandler struct {
	Mode          StreamMode
	BodySchema    reflect.Type
	ReturnsSchema reflect.Type
	Before        Handler
	Handle        func(*StreamContext) error
	// Subprotocol is the single Sec-WebSocket-Protocol token this route agrees
	// to speak. Empty leaves the upgrade unnegotiated and the socket on the raw
	// transport stream.
	Subprotocol string
	// Serve drives a socket that negotiated Subprotocol. The framework does not
	// run Before on that path: a negotiated protocol carries its admission
	// material in its own first frame, so the route's security and validation
	// chain runs from Serve, once those headers are reconstructed, and never on
	// the bare upgrade request.
	Serve func(*Context, *WebSocketConn) error
	// AdmitOnUpgrade declares a wire that carries no admission material of its
	// own: the upgrade request is the admission. Before runs on that request,
	// the client must offer exactly Subprotocol — or no token at all when
	// Subprotocol is empty — and every admitted socket is handed to Serve. The
	// route has no raw transport stream and no SSE: it speaks one wire.
	AdmitOnUpgrade bool
	// SSEWire is the negotiated SSE wire this route speaks besides the legacy
	// framing. Nil keeps every SSE request on the legacy framing.
	SSEWire *SSEWire
}

// SSEWire is one negotiated SSE wire. The transport owns only its mechanics:
// the api plugin fills it from the first-party client contract
// (go.putnami.dev/protocol/clientcontract ADR 0013), the way it fills
// Subprotocol for a WebSocket.
//
// A request whose Header field lines Negotiates accepts is acknowledged with
// Header: Token on the admitted response head, before the first body byte. Its
// stream ends with Complete when the handler returns without error, every send
// was written and the request was not canceled. It ends with the typed error
// frame after a handler failure, and with nothing after a cancellation, so the
// consumer reads an interruption and never a success. Every other request
// keeps the legacy framing, byte for byte.
type SSEWire struct {
	// Header names the header that negotiates the wire, on the request and on
	// the acknowledgment.
	Header string
	// Token is the value an acknowledged response carries in Header.
	Token string
	// Negotiates reports whether the request's field lines of Header ask for
	// this wire. Nil never negotiates.
	Negotiates func(lines []string) bool
	// Complete is the exact successful terminal frame.
	Complete string
}

// negotiated reports whether request asks for the wire this route speaks.
func (w *SSEWire) negotiated(request *http.Request) bool {
	return w != nil && w.Header != "" && w.Negotiates != nil && w.Negotiates(request.Header.Values(w.Header))
}

// StreamContext is the low-level transport context for SSE/WebSocket streams.
// API users normally see the typed wrappers in go.putnami.dev/api.
type StreamContext struct {
	*Context

	send        func(any) error
	rawMessages <-chan []byte

	errMu sync.RWMutex
	err   error

	resultMu  sync.RWMutex
	result    any
	hasResult bool
}

// SetResult records the single terminal value a client or bidirectional stream
// returns to its caller. The transport carries it in the protocol's terminal
// frame; a transport with no terminal value ignores it.
func (c *StreamContext) SetResult(value any) {
	if c == nil {
		return
	}
	c.resultMu.Lock()
	c.result, c.hasResult = value, true
	c.resultMu.Unlock()
}

// Result returns the terminal value the handler declared, and whether it
// declared one at all.
func (c *StreamContext) Result() (any, bool) {
	if c == nil {
		return nil, false
	}
	c.resultMu.RLock()
	defer c.resultMu.RUnlock()
	return c.result, c.hasResult
}

// Send writes a message to the client. It is available for server and
// bidirectional streams.
func (c *StreamContext) Send(v any) error {
	if c == nil || c.send == nil {
		return errors.New("http stream: send is not available for this stream")
	}
	return c.send(v)
}

// RawMessages returns raw JSON message payloads read from the WebSocket client.
func (c *StreamContext) RawMessages() <-chan []byte {
	if c == nil || c.rawMessages == nil {
		ch := make(chan []byte)
		close(ch)
		return ch
	}
	return c.rawMessages
}

// Err returns the first transport error observed by the stream.
func (c *StreamContext) Err() error {
	if c == nil {
		return nil
	}
	c.errMu.RLock()
	defer c.errMu.RUnlock()
	return c.err
}

func (c *StreamContext) setErr(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
}

// NewStreamContext builds the transport context a stream handler receives.
// send writes one provider value; pass nil to model a receive-only stream.
// rawMessages supplies the client's message payloads; pass nil to model a
// send-only stream. A negotiated WebSocket protocol uses it to hand its own
// framed halves to the endpoint handler.
func NewStreamContext(ctx *Context, send func(any) error, rawMessages <-chan []byte) *StreamContext {
	return newStreamContext(ctx, send, rawMessages)
}

func newStreamContext(ctx *Context, send func(any) error, rawMessages <-chan []byte) *StreamContext {
	return &StreamContext{
		Context:     ctx,
		send:        send,
		rawMessages: rawMessages,
	}
}

// HandleStream registers a streaming API endpoint at path. Server streams are
// exposed as SSE and WebSocket. Client and bidirectional streams require
// WebSocket.
func (p *ServerPlugin) HandleStream(path string, stream StreamHandler) {
	p.routes.Add("GET", path, p.streamRouteHandler(stream))
	// A stream binds one public route, and it binds it under GET: SSE reads the
	// route as a plain GET, and a WebSocket upgrade is a GET the server answers
	// with 101. So the inventory fact is a GET fact, and httpRoutes() adds the
	// HEAD companion the server's GET fallback already serves.
	//
	// The fact belongs here rather than in api.Plugin: every caller that binds a
	// stream goes through HandleStream — the api plugin, the gRPC bridge, a
	// direct caller — so recording it here keeps schema/http-routes.json a
	// complete list of what the edge binds. A default-deny gateway policy reads
	// that file; a bound route missing from it is refused at the edge.
	p.httpRouteFacts = append(p.httpRouteFacts, httpRouteFact{method: "GET", path: path, source: routeSourceTypedAPI})
}

func (p *ServerPlugin) streamRouteHandler(stream StreamHandler) Handler {
	return func(ctx *Context) *Response {
		if stream.Handle == nil {
			return InternalError("stream handler is not configured")
		}
		upgrade := isWebSocketRequest(ctx.Request)
		admitOnUpgrade := stream.AdmitOnUpgrade && stream.Serve != nil
		subprotocol := ""
		switch {
		case upgrade && admitOnUpgrade:
			// A wire that admits on the upgrade speaks exactly the token it
			// declares, or none. Anything else is a client that expects another
			// vocabulary, and a socket it can only fail on.
			if !offersExactlyTheDeclaredSubprotocol(ctx.Request, stream.Subprotocol) {
				return JSONStatus(http.StatusBadRequest, map[string]string{
					"error":   "Bad Request",
					"message": "requested websocket subprotocol is not the one this endpoint speaks",
				})
			}
			subprotocol = stream.Subprotocol
		case upgrade:
			if stream.Serve != nil {
				subprotocol = negotiateWebSocketSubprotocol(ctx.Request, stream.Subprotocol)
			}
			if subprotocol == "" && len(requestSubprotocols(ctx.Request)) > 0 {
				// Accepting a socket whose offered vocabulary this route cannot
				// speak would hand the caller a connection it can only fail on.
				return JSONStatus(http.StatusBadRequest, map[string]string{
					"error":   "Bad Request",
					"message": "requested websocket subprotocol is not supported by this endpoint",
				})
			}
		}
		// A negotiated protocol admits in band, so its own Serve runs the chain
		// after it reconstructs the request headers from the first frame. A wire
		// that admits on the upgrade has nothing to reconstruct: the chain reads
		// the upgrade request itself.
		if (subprotocol == "" || admitOnUpgrade) && stream.Before != nil {
			if resp := stream.Before(ctx); resp != nil {
				return resp
			}
		}
		if upgrade {
			return p.serveWebSocket(ctx, stream, subprotocol)
		}
		if !admitOnUpgrade && stream.Mode == StreamModeServer && acceptsEventStream(ctx.Request) {
			return p.serveSSE(ctx, stream)
		}
		return JSONStatus(http.StatusBadRequest, map[string]string{
			"error":   "Bad Request",
			"message": `stream endpoint requires "Upgrade: websocket" or "Accept: text/event-stream"`,
		})
	}
}

func (p *ServerPlugin) serveSSE(ctx *Context, stream StreamHandler) *Response {
	rc := http.NewResponseController(ctx.Writer)
	// Disable the server-wide write deadline so a long-lived SSE stream isn't
	// killed by Server.WriteTimeout. A bounded per-write deadline is re-armed
	// before every flush below instead, so a stalled client still cannot block
	// a write indefinitely.
	_ = rc.SetWriteDeadline(time.Time{}) //nolint:errcheck

	writeTimeout := p.config.StreamWriteTimeout
	if writeTimeout == 0 {
		writeTimeout = defaultStreamWriteTimeout
	}
	if writeTimeout < 0 {
		writeTimeout = 0 // explicitly disabled
	}
	// armWriteDeadline bounds a single write: a client that stops reading
	// surfaces as a write error the handler can observe.
	armWriteDeadline := func() {
		if writeTimeout > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout)) //nolint:errcheck
		}
	}

	flusher, ok := ctx.Writer.(http.Flusher)
	if !ok {
		return InternalError("streaming is not supported by this response writer")
	}

	// Admission already ran (Before), so a negotiating request is acknowledged
	// on this response head, before the first body byte.
	negotiated := stream.SSEWire.negotiated(ctx.Request)
	header := ctx.Writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	if negotiated {
		header.Set(stream.SSEWire.Header, stream.SSEWire.Token)
	}
	armWriteDeadline()
	ctx.Writer.WriteHeader(http.StatusOK)
	flusher.Flush()

	// A send that failed may have left part of a frame on the wire, so a
	// negotiated stream that saw one never ends with the successful terminal.
	var sendFailed atomic.Bool
	sctx := newStreamContext(ctx, func(v any) error {
		err := writeSSEEvent(ctx.Writer, flusher, armWriteDeadline, "", v)
		if err != nil {
			sendFailed.Store(true)
		}
		return err
	}, nil)

	// A draining server (a graceful stop, a scale to zero, an instance
	// replacement) is the instance change a continuation exists for: it stops
	// the handler of a negotiated stream and ends the response with no
	// terminal, so the consumer reads an interruption and continues on another
	// instance. A legacy stream keeps its framing.
	drain := p.drainSignal()
	if negotiated {
		streamCtx, stopStream := context.WithCancel(ctx.Context())
		defer stopStream()
		ctx.SetContext(streamCtx)
		go func() {
			select {
			case <-drain:
				stopStream()
			case <-streamCtx.Done():
			}
		}()
	}

	err := stream.Handle(sctx)
	// A negotiated stream the consumer canceled, whose context ended, or whose
	// server began draining gets no terminal at all: the consumer reads an
	// interruption, never a success.
	canceled := negotiated && (ctx.Context().Err() != nil || drained(drain))
	if err == nil && negotiated && !canceled && !sendFailed.Load() {
		armWriteDeadline()
		if _, writeErr := io.WriteString(ctx.Writer, stream.SSEWire.Complete); writeErr != nil {
			sctx.setErr(writeErr)
		} else {
			flusher.Flush()
		}
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		sctx.setErr(err)
		status, body := perrors.HTTPErrorResponse(err)
		// The terminal frame is the first-party error envelope every unary
		// endpoint already writes (`{code, error, message, details?}`, ADR 0003
		// of the client contract) plus the status a stream has no response line
		// to carry: `{status, code, error, message, details?}`, the same frame
		// the TypeScript provider writes (stream-error.ts). `details` is the
		// declared body alone and is omitted when the error carries none, so a
		// generated client validates the declared schema against it directly.
		wire := struct {
			Status int `json:"status"`
			perrors.HTTPErrorBody
		}{Status: status, HTTPErrorBody: body}
		// Nobody reads a terminal on a negotiated stream the consumer left.
		if !canceled {
			if writeErr := writeSSEEvent(ctx.Writer, flusher, armWriteDeadline, "error", wire); writeErr != nil {
				sctx.setErr(writeErr)
			}
		}
		p.log.Error("sse stream handler error", err)
	}
	return nil
}

// drained reports whether the server has begun its graceful stop.
func drained(signal <-chan struct{}) bool {
	select {
	case <-signal:
		return true
	default:
		return false
	}
}

func writeSSEEvent(writer http.ResponseWriter, flusher http.Flusher, armWriteDeadline func(), event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	armWriteDeadline()
	if event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "data: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func acceptsEventStream(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		if strings.EqualFold(strings.TrimSpace(strings.Split(part, ";")[0]), "text/event-stream") {
			return true
		}
	}
	return false
}
