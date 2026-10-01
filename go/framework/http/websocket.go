package http

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// defaultWebSocketIdleTimeout is used when ServerConfig.WebSocketIdleTimeout
// is left at its zero value.
const defaultWebSocketIdleTimeout = 60 * time.Second

// RFC 6455 §7.4.1 close codes this server sends. A close reason travels in
// clear text on the wire, so only fixed framework text ever reaches it.
const (
	// WebSocketCloseNormal ends a completed conversation.
	WebSocketCloseNormal = 1000
	// WebSocketCloseGoingAway reports that the server is shutting down.
	WebSocketCloseGoingAway = 1001
	// WebSocketCloseProtocolError reports a framing or protocol violation.
	WebSocketCloseProtocolError = 1002
	// WebSocketCloseUnsupportedData reports a payload this endpoint cannot accept.
	WebSocketCloseUnsupportedData = 1003
	// WebSocketCloseInvalidPayload reports a payload that is not valid UTF-8 text.
	WebSocketCloseInvalidPayload = 1007
	// WebSocketClosePolicyViolation reports a refusal by the endpoint's own rules.
	WebSocketClosePolicyViolation = 1008
	// WebSocketCloseMessageTooBig reports a message past the negotiated bound.
	WebSocketCloseMessageTooBig = 1009
	// WebSocketCloseInternalError reports a fault on the server side.
	WebSocketCloseInternalError = 1011
)

// RFC 6455 §5.2 opcodes.
const (
	wsOpcodeContinuation byte = 0x0
	wsOpcodeText         byte = 0x1
	wsOpcodeBinary       byte = 0x2
	wsOpcodeClose        byte = 0x8
	wsOpcodePing         byte = 0x9
	wsOpcodePong         byte = 0xA
)

// wsControlPayloadMax is the RFC 6455 §5.5 bound on a control frame payload.
const wsControlPayloadMax = 125

// WebSocketProtocolError is a framing or protocol violation observed on a
// negotiated socket. It carries the close code the peer must be told and never
// carries frame material.
type WebSocketProtocolError struct {
	// CloseCode is the RFC 6455 §7.4.1 code that describes the violation.
	CloseCode int
	message   string
}

// Error implements error with fixed framework text.
func (err *WebSocketProtocolError) Error() string { return err.message }

func newWebSocketProtocolError(closeCode int, message string) *WebSocketProtocolError {
	return &WebSocketProtocolError{CloseCode: closeCode, message: message}
}

// WebSocketCloseError reports that the peer closed the conversation.
type WebSocketCloseError struct {
	// Code is the peer's close code, or WebSocketCloseNormal when it sent none.
	Code int
}

// Error implements error without echoing the peer's reason back into a log.
func (err *WebSocketCloseError) Error() string {
	return fmt.Sprintf("websocket peer closed the connection with code %d", err.Code)
}

func (p *ServerPlugin) serveWebSocket(ctx *Context, stream StreamHandler, subprotocol string) *Response {
	if !isWebSocketRequest(ctx.Request) {
		return JSONStatus(http.StatusBadRequest, map[string]string{
			"error":   "Bad Request",
			"message": "websocket upgrade required",
		})
	}
	hijacker, ok := ctx.Writer.(http.Hijacker)
	if !ok {
		return InternalError("websocket is not supported by this response writer")
	}
	key := strings.TrimSpace(ctx.Request.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return JSONStatus(http.StatusBadRequest, map[string]string{
			"error":   "Bad Request",
			"message": "missing Sec-WebSocket-Key",
		})
	}
	if ctx.Request.Header.Get("Sec-WebSocket-Version") != "13" {
		return JSONStatus(http.StatusBadRequest, map[string]string{
			"error":   "Bad Request",
			"message": "unsupported WebSocket version",
		})
	}
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		return JSONStatus(http.StatusBadRequest, map[string]string{
			"error":   "Bad Request",
			"message": "invalid Sec-WebSocket-Key",
		})
	}

	conn, rw, err := hijacker.Hijack()
	if err != nil {
		p.log.Error("websocket hijack failed", err)
		return nil
	}
	// Clear any inherited deadline. Idle protection is re-armed per-frame by
	// the read loop below (and per-write by writeFrame); Server.ReadTimeout /
	// WriteTimeout do not apply to a hijacked connection.
	_ = conn.SetDeadline(time.Time{}) //nolint:errcheck

	idleTimeout := p.config.WebSocketIdleTimeout
	if idleTimeout == 0 {
		idleTimeout = defaultWebSocketIdleTimeout
	}
	if idleTimeout < 0 {
		idleTimeout = 0 // explicitly disabled
	}

	if err := writeWebSocketHandshake(rw, key, subprotocol); err != nil {
		// Best-effort close on a failed handshake; the underlying error
		// is already what we're returning to the caller via the log.
		_ = conn.Close() //nolint:errcheck
		p.log.Error("websocket handshake failed", err)
		return nil
	}

	ws := &WebSocketConn{
		conn: conn, rw: rw,
		subprotocol: subprotocol, maxMessageBytes: DefaultMaxBodySize,
		draining: p.drainSignal(),
	}
	ws.SetIdleTimeout(idleTimeout)
	defer func() {
		_ = ws.Close() //nolint:errcheck // the conversation's own error is authoritative
	}()

	if stream.Serve != nil && (subprotocol != "" || stream.AdmitOnUpgrade) {
		return p.serveNegotiatedWebSocket(ctx, stream, ws)
	}
	return p.serveRawWebSocket(ctx, stream, ws)
}

// serveNegotiatedWebSocket hands a negotiated socket to the protocol that owns
// its frame vocabulary. This package keeps RFC 6455 and knows nothing of the
// admission, sequencing or terminal rules the protocol applies.
func (p *ServerPlugin) serveNegotiatedWebSocket(ctx *Context, stream StreamHandler, ws *WebSocketConn) *Response {
	baseCtx, cancel := context.WithCancel(ctx.Context())
	defer cancel()
	if err := stream.Serve(ctx.WithContext(baseCtx), ws); err != nil && !errors.Is(err, context.Canceled) {
		p.log.Error("websocket service error", err)
	}
	return nil
}

// serveRawWebSocket runs the transport-level stream: every text or binary
// message reaches the handler as raw JSON, and the handler's values are written
// back as text messages.
func (p *ServerPlugin) serveRawWebSocket(ctx *Context, stream StreamHandler, ws *WebSocketConn) *Response {
	baseCtx, cancel := context.WithCancel(ctx.Context())
	defer cancel()

	rawMessages := make(chan []byte)
	go func() {
		defer close(rawMessages)
		if err := ws.readLoop(baseCtx, rawMessages, stream.Mode != StreamModeServer); err != nil {
			p.log.Debug(fmt.Sprintf("websocket read loop ended: %v", err))
		}
		cancel()
	}()

	// Keepalive: ping the client at half the idle interval. A live client
	// answers with a pong, which resets the read deadline in readFrame; a dead
	// or stalled client triggers the read deadline and the read loop exits.
	if idle := ws.IdleTimeout(); idle > 0 {
		go func() {
			ticker := time.NewTicker(idle / 2)
			defer ticker.Stop()
			for {
				select {
				case <-baseCtx.Done():
					return
				case <-ticker.C:
					if err := ws.writeFrame(wsOpcodePing, nil); err != nil {
						cancel()
						return
					}
				}
			}
		}()
	}

	sctx := newStreamContext(ctx.WithContext(baseCtx), ws.SendJSON, rawMessages)
	if err := stream.Handle(sctx); err != nil && !errors.Is(err, context.Canceled) {
		sctx.setErr(err)
		p.log.Error("websocket stream handler error", err)
	}
	return nil
}

// negotiateWebSocketSubprotocol returns the single token this route agrees to
// speak. RFC 6455 §4.2.2 allows the server to echo only a token the client
// offered, so an unofferable route negotiates nothing and stays on the raw
// transport stream.
func negotiateWebSocketSubprotocol(r *http.Request, offered string) string {
	if offered == "" {
		return ""
	}
	for _, token := range requestSubprotocols(r) {
		if token == offered {
			return offered
		}
	}
	return ""
}

// offersExactlyTheDeclaredSubprotocol reports whether the client's offer fits
// a route that speaks one declared token, or none. RFC 6455 §4.2.2 lets a
// server select one offered token; a route with no token can select nothing,
// so any offer is a vocabulary it does not speak.
func offersExactlyTheDeclaredSubprotocol(r *http.Request, declared string) bool {
	offered := requestSubprotocols(r)
	if declared == "" {
		return len(offered) == 0
	}
	for _, token := range offered {
		if token == declared {
			return true
		}
	}
	return false
}

// requestSubprotocols lists the tokens the client offered, in order.
func requestSubprotocols(r *http.Request) []string {
	var tokens []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(header, ",") {
			if token := strings.TrimSpace(part); token != "" {
				tokens = append(tokens, token)
			}
		}
	}
	return tokens
}

func writeWebSocketHandshake(rw *bufio.ReadWriter, key, subprotocol string) error {
	if _, err := fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(rw, "Upgrade: websocket\r\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(rw, "Connection: Upgrade\r\n"); err != nil {
		return err
	}
	if subprotocol != "" {
		if _, err := fmt.Fprintf(rw, "Sec-WebSocket-Protocol: %s\r\n", subprotocol); err != nil {
			return err
		}
	}
	// The accept token is base64 of a SHA-1 digest, so it is drawn from a fixed
	// alphabet and cannot carry a header separator out of the client's key.
	if _, err := fmt.Fprintf(rw, "Sec-WebSocket-Accept: %s\r\n\r\n", webSocketAcceptToken(key)); err != nil { //nolint:gosec // G705: the value is a base64 digest, not request text
		return err
	}
	return rw.Flush()
}

// webSocketAcceptToken derives the RFC 6455 §1.3 accept token. SHA-1 is
// prescribed by the protocol as a token derivation, not as a security
// primitive.
func webSocketAcceptToken(key string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte(websocketGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func isWebSocketRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerContainsToken(r.Header.Get("Connection"), "upgrade")
}

func headerContainsToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// WebSocketConn is one negotiated RFC 6455 connection. It owns masking,
// continuation reassembly, control-frame answering, close codes and the
// message bound; it carries no knowledge of any application frame vocabulary.
type WebSocketConn struct {
	conn            net.Conn
	rw              *bufio.ReadWriter
	idleTimeout     atomic.Int64
	subprotocol     string
	maxMessageBytes int64
	draining        <-chan struct{}

	writeMu sync.Mutex
	once    sync.Once
}

// SetIdleTimeout bounds the time this connection waits between two peer
// frames. It is read by the reader and writer goroutines, so a protocol may
// narrow it for admission and widen it again once the conversation is open.
// Zero or less disables the bound.
func (c *WebSocketConn) SetIdleTimeout(timeout time.Duration) {
	if timeout < 0 {
		timeout = 0
	}
	c.idleTimeout.Store(int64(timeout))
}

// IdleTimeout reports the current bound between two peer frames.
func (c *WebSocketConn) IdleTimeout() time.Duration {
	return time.Duration(c.idleTimeout.Load())
}

// Subprotocol returns the token negotiated on the opening handshake, or the
// empty string when the socket speaks the raw transport stream.
func (c *WebSocketConn) Subprotocol() string { return c.subprotocol }

// Draining closes when the owning server begins a graceful stop. A protocol
// that must tell its peer why the conversation ends selects on it.
func (c *WebSocketConn) Draining() <-chan struct{} { return c.draining }

// SetMaxMessageBytes bounds one reassembled message. Values above the
// framework body bound, and values at or below zero, keep DefaultMaxBodySize:
// a declared policy may lower the ceiling but never raise it.
func (c *WebSocketConn) SetMaxMessageBytes(limit int64) {
	if limit <= 0 || limit > DefaultMaxBodySize {
		c.maxMessageBytes = DefaultMaxBodySize
		return
	}
	c.maxMessageBytes = limit
}

// MaxMessageBytes reports the bound one reassembled message may reach.
func (c *WebSocketConn) MaxMessageBytes() int64 { return c.maxMessageBytes }

// SetReadDeadline bounds the next read. The zero time removes the bound and
// leaves the connection under the server's idle protection alone.
func (c *WebSocketConn) SetReadDeadline(deadline time.Time) error {
	return c.conn.SetReadDeadline(deadline)
}

// ReadMessage returns the next reassembled application message and its data
// opcode. Ping frames are answered with a pong carrying the same payload, pong
// frames are observed, and a close frame is reported as *WebSocketCloseError.
func (c *WebSocketConn) ReadMessage() (byte, []byte, error) {
	var (
		message  []byte
		opcode   byte
		assembly bool
	)
	for {
		fin, frameOpcode, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		if frameOpcode >= wsOpcodeClose {
			if err := c.handleControlFrame(frameOpcode, payload); err != nil {
				return 0, nil, err
			}
			continue
		}
		if frameOpcode == wsOpcodeContinuation {
			if !assembly {
				return 0, nil, newWebSocketProtocolError(WebSocketCloseProtocolError,
					"websocket continuation frame has no message to continue")
			}
		} else {
			if assembly {
				return 0, nil, newWebSocketProtocolError(WebSocketCloseProtocolError,
					"websocket data frame interrupts a fragmented message")
			}
			assembly, opcode = true, frameOpcode
		}
		if int64(len(message))+int64(len(payload)) > c.maxMessageBytes {
			return 0, nil, newWebSocketProtocolError(WebSocketCloseMessageTooBig,
				"websocket message exceeds the negotiated bound")
		}
		message = append(message, payload...)
		if fin {
			if opcode == wsOpcodeText && !utf8.Valid(message) {
				return 0, nil, newWebSocketProtocolError(WebSocketCloseInvalidPayload,
					"websocket text message is not valid UTF-8")
			}
			return opcode, message, nil
		}
	}
}

// WriteMessage sends one unfragmented text message.
func (c *WebSocketConn) WriteMessage(payload []byte) error {
	if int64(len(payload)) > c.maxMessageBytes {
		return newWebSocketProtocolError(WebSocketCloseMessageTooBig,
			"websocket message exceeds the negotiated bound")
	}
	return c.writeFrame(wsOpcodeText, payload)
}

// WriteBinary sends one unfragmented binary message. A wire that carries raw
// octets uses it; the message bound applies exactly as it does to text.
func (c *WebSocketConn) WriteBinary(payload []byte) error {
	if int64(len(payload)) > c.maxMessageBytes {
		return newWebSocketProtocolError(WebSocketCloseMessageTooBig,
			"websocket message exceeds the negotiated bound")
	}
	return c.writeFrame(wsOpcodeBinary, payload)
}

// Ping sends one ping frame. The payload is bounded by RFC 6455 §5.5.
func (c *WebSocketConn) Ping(payload []byte) error {
	if len(payload) > wsControlPayloadMax {
		return newWebSocketProtocolError(WebSocketCloseProtocolError,
			"websocket ping payload exceeds the control frame bound")
	}
	return c.writeFrame(wsOpcodePing, payload)
}

// CloseWith sends a close frame carrying code and reason, then releases the
// connection. reason must be fixed framework text: a close reason is clear text
// on the wire and never carries credential, token or payload material.
func (c *WebSocketConn) CloseWith(code int, reason string) error {
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code)) //nolint:gosec // close codes are the fixed 16-bit vocabulary of RFC 6455 §7.4.1
	payload = append(payload, truncateCloseReason(reason)...)
	writeErr := c.writeFrame(wsOpcodeClose, payload)
	closeErr := c.closeConn()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// Close performs the best-effort close handshake and releases the connection.
func (c *WebSocketConn) Close() error {
	var err error
	c.once.Do(func() {
		_ = c.writeFrame(wsOpcodeClose, nil) //nolint:errcheck // the peer may already be gone; the release below is what matters
		err = c.conn.Close()
	})
	return err
}

// closeConn releases the socket exactly once without a second close frame.
func (c *WebSocketConn) closeConn() error {
	var err error
	c.once.Do(func() { err = c.conn.Close() })
	return err
}

// truncateCloseReason keeps a close reason inside the RFC 6455 control frame
// bound, cutting on a rune boundary so the reason stays valid UTF-8.
func truncateCloseReason(reason string) string {
	const maximum = wsControlPayloadMax - 2
	if len(reason) <= maximum {
		return reason
	}
	cut := maximum
	for cut > 0 && !utf8.ValidString(reason[:cut]) {
		cut--
	}
	return reason[:cut]
}

// SendJSON encodes a value as one text message.
func (c *WebSocketConn) SendJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteMessage(data)
}

// readLoop drives the raw transport stream: data messages reach out, control
// frames are answered inline, and a peer close ends the loop.
func (c *WebSocketConn) readLoop(ctx context.Context, out chan<- []byte, deliverMessages bool) error {
	for {
		_, payload, err := c.ReadMessage()
		if err != nil {
			var closed *WebSocketCloseError
			if errors.As(err, &closed) {
				return nil
			}
			return err
		}
		if !deliverMessages {
			continue
		}
		// Select on ctx so a handler that returns without draining
		// Messages()/RawMessages() does not park this goroutine (and its
		// fd) forever on a send with no consumer.
		select {
		case out <- payload:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// handleControlFrame answers a ping, observes a pong, and reports a close.
func (c *WebSocketConn) handleControlFrame(opcode byte, payload []byte) error {
	switch opcode {
	case wsOpcodePing:
		return c.writeFrame(wsOpcodePong, payload)
	case wsOpcodePong:
		return nil
	case wsOpcodeClose:
		// Echo the close frame back per RFC 6455 §5.5.1 before reporting it.
		// The peer is already shutting down, so a failed echo is non-fatal.
		_ = c.writeFrame(wsOpcodeClose, nil) //nolint:errcheck
		return newWebSocketCloseError(payload)
	default:
		return newWebSocketProtocolError(WebSocketCloseProtocolError, "websocket control opcode is reserved")
	}
}

// newWebSocketCloseError projects a close payload. A close frame carries either
// no payload or a two-byte code and a UTF-8 reason.
func newWebSocketCloseError(payload []byte) error {
	if len(payload) < 2 {
		return &WebSocketCloseError{Code: WebSocketCloseNormal}
	}
	return &WebSocketCloseError{Code: int(binary.BigEndian.Uint16(payload[:2]))}
}

// readFrame reads exactly one frame header and its payload, applying the
// RFC 6455 §5 rules a server must enforce on a client frame.
func (c *WebSocketConn) readFrame() (bool, byte, []byte, error) {
	// Arm a rolling read deadline so an idle or slow client cannot pin a
	// goroutine and file descriptor open forever. The deadline covers reading a
	// whole frame; it is reset on entry to each frame read.
	if idle := c.IdleTimeout(); idle > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(idle)) //nolint:errcheck
	}
	var header [2]byte
	if _, err := io.ReadFull(c.rw, header[:]); err != nil {
		return false, 0, nil, err
	}
	fin := header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		return false, 0, nil, newWebSocketProtocolError(WebSocketCloseProtocolError,
			"websocket frame sets a reserved bit and no extension was negotiated")
	}
	opcode := header[0] & 0x0f
	if !validWebSocketOpcode(opcode) {
		return false, 0, nil, newWebSocketProtocolError(WebSocketCloseProtocolError,
			"websocket frame carries a reserved opcode")
	}
	masked := header[1]&0x80 != 0
	if !masked {
		// RFC 6455 §5.1: every client frame is masked. An unmasked one is a
		// protocol violation, not a payload this server may read.
		return false, 0, nil, newWebSocketProtocolError(WebSocketCloseProtocolError,
			"websocket client frame is not masked")
	}
	indicator := header[1] & 0x7f
	if opcode >= wsOpcodeClose {
		// RFC 6455 section 5.5 bounds a control frame before any length is read:
		// an extended length indicator is itself the violation, so it must not be
		// reported as an oversized message.
		if !fin {
			return false, 0, nil, newWebSocketProtocolError(WebSocketCloseProtocolError,
				"websocket control frame is fragmented")
		}
		if indicator > wsControlPayloadMax {
			return false, 0, nil, newWebSocketProtocolError(WebSocketCloseProtocolError,
				"websocket control frame exceeds its payload bound")
		}
	}
	length, err := c.readFrameLength(indicator)
	if err != nil {
		return false, 0, nil, err
	}

	var mask [4]byte
	if _, err := io.ReadFull(c.rw, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.rw, payload); err != nil {
		return false, 0, nil, err
	}
	wsApplyMask(payload, mask)
	return fin, opcode, payload, nil
}

// readFrameLength resolves the RFC 6455 §5.2 extended payload length and
// refuses a message this connection could never buffer.
func (c *WebSocketConn) readFrameLength(indicator byte) (uint64, error) {
	length := uint64(indicator)
	switch indicator {
	case 126:
		var buf [2]byte
		if _, err := io.ReadFull(c.rw, buf[:]); err != nil {
			return 0, err
		}
		length = uint64(binary.BigEndian.Uint16(buf[:]))
	case 127:
		var buf [8]byte
		if _, err := io.ReadFull(c.rw, buf[:]); err != nil {
			return 0, err
		}
		length = binary.BigEndian.Uint64(buf[:])
	}
	limit := c.maxMessageBytes
	if limit <= 0 || limit > DefaultMaxBodySize {
		limit = DefaultMaxBodySize
	}
	if length > uint64(limit) { //nolint:gosec // limit is clamped to (0, DefaultMaxBodySize] on the line above
		return 0, newWebSocketProtocolError(WebSocketCloseMessageTooBig, "websocket message too large")
	}
	return length, nil
}

// validWebSocketOpcode reports whether opcode is one RFC 6455 §5.2 defines.
func validWebSocketOpcode(opcode byte) bool {
	switch opcode {
	case wsOpcodeContinuation, wsOpcodeText, wsOpcodeBinary, wsOpcodeClose, wsOpcodePing, wsOpcodePong:
		return true
	default:
		return false
	}
}

// wsApplyMask applies the RFC 6455 §5.3 masking transform in place. The
// transform is its own inverse.
func wsApplyMask(payload []byte, key [4]byte) {
	for i := range payload {
		payload[i] ^= key[i%4]
	}
}

func (c *WebSocketConn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	// Bound writes too: a stalled client that stops reading must not let a
	// write block indefinitely on the hijacked socket.
	if idle := c.IdleTimeout(); idle > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(idle)) //nolint:errcheck
	}

	header := []byte{0x80 | opcode}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(length))
	case length <= 0xffff:
		// length is bounded by the case condition (0xffff = 16 bits),
		// so byte() truncation is intentional and produces the protocol's
		// big-endian 16-bit length field.
		header = append(header, 126, byte(length>>8), byte(length)) //nolint:gosec

	default:
		header = append(header, 127)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(length))
		header = append(header, buf[:]...)
	}
	if _, err := c.rw.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.rw.Write(payload); err != nil {
			return err
		}
	}
	return c.rw.Flush()
}
