package client

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// sseLogEntry is one message of the log tail: the line and the provider's
// position after it.
type sseLogEntry struct {
	Cursor string `json:"cursor"`
	Line   string `json:"line"`
}

// durableSSELog is the event log two provider instances share. It is the only
// thing they share: an instance keeps nothing about a stream it served, so a
// continuation that lands on the other instance is placed by the cursor alone.
// Position "c<n>" is the position after entry n.
type durableSSELog struct {
	mu      sync.Mutex
	lines   []string
	closed  bool
	floor   int
	changed chan struct{}
}

func newDurableSSELog() *durableSSELog {
	return &durableSSELog{changed: make(chan struct{})}
}

func (log *durableSSELog) append(lines ...string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.lines = append(log.lines, lines...)
	close(log.changed)
	log.changed = make(chan struct{})
}

// end marks the log complete: a stream that reaches its end completes.
func (log *durableSSELog) end() {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.closed = true
	close(log.changed)
	log.changed = make(chan struct{})
}

// retainFrom drops the positions before floor, so a cursor older than it is
// refused.
func (log *durableSSELog) retainFrom(floor int) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.floor = floor
}

// position resolves a cursor the provider issued. The empty cursor is the
// start; anything the log did not issue, or no longer retains, is refused.
func (log *durableSSELog) position(cursor string) (int, bool) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if cursor == "" {
		return 0, true
	}
	index, err := strconv.Atoi(strings.TrimPrefix(cursor, "c"))
	if !strings.HasPrefix(cursor, "c") || err != nil || index < 1 || index > len(log.lines) || index < log.floor {
		return 0, false
	}
	return index, true
}

func (log *durableSSELog) length() int {
	log.mu.Lock()
	defer log.mu.Unlock()
	return len(log.lines)
}

// next waits for the entry after position after. It reports the end of the log
// or, with ok false, a context that ended first.
func (log *durableSSELog) next(ctx context.Context, after int) (entry sseLogEntry, end, ok bool) {
	for {
		log.mu.Lock()
		if after < len(log.lines) {
			entry = sseLogEntry{Cursor: fmt.Sprintf("c%d", after+1), Line: log.lines[after]}
			log.mu.Unlock()
			return entry, false, true
		}
		if log.closed {
			log.mu.Unlock()
			return sseLogEntry{}, true, true
		}
		changed := log.changed
		log.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return sseLogEntry{}, false, false
		}
	}
}

// sseProviderInstance is one provider instance: the real go.putnami.dev/http
// server stream route with the negotiated wire the api plugin fills from a
// continuation declaration.
type sseProviderInstance struct {
	plugin   *phttp.ServerPlugin
	handler  http.Handler
	requests sseRequestLog
	sent     atomic.Int32
}

// newSSEProviderInstance serves the log. A cursor-mode instance continues
// exclusively after the cursor it is given; a best-effort one tails the live
// log from the moment the connection opens, ignoring any position.
func newSSEProviderInstance(log *durableSSELog, live bool) *sseProviderInstance {
	instance := &sseProviderInstance{plugin: phttp.NewServerPlugin(phttp.ServerConfig{})}
	instance.plugin.HandleStream("/logs/tail", phttp.StreamHandler{
		Mode: phttp.StreamModeServer,
		SSEWire: &phttp.SSEWire{
			Header: clientcontract.SSEWireHeader, Token: clientcontract.SSEWireV1,
			Negotiates: clientcontract.NegotiatesSSEWire, Complete: clientcontract.SSECompleteFrame,
		},
		Handle: func(stream *phttp.StreamContext) error {
			// The start is fixed before the request is observable, so a test
			// that waits for the request knows what the stream starts after.
			after := log.length()
			position, known := log.position(stream.Query("cursor"))
			instance.requests.record(stream.Request)
			if !live {
				if !known {
					return perrors.BadRequest("the position is not in the retained log")
				}
				after = position
			}
			for {
				entry, end, ok := log.next(stream.Context.Context(), after)
				if !ok || end {
					// A drained or abandoned stream returns with no terminal; a
					// finished log returns and the wire writes `complete`.
					return nil
				}
				if err := stream.Send(entry); err != nil {
					return err
				}
				instance.sent.Add(1)
				after++
			}
		},
	})
	instance.handler = instance.plugin.Handler()
	return instance
}

// drain is the instance replacement a continuation exists for: the server's
// graceful stop ends every negotiated stream with no terminal.
func (instance *sseProviderInstance) drain(t *testing.T) {
	t.Helper()
	if err := instance.plugin.Stop(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}

// sseFront routes every connection to the current instance, the way a load
// balancer does. It is the one double in these tests: the routing boundary.
type sseFront struct {
	*httptest.Server
	current atomic.Pointer[sseProviderInstance]
}

func newSSEFront(t *testing.T, first *sseProviderInstance) *sseFront {
	t.Helper()
	front := &sseFront{}
	front.current.Store(first)
	front.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		front.current.Load().handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(front.Close)
	return front
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func receiveEntry(t *testing.T, stream *Stream[sseLogEntry]) sseLogEntry {
	t.Helper()
	select {
	case entry, ok := <-stream.Messages():
		if !ok {
			t.Fatalf("the stream ended early: %v", stream.Err())
		}
		return entry
	case <-time.After(5 * time.Second):
		t.Fatal("no message reached the caller")
		return sseLogEntry{}
	}
}

func serviceSecurity() clientcontract.Security {
	return clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
		AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}},
	}}}
}

func maxBuffered(operation Operation, limit int) Operation {
	operation.Contract.Resilience.Stream.MaxBufferedMessages = &limit
	return operation
}

// TestACursorStreamContinuesOnAnotherInstanceAfterTheLastPositionTheCallerReceived
// is the delivery-api shape: a real provider instance is replaced while the
// caller is paused with three values decoded and queued but not received. The
// session reopens on the other instance, with a credential renewed at the
// instant it dials, after the position the caller did receive — so the queued
// values arrive once, from the second instance, and nothing is skipped.
func TestACursorStreamContinuesOnAnotherInstanceAfterTheLastPositionTheCallerReceived(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"a-cursor-stream-continues-on-another-instance-after-the-last-position-the-caller-received")
	observer := &recordingServiceTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()

	log := newDurableSSELog()
	first, second := newSSEProviderInstance(log, false), newSSEProviderInstance(log, false)
	front := newSSEFront(t, first)
	clock := newTestClock()
	var issued atomic.Int32
	bound := resumeClientWithClock(t, front.URL, clock, &issued)
	operation := maxBuffered(sseLogOperation(cursorContinuation(), 4096, true, serviceSecurity()), 4)
	stream, err := OpenServerStream[sseLogEntry](t.Context(), bound,
		&Request{QueryValues: url.Values{"selector": {"svc=api"}}}, operation)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	log.append("one", "two")
	received := make([]sseLogEntry, 0, 6)
	received = append(received, receiveEntry(t, stream), receiveEntry(t, stream))
	log.append("three", "four", "five")
	// The first instance wrote them, so the reader decodes and queues them
	// before it reaches the end of the body; the caller takes none of them.
	eventually(t, "the first instance to write five entries", func() bool { return first.sent.Load() == 5 })

	clock.advance(2 * time.Hour)
	front.current.Store(second)
	first.drain(t)
	eventually(t, "the continuation to reach the second instance", func() bool { return len(second.requests.all()) == 1 })

	log.append("six")
	log.end()
	for entry := range stream.Messages() {
		received = append(received, entry)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("the continued stream ended with %v", err)
	}
	want := []sseLogEntry{{"c1", "one"}, {"c2", "two"}, {"c3", "three"}, {"c4", "four"}, {"c5", "five"}, {"c6", "six"}}
	if !reflect.DeepEqual(received, want) {
		t.Fatalf("received %v\nwant %v: a queued value was skipped or arrived twice", received, want)
	}

	opened, continued := first.requests.all(), second.requests.all()
	if len(opened) != 1 || len(continued) != 1 {
		t.Fatalf("connections = %d on the first instance and %d on the second, want one each", len(opened), len(continued))
	}
	if want := (url.Values{"selector": {"svc=api"}}); !reflect.DeepEqual(opened[0].query, want) {
		t.Fatalf("first query = %v, want %v", opened[0].query, want)
	}
	// c2 is the last position the caller received; c3 to c5 were only queued.
	if want := (url.Values{"selector": {"svc=api"}, "cursor": {"c2"}}); !reflect.DeepEqual(continued[0].query, want) {
		t.Fatalf("continuation query = %v, want %v", continued[0].query, want)
	}
	if opened[0].authorization != "Bearer token-1" || continued[0].authorization != "Bearer token-2" {
		t.Fatalf("authorizations = %q then %q; the continuation must carry a credential resolved when it dials",
			opened[0].authorization, continued[0].authorization)
	}
	for _, request := range append(opened, continued...) {
		if !clientcontract.NegotiatesSSEWire(request.wire) {
			t.Fatalf("a connection did not ask for the negotiated wire: %q", request.wire)
		}
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.calls) != 1 || observer.calls[0].Code != "" || observer.calls[0].Attempts != 2 {
		t.Fatalf("call measurements = %#v, want one successful call over two attempts", observer.calls)
	}
}

// TestABestEffortStreamReopensTheOriginalQueryAndKeepsItsQueue is the
// observability-api shape: the reopened connection carries the original
// selector and no position, and the values queued before the break still
// reach the caller, before the ones the reopened connection brings.
func TestABestEffortStreamReopensTheOriginalQueryAndKeepsItsQueue(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"a-best-effort-stream-reopens-the-original-query-and-keeps-its-queue")
	log := newDurableSSELog()
	first, second := newSSEProviderInstance(log, true), newSSEProviderInstance(log, true)
	front := newSSEFront(t, first)
	bound := resumeClientWithClock(t, front.URL, newTestClock(), new(atomic.Int32))
	operation := sseLogOperation(bestEffortContinuation(), 4096, true, serviceSecurity())
	stream, err := OpenServerStream[sseLogEntry](t.Context(), bound,
		&Request{QueryValues: url.Values{"selector": {"svc=api"}}}, operation)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	eventually(t, "the first connection", func() bool { return len(first.requests.all()) == 1 })

	log.append("a1", "a2", "a3")
	received := make([]string, 0, 4)
	received = append(received, receiveEntry(t, stream).Line)
	eventually(t, "the first instance to write three entries", func() bool { return first.sent.Load() == 3 })
	front.current.Store(second)
	first.drain(t)
	eventually(t, "the reopening to reach the second instance", func() bool { return len(second.requests.all()) == 1 })
	log.append("b1")
	log.end()
	for entry := range stream.Messages() {
		received = append(received, entry.Line)
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a1", "a2", "a3", "b1"}; !reflect.DeepEqual(received, want) {
		t.Fatalf("received %v, want %v", received, want)
	}
	if want := (url.Values{"selector": {"svc=api"}}); !reflect.DeepEqual(second.requests.all()[0].query, want) {
		t.Fatalf("reopened query = %v, want the original %v and no position", second.requests.all()[0].query, want)
	}
}

// TestATypedRefusalOfAPositionEndsTheSessionWithoutAnotherReopening proves a
// provider's refusal of a cursor it no longer retains is the session's typed
// terminal: nothing restarts the stream from the beginning.
func TestATypedRefusalOfAPositionEndsTheSessionWithoutAnotherReopening(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"a-typed-refusal-of-a-position-ends-the-session-without-another-reopening")
	log := newDurableSSELog()
	first, second := newSSEProviderInstance(log, false), newSSEProviderInstance(log, false)
	front := newSSEFront(t, first)
	bound := resumeClientWithClock(t, front.URL, newTestClock(), new(atomic.Int32))
	stream, err := OpenServerStream[sseLogEntry](t.Context(), bound, &Request{},
		sseLogOperation(cursorContinuation(), 4096, true, serviceSecurity()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	log.append("one", "two", "three")
	for range 3 {
		receiveEntry(t, stream)
	}
	log.retainFrom(5)
	front.current.Store(second)
	first.drain(t)
	for range stream.Messages() {
		t.Fatal("a refused continuation delivered a value")
	}
	var remote *RemoteError
	if err := stream.Err(); !stderrors.As(err, &remote) || remote.StatusCode != http.StatusBadRequest {
		t.Fatalf("terminal = %T %v, want the provider's typed refusal", err, err)
	}
	if got := second.requests.all(); len(got) != 1 || got[0].query.Get("cursor") != "c3" {
		t.Fatalf("continuations = %#v, want exactly one, after c3", got)
	}
}

// TestANegotiatedStreamThatKeepsBreakingStopsAtTheFrameworkBound proves the
// five-continuation cap is shared with WebSocket resume: the sixth break ends
// the session with the break as its error, over one call measurement.
func TestANegotiatedStreamThatKeepsBreakingStopsAtTheFrameworkBound(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"a-negotiated-stream-that-keeps-breaking-stops-at-the-framework-bound")
	observer := &recordingServiceTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()
	requests := &sseRequestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := requests.record(request)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set(clientcontract.SSEWireHeader, clientcontract.SSEWireV1)
		writer.WriteHeader(http.StatusOK)
		// Every connection ends before a terminal.
		writeSSE(t, writer, fmt.Appendf(nil, "data: {\"cursor\":\"c%d\",\"line\":\"x\"}\n\n", connection))
	}))
	defer server.Close()
	bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	stream, err := OpenServerStream[sseLogEntry](t.Context(), bound, &Request{},
		sseLogOperation(cursorContinuation(), 4096, true, anonymousSecurity()))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() { //nolint:revive // the bound is asserted through the connections
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientResponse) || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("terminal = %v, want the sixth break", err)
	}
	if got := len(requests.all()); got != 1+maxStreamResumeAttempts {
		t.Fatalf("connections = %d, want the first and %d continuations", got, maxStreamResumeAttempts)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.calls) != 1 || observer.calls[0].Attempts != 1+maxStreamResumeAttempts {
		t.Fatalf("call measurements = %#v, want one call over six attempts", observer.calls)
	}
}

// forwardedUserStreamClient binds a client whose only credential is the
// forwarded user's, with the binding's one-shot re-mint.
func forwardedUserStreamClient(t *testing.T, endpoint string, refreshes *atomic.Int32) *Client {
	t.Helper()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"caller": {Kind: clientcontract.CredentialForwardedUserToken}})
	return boundTestClient(t, endpoint, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"caller": {
			Source: CredentialSourceForwardedUser,
			Refresh: func(context.Context) (string, error) {
				return fmt.Sprintf("user-%d", refreshes.Add(1)+1), nil
			},
		},
	}})
}

func callerSecurity() clientcontract.Security {
	return clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
		AllOf: []clientcontract.SecurityRequirement{{Profile: "caller"}},
	}}}
}

// TestAReopeningRefusedWith401ReMintsTheForwardedUserCredentialOnce proves a
// reopening reuses the bounded forwarded-user refresh a unary call has — one
// re-mint, then the provider's refusal stands — and that the first opening
// keeps its existing behavior of never re-minting.
func TestAReopeningRefusedWith401ReMintsTheForwardedUserCredentialOnce(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"every-reopening-resolves-credentials-and-a-refused-one-re-mints-a-forwarded-user-credential-once")
	unauthorized := func(writer http.ResponseWriter) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, `{"code":"unauthorized","error":"Unauthorized","message":"expired"}`)
	}
	admit := func(writer http.ResponseWriter, body string) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set(clientcontract.SSEWireHeader, clientcontract.SSEWireV1)
		writer.WriteHeader(http.StatusOK)
		writeSSE(t, writer, []byte(body))
	}
	cases := map[string]struct {
		accepts      func(connection int, authorization string) bool
		refreshes    int32
		connections  int
		wantTerminal func(error) bool
	}{
		"the re-minted credential continues the stream": {
			accepts: func(connection int, authorization string) bool {
				return authorization == "Bearer user-2" || connection == 1
			},
			refreshes:   1,
			connections: 3,
			wantTerminal: func(err error) bool {
				return err == nil
			},
		},
		"a second refusal after the re-mint stands": {
			accepts:     func(connection int, _ string) bool { return connection == 1 },
			refreshes:   1,
			connections: 3,
			wantTerminal: func(err error) bool {
				var remote *RemoteError
				return stderrors.As(err, &remote) && remote.StatusCode == http.StatusUnauthorized
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// The first connection breaks once the caller took c1, so every
			// reopening continues after it.
			taken := make(chan struct{})
			requests := &sseRequestLog{}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				connection := requests.record(request)
				switch {
				case !tc.accepts(connection, request.Header.Get("Authorization")):
					unauthorized(writer)
				case connection == 1:
					admit(writer, "data: {\"cursor\":\"c1\",\"line\":\"one\"}\n\n")
					select {
					case <-taken:
					case <-request.Context().Done():
					}
				default:
					admit(writer, clientcontract.SSECompleteFrame)
				}
			}))
			defer server.Close()
			var refreshes atomic.Int32
			bound := forwardedUserStreamClient(t, server.URL, &refreshes)
			ctx := WithForwardedUserToken(t.Context(), "user-1")
			stream, err := OpenServerStream[sseLogEntry](ctx, bound, &Request{},
				sseLogOperation(cursorContinuation(), 4096, true, callerSecurity()))
			if err != nil {
				t.Fatal(err)
			}
			if entry := receiveEntry(t, stream); entry.Cursor != "c1" {
				t.Fatalf("first entry = %#v", entry)
			}
			close(taken)
			for range stream.Messages() { //nolint:revive // the outcome is asserted through the connections
			}
			if !tc.wantTerminal(stream.Err()) {
				t.Fatalf("terminal = %T %v", stream.Err(), stream.Err())
			}
			if got := refreshes.Load(); got != tc.refreshes {
				t.Fatalf("re-mints = %d, want %d", got, tc.refreshes)
			}
			seen := requests.all()
			if len(seen) != tc.connections {
				t.Fatalf("connections = %d, want %d", len(seen), tc.connections)
			}
			if seen[1].authorization != "Bearer user-1" || seen[2].authorization != "Bearer user-2" {
				t.Fatalf("authorizations = %q, %q; want the caller's token, then the re-minted one", seen[1].authorization, seen[2].authorization)
			}
			if seen[1].query.Get("cursor") != "c1" || seen[2].query.Get("cursor") != "c1" {
				t.Fatalf("the re-minted opening lost its position: %v then %v", seen[1].query, seen[2].query)
			}
		})
	}

	t.Run("the first opening never re-mints", func(t *testing.T) {
		requests := &sseRequestLog{}
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.record(request)
			unauthorized(writer)
		}))
		defer server.Close()
		var refreshes atomic.Int32
		bound := forwardedUserStreamClient(t, server.URL, &refreshes)
		_, err := OpenServerStream[sseLogEntry](WithForwardedUserToken(t.Context(), "user-1"), bound, &Request{},
			sseLogOperation(cursorContinuation(), 4096, true, callerSecurity()))
		var remote *RemoteError
		if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
			t.Fatalf("open error = %T %v, want the provider's 401", err, err)
		}
		if refreshes.Load() != 0 || len(requests.all()) != 1 {
			t.Fatalf("re-mints = %d over %d connections, want none over one", refreshes.Load(), len(requests.all()))
		}
	})
}

// TestAProviderThatDoesNotAcknowledgeTheWireIsRefusedWithoutFallback proves a
// new runtime never reads an old provider's end of body as resumable, and never
// falls back to another declared transport because of it.
func TestAProviderThatDoesNotAcknowledgeTheWireIsRefusedWithoutFallback(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"a-provider-that-does-not-acknowledge-the-wire-is-refused-without-fallback")
	requests := &sseRequestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.record(request)
		// An old provider: the legacy framing, no acknowledgment.
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writeSSE(t, writer, []byte("data: {\"cursor\":\"c1\",\"line\":\"one\"}\n\n"))
	}))
	defer server.Close()
	operation := sseLogOperation(cursorContinuation(), 4096, true, anonymousSecurity())
	// A declared-replayable operation that also declares a WebSocket: the
	// dispatcher may fall back, but only when the provider says a wire is
	// absent. A missing acknowledgment says the provider is too old.
	operation.Contract.Transports = append(operation.Contract.Transports, clientcontract.Transport{
		Protocol: clientcontract.TransportWebSocket, Path: "/logs/tail", Encoding: clientcontract.EncodingJSON,
		WebSocket: &clientcontract.WebSocketTransport{Subprotocol: clientcontract.WebSocketSubprotocolV1},
	})
	bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	_, err := OpenOperationServerStream[sseLogEntry](t.Context(), bound, &OperationCall{Request: &Request{}}, operation)
	if err == nil || !perrors.Is(err, CodeClientResponse) || !strings.Contains(err.Error(), clientcontract.SSEWireV1) {
		t.Fatalf("open error = %v, want the unacknowledged wire refused", err)
	}
	if streamTransportUnavailable(err) {
		t.Fatal("a missing acknowledgment was classified as an absent wire, which a fallback acts on")
	}
	if got := requests.all(); len(got) != 1 {
		t.Fatalf("requests = %d, want the one SSE opening and no fallback", len(got))
	}
}

// TestClosingAReopeningStreamReleasesItsConnectionAndEmitsOneMeasurement
// proves a caller that closes a stream while its continuation is still in the
// handshake releases that connection and ends the session once.
func TestClosingAReopeningStreamReleasesItsConnectionAndEmitsOneMeasurement(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"closing-a-reopening-stream-releases-its-connection-and-emits-one-measurement")
	observer := &recordingServiceTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()
	taken := make(chan struct{})
	reopening := make(chan struct{})
	released := make(chan struct{})
	requests := &sseRequestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requests.record(request) == 1 {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header().Set(clientcontract.SSEWireHeader, clientcontract.SSEWireV1)
			writer.WriteHeader(http.StatusOK)
			writeSSE(t, writer, []byte("data: {\"cursor\":\"c1\",\"line\":\"one\"}\n\n"))
			select {
			case <-taken:
			case <-request.Context().Done():
			}
			return
		}
		// The continuation's handshake never completes on its own.
		close(reopening)
		<-request.Context().Done()
		close(released)
	}))
	defer server.Close()
	bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	stream, err := OpenServerStream[sseLogEntry](t.Context(), bound, &Request{},
		sseLogOperation(cursorContinuation(), 4096, true, anonymousSecurity()))
	if err != nil {
		t.Fatal(err)
	}
	if entry := receiveEntry(t, stream); entry.Cursor != "c1" {
		t.Fatalf("first entry = %#v", entry)
	}
	close(taken)
	select {
	case <-reopening:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reopened")
	}
	_ = stream.Close()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the stream left the continuation's connection open")
	}
	for range stream.Messages() { //nolint:revive // draining to the close
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientCanceled) {
		t.Fatalf("terminal = %v, want %s", err, CodeClientCanceled)
	}
	eventually(t, "the single call measurement", func() bool {
		observer.mu.Lock()
		defer observer.mu.Unlock()
		return len(observer.calls) == 1
	})
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.calls[0].Code != string(CodeClientCanceled) || observer.calls[0].Attempts != 2 {
		t.Fatalf("call measurement = %#v, want one canceled call over two attempts", observer.calls[0])
	}
}

// TestAReopeningNeverResetsTheIdleBudget proves the declared idle budget runs
// across every connection of one session (clientcontract ADR 0013 §5): the
// admission of a reopening is not stream activity. Every reopening is admitted
// and acknowledged, stays silent for half the budget and breaks, so five of
// them outlast it; had an admission reset the budget, the session would have
// reached the sixth break instead of its idle deadline.
func TestAReopeningNeverResetsTheIdleBudget(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"a-reopening-never-resets-the-idle-budget")
	const idle, hold = 300 * time.Millisecond, 150 * time.Millisecond
	taken := make(chan struct{})
	requests := &sseRequestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := requests.record(request)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set(clientcontract.SSEWireHeader, clientcontract.SSEWireV1)
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		if connection == 1 {
			// The break waits for the caller to take c1, so the session has a
			// delivered position and the idle budget started from that read.
			writeSSE(t, writer, []byte("data: {\"cursor\":\"c1\",\"line\":\"one\"}\n\n"))
			select {
			case <-taken:
			case <-request.Context().Done():
			}
			return
		}
		select {
		case <-time.After(hold):
		case <-request.Context().Done():
		}
	}))
	defer server.Close()
	operation := sseLogOperation(cursorContinuation(), 4096, true, anonymousSecurity())
	idleMs := int(idle / time.Millisecond)
	operation.Contract.Resilience.Stream.IdleTimeoutMs = &idleMs
	bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	stream, err := OpenServerStream[sseLogEntry](t.Context(), bound, &Request{}, operation)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if entry := receiveEntry(t, stream); entry.Cursor != "c1" {
		t.Fatalf("first entry = %#v", entry)
	}
	close(taken)
	for entry := range stream.Messages() {
		t.Fatalf("a silent reopening delivered %#v", entry)
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("terminal = %v, want the idle deadline", err)
	}
	if got := len(requests.all()); got >= 1+maxStreamResumeAttempts {
		t.Fatalf("connections = %d: a reopening reset the idle budget", got)
	}
}

// TestAReopeningHasItsOwnBoundedHandshakeAndItsFailureEndsTheSession proves
// each connection gets its own bounded handshake: a reopening whose provider
// never answers is released by the declared handshake budget, and that failed
// reopening ends the session instead of reopening again.
func TestAReopeningHasItsOwnBoundedHandshakeAndItsFailureEndsTheSession(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"a-reopening-has-its-own-bounded-handshake-and-its-failure-ends-the-session")
	taken := make(chan struct{})
	requests := &sseRequestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requests.record(request) == 1 {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header().Set(clientcontract.SSEWireHeader, clientcontract.SSEWireV1)
			writer.WriteHeader(http.StatusOK)
			writeSSE(t, writer, []byte("data: {\"cursor\":\"c1\",\"line\":\"one\"}\n\n"))
			select {
			case <-taken:
			case <-request.Context().Done():
			}
			return
		}
		// The reopening's provider never answers.
		<-request.Context().Done()
	}))
	defer server.Close()
	operation := sseLogOperation(cursorContinuation(), 4096, true, anonymousSecurity())
	// The SSE handshake budget is the declared attempt timeout in both runtimes.
	handshakeMs := 200
	operation.Contract.Resilience.AttemptTimeoutMs = &handshakeMs
	bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	stream, err := OpenServerStream[sseLogEntry](t.Context(), bound, &Request{}, operation)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if entry := receiveEntry(t, stream); entry.Cursor != "c1" {
		t.Fatalf("first entry = %#v", entry)
	}
	close(taken)
	for entry := range stream.Messages() {
		t.Fatalf("a reopening that never answered delivered %#v", entry)
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("terminal = %v, want the reopening's handshake deadline", err)
	}
	if got := len(requests.all()); got != 2 {
		t.Fatalf("connections = %d, want the first and one reopening", got)
	}
}
