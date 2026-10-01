package client

import (
	"encoding/base64"
	stderrors "errors"
	"net/http"
	"strings"
	"testing"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// webSocketServerStreamOperation is the harness's declared server stream.
func webSocketServerStreamOperation(policy *clientcontract.ResiliencePolicy,
	declared []clientcontract.DeclaredError) Operation {
	message := stringObjectSchema("value")
	return webSocketTestOperation("watchItems", clientcontract.StreamServer, clientcontract.EncodingJSON, false,
		&clientcontract.MessageShapes{Output: &message}, policy, declared)
}

// TestWebSocketUpgradeCarriesNoPrivateHeader proves admission is in band: the
// opening handshake offers only the RFC 6455 negotiation headers, so a browser
// client — which cannot set a header at all — negotiates the same way.
func TestWebSocketUpgradeCarriesNoPrivateHeader(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.send(webSocketResultFrame(""))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	request := &Request{Headers: http.Header{"X-Correlation-Key": []string{"call-1"}}}
	stream, err := OpenServerStreamWS[map[string]string](t.Context(), bound, request, webSocketServerStreamOperation(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	header := server.upgradeHeader(t)
	for _, forbidden := range []string{
		"Authorization", "Cookie", "X-Client-Id", "X-Request-Id", "Traceparent", "Tracestate",
		"Baggage", "Proxy-Authorization", "X-Correlation-Key",
	} {
		if header.Get(forbidden) != "" {
			t.Errorf("opening handshake carries %s", forbidden)
		}
	}
	for name, values := range header {
		if strings.Contains(strings.Join(values, " "), "stream-token") {
			t.Errorf("opening handshake header %s carries credential material", name)
		}
	}
	if header.Get("Sec-WebSocket-Version") != "13" {
		t.Errorf("Sec-WebSocket-Version = %q", header.Get("Sec-WebSocket-Version"))
	}
	if header.Get("Sec-WebSocket-Protocol") != clientcontract.WebSocketSubprotocolV1 {
		t.Errorf("Sec-WebSocket-Protocol = %q", header.Get("Sec-WebSocket-Protocol"))
	}
	key, decodeErr := base64.StdEncoding.DecodeString(header.Get("Sec-WebSocket-Key"))
	if decodeErr != nil || len(key) != 16 {
		t.Errorf("Sec-WebSocket-Key = %q", header.Get("Sec-WebSocket-Key"))
	}
	server.assertNoFailures(t)
}

// TestWebSocketDialRefusesAnUnprovenSwitch keeps a socket the provider did not
// prove it negotiated from ever carrying a first-party frame.
func TestWebSocketDialRefusesAnUnprovenSwitch(t *testing.T) {
	tests := []struct {
		name    string
		options wsTestOptions
	}{
		{name: "accept-does-not-prove-the-key", options: wsTestOptions{corruptAccept: true}},
		{name: "subprotocol-is-absent", options: wsTestOptions{subprotocol: "none"}},
		{name: "subprotocol-is-another-token", options: wsTestOptions{subprotocol: "chat"}},
		{name: "extension-was-never-offered", options: wsTestOptions{extensions: "permessage-deflate"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.options.play = func(peer *wsTestPeer) { peer.drain() }
			server := newWSTestServer(t, test.options)
			bound := webSocketTestClient(t, server.URL)
			_, err := OpenServerStreamWS[map[string]string](t.Context(), bound, &Request{},
				webSocketServerStreamOperation(nil, nil))
			if err == nil || !perrors.Is(err, CodeClientResponse) {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

// TestWebSocketDialProjectsAnOrdinaryHTTPRefusal keeps a provider that answers
// the upgrade with a plain response inside the operation's typed errors, and
// invalidates the credential the refusal names.
func TestWebSocketDialProjectsAnOrdinaryHTTPRefusal(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{
		rejectStatus: http.StatusUnauthorized,
		rejectBody:   `{"code":"unauthorized","message":"expired"}`,
	})
	bound := webSocketTestClient(t, server.URL)
	declared := []clientcontract.DeclaredError{{Status: http.StatusUnauthorized, Code: "unauthorized"}}
	_, err := OpenServerStreamWS[map[string]string](t.Context(), bound, &Request{},
		webSocketServerStreamOperation(nil, declared))
	var remote *RemoteError
	if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized || remote.RemoteCode != "unauthorized" {
		t.Fatalf("error = %T %v", err, err)
	}
	if remote.ServiceID != "inventory" || remote.OperationID != "watchItems" {
		t.Fatalf("remote error identity = %q %q", remote.ServiceID, remote.OperationID)
	}
}

// TestWebSocketDialHonorsTheHandshakeBudget bounds connect-to-admission on its
// own declared budget rather than on the operation's duration.
func TestWebSocketDialHonorsTheHandshakeBudget(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		<-blocked
	}})
	bound := webSocketTestClient(t, server.URL)
	policy := &clientcontract.ResiliencePolicy{
		TimeoutMs: intPointer(60000),
		Stream:    &clientcontract.StreamPolicy{HandshakeTimeoutMs: intPointer(40)},
	}
	_, err := OpenServerStreamWS[map[string]string](t.Context(), bound, &Request{},
		webSocketServerStreamOperation(policy, nil))
	if err == nil || !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("error = %T %v", err, err)
	}
}

// TestResolveWebSocketBudgetsReadsTheDeclaredHandshakeBudget pins the one field
// the shared projection still defaults, and keeps the default when the provider
// stays silent.
func TestResolveWebSocketBudgetsReadsTheDeclaredHandshakeBudget(t *testing.T) {
	silent := resolveWebSocketBudgets(nil, &clientcontract.ResiliencePolicy{AttemptTimeoutMs: intPointer(1500)})
	if silent.Handshake.Milliseconds() != 1500 {
		t.Fatalf("handshake budget without a declared field = %v", silent.Handshake)
	}
	declared := resolveWebSocketBudgets(nil, &clientcontract.ResiliencePolicy{
		AttemptTimeoutMs: intPointer(1500),
		Stream:           &clientcontract.StreamPolicy{HandshakeTimeoutMs: intPointer(200)},
	})
	if declared.Handshake.Milliseconds() != 200 {
		t.Fatalf("declared handshake budget = %v", declared.Handshake)
	}
	capped := resolveWebSocketBudgets(nil, &clientcontract.ResiliencePolicy{
		TimeoutMs: intPointer(100),
		Stream:    &clientcontract.StreamPolicy{HandshakeTimeoutMs: intPointer(5000)},
	})
	if capped.Handshake.Milliseconds() != 100 {
		t.Fatalf("handshake budget past the declared duration = %v", capped.Handshake)
	}
}
