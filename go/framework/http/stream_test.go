package http

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

func TestServerPlugin_StreamSSE(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{})
	sp.HandleStream("/notifications", StreamHandler{
		Mode: StreamModeServer,
		Handle: func(ctx *StreamContext) error {
			return ctx.Send(map[string]string{"message": "ready"})
		},
	})

	match := sp.routes.lookup("GET", "/notifications")
	if match == nil {
		t.Fatal("expected stream route match")
	}

	req := httptest.NewRequest("GET", "/notifications", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()

	resp := match.Handlers[0](NewContext(rec, req))
	if resp != nil {
		t.Fatalf("stream handler returned response %+v, want nil", resp)
	}
	if rec.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if !strings.Contains(rec.Body.String(), `data: {"message":"ready"}`) {
		t.Errorf("SSE body missing data frame: %q", rec.Body.String())
	}
}

// TestServerPlugin_StreamSSEWritesSanitizedTypedTerminalError pins the SSE
// terminal frame to the one wire both generated clients read: the first-party
// error envelope plus `status`, flat, with `details` holding the declared body
// alone. The TypeScript provider writes the same frame (stream-error.ts); a
// nested envelope under `details` would leave the message where no client
// reads it and the declared body where no declared schema can match it.
func TestServerPlugin_StreamSSEWritesSanitizedTypedTerminalError(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{})
	sp.HandleStream("/notifications", StreamHandler{
		Mode: StreamModeServer,
		Handle: func(*StreamContext) error {
			return perrors.User(perrors.Code("events.not_found"), "missing", perrors.String("details", "safe"))
		},
	})
	match := sp.routes.lookup("GET", "/notifications")
	req := httptest.NewRequest("GET", "/notifications", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	if resp := match.Handlers[0](NewContext(rec, req)); resp != nil {
		t.Fatalf("stream handler returned response %+v", resp)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: error\n") {
		t.Fatalf("typed terminal event = %q", body)
	}
	if got := sseTerminalData(t, body); got != `{"status":500,"code":"events.not_found","error":"Internal Server Error","message":"missing","details":"safe"}` {
		t.Fatalf("terminal frame = %s, want the flat first-party envelope plus status", got)
	}
}

// TestServerPlugin_StreamSSETerminalOmitsUndeclaredDetails proves `details` is
// absent, not an empty envelope, when the error declares no detail body.
func TestServerPlugin_StreamSSETerminalOmitsUndeclaredDetails(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{})
	sp.HandleStream("/notifications", StreamHandler{
		Mode: StreamModeServer,
		Handle: func(*StreamContext) error {
			return perrors.NotFound("widget 4f0c does not exist")
		},
	})
	match := sp.routes.lookup("GET", "/notifications")
	req := httptest.NewRequest("GET", "/notifications", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	if resp := match.Handlers[0](NewContext(rec, req)); resp != nil {
		t.Fatalf("stream handler returned response %+v", resp)
	}
	if got := sseTerminalData(t, rec.Body.String()); got != `{"status":404,"code":"not_found","error":"Not Found","message":"widget 4f0c does not exist"}` {
		t.Fatalf("terminal frame = %s, want no details member", got)
	}
}

// sseTerminalData returns the data line of the single `error` event in an SSE
// body, verbatim.
func sseTerminalData(t *testing.T, body string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if line == "event: error" && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "data: ") {
			return strings.TrimPrefix(lines[i+1], "data: ")
		}
	}
	t.Fatalf("no error event in %q", body)
	return ""
}

func TestServerPlugin_StreamRequiresNegotiation(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{})
	sp.HandleStream("/notifications", StreamHandler{
		Mode:   StreamModeServer,
		Handle: func(_ *StreamContext) error { return nil },
	})

	match := sp.routes.lookup("GET", "/notifications")
	req := httptest.NewRequest("GET", "/notifications", nil)
	resp := match.Handlers[0](NewContext(httptest.NewRecorder(), req))
	if resp == nil || resp.Status != stdhttp.StatusBadRequest {
		t.Fatalf("response = %+v, want 400", resp)
	}
}

func TestServerPlugin_StreamWebSocketEcho(t *testing.T) {
	sp := NewServerPlugin(ServerConfig{})
	sp.HandleStream("/chat", StreamHandler{
		Mode: StreamModeBidirectional,
		Handle: func(ctx *StreamContext) error {
			raw := <-ctx.RawMessages()
			var msg map[string]string
			if err := json.Unmarshal(raw, &msg); err != nil {
				return err
			}
			return ctx.Send(msg)
		},
	})

	server := httptest.NewServer(sp.buildHandler())
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial websocket server: %v", err)
	}
	defer conn.Close()

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	if _, err := rw.WriteString("GET /chat HTTP/1.1\r\n"); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	if _, err := rw.WriteString("Host: " + u.Host + "\r\n"); err != nil {
		t.Fatalf("write host: %v", err)
	}
	if _, err := rw.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"); err != nil {
		t.Fatalf("write headers: %v", err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatalf("flush handshake: %v", err)
	}
	status, err := rw.ReadString('\n')
	if err != nil {
		t.Fatalf("read handshake status: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q, want 101", status)
	}
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			t.Fatalf("read handshake header: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	if err := writeMaskedTextFrame(rw, []byte(`{"text":"hi"}`)); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
	payload, err := readServerTextFrame(rw)
	if err != nil {
		t.Fatalf("read websocket echo: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal echo: %v", err)
	}
	if got["text"] != "hi" {
		t.Errorf("echo text = %q, want hi", got["text"])
	}
}

// When a stream handler stops draining and the reader parks on a send to
// an unconsumed channel, canceling the context must unblock it so the reader
// goroutine (and its fd) does not leak.
func TestWebSocketReadLoop_UnblocksOnContextCancel(t *testing.T) {
	spectest.Proves(t, "go/http-services", "bounded-streams", "websocket-read-loop-unblocks-on-cancellation")
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ws := &WebSocketConn{
		conn: serverConn,
		rw:   bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn)),
	}
	clientRW := bufio.NewReadWriter(bufio.NewReader(clientConn), bufio.NewWriter(clientConn))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan []byte) // unbuffered, intentionally never consumed
	done := make(chan error, 1)
	go func() { done <- ws.readLoop(ctx, out, true) }()

	// Deliver one frame; readLoop reads it and parks on `out <- payload`.
	go func() { _ = writeMaskedTextFrame(clientRW, []byte("hi")) }()

	// Let the reader park on the send, then cancel — it must return promptly.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// readLoop returned — the reader goroutine did not leak.
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not exit after context cancel — it parked on the send (goroutine leak)")
	}
}

func writeMaskedTextFrame(rw *bufio.ReadWriter, payload []byte) error {
	header := []byte{0x81}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, 0x80|byte(length))
	case length <= 0xffff:
		header = append(header, 0x80|126, byte(length>>8), byte(length))
	default:
		header = append(header, 0x80|127)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(length))
		header = append(header, buf[:]...)
	}
	mask := [4]byte{1, 2, 3, 4}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := rw.Write(header); err != nil {
		return err
	}
	if _, err := rw.Write(masked); err != nil {
		return err
	}
	return rw.Flush()
}

func readServerTextFrame(rw *bufio.ReadWriter) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(rw, header[:]); err != nil {
		return nil, err
	}
	opcode := header[0] & 0x0f
	if opcode != 0x1 {
		return nil, io.ErrUnexpectedEOF
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var buf [2]byte
		if _, err := io.ReadFull(rw, buf[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(buf[:]))
	case 127:
		var buf [8]byte
		if _, err := io.ReadFull(rw, buf[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(buf[:])
	}
	payload := make([]byte, length)
	_, err := io.ReadFull(rw, payload)
	return payload, err
}

// TestServerPlugin_WebSocketIdleTimeout verifies that an idle hijacked
// WebSocket connection is dropped by the server within the configured idle
// timeout instead of being held open forever. Without a server-side read
// deadline, the client read below would block until its own deadline.
func TestServerPlugin_WebSocketIdleTimeout(t *testing.T) {
	spectest.Proves(t, "go/http-services", "bounded-streams", "websocket-idle-timeout-drops-the-connection")
	sp := NewServerPlugin(ServerConfig{WebSocketIdleTimeout: 100 * time.Millisecond})
	sp.HandleStream("/idle", StreamHandler{
		Mode: StreamModeBidirectional,
		Handle: func(ctx *StreamContext) error {
			base := ctx.Context.Context() // underlying request/stream context
			<-base.Done()                 // unblocks when the read loop times out
			return base.Err()
		},
	})

	server := httptest.NewServer(sp.buildHandler())
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial websocket server: %v", err)
	}
	defer conn.Close()

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	if _, err := rw.WriteString("GET /idle HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatalf("flush handshake: %v", err)
	}
	status, err := rw.ReadString('\n')
	if err != nil {
		t.Fatalf("read handshake status: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q, want 101", status)
	}
	for {
		line, err := rw.ReadString('\n')
		if err != nil {
			t.Fatalf("read handshake header: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	// Client stays idle. Give the server generous slack over its 100ms idle
	// timeout; the connection must close (a control frame or EOF) well within.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 1)
	_, err = rw.Read(buf)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection was not closed within the idle timeout — read timed out client-side")
	}
	// Any non-timeout outcome (close frame byte read, or io.EOF) means the
	// server tore the idle connection down as intended.
}

// deadlineRecorder is a ResponseWriter that records every write deadline
// http.ResponseController arms on it, so a test can observe the per-write
// bound instead of inferring it from a stalled connection.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func (d *deadlineRecorder) Flush() { d.ResponseRecorder.Flush() }

// TestServerPlugin_SSEWritesAreDeadlineBounded pins the SSE half of
// bounded-streams: a client that stops reading fills the TCP send buffer, and
// without a per-write deadline the handler's Write blocks forever, pinning a
// goroutine and its fd. The server first clears the server-wide write deadline
// (so a long-lived stream is not killed by Server.WriteTimeout) and then re-arms
// a bounded one before every frame.
func TestServerPlugin_SSEWritesAreDeadlineBounded(t *testing.T) {
	spectest.Proves(t, "go/http-services", "bounded-streams", "sse-writes-are-deadline-bounded")

	sp := NewServerPlugin(ServerConfig{StreamWriteTimeout: 250 * time.Millisecond})
	sp.HandleStream("/notifications", StreamHandler{
		Mode: StreamModeServer,
		Handle: func(ctx *StreamContext) error {
			if err := ctx.Send(map[string]string{"n": "1"}); err != nil {
				return err
			}
			return ctx.Send(map[string]string{"n": "2"})
		},
	})

	match := sp.routes.lookup("GET", "/notifications")
	if match == nil {
		t.Fatal("expected stream route match")
	}
	req := httptest.NewRequest("GET", "/notifications", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

	start := time.Now()
	if resp := match.Handlers[0](NewContext(rec, req)); resp != nil {
		t.Fatalf("stream handler returned response %+v, want nil", resp)
	}

	// First: the server-wide deadline is cleared, so a long-lived stream is not
	// cut off by Server.WriteTimeout.
	if len(rec.deadlines) == 0 {
		t.Fatal("no write deadline was ever set on the SSE response")
	}
	if !rec.deadlines[0].IsZero() {
		t.Errorf("first deadline = %v, want the zero time (server-wide deadline cleared)", rec.deadlines[0])
	}

	// Then: one bounded deadline per frame — the header flush and both sends.
	bounded := rec.deadlines[1:]
	if len(bounded) < 3 {
		t.Fatalf("armed %d bounded deadlines, want at least 3 (header + 2 sends); got %v", len(bounded), bounded)
	}
	for i, deadline := range bounded {
		if deadline.IsZero() {
			t.Errorf("bounded deadline[%d] is zero — that write was unbounded", i)
			continue
		}
		if d := deadline.Sub(start); d <= 0 || d > 5*time.Second {
			t.Errorf("bounded deadline[%d] is %v from start, want ~250ms", i, d)
		}
	}

	// Each frame re-arms rather than reusing the first bound, so a slow stream
	// cannot inherit an already-expired deadline.
	for i := 1; i < len(bounded); i++ {
		if !bounded[i].After(bounded[i-1]) && bounded[i] != bounded[i-1] {
			t.Errorf("deadline[%d] = %v went backwards from %v", i, bounded[i], bounded[i-1])
		}
	}
}
