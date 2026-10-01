package api

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

// tunnelQuery is the target a byte stream opens, carried on the upgrade.
type tunnelQuery struct {
	Database string `json:"database" validate:"required"`
}

// eventClientFrame and eventServerFrame are a provider-owned vocabulary: the
// framework carries them as JSON values and never wraps them.
type eventClientFrame struct {
	Type  string `json:"type"`
	Topic string `json:"topic,omitempty"`
}

type eventServerFrame struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

// requireTunnelKey is the endpoint's own security chain: it reads the upgrade
// request, which is the admission of a provider-owned wire.
func requireTunnelKey(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
	if ctx.Request.Header.Get("X-Tunnel-Key") != "granted" {
		return phttp.ErrorResponse(perrors.Unauthorized("tunnel key is required"))
	}
	return next()
}

func providerWireProvider(t *testing.T) *wsProvider {
	t.Helper()
	return newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(Endpoint("GET", "/tunnel").
			Query(Type[tunnelQuery]()).
			Body(ByteStream()).
			Returns(ByteStream()).
			Use(requireTunnelKey).
			Handle(ByteTunnel(func(ctx *ByteStreamContext) error {
				if _, err := ctx.Write([]byte("db=" + ctx.QueryParams().Get("database"))); err != nil {
					return err
				}
				_, err := io.Copy(ctx, ctx)
				return err
			})))
		plugin.Register(Endpoint("GET", "/events").
			Body(StreamOf[eventClientFrame]()).
			Returns(StreamOf[eventServerFrame]()).
			Subprotocol("acme.events.v1").
			Handle(BidiStream(func(stream *BidiStreamContext[eventClientFrame, eventServerFrame]) error {
				for frame := range stream.Messages() {
					if frame.Type == "fail" {
						return perrors.NotFound("topic is unknown")
					}
					if err := stream.Send(eventServerFrame{Type: "event", Data: frame.Topic}); err != nil {
						return err
					}
				}
				return stream.Err()
			})))
	}})
}

// dialProviderWire performs a hand-made opening handshake that carries header
// on the upgrade request, the way a provider-owned wire admits.
func dialProviderWire(t *testing.T, provider *wsProvider, path, subprotocol string, header http.Header) (*wsPeer, *http.Response) {
	t.Helper()
	target, err := url.Parse(provider.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	var request strings.Builder
	fmt.Fprintf(&request, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n", target.RequestURI(), target.Host)
	fmt.Fprintf(&request, "Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n", key)
	if subprotocol != "" {
		fmt.Fprintf(&request, "Sec-WebSocket-Protocol: %s\r\n", subprotocol)
	}
	for name, values := range header {
		for _, value := range values {
			fmt.Fprintf(&request, "%s: %s\r\n", name, value)
		}
	}
	if _, err := conn.Write([]byte(request.String() + "\r\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusSwitchingProtocols && response.Header.Get("Sec-WebSocket-Accept") != wsExpectedAccept(key) {
		t.Fatalf("accept token = %q", response.Header.Get("Sec-WebSocket-Accept"))
	}
	return &wsPeer{t: t, conn: conn, rw: bufio.NewReadWriter(reader, bufio.NewWriter(conn))}, response
}

func (peer *wsPeer) readData(t *testing.T) (byte, []byte) {
	t.Helper()
	peer.deadline()
	opcode, payload, err := peer.readMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	return opcode, payload
}

func (peer *wsPeer) write(t *testing.T, opcode byte, payload []byte) {
	t.Helper()
	peer.deadline()
	if err := peer.writeFrame(true, opcode, payload); err != nil {
		t.Fatalf("write message: %v", err)
	}
}

func granted() http.Header { return http.Header{"X-Tunnel-Key": {"granted"}} }

// expectCloseEcho reads the provider's answer to a normal client close. The
// framework echoes the close frame without a payload (RFC 6455 section 5.5.1),
// which a reader sees as 1005, no status: anything else would be the provider
// ending the stream on its own terms.
func expectCloseEcho(t *testing.T, peer *wsPeer) {
	t.Helper()
	if code := peer.expectClose(); code != phttp.WebSocketCloseNormal && code != 1005 {
		t.Fatalf("close echo = %d, want the bare echo of a normal close", code)
	}
}

func TestAByteStreamCarriesRawOctetsBothWaysAfterAnUpgradeAdmission(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "provider-owned-websocket-wires",
		"a-byte-stream-carries-raw-octets-both-ways-after-an-upgrade-admission")
	provider := providerWireProvider(t)

	t.Run("the endpoint chain refuses an upgrade without the credential", func(t *testing.T) {
		_, response := dialProviderWire(t, provider, "/tunnel?database=main", "", nil)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 on the upgrade itself", response.StatusCode)
		}
	})
	t.Run("the endpoint validation refuses an upgrade without the target", func(t *testing.T) {
		_, response := dialProviderWire(t, provider, "/tunnel", "", granted())
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for a missing declared query parameter", response.StatusCode)
		}
	})
	t.Run("a subprotocol the tunnel does not declare is refused", func(t *testing.T) {
		_, response := dialProviderWire(t, provider, "/tunnel?database=main", "putnami.service.v1", granted())
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", response.StatusCode)
		}
	})
	t.Run("octets travel unchanged in binary messages", func(t *testing.T) {
		peer, response := dialProviderWire(t, provider, "/tunnel?database=main", "", granted())
		if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Sec-WebSocket-Protocol") != "" {
			t.Fatalf("handshake = %d %q, want 101 with no subprotocol", response.StatusCode, response.Header.Get("Sec-WebSocket-Protocol"))
		}
		if opcode, payload := peer.readData(t); opcode != 0x2 || string(payload) != "db=main" {
			t.Fatalf("first message = %#x %q, want the binary greeting", opcode, payload)
		}
		octets := []byte{0x00, 0xff, 0xfe, 0x80, 0x22, 0x5c, 0x0a}
		peer.write(t, 0x2, octets)
		if opcode, payload := peer.readData(t); opcode != 0x2 || string(payload) != string(octets) {
			t.Fatalf("echo = %#x %v, want %v", opcode, payload, octets)
		}
		// A normal close ends the client direction: the handler reads io.EOF,
		// returns, and the provider answers with its close echo alone.
		peer.write(t, 0x8, []byte{0x03, 0xe8})
		expectCloseEcho(t, peer)
	})
	t.Run("a text message on a byte stream is refused", func(t *testing.T) {
		peer, _ := dialProviderWire(t, provider, "/tunnel?database=main", "", granted())
		peer.readData(t)
		peer.write(t, 0x1, []byte(`{"type":"not-bytes"}`))
		if code := peer.expectClose(); code != phttp.WebSocketCloseUnsupportedData {
			t.Fatalf("close code = %d, want %d", code, phttp.WebSocketCloseUnsupportedData)
		}
	})
}

func TestATypedProviderWireCarriesJSONFramesUnderItsDeclaredSubprotocol(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "provider-owned-websocket-wires",
		"a-typed-provider-wire-carries-json-frames-under-its-declared-subprotocol")
	provider := providerWireProvider(t)

	for name, offer := range map[string]string{"no token": "", "the first-party token": "putnami.service.v1", "another token": "acme.events.v2"} {
		t.Run(name+" is refused", func(t *testing.T) {
			_, response := dialProviderWire(t, provider, "/events", offer, nil)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", response.StatusCode)
			}
		})
	}
	t.Run("frames are the declared JSON values with no envelope", func(t *testing.T) {
		peer, response := dialProviderWire(t, provider, "/events", "acme.events.v1", nil)
		if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Sec-WebSocket-Protocol") != "acme.events.v1" {
			t.Fatalf("handshake = %d %q", response.StatusCode, response.Header.Get("Sec-WebSocket-Protocol"))
		}
		peer.write(t, 0x1, []byte(`{"type":"subscribe","topic":"orders"}`))
		if opcode, payload := peer.readData(t); opcode != 0x1 || string(payload) != `{"type":"event","data":"orders"}` {
			t.Fatalf("frame = %#x %s, want the provider's own JSON value", opcode, payload)
		}
		peer.write(t, 0x8, []byte{0x03, 0xe8})
		expectCloseEcho(t, peer)
	})
	t.Run("a binary message on a typed wire is refused", func(t *testing.T) {
		peer, _ := dialProviderWire(t, provider, "/events", "acme.events.v1", nil)
		peer.write(t, 0x2, []byte{0x01})
		if code := peer.expectClose(); code != phttp.WebSocketCloseUnsupportedData {
			t.Fatalf("close code = %d, want %d", code, phttp.WebSocketCloseUnsupportedData)
		}
	})
	t.Run("a frame that is not a JSON value is refused", func(t *testing.T) {
		peer, _ := dialProviderWire(t, provider, "/events", "acme.events.v1", nil)
		peer.write(t, 0x1, []byte(`{"type":`))
		if code := peer.expectClose(); code != phttp.WebSocketCloseInvalidPayload {
			t.Fatalf("close code = %d, want %d", code, phttp.WebSocketCloseInvalidPayload)
		}
	})
	t.Run("a handler error closes with the code its status maps to", func(t *testing.T) {
		peer, _ := dialProviderWire(t, provider, "/events", "acme.events.v1", nil)
		peer.write(t, 0x1, []byte(`{"type":"fail"}`))
		if code := peer.expectClose(); code != phttp.WebSocketClosePolicyViolation {
			t.Fatalf("close code = %d, want %d for a 4xx handler error", code, phttp.WebSocketClosePolicyViolation)
		}
	})
	t.Run("a server shutdown closes with going away", func(t *testing.T) {
		peer, _ := dialProviderWire(t, provider, "/events", "acme.events.v1", nil)
		peer.write(t, 0x1, []byte(`{"type":"subscribe","topic":"first"}`))
		peer.readData(t)
		provider.stop(t)
		if code := peer.expectClose(); code != phttp.WebSocketCloseGoingAway {
			t.Fatalf("close code = %d, want %d", code, phttp.WebSocketCloseGoingAway)
		}
	})
}

func TestAProviderOwnedWireThatCannotBePublishedIsRefusedAtRegistration(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "provider-owned-websocket-wires",
		"a-provider-owned-wire-that-cannot-be-published-is-refused-at-registration")
	echo := ByteTunnel(func(*ByteStreamContext) error { return nil })
	typed := BidiStream(func(*BidiStreamContext[eventClientFrame, eventServerFrame]) error { return nil })
	cases := map[string]func(){
		"a subprotocol on a unary endpoint": func() {
			Endpoint("GET", "/x").Returns(Type[eventServerFrame]()).Subprotocol("acme.v1").
				Handle(func(*phttp.EndpointContext) *phttp.Response { return nil })
		},
		"a subprotocol on a server stream": func() {
			Endpoint("GET", "/x").Returns(StreamOf[eventServerFrame]()).Subprotocol("acme.v1").
				Handle(ServerStream(func(*ServerStreamContext[eventServerFrame]) error { return nil }))
		},
		"a malformed token": func() {
			Endpoint("GET", "/x").Body(StreamOf[eventClientFrame]()).Returns(StreamOf[eventServerFrame]()).
				Subprotocol("acme v1").Handle(typed)
		},
		"a first-party token": func() {
			Endpoint("GET", "/x").Body(StreamOf[eventClientFrame]()).Returns(StreamOf[eventServerFrame]()).
				Subprotocol("putnami.service.v2").Handle(typed)
		},
		"octets one way only": func() {
			Endpoint("GET", "/x").Body(ByteStream()).Returns(StreamOf[eventServerFrame]()).Handle(echo)
		},
		"a byte handler on a typed wire": func() {
			Endpoint("GET", "/x").Body(StreamOf[eventClientFrame]()).Returns(StreamOf[eventServerFrame]()).
				Subprotocol("acme.v1").Handle(echo)
		},
		"a typed handler on a byte stream": func() {
			Endpoint("GET", "/x").Body(ByteStream()).Returns(ByteStream()).Handle(typed)
		},
		"a declared resume": func() {
			Endpoint("GET", "/x").Body(ByteStream()).Returns(ByteStream()).
				Client(ClientOperationOptions{Resume: true}).Handle(echo)
		},
	}
	for name, register := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s was accepted", name)
				}
			}()
			register()
		})
	}

	definition := Endpoint("GET", "/tunnel").Body(ByteStream()).Returns(ByteStream()).Subprotocol("pg.tunnel.v1").Handle(echo)
	if wire := definition.ProviderWire(); wire == nil || !wire.Bytes || wire.Subprotocol != "pg.tunnel.v1" {
		t.Fatalf("discovered wire = %+v", wire)
	}
	plugin := New(phttp.NewServerPlugin(phttp.ServerConfig{}))
	plugin.Register(definition)
	if err := plugin.Configure(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	routes := plugin.DiscoveredRoutes()
	if len(routes) != 1 || routes[0].ProviderWire == nil || !routes[0].ProviderWire.Bytes {
		t.Fatalf("discovered routes = %+v", routes)
	}
	if Endpoint("GET", "/watch").Returns(StreamOf[eventServerFrame]()).
		Handle(ServerStream(func(*ServerStreamContext[eventServerFrame]) error { return nil })).ProviderWire() != nil {
		t.Fatal("a first-party stream reads as a provider-owned wire")
	}
}
