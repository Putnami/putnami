package http

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// upgradeAdmittedRoute declares a wire that admits on the upgrade: its chain
// refuses a request without the right key, and its Serve writes one binary
// message carrying the key it was admitted with.
func upgradeAdmittedRoute(t *testing.T, subprotocol string) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	served := &atomic.Bool{}
	plugin := NewServerPlugin(ServerConfig{})
	plugin.HandleStream("/tunnel", StreamHandler{
		Mode:           StreamModeBidirectional,
		Subprotocol:    subprotocol,
		AdmitOnUpgrade: true,
		Before: func(ctx *Context) *Response {
			if ctx.Header("X-Tunnel-Key") != "granted" {
				return JSONStatus(http.StatusUnauthorized, map[string]string{"code": "unauthorized"})
			}
			return nil
		},
		Handle: func(*StreamContext) error { return nil },
		Serve: func(ctx *Context, conn *WebSocketConn) error {
			served.Store(true)
			return conn.WriteBinary([]byte{0x00, 0xff, byte(len(ctx.Header("X-Tunnel-Key")))})
		},
	})
	server := httptest.NewServer(plugin.Handler())
	t.Cleanup(server.Close)
	return server, served
}

// upgradeTunnel writes one hand-made upgrade request and returns the status
// line, the response headers and a reader positioned after them.
func upgradeTunnel(t *testing.T, server *httptest.Server, header string) (string, http.Header, *bufio.Reader) {
	t.Helper()
	host := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	request := "GET /tunnel HTTP/1.1\r\nHost: " + host +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13" +
		"\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" + header + "\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	headers := http.Header{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		name, value, _ := strings.Cut(line, ":")
		headers.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	return strings.TrimSpace(status), headers, reader
}

func TestAWireThatAdmitsOnTheUpgradeRunsTheChainOnTheUpgradeRequest(t *testing.T) {
	spectest.Proves(t, "go/http-services", "upgrade-admitted-wires", "the-chain-runs-on-the-upgrade-request")
	server, served := upgradeAdmittedRoute(t, "acme.tunnel.v1")

	status, _, _ := upgradeTunnel(t, server, "Sec-WebSocket-Protocol: acme.tunnel.v1\r\n")
	if !strings.Contains(status, "401") || served.Load() {
		t.Fatalf("an upgrade without the key: status %q, served %v; want 401 before any socket", status, served.Load())
	}

	status, headers, reader := upgradeTunnel(t, server,
		"Sec-WebSocket-Protocol: acme.tunnel.v1\r\nX-Tunnel-Key: granted\r\n")
	if !strings.Contains(status, "101") {
		t.Fatalf("an admitted upgrade answered %q", status)
	}
	if got := headers.Get("Sec-WebSocket-Protocol"); got != "acme.tunnel.v1" {
		t.Fatalf("negotiated subprotocol = %q, want the declared token", got)
	}
	opcode, payload := readServerFrameFrom(t, reader)
	if opcode != wsOpcodeBinary || string(payload) != string([]byte{0x00, 0xff, byte(len("granted"))}) {
		t.Fatalf("Serve wrote opcode %#x payload %v, want the binary message it was admitted with", opcode, payload)
	}
}

func TestAWireThatAdmitsOnTheUpgradeSpeaksExactlyItsDeclaredToken(t *testing.T) {
	spectest.Proves(t, "go/http-services", "upgrade-admitted-wires", "the-route-speaks-exactly-its-declared-token")
	declared, _ := upgradeAdmittedRoute(t, "acme.tunnel.v1")
	for name, offer := range map[string]string{
		"no offer":             "",
		"the first-party wire": "Sec-WebSocket-Protocol: putnami.service.v1\r\n",
		"another vocabulary":   "Sec-WebSocket-Protocol: acme.tunnel.v2\r\n",
	} {
		status, _, _ := upgradeTunnel(t, declared, offer+"X-Tunnel-Key: granted\r\n")
		if !strings.Contains(status, "400") {
			t.Fatalf("%s on a route that declares acme.tunnel.v1: %q, want 400", name, status)
		}
	}
	// Several offers are allowed; the declared one is selected.
	status, headers, _ := upgradeTunnel(t, declared,
		"Sec-WebSocket-Protocol: acme.tunnel.v2, acme.tunnel.v1\r\nX-Tunnel-Key: granted\r\n")
	if !strings.Contains(status, "101") || headers.Get("Sec-WebSocket-Protocol") != "acme.tunnel.v1" {
		t.Fatalf("an offer list that names the declared token: %q %v", status, headers)
	}

	undeclared, _ := upgradeAdmittedRoute(t, "")
	status, _, _ = upgradeTunnel(t, undeclared, "Sec-WebSocket-Protocol: acme.tunnel.v1\r\nX-Tunnel-Key: granted\r\n")
	if !strings.Contains(status, "400") {
		t.Fatalf("an offer on a route that declares no token: %q, want 400", status)
	}
	status, headers, reader := upgradeTunnel(t, undeclared, "X-Tunnel-Key: granted\r\n")
	if !strings.Contains(status, "101") || headers.Get("Sec-WebSocket-Protocol") != "" {
		t.Fatalf("no offer on a route that declares no token: %q %v", status, headers)
	}
	if opcode, _ := readServerFrameFrom(t, reader); opcode != wsOpcodeBinary {
		t.Fatalf("the admitted socket was not handed to Serve: opcode %#x", opcode)
	}

	// A plain GET is not an event stream on a wire route, whatever it accepts.
	response, err := http.Get(declared.URL + "/tunnel")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a plain GET answered %d", response.StatusCode)
	}
}

func TestABinaryMessageIsWrittenUnderTheMessageBound(t *testing.T) {
	spectest.Proves(t, "go/http-services", "upgrade-admitted-wires", "a-binary-message-is-written-under-the-message-bound")
	conn, client := newTestWebSocketConn(t, 4)
	done := make(chan error, 1)
	go func() { done <- conn.WriteBinary([]byte{1, 2, 3, 4}) }()
	fin, opcode, payload := readServerFrame(t, client)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !fin || opcode != wsOpcodeBinary || string(payload) != "\x01\x02\x03\x04" {
		t.Fatalf("binary frame = fin %v opcode %#x payload %v", fin, opcode, payload)
	}
	var protocolErr *WebSocketProtocolError
	if err := conn.WriteBinary([]byte{1, 2, 3, 4, 5}); !errors.As(err, &protocolErr) || protocolErr.CloseCode != WebSocketCloseMessageTooBig {
		t.Fatalf("an oversized binary message: %v, want the message-too-big refusal", err)
	}
}

// readServerFrameFrom reads one unmasked, unfragmented server frame from a
// reader that has already consumed the handshake response.
func readServerFrameFrom(t *testing.T, reader *bufio.Reader) (byte, []byte) {
	t.Helper()
	header := make([]byte, 2)
	if err := readFull(reader, header); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	length := int(header[1] & 0x7f)
	if length >= 126 {
		t.Fatalf("test frames are short; got length indicator %d", length)
	}
	payload := make([]byte, length)
	if err := readFull(reader, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if header[0]&0x80 == 0 {
		t.Fatal("the server fragmented a short frame")
	}
	return header[0] & 0x0f, payload
}
