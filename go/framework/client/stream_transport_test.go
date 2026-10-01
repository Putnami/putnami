package client

import (
	stderrors "errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// streamFallbackPath is the one path both declared transports of these tests
// are served on: a fallback is about the wire, not about the URL.
const streamFallbackPath = "/items/watch"

// declaredStreamOperation builds a server stream whose declared transport order
// is exactly the one the test is about.
func declaredStreamOperation(order []clientcontract.TransportProtocol,
	idempotency clientcontract.IdempotencyKind) Operation {
	message := clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"value": {Type: "string"}},
		Required: []string{"value"}, AdditionalProperties: additionalForbidden(),
	}
	transports := make([]clientcontract.Transport, 0, len(order))
	for _, protocol := range order {
		transport := clientcontract.Transport{
			Protocol: protocol, Path: streamFallbackPath, Encoding: clientcontract.EncodingJSON,
		}
		if protocol == clientcontract.TransportWebSocket {
			transport.WebSocket = &clientcontract.WebSocketTransport{
				Subprotocol: clientcontract.WebSocketSubprotocolV1,
			}
		}
		transports = append(transports, transport)
	}
	return Operation{
		ID: "watchItems",
		Contract: clientcontract.OperationV1{
			Stream:     clientcontract.StreamServer,
			Messages:   &clientcontract.MessageShapes{Output: &message},
			Transports: transports,
			Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
				AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}},
			}}},
			Errors:      []clientcontract.DeclaredError{},
			Idempotency: clientcontract.Idempotency{Kind: idempotency},
		},
		Successes: []OperationSuccess{{Status: http.StatusOK,
			Content: []OperationContent{{MediaType: "text/event-stream", Schema: &message}}}},
	}
}

// dualTransportServer answers both declared transports of one path: an ordinary
// HTTP request is the SSE half, an upgrade request is the WebSocket half. One
// origin serving both is what makes a fallback observable at all.
type dualTransportServer struct {
	*httptest.Server

	// peers records what the scripted provider side complained about, so a
	// harness fault is reported as one instead of surfacing as a client error.
	peers *wsTestServer

	mu       sync.Mutex
	sse      int
	upgrades int
	failures []string
}

type dualTransportOptions struct {
	// sseStatus answers the SSE half with an ordinary status. Zero streams the
	// scripted events instead.
	sseStatus int
	// sseBody is the body of that ordinary answer.
	sseBody string
	// sseEvents is the scripted SSE stream.
	sseEvents string
	// upgradeStatus refuses the upgrade with an ordinary status. Zero completes
	// the RFC 6455 handshake and plays the script.
	upgradeStatus int
	// subprotocol overrides the echoed token. "none" echoes nothing at all.
	subprotocol string
	// play scripts the framed conversation, once per accepted connection.
	play func(peer *wsTestPeer, connection int)
}

func newDualTransportServer(t *testing.T, options dualTransportOptions) *dualTransportServer {
	t.Helper()
	harness := &dualTransportServer{peers: &wsTestServer{}}
	harness.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Upgrade") == "" {
			harness.serveSSE(writer, options)
			return
		}
		harness.serveUpgrade(t, writer, request, options)
	}))
	t.Cleanup(harness.Close)
	return harness
}

func (harness *dualTransportServer) serveSSE(writer http.ResponseWriter, options dualTransportOptions) {
	harness.mu.Lock()
	harness.sse++
	harness.mu.Unlock()
	if options.sseStatus != 0 {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(options.sseStatus)
		_, _ = writer.Write([]byte(options.sseBody))
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(options.sseEvents))
}

func (harness *dualTransportServer) serveUpgrade(t *testing.T, writer http.ResponseWriter,
	request *http.Request, options dualTransportOptions) {
	harness.mu.Lock()
	harness.upgrades++
	connection := harness.upgrades
	harness.mu.Unlock()
	if options.upgradeStatus != 0 {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(options.upgradeStatus)
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		harness.fail("test server response does not support hijacking")
		return
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		harness.fail("hijack failed: " + err.Error())
		return
	}
	defer func() { _ = conn.Close() }()
	if err := harness.writeUpgrade(conn, request, options.subprotocol); err != nil {
		harness.fail("upgrade failed: " + err.Error())
		return
	}
	if options.play == nil {
		return
	}
	options.play(&wsTestPeer{
		t: t, server: harness.peers, request: request,
		conn: newWSConn(conn, buffered.Reader, wsRoleServer, 1<<20),
	}, connection)
}

func (harness *dualTransportServer) writeUpgrade(conn net.Conn, request *http.Request, subprotocol string) error {
	if subprotocol == "" {
		subprotocol = request.Header.Get("Sec-WebSocket-Protocol")
	}
	response := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + webSocketAcceptToken(request.Header.Get("Sec-WebSocket-Key")) + "\r\n"
	if subprotocol != "none" {
		response += "Sec-WebSocket-Protocol: " + subprotocol + "\r\n"
	}
	_, err := conn.Write([]byte(response + "\r\n"))
	return err
}

func (harness *dualTransportServer) fail(message string) {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	harness.failures = append(harness.failures, message)
}

func (harness *dualTransportServer) counts() (sse, upgrades int) {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return harness.sse, harness.upgrades
}

// assertNoFailures fails the test with every provider-side complaint, on both
// halves of the origin.
func (harness *dualTransportServer) assertNoFailures(t *testing.T) {
	t.Helper()
	harness.mu.Lock()
	for _, failure := range harness.failures {
		t.Errorf("provider: %s", failure)
	}
	harness.mu.Unlock()
	harness.peers.assertNoFailures(t)
}

// streamCall is the call a generated server-stream method hands the runtime:
// the REST projection plus the path parameters a Connect envelope would carry.
// The dispatcher, not the generated code, decides which wire takes it.
func streamCall() *OperationCall {
	return &OperationCall{Request: &Request{}}
}

// dualTransportClient binds a generated client to the dual-transport provider.
func dualTransportClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	})
	return boundTestClient(t, endpoint, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
}

// playServerStream answers the admission frame and delivers the scripted values
// on the WebSocket half.
func playServerStream(values ...string) func(peer *wsTestPeer, connection int) {
	return func(peer *wsTestPeer, _ int) {
		if peer.expect(clientcontract.WebSocketFrameInit) == nil {
			return
		}
		peer.send(webSocketReadyFrame(false, ""))
		for index, value := range values {
			peer.send(webSocketMessageFrame(fmt.Sprint(index+1), `{"value":"`+value+`"}`))
		}
		peer.send(webSocketResultFrame(""))
		peer.drain()
	}
}

// A provider that answers "this wire is not served here" on its first declared
// transport is not a provider that refused the call. The runtime opens the next
// declared transport, and the caller reads the same typed stream it would have
// read from the first one.
func TestServerStreamFallsBackWhenTheFirstDeclaredTransportIsNotServed(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-transport-preference",
		"a-transport-the-provider-does-not-serve-falls-back-to-the-next-declared-one")

	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUpgradeRequired} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := newDualTransportServer(t, dualTransportOptions{
				sseStatus: status,
				play:      playServerStream("one", "two"),
			})
			bound := dualTransportClient(t, server.URL)
			operation := declaredStreamOperation(
				[]clientcontract.TransportProtocol{clientcontract.TransportSSE, clientcontract.TransportWebSocket},
				clientcontract.IdempotencySafe)
			stream, err := OpenOperationServerStream[streamItem](t.Context(), bound, streamCall(), operation)
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
			sse, upgrades := server.counts()
			if sse != 1 || upgrades != 1 {
				t.Fatalf("attempts = %d sse, %d upgrades; want exactly one of each", sse, upgrades)
			}
		})
	}
}

// A refused credential says something about the call, not about the wire.
// Opening the next declared transport would only ask the same question again,
// so the typed refusal reaches the caller from the first transport.
func TestServerStreamDoesNotFallBackOnAnAnswerAboutTheCall(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-transport-preference",
		"an-answer-about-the-call-is-surfaced-instead-of-trying-another-wire")

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := newDualTransportServer(t, dualTransportOptions{
				sseStatus: status, sseBody: `{"code":"unauthorized","message":"no"}`,
				play: playServerStream("one"),
			})
			bound := dualTransportClient(t, server.URL)
			operation := declaredStreamOperation(
				[]clientcontract.TransportProtocol{clientcontract.TransportSSE, clientcontract.TransportWebSocket},
				clientcontract.IdempotencySafe)
			if _, err := OpenOperationServerStream[streamItem](t.Context(), bound, streamCall(), operation); err == nil {
				t.Fatal("a refused call opened a stream")
			}
			if sse, upgrades := server.counts(); sse != 1 || upgrades != 0 {
				t.Fatalf("attempts = %d sse, %d upgrades; want the second wire untouched", sse, upgrades)
			}
		})
	}
}

// A fallback is a second opening of the same operation. An operation the
// provider did not declare replayable gets exactly one, on its first declared
// transport, whatever the second one would have answered.
func TestServerStreamNeverReopensAnOperationTheProviderDidNotDeclareReplayable(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-transport-preference",
		"an-operation-that-is-not-declared-replayable-is-never-opened-twice")

	server := newDualTransportServer(t, dualTransportOptions{
		sseStatus: http.StatusNotFound,
		play:      playServerStream("one"),
	})
	bound := dualTransportClient(t, server.URL)
	operation := declaredStreamOperation(
		[]clientcontract.TransportProtocol{clientcontract.TransportSSE, clientcontract.TransportWebSocket},
		clientcontract.IdempotencyNonIdempotent)
	if _, err := OpenOperationServerStream[streamItem](t.Context(), bound, streamCall(), operation); err == nil {
		t.Fatal("a non-replayable operation fell back to a second transport")
	}
	if sse, upgrades := server.counts(); sse != 1 || upgrades != 0 {
		t.Fatalf("attempts = %d sse, %d upgrades; want exactly one opening", sse, upgrades)
	}
}

// The last declared transport has nothing to fall back to. Its own answer is
// what the caller reads, rather than an invented "everything failed" error that
// would hide which wire refused.
func TestServerStreamSurfacesTheLastAnswerWhenEveryDeclaredTransportIsAbsent(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-transport-preference",
		"an-exhausted-fallback-surfaces-the-answer-of-the-last-declared-transport")

	server := newDualTransportServer(t, dualTransportOptions{
		sseStatus: http.StatusNotFound, upgradeStatus: http.StatusNotFound,
	})
	bound := dualTransportClient(t, server.URL)
	operation := declaredStreamOperation(
		[]clientcontract.TransportProtocol{clientcontract.TransportSSE, clientcontract.TransportWebSocket},
		clientcontract.IdempotencySafe)
	_, err := OpenOperationServerStream[streamItem](t.Context(), bound, streamCall(), operation)
	if err == nil {
		t.Fatal("an absent operation opened a stream")
	}
	var remote *RemoteError
	if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusNotFound {
		t.Fatalf("terminal error = %v, want the last transport's own answer", err)
	}
	if sse, upgrades := server.counts(); sse != 1 || upgrades != 1 {
		t.Fatalf("attempts = %d sse, %d upgrades; want each declared transport tried once", sse, upgrades)
	}
}

// A provider that completes the handshake without selecting the first-party
// subprotocol serves WebSocket at this path, but not this wire. That is the
// same class as a 404, and the declared fallback acts on it.
func TestServerStreamFallsBackWhenTheProviderDoesNotNegotiateTheFirstPartyWire(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-transport-preference",
		"a-provider-that-does-not-negotiate-the-first-party-wire-falls-back")

	server := newDualTransportServer(t, dualTransportOptions{
		sseEvents:   "data: {\"value\":\"one\"}\n\n",
		subprotocol: "none",
	})
	bound := dualTransportClient(t, server.URL)
	operation := declaredStreamOperation(
		[]clientcontract.TransportProtocol{clientcontract.TransportWebSocket, clientcontract.TransportSSE},
		clientcontract.IdempotencySafe)
	stream, err := OpenOperationServerStream[streamItem](t.Context(), bound, streamCall(), operation)
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 1)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(values) != "[one]" {
		t.Fatalf("stream values = %v", values)
	}
	if sse, upgrades := server.counts(); sse != 1 || upgrades != 1 {
		t.Fatalf("attempts = %d sse, %d upgrades; want the declared order followed once each", sse, upgrades)
	}
}

// The declared order is the dispatch order and nothing else decides. The same
// operation, declared the other way round, opens the other wire first.
func TestServerStreamOpensTheFirstDeclaredTransport(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-transport-preference",
		"the-declared-order-is-the-dispatch-order")

	cases := []struct {
		name         string
		order        []clientcontract.TransportProtocol
		wantSSE      int
		wantUpgrades int
	}{
		{
			name:    "sse declared first",
			order:   []clientcontract.TransportProtocol{clientcontract.TransportSSE, clientcontract.TransportWebSocket},
			wantSSE: 1,
		},
		{
			name:         "websocket declared first",
			order:        []clientcontract.TransportProtocol{clientcontract.TransportWebSocket, clientcontract.TransportSSE},
			wantUpgrades: 1,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newDualTransportServer(t, dualTransportOptions{
				sseEvents: "data: {\"value\":\"one\"}\n\n",
				play:      playServerStream("one"),
			})
			bound := dualTransportClient(t, server.URL)
			operation := declaredStreamOperation(testCase.order, clientcontract.IdempotencySafe)
			stream, err := OpenOperationServerStream[streamItem](t.Context(), bound, streamCall(), operation)
			if err != nil {
				t.Fatal(err)
			}
			values := make([]string, 0, 1)
			for message := range stream.Messages() {
				values = append(values, message.Value)
			}
			if err := stream.Err(); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(values) != "[one]" {
				t.Fatalf("stream values = %v", values)
			}
			sse, upgrades := server.counts()
			if sse != testCase.wantSSE || upgrades != testCase.wantUpgrades {
				t.Fatalf("attempts = %d sse, %d upgrades; want %d and %d",
					sse, upgrades, testCase.wantSSE, testCase.wantUpgrades)
			}
		})
	}
}

// Once the provider has admitted the call and delivered a value, there is no
// fallback left: the next declared transport would deliver that value a second
// time. A break after admission is a terminal, not a reason to try another wire.
func TestServerStreamNeverFallsBackAfterAMessageWasDelivered(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-transport-preference",
		"a-break-after-admission-never-opens-another-declared-transport")

	server := newDualTransportServer(t, dualTransportOptions{
		play: func(peer *wsTestPeer, _ int) {
			if peer.expect(clientcontract.WebSocketFrameInit) == nil {
				return
			}
			peer.send(webSocketReadyFrame(false, ""))
			peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
			// The socket ends without a terminal frame. Resume is not declared,
			// so this is where the stream stops.
		},
	})
	bound := dualTransportClient(t, server.URL)
	operation := declaredStreamOperation(
		[]clientcontract.TransportProtocol{clientcontract.TransportWebSocket, clientcontract.TransportSSE},
		clientcontract.IdempotencySafe)
	stream, err := OpenOperationServerStream[streamItem](t.Context(), bound, streamCall(), operation)
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
		t.Fatal("a socket that ended without a terminal frame reported success")
	}
	if sse, upgrades := server.counts(); sse != 0 || upgrades != 1 {
		t.Fatalf("attempts = %d sse, %d upgrades; want the second wire untouched", sse, upgrades)
	}
}

// A provider that permanently does not serve one of its declared wires must not
// cost the caller the wire that does work. Each attempt records its own
// pre-admission verdict, and the admission of the transport that answers resets
// the consecutive count, so a fallback repeated past any threshold keeps
// working instead of opening the circuit for the whole operation.
func TestRepeatedFallbackKeepsWorkingInsteadOfOpeningTheCircuit(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-transport-preference",
		"a-repeated-fallback-does-not-open-the-circuit-for-the-operation")

	server := newDualTransportServer(t, dualTransportOptions{
		sseEvents:   "data: {\"value\":\"one\"}\n\n",
		subprotocol: "none",
	})
	bound := dualTransportClient(t, server.URL)
	operation := declaredStreamOperation(
		[]clientcontract.TransportProtocol{clientcontract.TransportWebSocket, clientcontract.TransportSSE},
		clientcontract.IdempotencySafe)
	// The framework circuit opens after five consecutive failures, so six
	// fallbacks in a row is past the threshold a silent declaration holds.
	for attempt := range 6 {
		stream, err := OpenOperationServerStream[streamItem](t.Context(), bound, streamCall(), operation)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
		values := make([]string, 0, 1)
		for message := range stream.Messages() {
			values = append(values, message.Value)
		}
		if err := stream.Err(); err != nil {
			t.Fatalf("attempt %d terminal: %v", attempt+1, err)
		}
		if fmt.Sprint(values) != "[one]" {
			t.Fatalf("attempt %d values = %v", attempt+1, values)
		}
	}
	if sse, upgrades := server.counts(); sse != 6 || upgrades != 6 {
		t.Fatalf("attempts = %d sse, %d upgrades; want six fallbacks, none refused by the circuit", sse, upgrades)
	}
}

// A declared shape this runtime cannot carry at all is refused before any
// socket opens, naming the operation.
func TestServerStreamRefusesADeclarationWithNoCarriableTransport(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-transport-preference",
		"a-declaration-with-no-carriable-transport-is-refused-before-any-socket")

	server := newDualTransportServer(t, dualTransportOptions{})
	bound := dualTransportClient(t, server.URL)
	operation := declaredStreamOperation(
		[]clientcontract.TransportProtocol{clientcontract.TransportRESTJSON}, clientcontract.IdempotencySafe)
	_, err := OpenOperationServerStream[streamItem](t.Context(), bound, streamCall(), operation)
	if err == nil {
		t.Fatal("a declaration with no carriable transport opened a stream")
	}
	if sse, upgrades := server.counts(); sse != 0 || upgrades != 0 {
		t.Fatalf("attempts = %d sse, %d upgrades; want nothing on the wire", sse, upgrades)
	}
}
