package client

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// providerWireTestOperation declares one provider-owned wire: raw octets when
// bytes is true, JSON frames under acme.events.v1 otherwise.
func providerWireTestOperation(bytes bool, policy *clientcontract.ResiliencePolicy,
	declared []clientcontract.DeclaredError) Operation {
	transport := clientcontract.Transport{
		Protocol: clientcontract.TransportWebSocket, Path: "/tunnel", Encoding: clientcontract.EncodingBinary,
		WebSocket: &clientcontract.WebSocketTransport{Wire: clientcontract.WebSocketWireProvider},
	}
	var shapes *clientcontract.MessageShapes
	if !bytes {
		transport.Encoding = clientcontract.EncodingJSON
		transport.WebSocket.Subprotocol = "acme.events.v1"
		in, out := stringObjectSchema("type"), stringObjectSchema("type", "data")
		in.Required, out.Required = []string{"type"}, []string{"type"}
		shapes = &clientcontract.MessageShapes{Input: &in, Output: &out}
	}
	if declared == nil {
		declared = []clientcontract.DeclaredError{}
	}
	return Operation{
		ID: "openTunnel",
		Contract: clientcontract.OperationV1{
			Stream:     clientcontract.StreamBidirectional,
			Messages:   shapes,
			Transports: []clientcontract.Transport{transport},
			Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
				AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}},
			}}},
			Errors:      declared,
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent},
			Resilience:  policy,
		},
	}
}

type tunnelEvent struct {
	Type string  `json:"type"`
	Data *string `json:"data,omitempty"`
}

type tunnelCommand struct {
	Type string `json:"type"`
}

func streamPolicy(stream clientcontract.StreamPolicy) *clientcontract.ResiliencePolicy {
	return &clientcontract.ResiliencePolicy{Stream: &stream}
}

// readClose reads until the client's close frame and returns its code.
func (peer *wsTestPeer) readClose() int {
	for {
		_, _, err := peer.conn.readMessage()
		var closed *wsCloseError
		if stderrors.As(err, &closed) {
			return closed.Code
		}
		if err != nil {
			return 0
		}
	}
}

func TestAByteStreamIsAReadWriteCloserAdmittedOnTheUpgrade(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "provider-owned-websocket-wires",
		"a-byte-stream-is-a-read-write-closer-admitted-on-the-upgrade")
	var sizes []int
	var mu sync.Mutex
	closeCode := make(chan int, 1)
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		for {
			opcode, payload, err := peer.conn.readMessage()
			var closed *wsCloseError
			if stderrors.As(err, &closed) {
				closeCode <- closed.Code
				return
			}
			if err != nil || opcode != wsOpcodeBinary {
				peer.server.fail("the provider read something other than a binary message")
				return
			}
			mu.Lock()
			sizes = append(sizes, len(payload))
			mu.Unlock()
			if err := peer.conn.writeBinary(payload); err != nil {
				return
			}
		}
	}})
	operation := providerWireTestOperation(true, streamPolicy(clientcontract.StreamPolicy{MaxFrameBytes: int64Pointer(64)}), nil)
	stream, err := OpenByteStream(context.Background(), webSocketTestClient(t, server.URL),
		&Request{QueryValues: map[string][]string{"database": {"main"}}}, operation)
	if err != nil {
		t.Fatalf("OpenByteStream: %v", err)
	}
	var tunnel io.ReadWriteCloser = stream
	payload := bytes.Repeat([]byte{0x00, 0xff, 0x80}, 100)
	if n, err := tunnel.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("write = %d, %v", n, err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(tunnel, echoed); err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("echo = %v, %v", echoed, err)
	}
	if err := tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	if code := <-closeCode; code != wsCloseNormal {
		t.Fatalf("the provider read close code %d, want 1000", code)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("a caller close ended with %v", err)
	}
	if _, err := tunnel.Write([]byte{1}); err == nil {
		t.Fatal("a closed byte stream accepted a write")
	}
	if _, err := tunnel.Read(make([]byte, 1)); err == nil || err == io.EOF {
		t.Fatalf("a closed byte stream read %v, want the closed error", err)
	}
	server.waitForPlay(t)
	server.assertNoFailures(t)

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, size := range sizes {
		if size > 64 {
			t.Fatalf("a binary message of %d octets passed the 64-octet frame budget", size)
		}
		total += size
	}
	if total != len(payload) || len(sizes) < 5 {
		t.Fatalf("the provider read %d octets in %d messages", total, len(sizes))
	}
	header := server.upgradeHeader(t)
	if header.Get("Authorization") != "Bearer stream-token" || header.Get("X-Client-Id") != "consumer.workload" {
		t.Fatalf("the upgrade did not carry the declared credential and identity: %v", header)
	}
	if header.Get("Sec-WebSocket-Protocol") != "" {
		t.Fatalf("a byte stream with no declared token offered %q", header.Get("Sec-WebSocket-Protocol"))
	}
}

func TestAFrameStreamCarriesValidatedJSONFramesUnderTheDeclaredSubprotocol(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "provider-owned-websocket-wires",
		"a-frame-stream-carries-validated-json-frames-under-the-declared-subprotocol")
	closeCode := make(chan int, 1)
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		opcode, payload, err := peer.conn.readMessage()
		if err != nil || opcode != wsOpcodeText || string(payload) != `{"type":"subscribe"}` {
			peer.server.fail("the provider read " + string(payload) + ", not the bare frame")
			return
		}
		_ = peer.conn.writeText([]byte(`{"type":"event","data":"orders-1"}`))
		// A property the declared schema does not name is dropped.
		_ = peer.conn.writeText([]byte(`{"type":"event","data":"orders-2","addedLater":true}`))
		// A frame the declared schema refuses ends the stream.
		_ = peer.conn.writeText([]byte(`{"type":"event","data":42}`))
		closeCode <- peer.readClose()
	}})
	stream, err := OpenFrameStream[tunnelCommand, tunnelEvent](context.Background(),
		webSocketTestClient(t, server.URL), &Request{}, providerWireTestOperation(false, nil, nil))
	if err != nil {
		t.Fatalf("OpenFrameStream: %v", err)
	}
	if got := server.upgradeHeader(t).Get("Sec-WebSocket-Protocol"); got != "acme.events.v1" {
		t.Fatalf("offered subprotocol = %q", got)
	}
	if err := stream.Send(context.Background(), tunnelCommand{Type: "subscribe"}); err != nil {
		t.Fatal(err)
	}
	frame, err := stream.Recv(context.Background())
	if err != nil || frame.Type != "event" || frame.Data == nil || *frame.Data != "orders-1" {
		t.Fatalf("frame = %+v, %v", frame, err)
	}
	frame, err = stream.Recv(context.Background())
	if err != nil || frame.Data == nil || *frame.Data != "orders-2" {
		t.Fatalf("a frame carrying an added property = %+v, %v", frame, err)
	}
	if _, err := stream.Recv(context.Background()); !errors.Is(err, CodeClientResponse) {
		t.Fatalf("a frame outside the declared schema read %v", err)
	}
	if code := <-closeCode; code != wsCloseInvalidPayload {
		t.Fatalf("the provider read close code %d, want %d", code, wsCloseInvalidPayload)
	}
	<-stream.Done()
	if err := stream.Send(context.Background(), tunnelCommand{Type: "late"}); err == nil {
		t.Fatal("an ended frame stream accepted a send")
	}
	server.waitForPlay(t)
	server.assertNoFailures(t)
}

func TestAProviderOwnedWireEndsOnItsCloseCodeAndARefusalIsTyped(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "provider-owned-websocket-wires",
		"a-provider-owned-wire-ends-on-its-close-code-and-a-refusal-is-typed")
	t.Run("a normal provider close is the end of the stream", func(t *testing.T) {
		server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
			_ = peer.conn.writeBinary([]byte("bye"))
			_ = peer.conn.writeClose(wsCloseNormal, "")
		}})
		stream, err := OpenByteStream(context.Background(), webSocketTestClient(t, server.URL), &Request{},
			providerWireTestOperation(true, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(stream)
		if err != nil || string(content) != "bye" {
			t.Fatalf("read %q, %v; want the octets then io.EOF", content, err)
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("a normal provider close ended with %v", err)
		}
	})
	t.Run("any other close is a typed terminal naming the code", func(t *testing.T) {
		server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
			_ = peer.conn.writeClose(wsCloseInternalError, "provider failed")
		}})
		stream, err := OpenByteStream(context.Background(), webSocketTestClient(t, server.URL), &Request{},
			providerWireTestOperation(true, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Read(make([]byte, 8)); !errors.Is(err, CodeClientResponse) || !strings.Contains(err.Error(), "1011") {
			t.Fatalf("read %v, want the typed close code", err)
		}
	})
	t.Run("a message of the wrong kind is refused", func(t *testing.T) {
		closeCode := make(chan int, 1)
		server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
			_ = peer.conn.writeBinary([]byte{0x01})
			closeCode <- peer.readClose()
		}})
		stream, err := OpenFrameStream[tunnelCommand, tunnelEvent](context.Background(),
			webSocketTestClient(t, server.URL), &Request{}, providerWireTestOperation(false, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(context.Background()); !errors.Is(err, CodeClientResponse) {
			t.Fatalf("a binary message on a typed wire read %v", err)
		}
		if code := <-closeCode; code != wsCloseUnsupportedData {
			t.Fatalf("close code = %d, want %d", code, wsCloseUnsupportedData)
		}
	})
	t.Run("an upgrade refusal is the declared typed error", func(t *testing.T) {
		server := newWSTestServer(t, wsTestOptions{
			rejectStatus: http.StatusForbidden,
			rejectBody:   `{"error":"Forbidden","code":"forbidden","message":"tunnel is closed"}`,
		})
		operation := providerWireTestOperation(true, nil, []clientcontract.DeclaredError{{Status: 403, Code: "forbidden"}})
		_, err := OpenByteStream(context.Background(), webSocketTestClient(t, server.URL), &Request{}, operation)
		var remote *RemoteError
		if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusForbidden || remote.RemoteCode != "forbidden" {
			t.Fatalf("a refused upgrade returned %v, want the declared 403", err)
		}
	})
	t.Run("a provider that does not echo the declared token is refused", func(t *testing.T) {
		server := newWSTestServer(t, wsTestOptions{subprotocol: "none"})
		_, err := OpenFrameStream[tunnelCommand, tunnelEvent](context.Background(),
			webSocketTestClient(t, server.URL), &Request{}, providerWireTestOperation(false, nil, nil))
		if !errors.Is(err, CodeClientResponse) || !strings.Contains(err.Error(), "declared websocket subprotocol") {
			t.Fatalf("an unnegotiated wire returned %v", err)
		}
	})
	t.Run("the wrong entry point is refused before any byte", func(t *testing.T) {
		server := newWSTestServer(t, wsTestOptions{})
		bound := webSocketTestClient(t, server.URL)
		if _, err := OpenFrameStream[tunnelCommand, tunnelEvent](context.Background(), bound, &Request{},
			providerWireTestOperation(true, nil, nil)); !errors.Is(err, CodeClientConfig) {
			t.Fatalf("a byte stream opened as frames: %v", err)
		}
		if _, err := OpenByteStream(context.Background(), bound, &Request{},
			providerWireTestOperation(false, nil, nil)); !errors.Is(err, CodeClientConfig) {
			t.Fatalf("a frame stream opened as bytes: %v", err)
		}
		firstParty := webSocketTestOperation("chat", clientcontract.StreamBidirectional, clientcontract.EncodingJSON, false,
			&clientcontract.MessageShapes{Input: &clientcontract.Schema{Type: "string"}, Output: &clientcontract.Schema{Type: "string"}}, nil, nil)
		if _, err := OpenByteStream(context.Background(), bound, &Request{}, firstParty); !errors.Is(err, CodeClientConfig) {
			t.Fatalf("a first-party conversation opened as bytes: %v", err)
		}
		if server.upgradeCount() != 0 {
			t.Fatal("a refused entry point reached the provider")
		}
	})
}

func TestTheDeclaredBudgetsBoundAProviderOwnedWire(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "provider-owned-websocket-wires",
		"the-declared-budgets-bound-a-provider-owned-wire")
	t.Run("a silent provider trips the idle budget", func(t *testing.T) {
		release := make(chan struct{})
		server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
			<-release
		}})
		defer close(release)
		idle := 150
		stream, err := OpenByteStream(context.Background(), webSocketTestClient(t, server.URL), &Request{},
			providerWireTestOperation(true, streamPolicy(clientcontract.StreamPolicy{IdleTimeoutMs: &idle}), nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Read(make([]byte, 1)); !errors.Is(err, CodeClientDeadline) {
			t.Fatalf("a silent provider read %v, want the idle deadline", err)
		}
	})
	t.Run("the declared heartbeat keeps a quiet wire alive", func(t *testing.T) {
		server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					// Reading answers the client's pings with pongs.
					if _, _, err := peer.conn.readMessage(); err != nil {
						return
					}
				}
			}()
			time.Sleep(450 * time.Millisecond)
			_ = peer.conn.writeBinary([]byte("alive"))
			<-done
		}})
		idle, heartbeat := 200, 40
		stream, err := OpenByteStream(context.Background(), webSocketTestClient(t, server.URL), &Request{},
			providerWireTestOperation(true, streamPolicy(clientcontract.StreamPolicy{IdleTimeoutMs: &idle, HeartbeatMs: &heartbeat}), nil))
		if err != nil {
			t.Fatal(err)
		}
		content := make([]byte, 5)
		if _, err := io.ReadFull(stream, content); err != nil || string(content) != "alive" {
			t.Fatalf("read %q, %v; the heartbeat did not keep the wire alive", content, err)
		}
		_ = stream.Close()
	})
	t.Run("the caller's context bounds the stream", func(t *testing.T) {
		release := make(chan struct{})
		server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
			<-release
		}})
		defer close(release)
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := OpenByteStream(ctx, webSocketTestClient(t, server.URL), &Request{},
			providerWireTestOperation(true, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		if _, err := stream.Read(make([]byte, 1)); !errors.Is(err, CodeClientCanceled) {
			t.Fatalf("a canceled caller read %v", err)
		}
		<-stream.Done()
	})
}
