package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// resumePolicy declares the operation half of the resume agreement: the
// provider states the transport can continue, the operation states the runtime
// may ask it to.
func resumePolicy(reconnect bool) *clientcontract.ResiliencePolicy {
	return &clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{Reconnect: &reconnect}}
}

// resumeOperation is a declared-safe server stream carried by a resume-capable
// first-party WebSocket transport.
func resumeOperation(resumeDeclared, reconnect bool) Operation {
	message := clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"value": {Type: "string"}},
		Required: []string{"value"}, AdditionalProperties: additionalForbidden(),
	}
	return webSocketTestOperation("watchItems", clientcontract.StreamServer, clientcontract.EncodingJSON,
		resumeDeclared, &clientcontract.MessageShapes{Output: &message}, resumePolicy(reconnect), nil)
}

// resumeInit reads the resume request one connection presented, or nil when it
// asked for a fresh stream.
func resumeInit(t *testing.T, peer *wsTestPeer) *clientcontract.WebSocketInitFrameV1 {
	t.Helper()
	frame := peer.expect(clientcontract.WebSocketFrameInit)
	init, ok := frame.(*clientcontract.WebSocketInitFrameV1)
	if !ok {
		peer.server.fail("the first client frame is not an init frame")
		return nil
	}
	return init
}

// wsResumeHarness is a scripted provider that serves several connections of one
// stream, so a break and its continuation are observable as separate sockets.
type wsResumeHarness struct {
	*dualTransportServer

	mu    sync.Mutex
	inits []*clientcontract.WebSocketInitFrameV1
}

func (harness *wsResumeHarness) recordInit(init *clientcontract.WebSocketInitFrameV1) {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	harness.inits = append(harness.inits, init)
}

func (harness *wsResumeHarness) init(t *testing.T, index int) *clientcontract.WebSocketInitFrameV1 {
	t.Helper()
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if index >= len(harness.inits) {
		t.Fatalf("connection %d never reached the provider; %d did", index+1, len(harness.inits))
	}
	return harness.inits[index]
}

func newResumeHarness(t *testing.T, play func(harness *wsResumeHarness, peer *wsTestPeer, connection int)) *wsResumeHarness {
	t.Helper()
	harness := &wsResumeHarness{}
	harness.dualTransportServer = newDualTransportServer(t, dualTransportOptions{
		play: func(peer *wsTestPeer, connection int) { play(harness, peer, connection) },
	})
	return harness
}

// A socket that ends without a terminal frame is not the end of a stream the
// provider declared resumable. The runtime opens a new socket, presents the
// token the provider issued and the last sequence the caller received, and the
// caller reads one continuous stream with no gap and no duplicate.
func TestServerStreamContinuesAfterATransportBreak(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-stream-resume",
		"a-declared-server-stream-continues-after-the-last-sequence-the-caller-received")

	harness := newResumeHarness(t, func(harness *wsResumeHarness, peer *wsTestPeer, connection int) {
		init := resumeInit(t, peer)
		if init == nil {
			return
		}
		harness.recordInit(init)
		switch connection {
		case 1:
			peer.send(webSocketReadyFrame(false, "grant-1"))
			peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
			peer.send(webSocketMessageFrame("2", `{"value":"two"}`))
			peer.abort()
		default:
			peer.send(webSocketReadyFrame(true, "grant-2"))
			peer.send(webSocketMessageFrame("3", `{"value":"three"}`))
			peer.send(webSocketResultFrame(""))
			peer.drain()
		}
	})
	bound := dualTransportClient(t, harness.URL)
	stream, err := OpenServerStreamWS[streamItem](t.Context(), bound, &Request{}, resumeOperation(true, true))
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 3)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(values) != "[one two three]" {
		t.Fatalf("stream values = %v; want one continuous stream with no gap and no duplicate", values)
	}
	second := harness.init(t, 1)
	if second.Resume == nil {
		t.Fatal("the continuation asked for a fresh stream")
	}
	if second.Resume.Token != "grant-1" || second.Resume.AfterSequence != "2" {
		t.Fatalf("resume request = %#v; want the issued token and the last delivered sequence", second.Resume)
	}
	harness.assertNoFailures(t)
}

// The token rotates on every ready frame. A continuation presents the token the
// connection it continues issued, never the one before it, so an observed token
// buys nothing after the socket that carried it ends.
func TestServerStreamResumePresentsTheRotatedToken(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-stream-resume",
		"each-continuation-presents-the-token-the-previous-connection-issued")

	harness := newResumeHarness(t, func(harness *wsResumeHarness, peer *wsTestPeer, connection int) {
		init := resumeInit(t, peer)
		if init == nil {
			return
		}
		harness.recordInit(init)
		switch connection {
		case 1:
			peer.send(webSocketReadyFrame(false, "grant-1"))
			peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
			peer.abort()
		case 2:
			peer.send(webSocketReadyFrame(true, "grant-2"))
			peer.send(webSocketMessageFrame("2", `{"value":"two"}`))
			peer.abort()
		default:
			peer.send(webSocketReadyFrame(true, "grant-3"))
			peer.send(webSocketResultFrame(""))
			peer.drain()
		}
	})
	bound := dualTransportClient(t, harness.URL)
	stream, err := OpenServerStreamWS[streamItem](t.Context(), bound, &Request{}, resumeOperation(true, true))
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 2)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(values) != "[one two]" {
		t.Fatalf("stream values = %v", values)
	}
	for index, want := range []struct{ token, after string }{{"grant-1", "1"}, {"grant-2", "2"}} {
		resume := harness.init(t, index+1).Resume
		if resume == nil || resume.Token != want.token || resume.AfterSequence != want.after {
			t.Fatalf("connection %d resume = %#v; want token %q after %q", index+2, resume, want.token, want.after)
		}
	}
	harness.assertNoFailures(t)
}

// Resume is a declaration, not a runtime guess. A provider that does not
// declare its transport resumable, and an operation that does not declare
// reconnect, both end the stream at the break with a typed terminal.
func TestServerStreamDoesNotContinueWhatWasNotDeclaredResumable(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-stream-resume",
		"a-stream-that-was-not-declared-resumable-ends-at-the-break")

	cases := []struct {
		name      string
		transport bool
		reconnect bool
	}{
		{name: "operation declares no reconnect", transport: true, reconnect: false},
		{name: "neither half is declared", transport: false, reconnect: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newResumeHarness(t, func(harness *wsResumeHarness, peer *wsTestPeer, _ int) {
				init := resumeInit(t, peer)
				if init == nil {
					return
				}
				harness.recordInit(init)
				peer.send(webSocketReadyFrame(false, "grant-1"))
				peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
				peer.abort()
			})
			bound := dualTransportClient(t, harness.URL)
			operation := resumeOperation(testCase.transport, testCase.reconnect)
			stream, err := OpenServerStreamWS[streamItem](t.Context(), bound, &Request{}, operation)
			if err != nil {
				t.Fatal(err)
			}
			values := make([]string, 0, 1)
			for message := range stream.Messages() {
				values = append(values, message.Value)
			}
			if fmt.Sprint(values) != "[one]" {
				t.Fatalf("stream values = %v", values)
			}
			if stream.Err() == nil {
				t.Fatal("a break on a stream nobody declared resumable reported success")
			}
			if _, upgrades := harness.counts(); upgrades != 1 {
				t.Fatalf("upgrades = %d; want the stream to end at its first socket", upgrades)
			}
		})
	}
}

// A provider that answers a continuation with a fresh stream would deliver the
// sequences the caller already consumed a second time. The runtime refuses that
// answer instead of consuming it, and the caller learns the stream stopped.
func TestServerStreamRefusesAProviderThatOffersAFreshStreamInsteadOfContinuing(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-stream-resume",
		"a-provider-that-answers-a-continuation-with-a-fresh-stream-is-refused")

	harness := newResumeHarness(t, func(harness *wsResumeHarness, peer *wsTestPeer, connection int) {
		init := resumeInit(t, peer)
		if init == nil {
			return
		}
		harness.recordInit(init)
		if connection == 1 {
			peer.send(webSocketReadyFrame(false, "grant-1"))
			peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
			peer.abort()
			return
		}
		peer.send(webSocketReadyFrame(false, "grant-2"))
		peer.drain()
	})
	bound := dualTransportClient(t, harness.URL)
	stream, err := OpenServerStreamWS[streamItem](t.Context(), bound, &Request{}, resumeOperation(true, true))
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 1)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if fmt.Sprint(values) != "[one]" {
		t.Fatalf("stream values = %v; want nothing delivered twice", values)
	}
	if stream.Err() == nil {
		t.Fatal("a refused continuation reported success")
	}
	if want := "refused to continue"; !strings.Contains(stream.Err().Error(), want) {
		t.Fatalf("terminal error = %v, want %q", stream.Err(), want)
	}
}

// A token the provider no longer honors — expired, rotated away or spent — is a
// typed terminal for the caller. Nothing is delivered twice and nothing is
// retried behind its back.
func TestServerStreamSurfacesARefusedResumeToken(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-stream-resume",
		"a-token-the-provider-no-longer-honors-ends-the-stream-with-a-typed-terminal")

	harness := newResumeHarness(t, func(harness *wsResumeHarness, peer *wsTestPeer, connection int) {
		init := resumeInit(t, peer)
		if init == nil {
			return
		}
		harness.recordInit(init)
		if connection == 1 {
			peer.send(webSocketReadyFrame(false, "grant-1"))
			peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
			peer.abort()
			return
		}
		peer.send(&clientcontract.WebSocketErrorFrameV1{
			V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameError,
			Error: clientcontract.WebSocketRemoteErrorV1{
				Status: http.StatusBadRequest, Code: "client_contract.invalid_resilience",
				Message: "resume token is unknown, expired or already spent",
			},
		})
		peer.drain()
	})
	bound := dualTransportClient(t, harness.URL)
	stream, err := OpenServerStreamWS[streamItem](t.Context(), bound, &Request{}, resumeOperation(true, true))
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 1)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if fmt.Sprint(values) != "[one]" {
		t.Fatalf("stream values = %v; want nothing delivered twice", values)
	}
	if stream.Err() == nil {
		t.Fatal("a refused resume token reported success")
	}
	if _, upgrades := harness.counts(); upgrades != 2 {
		t.Fatalf("upgrades = %d; want exactly one continuation attempt", upgrades)
	}
}

// A socket that keeps breaking cannot keep one session alive forever. The
// framework bound stops the continuations and the caller reads the terminal.
func TestServerStreamResumeStopsAtItsBound(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-stream-resume",
		"a-stream-that-keeps-breaking-stops-at-the-framework-resume-bound")

	var sequence atomic.Int64
	harness := newResumeHarness(t, func(harness *wsResumeHarness, peer *wsTestPeer, connection int) {
		init := resumeInit(t, peer)
		if init == nil {
			return
		}
		harness.recordInit(init)
		next := sequence.Add(1)
		peer.send(webSocketReadyFrame(connection > 1, fmt.Sprintf("grant-%d", connection)))
		peer.send(webSocketMessageFrame(fmt.Sprint(next), fmt.Sprintf(`{"value":"v%d"}`, next)))
		peer.abort()
	})
	bound := dualTransportClient(t, harness.URL)
	stream, err := OpenServerStreamWS[streamItem](t.Context(), bound, &Request{}, resumeOperation(true, true))
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, maxStreamResumeAttempts+1)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if stream.Err() == nil {
		t.Fatal("an exhausted resume budget reported success")
	}
	if len(values) != maxStreamResumeAttempts+1 {
		t.Fatalf("delivered %d values; want the first socket plus %d continuations", len(values), maxStreamResumeAttempts)
	}
	if _, upgrades := harness.counts(); upgrades != maxStreamResumeAttempts+1 {
		t.Fatalf("upgrades = %d; want the framework bound honored exactly", upgrades)
	}
}

// A caller that withdraws from a continued stream withdraws from the socket
// that is live, not the one that broke. The session is shared across every
// socket, so one cancel reaches the provider that is actually serving.
func TestServerStreamResumeCancelReachesTheLiveSocket(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-stream-resume",
		"a-caller-withdrawal-on-a-continued-stream-reaches-the-live-socket")

	canceled := make(chan struct{})
	var closeOnce sync.Once
	harness := newResumeHarness(t, func(harness *wsResumeHarness, peer *wsTestPeer, connection int) {
		init := resumeInit(t, peer)
		if init == nil {
			return
		}
		harness.recordInit(init)
		if connection == 1 {
			peer.send(webSocketReadyFrame(false, "grant-1"))
			peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
			peer.abort()
			return
		}
		peer.send(webSocketReadyFrame(true, "grant-2"))
		peer.send(webSocketMessageFrame("2", `{"value":"two"}`))
		// The caller withdraws while this socket is the live one. The cancel
		// frame has to arrive here, not on the socket that already broke.
		for {
			frame := peer.read()
			if frame == nil {
				return
			}
			if _, ok := frame.(*clientcontract.WebSocketCancelFrameV1); ok {
				closeOnce.Do(func() { close(canceled) })
				return
			}
		}
	})
	bound := dualTransportClient(t, harness.URL)
	stream, err := OpenServerStreamWS[streamItem](t.Context(), bound, &Request{}, resumeOperation(true, true))
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 2)
	for message := range stream.Messages() {
		values = append(values, message.Value)
		if len(values) == 2 {
			if err := stream.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}
	}
	if fmt.Sprint(values) != "[one two]" {
		t.Fatalf("stream values = %v", values)
	}
	select {
	case <-canceled:
	case <-t.Context().Done():
		t.Fatal("the live socket never read the caller's cancel frame")
	}
}

// A continuation re-resolves the credentials it carries. A token that expired
// while the first socket was open is renewed before the new init leaves, so the
// provider never reads a credential this session cannot still prove.
func TestServerStreamResumeCarriesARenewedCredential(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-stream-resume",
		"a-continuation-carries-a-credential-renewed-at-the-instant-it-dials")

	clock := newTestClock()
	var issued atomic.Int32
	harness := newResumeHarness(t, func(harness *wsResumeHarness, peer *wsTestPeer, connection int) {
		init := resumeInit(t, peer)
		if init == nil {
			return
		}
		harness.recordInit(init)
		if connection == 1 {
			peer.send(webSocketReadyFrame(false, "grant-1"))
			peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
			// The credential this socket carried reaches its expiry before the
			// continuation dials.
			clock.advance(2 * time.Hour)
			peer.abort()
			return
		}
		peer.send(webSocketReadyFrame(true, "grant-2"))
		peer.send(webSocketResultFrame(""))
		peer.drain()
	})
	bound := resumeClientWithClock(t, harness.URL, clock, &issued)
	stream, err := OpenServerStreamWS[streamItem](t.Context(), bound, &Request{}, resumeOperation(true, true))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() { //nolint:revive // the values are asserted through the init frames
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	first, second := harness.init(t, 0), harness.init(t, 1)
	if len(first.Credentials) != 1 || len(second.Credentials) != 1 {
		t.Fatalf("credentials = %v then %v; want one declared profile each", first.Credentials, second.Credentials)
	}
	if first.Credentials[0].Value == second.Credentials[0].Value {
		t.Fatalf("the continuation carried the credential the first socket carried: %q", second.Credentials[0].Value)
	}
	if want := "Bearer token-2"; second.Credentials[0].Value != want {
		t.Fatalf("continuation credential = %q, want %q", second.Credentials[0].Value, want)
	}
}

// resumeClientWithClock binds a generated client whose credential freshness is
// decided by a controlled clock, so an expiry is proved by advancing time
// rather than by waiting for one.
func resumeClientWithClock(t *testing.T, endpoint string, clock *testClock, issued *atomic.Int32) *Client {
	t.Helper()
	binding := ServiceBinding{URL: endpoint, ClientID: "consumer.workload",
		Credentials: map[string]CredentialBinding{
			"service": {Provider: TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
				return Credential{
					Value:  fmt.Sprintf("token-%d", issued.Add(1)),
					Expiry: clock.Now().Add(time.Hour),
				}, nil
			})},
		}}
	bindings, err := newServiceBindings(ServicesOptions{Services: map[string]ServiceBinding{"inventory": binding}})
	if err != nil {
		t.Fatal(err)
	}
	bindings.credentials.now = clock.Now
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	})
	client, err := NewServiceClient(bindings, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// The two halves of the resume agreement cannot disagree. An operation that
// declares reconnect on a transport whose provider states no resume is refused
// by the published contract itself, before any socket opens — the runtime never
// has to decide what a half-declared continuation would mean.
func TestServerStreamRefusesReconnectWithoutProviderResume(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-stream-resume",
		"reconnect-without-provider-resume-is-refused-before-any-socket")

	harness := newResumeHarness(t, func(_ *wsResumeHarness, peer *wsTestPeer, _ int) { peer.drain() })
	bound := dualTransportClient(t, harness.URL)
	_, err := OpenServerStreamWS[streamItem](t.Context(), bound, &Request{}, resumeOperation(false, true))
	if err == nil {
		t.Fatal("a half-declared continuation opened a stream")
	}
	if want := "invalid generated operation contract"; !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal = %v, want %q", err, want)
	}
	if _, upgrades := harness.counts(); upgrades != 0 {
		t.Fatalf("upgrades = %d; want nothing on the wire", upgrades)
	}
}
