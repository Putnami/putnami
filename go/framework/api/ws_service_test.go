package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// watchEndpoint declares the server stream used across these tests.
func watchEndpoint(handle func(*ServerStreamContext[wsEvent]) error) EndpointDefinition {
	return Endpoint("GET", "/watch").
		Returns(StreamOf[wsEvent]()).
		Handle(ServerStream(handle))
}

// uploadEndpoint declares the client stream used across these tests.
func uploadEndpoint(handle func(*ClientStreamContext[wsEvent, wsSummary]) error) EndpointDefinition {
	return Endpoint("GET", "/upload").
		Body(StreamOf[wsEvent]()).
		Returns(Type[wsSummary]()).
		Handle(ClientStream(handle))
}

// chatEndpoint declares the bidirectional stream used across these tests.
func chatEndpoint(handle func(*BidiStreamContext[wsEvent, wsEvent]) error) EndpointDefinition {
	return Endpoint("GET", "/chat").
		Body(StreamOf[wsEvent]()).
		Returns(StreamOf[wsEvent]()).
		Handle(BidiStream(handle))
}

// --- negotiation -----------------------------------------------------------

func TestWebSocketProvider_NegotiatesTheFirstPartySubprotocol(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "the-first-party-subprotocol-is-negotiated")
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(watchEndpoint(func(_ *ServerStreamContext[wsEvent]) error { return nil }))
	}})

	t.Run("echoes the offered first-party token", func(t *testing.T) {
		_, response, err := dialWSPeerRaw(t, provider, "/watch", clientcontract.WebSocketSubprotocolV1)
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("status = %d, want 101", response.StatusCode)
		}
		if got := response.Header.Get("Sec-WebSocket-Protocol"); got != clientcontract.WebSocketSubprotocolV1 {
			t.Fatalf("negotiated subprotocol = %q, want %q", got, clientcontract.WebSocketSubprotocolV1)
		}
	})

	t.Run("refuses a subprotocol it cannot speak", func(t *testing.T) {
		_, response, err := dialWSPeerRaw(t, provider, "/watch", "some.other.protocol")
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for an unspeakable subprotocol", response.StatusCode)
		}
	})

	t.Run("keeps the raw transport stream when no token is offered", func(t *testing.T) {
		_, response, err := dialWSPeerRaw(t, provider, "/watch", "")
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		if response.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("status = %d, want 101", response.StatusCode)
		}
		if got := response.Header.Get("Sec-WebSocket-Protocol"); got != "" {
			t.Fatalf("provider echoed %q for a client that offered nothing", got)
		}
	})
}

// --- admission -------------------------------------------------------------

func TestWebSocketProvider_RefusesAnApplicationMessageBeforeAdmission(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "an-application-message-before-admission-is-refused")
	ran := false
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(chatEndpoint(func(_ *BidiStreamContext[wsEvent, wsEvent]) error {
			ran = true
			return nil
		}))
	}})
	peer := dialWSPeer(t, provider, "/chat")
	peer.send(wsMessage(t, "1", event("too-early")))

	failure := peer.expectError()
	if failure.Code != clientcontract.ErrorCodeInvalidTransport {
		t.Fatalf("refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeInvalidTransport)
	}
	if failure.Status != http.StatusBadRequest {
		t.Fatalf("refusal status = %d, want 400", failure.Status)
	}
	if ran {
		t.Fatal("the endpoint handler ran before admission")
	}
	if code := peer.expectClose(); code != phttp.WebSocketClosePolicyViolation {
		t.Fatalf("close code = %d, want %d", code, phttp.WebSocketClosePolicyViolation)
	}
}

func TestWebSocketProvider_RunsTheEndpointSecurityChainBeforeAnyHandler(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "the-endpoint-security-chain-runs-before-any-handler")
	ran := false
	provider := newWSProvider(t, wsProviderOptions{
		credentials: map[string]clientcontract.CredentialProfile{
			"service": {Kind: clientcontract.CredentialServiceToken},
		},
		register: func(plugin *Plugin) {
			plugin.Register(Endpoint("GET", "/watch").
				Returns(StreamOf[wsEvent]()).
				Use(func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
					if ctx.Request.Header.Get("Authorization") != "Bearer harness-token" {
						return phttp.ErrorResponse(perrors.Unauthorized("service credential is required"))
					}
					return next()
				}).
				Handle(ServerStream(func(stream *ServerStreamContext[wsEvent]) error {
					ran = true
					return stream.Send(wsEvent{ID: "admitted", Payload: []byte{}})
				})))
		},
	})

	t.Run("refuses an unauthenticated admission with the endpoint's own status", func(t *testing.T) {
		peer := dialWSPeer(t, provider, "/watch")
		peer.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
			frame.Credentials = []clientcontract.WebSocketCredentialV1{}
		}))
		failure := peer.expectError()
		if failure.Status != http.StatusUnauthorized {
			t.Fatalf("refusal status = %d, want 401", failure.Status)
		}
		if failure.Code != string(perrors.CodeUnauthorized) {
			t.Fatalf("refusal code = %q, want %q", failure.Code, perrors.CodeUnauthorized)
		}
		if ran {
			t.Fatal("the endpoint handler ran for a refused admission")
		}
	})

	t.Run("admits once the reconstructed credential satisfies the chain", func(t *testing.T) {
		peer := dialWSPeer(t, provider, "/watch")
		peer.send(wsInit("getWatch"))
		peer.expect(clientcontract.WebSocketFrameReady)
		message, ok := peer.expect(clientcontract.WebSocketFrameMessage).(*clientcontract.WebSocketMessageFrameV1)
		if !ok {
			t.Fatal("provider did not deliver a message frame")
		}
		if message.Sequence != "1" {
			t.Fatalf("first provider sequence = %q, want 1", message.Sequence)
		}
		event := wsDecode[wsEvent](t, message.Payload)
		if event.ID != "admitted" {
			t.Fatalf("delivered event = %#v", event)
		}
		if event.Payload == nil || len(event.Payload) != 0 {
			t.Fatalf("empty bytes were delivered as %#v, want an empty non-nil slice", event.Payload)
		}
		peer.expect(clientcontract.WebSocketFrameResult)
		if !ran {
			t.Fatal("the endpoint handler never ran")
		}
	})
}

func TestWebSocketProvider_ReconstructsDeclaredHeadersFromTheInitFrame(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "declared-credentials-and-headers-are-reconstructed-from-the-init-frame")
	var seen http.Header
	var clientID string
	provider := newWSProvider(t, wsProviderOptions{
		credentials: map[string]clientcontract.CredentialProfile{
			"service": {Kind: clientcontract.CredentialServiceToken},
			"tenant":  {Kind: clientcontract.CredentialAPIKey, Header: "X-Tenant-Key"},
		},
		register: func(plugin *Plugin) {
			plugin.Register(Endpoint("GET", "/watch").
				Returns(StreamOf[wsEvent]()).
				Use(func(ctx *phttp.Context, next func() *phttp.Response) *phttp.Response {
					seen = ctx.Request.Header.Clone()
					clientID = ctx.Request.Header.Get("X-Client-Id")
					return next()
				}).
				Handle(ServerStream(func(_ *ServerStreamContext[wsEvent]) error { return nil })))
		},
	})

	t.Run("declared profiles and ordinary headers are replayed", func(t *testing.T) {
		peer := dialWSPeer(t, provider, "/watch")
		peer.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
			frame.Credentials = append(frame.Credentials,
				clientcontract.WebSocketCredentialV1{Profile: "tenant", Value: "tenant-secret"})
			frame.Headers = []clientcontract.WebSocketHeaderV1{
				{Name: "X-Correlation-Key", Values: []string{"call-1"}},
			}
			frame.Context = &clientcontract.WebSocketContextV1{
				Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
				RequestID:   "request-1",
			}
		}))
		peer.expect(clientcontract.WebSocketFrameReady)
		peer.expect(clientcontract.WebSocketFrameResult)

		for header, want := range map[string]string{
			"Authorization":     "Bearer harness-token",
			"X-Tenant-Key":      "tenant-secret",
			"X-Correlation-Key": "call-1",
			"Traceparent":       "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"X-Request-Id":      "request-1",
		} {
			if got := seen.Get(header); got != want {
				t.Errorf("reconstructed %s = %q, want %q", header, got, want)
			}
		}
		if clientID != "harness-consumer" {
			t.Errorf("client identity = %q, want harness-consumer", clientID)
		}
		if _, forwarded := phttp.ForwardedBearerTokenFromContext(t.Context()); forwarded {
			t.Error("the test context should not carry a forwarded bearer")
		}
	})

	t.Run("an ordinary header cannot override a declared credential profile", func(t *testing.T) {
		peer := dialWSPeer(t, provider, "/watch")
		peer.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
			frame.Headers = []clientcontract.WebSocketHeaderV1{
				{Name: "X-Tenant-Key", Values: []string{"forged"}},
			}
		}))
		failure := peer.expectError()
		if failure.Code != clientcontract.ErrorCodeInvalidSecurity {
			t.Fatalf("refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeInvalidSecurity)
		}
	})

	t.Run("an undeclared credential profile is refused", func(t *testing.T) {
		peer := dialWSPeer(t, provider, "/watch")
		peer.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
			frame.Credentials = []clientcontract.WebSocketCredentialV1{{Profile: "ghost", Value: "x"}}
		}))
		failure := peer.expectError()
		if failure.Code != clientcontract.ErrorCodeUnknownProfile {
			t.Fatalf("refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeUnknownProfile)
		}
	})

	t.Run("an init for another operation is refused", func(t *testing.T) {
		peer := dialWSPeer(t, provider, "/watch")
		peer.send(wsInit("getSomethingElse"))
		failure := peer.expectError()
		if failure.Code != clientcontract.ErrorCodeInvalidTransport {
			t.Fatalf("refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeInvalidTransport)
		}
	})
}

// --- three directions ------------------------------------------------------

func TestWebSocketProvider_ServerStreamDeliversExactSequencesThenAnEmptyResult(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "the-three-stream-directions-carry-their-declared-values")
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(watchEndpoint(func(stream *ServerStreamContext[wsEvent]) error {
			if err := stream.Send(wsEvent{ID: "first", Payload: []byte("ab")}); err != nil {
				return err
			}
			return stream.Send(wsEvent{ID: "second", Payload: []byte{}})
		}))
	}})
	peer := dialWSPeer(t, provider, "/watch")
	peer.send(wsInit("getWatch"))

	ready, ok := peer.expect(clientcontract.WebSocketFrameReady).(*clientcontract.WebSocketReadyFrameV1)
	if !ok || ready.Resumed == nil || *ready.Resumed {
		t.Fatalf("ready frame = %#v, want a fresh admission", ready)
	}
	for index, want := range []struct {
		sequence string
		id       string
		payload  []byte
	}{{"1", "first", []byte("ab")}, {"2", "second", []byte{}}} {
		message, isMessage := peer.expect(clientcontract.WebSocketFrameMessage).(*clientcontract.WebSocketMessageFrameV1)
		if !isMessage {
			t.Fatalf("frame %d is not a message", index)
		}
		if message.Sequence != want.sequence {
			t.Fatalf("sequence %d = %q, want %q", index, message.Sequence, want.sequence)
		}
		event := wsDecode[wsEvent](t, message.Payload)
		if event.ID != want.id || !reflect.DeepEqual(event.Payload, want.payload) {
			t.Fatalf("event %d = %#v, want id %q payload %#v", index, event, want.id, want.payload)
		}
	}
	result, isResult := peer.expect(clientcontract.WebSocketFrameResult).(*clientcontract.WebSocketResultFrameV1)
	if !isResult || result.Payload != nil {
		t.Fatalf("server stream result = %#v, want no payload", result)
	}
	if code := peer.expectClose(); code != phttp.WebSocketCloseNormal {
		t.Fatalf("close code = %d, want %d", code, phttp.WebSocketCloseNormal)
	}
}

func TestWebSocketProvider_ClientStreamHalfClosesThenReturnsTheDeclaredResult(t *testing.T) {
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(uploadEndpoint(func(stream *ClientStreamContext[wsEvent, wsSummary]) error {
			count := 0
			for range stream.Messages() {
				count++
			}
			if err := stream.Err(); err != nil {
				return err
			}
			stream.Result(wsSummary{Count: count})
			return nil
		}))
	}})
	peer := dialWSPeer(t, provider, "/upload")
	peer.send(wsInit("getUpload"))
	peer.expect(clientcontract.WebSocketFrameReady)
	peer.send(wsMessage(t, "1", event("a")))
	peer.send(wsMessage(t, "2", event("b")))
	peer.send(&clientcontract.WebSocketHalfCloseFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameHalfClose,
	})

	result, ok := peer.expect(clientcontract.WebSocketFrameResult).(*clientcontract.WebSocketResultFrameV1)
	if !ok || result.Payload == nil {
		t.Fatalf("client stream result = %#v, want a declared payload", result)
	}
	if summary := wsDecode[wsSummary](t, *result.Payload); summary.Count != 2 {
		t.Fatalf("summary = %#v, want two uploaded messages", summary)
	}
}

func TestWebSocketProvider_ClientStreamWithoutADeclaredResultIsARefusal(t *testing.T) {
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(uploadEndpoint(func(stream *ClientStreamContext[wsEvent, wsSummary]) error {
			for range stream.Messages() {
			}
			return nil
		}))
	}})
	peer := dialWSPeer(t, provider, "/upload")
	peer.send(wsInit("getUpload"))
	peer.expect(clientcontract.WebSocketFrameReady)
	peer.send(&clientcontract.WebSocketHalfCloseFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameHalfClose,
	})

	failure := peer.expectError()
	if failure.Status != http.StatusInternalServerError {
		t.Fatalf("refusal status = %d, want 500", failure.Status)
	}
	if code := peer.expectClose(); code != phttp.WebSocketCloseInternalError {
		t.Fatalf("close code = %d, want %d", code, phttp.WebSocketCloseInternalError)
	}
}

func TestWebSocketProvider_BidirectionalHalfCloseKeepsTheProviderDirectionOpen(t *testing.T) {
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(chatEndpoint(func(stream *BidiStreamContext[wsEvent, wsEvent]) error {
			for message := range stream.Messages() {
				if err := stream.Send(event("ack:" + message.ID)); err != nil {
					return err
				}
			}
			if err := stream.Err(); err != nil {
				return err
			}
			if err := stream.Send(event("after-half-close")); err != nil {
				return err
			}
			stream.Result(event("final"))
			return nil
		}))
	}})
	peer := dialWSPeer(t, provider, "/chat")
	peer.send(wsInit("getChat"))
	peer.expect(clientcontract.WebSocketFrameReady)
	peer.send(wsMessage(t, "1", event("hello")))

	ack, ok := peer.expect(clientcontract.WebSocketFrameMessage).(*clientcontract.WebSocketMessageFrameV1)
	if !ok || wsDecode[wsEvent](t, ack.Payload).ID != "ack:hello" {
		t.Fatalf("provider ack = %#v", ack)
	}
	peer.send(&clientcontract.WebSocketHalfCloseFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameHalfClose,
	})
	after, ok := peer.expect(clientcontract.WebSocketFrameMessage).(*clientcontract.WebSocketMessageFrameV1)
	if !ok || after.Sequence != "2" || wsDecode[wsEvent](t, after.Payload).ID != "after-half-close" {
		t.Fatalf("provider frame after half-close = %#v", after)
	}
	result, ok := peer.expect(clientcontract.WebSocketFrameResult).(*clientcontract.WebSocketResultFrameV1)
	if !ok || result.Payload == nil || wsDecode[wsEvent](t, *result.Payload).ID != "final" {
		t.Fatalf("bidirectional result = %#v", result)
	}
}

// --- framing ---------------------------------------------------------------

func TestWebSocketProvider_ReassemblesContinuationFrames(t *testing.T) {
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(chatEndpoint(func(stream *BidiStreamContext[wsEvent, wsEvent]) error {
			for message := range stream.Messages() {
				if err := stream.Send(event("echo:" + message.ID)); err != nil {
					return err
				}
			}
			return stream.Err()
		}))
	}})
	peer := dialWSPeer(t, provider, "/chat")

	initBytes, err := json.Marshal(wsInit("getChat"))
	if err != nil {
		t.Fatalf("encode init: %v", err)
	}
	peer.sendFragments(initBytes, 7)
	peer.expect(clientcontract.WebSocketFrameReady)

	messageBytes, err := json.Marshal(wsMessage(t, "1", event("fragmented")))
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	peer.sendFragments(messageBytes, 5)
	echo, ok := peer.expect(clientcontract.WebSocketFrameMessage).(*clientcontract.WebSocketMessageFrameV1)
	if !ok || wsDecode[wsEvent](t, echo.Payload).ID != "echo:fragmented" {
		t.Fatalf("reassembled echo = %#v", echo)
	}
}

func TestWebSocketProvider_RefusesAMessagePastTheDeclaredFrameBound(t *testing.T) {
	provider := newWSProvider(t, wsProviderOptions{
		defaults: &clientcontract.ResiliencePolicy{
			Stream: &clientcontract.StreamPolicy{MaxFrameBytes: bytesBound(256)},
		},
		register: func(plugin *Plugin) {
			plugin.Register(chatEndpoint(func(stream *BidiStreamContext[wsEvent, wsEvent]) error {
				for range stream.Messages() {
				}
				return stream.Err()
			}))
		},
	})
	peer := dialWSPeer(t, provider, "/chat")
	peer.send(wsInit("getChat"))
	peer.expect(clientcontract.WebSocketFrameReady)
	peer.send(wsMessage(t, "1", event(strings.Repeat("x", 512))))

	if code := peer.expectClose(); code != phttp.WebSocketCloseMessageTooBig &&
		code != phttp.WebSocketClosePolicyViolation {
		t.Fatalf("close code = %d, want the message-too-big or policy code", code)
	}
}

func TestWebSocketProvider_RefusesANonTextFrame(t *testing.T) {
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(chatEndpoint(func(stream *BidiStreamContext[wsEvent, wsEvent]) error {
			for range stream.Messages() {
			}
			return stream.Err()
		}))
	}})
	peer := dialWSPeer(t, provider, "/chat")
	peer.deadline()
	if err := peer.writeFrame(true, 0x2, []byte{0x00, 0x01}); err != nil {
		t.Fatalf("write binary frame: %v", err)
	}
	failure := peer.expectError()
	if failure.Code != clientcontract.ErrorCodeInvalidTransport {
		t.Fatalf("refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeInvalidTransport)
	}
}

// --- budgets, heartbeat, cancellation, shutdown ----------------------------

func TestWebSocketProvider_AnswersAPingWhileAdmissionIsStillWaiting(t *testing.T) {
	release := make(chan struct{})
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(Endpoint("GET", "/watch").
			Returns(StreamOf[wsEvent]()).
			Use(func(_ *phttp.Context, next func() *phttp.Response) *phttp.Response {
				// An asynchronous credential check: admission is held here until
				// the test releases it.
				<-release
				return next()
			}).
			Handle(ServerStream(func(_ *ServerStreamContext[wsEvent]) error { return nil })))
	}})
	peer := dialWSPeer(t, provider, "/watch")
	peer.send(wsInit("getWatch"))
	peer.send(&clientcontract.WebSocketHeartbeatFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePing, Nonce: "await-ready-1",
	})

	pong, ok := peer.expect(clientcontract.WebSocketFramePong).(*clientcontract.WebSocketHeartbeatFrameV1)
	if !ok || pong.Nonce != "await-ready-1" {
		t.Fatalf("pong during await-ready = %#v", pong)
	}
	close(release)
	peer.expect(clientcontract.WebSocketFrameReady)
}

func TestWebSocketProvider_AdmissionDeadlineEndsTheSocketWithATypedError(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "admission-heartbeat-and-idle-follow-the-declared-policy")
	provider := newWSProvider(t, wsProviderOptions{
		defaults: &clientcontract.ResiliencePolicy{
			Stream: &clientcontract.StreamPolicy{HandshakeTimeoutMs: millis(40)},
		},
		register: func(plugin *Plugin) {
			plugin.Register(watchEndpoint(func(_ *ServerStreamContext[wsEvent]) error { return nil }))
		},
	})
	peer := dialWSPeer(t, provider, "/watch")

	// The client never sends its init frame. The declared handshake budget, not
	// a real sleep in this test, is what ends the conversation.
	failure := peer.expectError()
	if failure.Status != http.StatusRequestTimeout && failure.Status != http.StatusGatewayTimeout {
		t.Fatalf("admission deadline status = %d, want a timeout status", failure.Status)
	}
	if failure.Code != string(perrors.CodeTimeout) {
		t.Fatalf("admission deadline code = %q, want %q", failure.Code, perrors.CodeTimeout)
	}
}

// TestWebSocketProvider_AdmissionDeadlineBeatsItsOwnTransportBackstop pins WHICH
// deadline ends a caller that never sends its init frame.
//
// Two timers bound admission: the declared handshake budget admit() owns, and
// the transport idle timeout that unblocks the read pump. They used to be armed
// at the SAME value, so the terminal depended on which fired first — the budget
// answers with the declared CodeTimeout error frame, while the transport timeout
// surfaces as a read failure classified CodeCancelled, which terminate answers
// with a bare normal close and no error frame at all. A caller learned which
// timer won, not what the policy declared.
//
// The budget here is one millisecond: the most hostile case a declared policy
// can express, and one no amount of CPU starvation can make the backstop win.
func TestWebSocketProvider_AdmissionDeadlineBeatsItsOwnTransportBackstop(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "admission-heartbeat-and-idle-follow-the-declared-policy")
	provider := newWSProvider(t, wsProviderOptions{
		defaults: &clientcontract.ResiliencePolicy{
			Stream: &clientcontract.StreamPolicy{HandshakeTimeoutMs: millis(1)},
		},
		register: func(plugin *Plugin) {
			plugin.Register(watchEndpoint(func(_ *ServerStreamContext[wsEvent]) error { return nil }))
		},
	})
	peer := dialWSPeer(t, provider, "/watch")

	failure := peer.expectError()
	if failure.Code != string(perrors.CodeTimeout) {
		t.Fatalf("admission deadline code = %q, want %q — the transport backstop won its own race",
			failure.Code, perrors.CodeTimeout)
	}
}

// The backstop is armed strictly after the budget it backs, for every policy a
// project can declare. This is the rule the test above exercises end to end.
func TestWebSocketAdmissionBackstopIsArmedAfterTheDeclaredBudget(t *testing.T) {
	for _, declared := range []*int{nil, millis(1), millis(40), millis(10_000)} {
		policy := &clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{HandshakeTimeoutMs: declared}}
		budgets := resolveWebSocketBudgets(policy, nil)
		backstop := webSocketAdmissionBackstop(budgets.Handshake)
		if backstop <= budgets.Handshake {
			t.Errorf("declared=%v: backstop %v does not outlast the budget %v", declared, backstop, budgets.Handshake)
		}
	}
}

func TestWebSocketProvider_SendsTheDeclaredHeartbeatCadence(t *testing.T) {
	hold := make(chan struct{})
	provider := newWSProvider(t, wsProviderOptions{
		defaults: &clientcontract.ResiliencePolicy{
			Stream: &clientcontract.StreamPolicy{HeartbeatMs: millis(20), IdleTimeoutMs: millis(5000)},
		},
		register: func(plugin *Plugin) {
			plugin.Register(watchEndpoint(func(_ *ServerStreamContext[wsEvent]) error {
				<-hold
				return nil
			}))
		},
	})
	t.Cleanup(func() { close(hold) })
	peer := dialWSPeer(t, provider, "/watch")
	peer.send(wsInit("getWatch"))
	peer.expect(clientcontract.WebSocketFrameReady)

	frame, err := peer.next()
	if err != nil {
		t.Fatalf("waiting for the declared heartbeat: %v", err)
	}
	ping, ok := frame.(*clientcontract.WebSocketHeartbeatFrameV1)
	if !ok || ping.Type != clientcontract.WebSocketFramePing {
		t.Fatalf("provider frame = %#v, want a ping at the declared cadence", frame)
	}
	requireContains(t, ping.Nonce, "provider-")
}

func TestWebSocketProvider_CancelEndsTheStreamWithoutASecondTerminal(t *testing.T) {
	observed := make(chan error, 1)
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(watchEndpoint(func(stream *ServerStreamContext[wsEvent]) error {
			<-handlerContext(stream.StreamContext).Done()
			observed <- handlerContext(stream.StreamContext).Err()
			return handlerContext(stream.StreamContext).Err()
		}))
	}})
	peer := dialWSPeer(t, provider, "/watch")
	peer.send(wsInit("getWatch"))
	peer.expect(clientcontract.WebSocketFrameReady)
	peer.send(&clientcontract.WebSocketCancelFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameCancel,
		Code: clientcontract.WebSocketCancelCodeCanceled,
	})

	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler context was not canceled by the client cancel frame")
	}
	if code := peer.expectClose(); code != phttp.WebSocketCloseNormal {
		t.Fatalf("close code after cancel = %d, want %d", code, phttp.WebSocketCloseNormal)
	}
}

func TestWebSocketProvider_ApplicationShutdownEndsTheConversation(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "cancellation-and-application-shutdown-end-the-conversation")
	t.Run("during admission", func(t *testing.T) {
		provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
			plugin.Register(watchEndpoint(func(_ *ServerStreamContext[wsEvent]) error { return nil }))
		}})
		peer := dialWSPeer(t, provider, "/watch")
		provider.stop(t)

		failure := peer.expectError()
		if failure.Status != http.StatusServiceUnavailable {
			t.Fatalf("shutdown status = %d, want 503", failure.Status)
		}
		if code := peer.expectClose(); code != phttp.WebSocketCloseGoingAway {
			t.Fatalf("close code = %d, want %d", code, phttp.WebSocketCloseGoingAway)
		}
	})

	t.Run("during an active stream", func(t *testing.T) {
		hold := make(chan struct{})
		provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
			plugin.Register(watchEndpoint(func(stream *ServerStreamContext[wsEvent]) error {
				if err := stream.Send(event("live")); err != nil {
					return err
				}
				<-handlerContext(stream.StreamContext).Done()
				close(hold)
				return handlerContext(stream.StreamContext).Err()
			}))
		}})
		peer := dialWSPeer(t, provider, "/watch")
		peer.send(wsInit("getWatch"))
		peer.expect(clientcontract.WebSocketFrameReady)
		peer.expect(clientcontract.WebSocketFrameMessage)
		provider.stop(t)
		<-hold

		failure := peer.expectError()
		if failure.Status != http.StatusServiceUnavailable {
			t.Fatalf("shutdown status = %d, want 503", failure.Status)
		}
		if strings.Contains(strings.ToLower(failure.Message), "token") {
			t.Fatalf("shutdown message leaked material: %q", failure.Message)
		}
		if code := peer.expectClose(); code != phttp.WebSocketCloseGoingAway {
			t.Fatalf("close code = %d, want %d", code, phttp.WebSocketCloseGoingAway)
		}
	})
}

func TestWebSocketProvider_BoundsTheInboundQueueWithoutDroppingAMessage(t *testing.T) {
	release := make(chan struct{})
	delivered := make(chan string, 8)
	provider := newWSProvider(t, wsProviderOptions{
		defaults: &clientcontract.ResiliencePolicy{
			Stream: &clientcontract.StreamPolicy{MaxBufferedMessages: count(1), IdleTimeoutMs: millis(5000)},
		},
		register: func(plugin *Plugin) {
			plugin.Register(uploadEndpoint(func(stream *ClientStreamContext[wsEvent, wsSummary]) error {
				<-release
				count := 0
				for message := range stream.Messages() {
					delivered <- message.ID
					count++
				}
				if err := stream.Err(); err != nil {
					return err
				}
				stream.Result(wsSummary{Count: count})
				return nil
			}))
		},
	})
	peer := dialWSPeer(t, provider, "/upload")
	peer.send(wsInit("getUpload"))
	peer.expect(clientcontract.WebSocketFrameReady)
	for index, id := range []string{"a", "b", "c"} {
		peer.send(wsMessage(t, []string{"1", "2", "3"}[index], event(id)))
	}
	peer.send(&clientcontract.WebSocketHalfCloseFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameHalfClose,
	})
	close(release)

	result, ok := peer.expect(clientcontract.WebSocketFrameResult).(*clientcontract.WebSocketResultFrameV1)
	if !ok || result.Payload == nil {
		t.Fatalf("client stream result = %#v", result)
	}
	if summary := wsDecode[wsSummary](t, *result.Payload); summary.Count != 3 {
		t.Fatalf("delivered %d messages past a queue bound of 1, want 3 with none dropped", summary.Count)
	}
	close(delivered)
	got := make([]string, 0, 3)
	for id := range delivered {
		got = append(got, id)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delivered order = %v, want %v", got, want)
	}
}

// --- refusals the provider never downgrades --------------------------------

func TestWebSocketProvider_RefusesProtoEncodingAtAdmissionBeforeAnyMessage(t *testing.T) {
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(uploadEndpoint(func(stream *ClientStreamContext[wsEvent, wsSummary]) error {
			for range stream.Messages() {
			}
			stream.Result(wsSummary{})
			return nil
		}))
	}})
	peer := dialWSPeer(t, provider, "/upload")
	empty := ""
	peer.send(wsInit("getUpload", func(frame *clientcontract.WebSocketInitFrameV1) {
		frame.Request = &clientcontract.WebSocketEncodedPayloadV1{
			Encoding: clientcontract.EncodingProto, Base64: &empty,
		}
	}))

	failure := peer.expectError()
	if failure.Code != clientcontract.ErrorCodeInvalidTransport {
		t.Fatalf("proto refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeInvalidTransport)
	}
	requireContains(t, failure.Message, "encoding")
}

func TestWebSocketProvider_RefusesAResumeItDoesNotDeclare(t *testing.T) {
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(watchEndpoint(func(_ *ServerStreamContext[wsEvent]) error { return nil }))
	}})
	peer := dialWSPeer(t, provider, "/watch")
	peer.send(wsInit("getWatch", func(frame *clientcontract.WebSocketInitFrameV1) {
		frame.Resume = &clientcontract.WebSocketResumeRequestV1{Token: "token", AfterSequence: "7"}
	}))

	failure := peer.expectError()
	if failure.Code != clientcontract.ErrorCodeInvalidResilience {
		t.Fatalf("resume refusal code = %q, want %q", failure.Code, clientcontract.ErrorCodeInvalidResilience)
	}
}

// The six corpus scenes whose illegal frame is a provider frame cannot be
// driven from a client, so they are proved on the emitting side: the published
// conversation refuses the frame before a byte reaches the wire.
func TestWebSocketProvider_NeverEmitsAFrameItsOwnContractRefuses(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "the-provider-never-emits-a-frame-its-contract-refuses")
	resumed, notResumed := true, false
	tests := []struct {
		name     string
		stream   clientcontract.StreamMode
		admitted bool
		frame    any
		want     string
	}{
		{
			name:     "a server stream result carries no payload",
			stream:   clientcontract.StreamServer,
			admitted: true,
			frame: &clientcontract.WebSocketResultFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameResult,
				Payload: &clientcontract.WebSocketEncodedPayloadV1{
					Encoding: clientcontract.EncodingJSON, Value: json.RawMessage(`{"total":1}`),
				},
			},
			want: clientcontract.ErrorCodeInvalidTransport,
		},
		{
			name:     "a provider message before ready is refused",
			stream:   clientcontract.StreamServer,
			admitted: false,
			frame:    mustWSMessage(1),
			want:     clientcontract.ErrorCodeInvalidTransport,
		},
		{
			name:     "a provider sequence past uint64 is refused",
			stream:   clientcontract.StreamServer,
			admitted: true,
			frame: &clientcontract.WebSocketMessageFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameMessage,
				Sequence: "18446744073709551616",
				Payload: clientcontract.WebSocketEncodedPayloadV1{
					Encoding: clientcontract.EncodingJSON, Value: json.RawMessage(`{}`),
				},
			},
			want: clientcontract.ErrorCodeInvalidTransport,
		},
		{
			name:     "half-close is never a provider frame",
			stream:   clientcontract.StreamBidirectional,
			admitted: true,
			frame: &clientcontract.WebSocketHalfCloseFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameHalfClose,
			},
			want: clientcontract.ErrorCodeInvalidTransport,
		},
		{
			name:   "a resumed ready carries its resume token",
			stream: clientcontract.StreamServer,
			frame: &clientcontract.WebSocketReadyFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameReady, Resumed: &resumed,
			},
			want: clientcontract.ErrorCodeRequired,
		},
		{
			name:   "a resumed ready needs a resume the transport declared",
			stream: clientcontract.StreamServer,
			frame: &clientcontract.WebSocketReadyFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameReady,
				Resumed: &resumed, ResumeToken: "token",
			},
			want: clientcontract.ErrorCodeInvalidResilience,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := newSendOnlySession(t, tc.stream)
			// Every scene but the two ready ones starts from an admitted
			// conversation; a ready frame is refused from await-ready itself.
			if _, isReady := tc.frame.(*clientcontract.WebSocketReadyFrameV1); !isReady && tc.admitted {
				if err := session.send(&clientcontract.WebSocketReadyFrameV1{
					V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameReady,
					Resumed: &notResumed,
				}); err != nil {
					t.Fatalf("ready: %v", err)
				}
			}
			before := len(session.written)
			err := session.send(tc.frame)
			if err == nil {
				t.Fatal("the provider emitted a frame its own contract refuses")
			}
			var refusal *webSocketRefusal
			if !asWebSocketRefusal(err, &refusal) {
				t.Fatalf("refusal = %v, want a typed contract refusal", err)
			}
			if refusal.code != tc.want {
				t.Fatalf("refusal code = %q, want %q (%v)", refusal.code, tc.want, err)
			}
			if len(session.written) != before {
				t.Fatal("the refused frame still reached the wire")
			}
		})
	}
}

// --- corpus conformance ----------------------------------------------------

// The published corpus is replayed against the real provider: every
// client-to-server frame of a scene is driven onto a live socket, and the
// provider either honors the scene or refuses it with the diagnostic code the
// corpus declares.
func TestWebSocketProvider_ReplaysThePublishedWebSocketCorpus(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "first-party-websocket-streams", "the-published-websocket-corpus-is-replayed-against-the-real-server")
	root := filepath.Join("..", "..", "..", "protocols", "clientcontract", "fixtures", "websocket")
	expectations := struct {
		Valid   []string          `json:"valid"`
		Invalid map[string]string `json:"invalid"`
	}{}
	raw, err := os.ReadFile(filepath.Join(root, "expectations.json"))
	if err != nil {
		t.Fatalf("read corpus expectations: %v", err)
	}
	if err := json.Unmarshal(raw, &expectations); err != nil {
		t.Fatalf("decode corpus expectations: %v", err)
	}

	// Scenes whose only illegal frame is a provider frame are proved by
	// TestWebSocketProvider_NeverEmitsAFrameItsOwnContractRefuses instead: a
	// client cannot drive them.
	providerSide := map[string]bool{
		"data-before-ready.json": true, "ready-resumed-undeclared.json": true,
		"ready-resumed-without-token.json": true, "sequence-overflow.json": true,
		"server-half-close.json": true, "server-stream-result-payload.json": true,
	}
	// Scenes whose init asks for a transport feature this provider does not
	// declare. The corpus calls the frames valid, and they are; this provider
	// refuses the request with the code the wire names for that refusal.
	undeclared := map[string]string{
		"client-stream-empty-proto.json":   clientcontract.ErrorCodeInvalidTransport,
		"server-resume-wide-sequence.json": clientcontract.ErrorCodeInvalidResilience,
	}
	// error-before-ready has no client frame at all: its provider frame is the
	// admission refusal proved by the admission-deadline test.
	clientLess := map[string]bool{"error-before-ready.json": true}

	for _, name := range expectations.Valid {
		if clientLess[name] {
			continue
		}
		t.Run("valid/"+name, func(t *testing.T) {
			scene := loadWSScene(t, filepath.Join(root, "valid", name))
			peer, operationID := scene.dial(t)
			refused := scene.driveClientFrames(t, peer, operationID)
			if want, isUndeclared := undeclared[name]; isUndeclared {
				if refused == nil {
					t.Fatal("the provider accepted a transport feature it does not declare")
				}
				if refused.Code != want {
					t.Fatalf("refusal code = %q, want %q", refused.Code, want)
				}
				return
			}
			if refused != nil {
				t.Fatalf("the provider refused a valid scene: %+v", *refused)
			}
		})
	}

	for name, want := range expectations.Invalid {
		if providerSide[name] {
			continue
		}
		t.Run("invalid/"+name, func(t *testing.T) {
			scene := loadWSScene(t, filepath.Join(root, "invalid", name))
			peer, operationID := scene.dial(t)
			refused := scene.driveClientFrames(t, peer, operationID)
			if refused == nil {
				t.Fatalf("the provider accepted a scene the corpus calls invalid (%s)", want)
			}
			if refused.Code != want {
				t.Fatalf("refusal code = %q, want %q", refused.Code, want)
			}
		})
	}
}

// nullableEvent carries a field that marshals to a JSON null when it is unset.
type nullableEvent struct {
	ID    string  `json:"id"`
	Label *string `json:"label"`
}

// The published wire refuses a JSON null anywhere inside a frame, application
// payloads included. A provider that sent one would put bytes on the wire that
// every conforming client refuses, so the refusal reaches the handler instead:
// Send fails with the contract's own diagnostic code and nothing is written.
func TestWebSocketProvider_RefusesAJSONNullInsideAProviderPayload(t *testing.T) {
	sendErr := make(chan error, 1)
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(Endpoint("GET", "/watch").
			Returns(StreamOf[nullableEvent]()).
			Handle(ServerStream(func(stream *ServerStreamContext[nullableEvent]) error {
				err := stream.Send(nullableEvent{ID: "unset-label"})
				sendErr <- err
				return err
			})))
	}})
	peer := dialWSPeer(t, provider, "/watch")
	peer.send(wsInit("getWatch"))
	peer.expect(clientcontract.WebSocketFrameReady)

	select {
	case err := <-sendErr:
		var refusal *webSocketRefusal
		if !asWebSocketRefusal(err, &refusal) {
			t.Fatalf("Send returned %v, want a typed contract refusal", err)
		}
		if refusal.code != clientcontract.ErrorCodeParseError {
			t.Fatalf("refusal code = %q, want %q", refusal.code, clientcontract.ErrorCodeParseError)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never observed the refusal")
	}
	failure := peer.expectError()
	if failure.Code != clientcontract.ErrorCodeParseError {
		t.Fatalf("terminal error code = %q, want %q", failure.Code, clientcontract.ErrorCodeParseError)
	}
}
