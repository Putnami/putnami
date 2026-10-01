package http

import (
	"bufio"
	"encoding/binary"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.putnami.dev/protocol/features/spectest"
)

// newTestWebSocketConn returns a server connection and the raw client end of
// the same pipe, so a test writes frame bytes by hand.
func newTestWebSocketConn(t *testing.T, maxMessageBytes int64) (*WebSocketConn, net.Conn) {
	t.Helper()
	clientEnd, serverEnd := net.Pipe()
	t.Cleanup(func() {
		_ = clientEnd.Close()
		_ = serverEnd.Close()
	})
	conn := &WebSocketConn{
		conn:            serverEnd,
		rw:              bufio.NewReadWriter(bufio.NewReader(serverEnd), bufio.NewWriter(serverEnd)),
		maxMessageBytes: maxMessageBytes,
	}
	return conn, clientEnd
}

// writeClientFrame writes one masked frame, as RFC 6455 section 5.1 requires of
// a client. mask reports whether the mask bit is set at all, so a test can send
// the unmasked frame a server must refuse.
func writeClientFrame(conn net.Conn, fin bool, rsv, opcode byte, mask bool, payload []byte) error {
	header := []byte{opcode | rsv}
	if fin {
		header[0] |= 0x80
	}
	length := len(payload)
	maskBit := byte(0)
	if mask {
		maskBit = 0x80
	}
	switch {
	case length < 126:
		header = append(header, byte(length)|maskBit)
	case length <= 0xffff:
		header = append(header, 126|maskBit, byte(length>>8), byte(length))
	default:
		header = append(header, 127|maskBit)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(length))
		header = append(header, buf[:]...)
	}
	body := append([]byte(nil), payload...)
	if mask {
		key := [4]byte{0x11, 0x22, 0x33, 0x44}
		for i := range body {
			body[i] ^= key[i%4]
		}
		header = append(header, key[:]...)
	}
	_, err := conn.Write(append(header, body...))
	return err
}

// readServerFrame reads one server frame from the raw client end.
func readServerFrame(t *testing.T, conn net.Conn) (bool, byte, []byte) {
	t.Helper()
	reader := bufio.NewReader(conn)
	header := make([]byte, 2)
	if err := readFull(reader, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	if header[1]&0x80 != 0 {
		t.Fatal("the server masked a frame")
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		buf := make([]byte, 2)
		if err := readFull(reader, buf); err != nil {
			t.Fatalf("read 16-bit length: %v", err)
		}
		length = uint64(binary.BigEndian.Uint16(buf))
	case 127:
		buf := make([]byte, 8)
		if err := readFull(reader, buf); err != nil {
			t.Fatalf("read 64-bit length: %v", err)
		}
		length = binary.BigEndian.Uint64(buf)
	}
	payload := make([]byte, length)
	if err := readFull(reader, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return header[0]&0x80 != 0, header[0] & 0x0f, payload
}

func readFull(reader *bufio.Reader, buf []byte) error {
	read := 0
	for read < len(buf) {
		n, err := reader.Read(buf[read:])
		read += n
		if err != nil {
			return err
		}
	}
	return nil
}

// The accept token is pinned to the vector RFC 6455 section 1.3 publishes, so a
// wrong salt is caught by the specification itself rather than by a peer that
// shares the same mistake.
func TestWebSocketAcceptTokenMatchesTheRFCExample(t *testing.T) {
	spectest.Proves(t, "go/http-services", "websocket-framing", "the-accept-token-matches-the-published-rfc-vector")
	if got, want := webSocketAcceptToken("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Fatalf("accept token = %q, want the RFC 6455 §1.3 value %q", got, want)
	}
}

func TestNegotiateWebSocketSubprotocolEchoesOnlyAnOfferedToken(t *testing.T) {
	spectest.Proves(t, "go/http-services", "websocket-framing", "only-an-offered-subprotocol-is-echoed")
	tests := []struct {
		name    string
		offered []string
		route   string
		want    string
	}{
		{name: "no route token", offered: []string{"putnami.service.v1"}, route: "", want: ""},
		{name: "exact offer", offered: []string{"putnami.service.v1"}, route: "putnami.service.v1", want: "putnami.service.v1"},
		{name: "second offer", offered: []string{"other", "putnami.service.v1"}, route: "putnami.service.v1", want: "putnami.service.v1"},
		{name: "no match", offered: []string{"other"}, route: "putnami.service.v1", want: ""},
		{name: "client offered nothing", offered: nil, route: "putnami.service.v1", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "/stream", nil)
			for _, token := range tc.offered {
				request.Header.Add("Sec-WebSocket-Protocol", token)
			}
			if got := negotiateWebSocketSubprotocol(request, tc.route); got != tc.want {
				t.Fatalf("negotiated = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWebSocketConnReassemblesContinuationFrames(t *testing.T) {
	spectest.Proves(t, "go/http-services", "websocket-framing", "continuation-frames-are-reassembled-under-the-body-bound")
	conn, peer := newTestWebSocketConn(t, DefaultMaxBodySize)
	payload := []byte(`{"type":"message","sequence":"1"}`)
	go func() {
		chunk := 7
		for offset, first := 0, true; offset < len(payload); first = false {
			end := min(offset+chunk, len(payload))
			opcode := wsOpcodeContinuation
			if first {
				opcode = wsOpcodeText
			}
			_ = writeClientFrame(peer, end == len(payload), 0, opcode, true, payload[offset:end])
			offset = end
		}
	}()
	opcode, message, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if opcode != wsOpcodeText || string(message) != string(payload) {
		t.Fatalf("reassembled opcode %d message %q, want a text %q", opcode, message, payload)
	}
}

func TestWebSocketConnRefusesFramingTheProtocolForbids(t *testing.T) {
	spectest.Proves(t, "go/http-services", "websocket-framing", "framing-the-protocol-forbids-is-refused-with-its-close-code")
	tests := []struct {
		name  string
		write func(net.Conn)
		want  int
	}{
		{
			name:  "a reserved bit with no negotiated extension",
			write: func(peer net.Conn) { _ = writeClientFrame(peer, true, 0x40, wsOpcodeText, true, []byte("x")) },
			want:  WebSocketCloseProtocolError,
		},
		{
			name:  "a reserved opcode",
			write: func(peer net.Conn) { _ = writeClientFrame(peer, true, 0, 0x3, true, []byte("x")) },
			want:  WebSocketCloseProtocolError,
		},
		{
			name:  "an unmasked client frame",
			write: func(peer net.Conn) { _ = writeClientFrame(peer, true, 0, wsOpcodeText, false, []byte("x")) },
			want:  WebSocketCloseProtocolError,
		},
		{
			name:  "a fragmented control frame",
			write: func(peer net.Conn) { _ = writeClientFrame(peer, false, 0, wsOpcodePing, true, nil) },
			want:  WebSocketCloseProtocolError,
		},
		{
			name: "an oversized control frame",
			write: func(peer net.Conn) {
				_ = writeClientFrame(peer, true, 0, wsOpcodePing, true, make([]byte, wsControlPayloadMax+1))
			},
			want: WebSocketCloseProtocolError,
		},
		{
			name:  "a continuation with no message to continue",
			write: func(peer net.Conn) { _ = writeClientFrame(peer, true, 0, wsOpcodeContinuation, true, []byte("x")) },
			want:  WebSocketCloseProtocolError,
		},
		{
			name: "a data frame interrupting a fragmented message",
			write: func(peer net.Conn) {
				_ = writeClientFrame(peer, false, 0, wsOpcodeText, true, []byte("a"))
				_ = writeClientFrame(peer, true, 0, wsOpcodeText, true, []byte("b"))
			},
			want: WebSocketCloseProtocolError,
		},
		{
			name:  "a text message that is not valid UTF-8",
			write: func(peer net.Conn) { _ = writeClientFrame(peer, true, 0, wsOpcodeText, true, []byte{0xff, 0xfe}) },
			want:  WebSocketCloseInvalidPayload,
		},
		{
			name:  "a message past the negotiated bound",
			write: func(peer net.Conn) { _ = writeClientFrame(peer, true, 0, wsOpcodeText, true, make([]byte, 64)) },
			want:  WebSocketCloseMessageTooBig,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, peer := newTestWebSocketConn(t, 32)
			go tc.write(peer)
			_, _, err := conn.ReadMessage()
			var violation *WebSocketProtocolError
			if !errors.As(err, &violation) {
				t.Fatalf("ReadMessage error = %T %v, want a protocol violation", err, err)
			}
			if violation.CloseCode != tc.want {
				t.Fatalf("close code = %d, want %d", violation.CloseCode, tc.want)
			}
		})
	}
}

func TestWebSocketConnAnswersAPingAndReportsAClose(t *testing.T) {
	spectest.Proves(t, "go/http-services", "websocket-framing", "a-ping-is-answered-and-a-peer-close-is-reported")
	t.Run("ping is answered with the same payload", func(t *testing.T) {
		conn, peer := newTestWebSocketConn(t, DefaultMaxBodySize)
		go func() { _ = writeClientFrame(peer, true, 0, wsOpcodePing, true, []byte("nonce-1")) }()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, _ = conn.ReadMessage()
		}()
		fin, opcode, payload := readServerFrame(t, peer)
		if !fin || opcode != wsOpcodePong || string(payload) != "nonce-1" {
			t.Fatalf("answer = fin %v opcode %d payload %q, want an unfragmented pong", fin, opcode, payload)
		}
	})

	t.Run("a peer close is reported with its code", func(t *testing.T) {
		conn, peer := newTestWebSocketConn(t, DefaultMaxBodySize)
		go func() {
			payload := make([]byte, 2)
			binary.BigEndian.PutUint16(payload, WebSocketCloseGoingAway)
			_ = writeClientFrame(peer, true, 0, wsOpcodeClose, true, payload)
			// Drain the echoed close so the pipe write does not block.
			_, _, _ = readServerFrame(t, peer)
		}()
		_, _, err := conn.ReadMessage()
		var closed *WebSocketCloseError
		if !errors.As(err, &closed) {
			t.Fatalf("ReadMessage error = %T %v, want a peer close", err, err)
		}
		if closed.Code != WebSocketCloseGoingAway {
			t.Fatalf("close code = %d, want %d", closed.Code, WebSocketCloseGoingAway)
		}
	})
}

func TestWebSocketConnBoundsTheMessageSizeToTheFrameworkCeiling(t *testing.T) {
	conn, _ := newTestWebSocketConn(t, DefaultMaxBodySize)
	for _, limit := range []int64{0, -1, DefaultMaxBodySize + 1, 1 << 40} {
		conn.SetMaxMessageBytes(limit)
		if got := conn.MaxMessageBytes(); got != DefaultMaxBodySize {
			t.Fatalf("SetMaxMessageBytes(%d) left the bound at %d, want %d", limit, got, DefaultMaxBodySize)
		}
	}
	conn.SetMaxMessageBytes(4096)
	if got := conn.MaxMessageBytes(); got != 4096 {
		t.Fatalf("a declared lower bound = %d, want 4096", got)
	}
	if err := conn.WriteMessage(make([]byte, 4097)); err == nil {
		t.Fatal("a write past the declared bound was accepted")
	}
}

// A close reason is clear text on the wire, so it is bounded to what a control
// frame carries and cut on a rune boundary rather than mid-character.
func TestWebSocketCloseReasonStaysInsideTheControlFrameBound(t *testing.T) {
	spectest.Proves(t, "go/http-services", "websocket-framing", "a-close-reason-stays-inside-the-control-frame-bound")
	long := strings.Repeat("é", 200)
	reason := truncateCloseReason(long)
	if len(reason) > wsControlPayloadMax-2 {
		t.Fatalf("reason is %d bytes, want at most %d", len(reason), wsControlPayloadMax-2)
	}
	if !utf8.ValidString(reason) {
		t.Fatal("the truncated reason is not valid UTF-8")
	}
	if short := truncateCloseReason("stream complete"); short != "stream complete" {
		t.Fatalf("a short reason was altered to %q", short)
	}
}

func TestWebSocketCloseWithSendsTheCodeAndReleasesTheConnection(t *testing.T) {
	conn, peer := newTestWebSocketConn(t, DefaultMaxBodySize)
	read := make(chan []byte, 1)
	go func() {
		_, opcode, payload := readServerFrame(t, peer)
		if opcode != wsOpcodeClose {
			t.Errorf("opcode = %d, want a close frame", opcode)
		}
		read <- payload
	}()
	if err := conn.CloseWith(WebSocketClosePolicyViolation, "stream failed"); err != nil {
		t.Fatalf("CloseWith: %v", err)
	}
	select {
	case payload := <-read:
		if len(payload) < 2 || int(binary.BigEndian.Uint16(payload[:2])) != WebSocketClosePolicyViolation {
			t.Fatalf("close payload = %v, want the policy-violation code", payload)
		}
		if string(payload[2:]) != "stream failed" {
			t.Fatalf("close reason = %q", payload[2:])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the close frame never reached the peer")
	}
	// A second close must not write a second frame onto a released socket.
	if err := conn.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
		t.Fatalf("second Close: %v", err)
	}
}

func TestServerPluginDrainSignalClosesOnceOnStop(t *testing.T) {
	spectest.Proves(t, "go/http-services", "graceful-shutdown", "stop-announces-the-drain-to-hijacked-connections")
	plugin := NewServerPlugin(ServerConfig{})
	signal := plugin.drainSignal()
	select {
	case <-signal:
		t.Fatal("the drain signal was closed before Stop")
	default:
	}
	if err := plugin.Stop(t.Context(), nil); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("Stop did not announce the drain to hijacked connections")
	}
	// Stop is idempotent: a second call must not close the channel twice.
	if err := plugin.Stop(t.Context(), nil); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// A route that cannot speak an offered subprotocol refuses the upgrade instead
// of accepting a socket the caller can only fail on.
func TestStreamRouteRefusesASubprotocolItCannotSpeak(t *testing.T) {
	spectest.Proves(t, "go/http-services", "websocket-framing", "an-unspeakable-subprotocol-refuses-the-upgrade")
	plugin := NewServerPlugin(ServerConfig{})
	plugin.HandleStream("/chat", StreamHandler{
		Mode:   StreamModeBidirectional,
		Handle: func(*StreamContext) error { return nil },
	})
	server := httptest.NewServer(plugin.Handler())
	t.Cleanup(server.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	request := "GET /chat HTTP/1.1\r\nHost: " + strings.TrimPrefix(server.URL, "http://") +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13" +
		"\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Protocol: putnami.service.v1\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(status, "400") {
		t.Fatalf("status = %q, want 400 for an unspeakable subprotocol", strings.TrimSpace(status))
	}
}
