package client

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

type streamItem struct {
	Value string `json:"value"`
}

func serverStreamOperation(policy *clientcontract.ResiliencePolicy) Operation {
	message := clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"value": {Type: "string"}},
		Required: []string{"value"}, AdditionalProperties: additionalForbidden(),
	}
	return Operation{
		ID: "watchItems",
		Contract: clientcontract.OperationV1{
			Stream:     clientcontract.StreamServer,
			Messages:   &clientcontract.MessageShapes{Output: &message},
			Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportSSE, Path: "/items/watch", Encoding: clientcontract.EncodingJSON}},
			Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
				AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}},
			}}},
			Errors:      []clientcontract.DeclaredError{},
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
			Resilience:  policy,
		},
		Successes: []OperationSuccess{{Status: http.StatusOK, Content: []OperationContent{{MediaType: "text/event-stream", Schema: &message}}}},
	}
}

func TestOpenServerStreamAppliesBindingAndValidatesEverySSEMessage(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-server-streams", "every-sse-message-is-validated-against-the-declared-output-schema")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer stream-token" || request.Header.Get("X-Client-Id") != "consumer.workload" {
			t.Errorf("identity headers = %#v", request.Header)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("data: {\"value\":\"one\"}\n\ndata: {\"value\":\"two\"}\n\n"))
	}))
	defer server.Close()

	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, serverStreamOperation(nil))
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
}

func TestOpenServerStreamRejectsMalformedAndOversizedSSEMessages(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		max     int64
	}{
		{name: "schema", payload: "data: {\"other\":true}\n\n", max: 1024},
		{name: "frame", payload: "data: {\"value\":\"too-long\"}\n\n", max: 16},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = writer.Write([]byte(test.payload))
			}))
			defer server.Close()
			policy := &clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{MaxFrameBytes: int64Pointer(test.max)}}
			descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
			bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
				"service": {Provider: freshSource("stream-token", nil)},
			}})
			stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, serverStreamOperation(policy))
			if err != nil {
				t.Fatal(err)
			}
			for range stream.Messages() {
			}
			if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientResponse) {
				t.Fatalf("terminal error = %T %v", err, err)
			}
		})
	}
}

func TestOpenServerStreamProjectsTypedTerminalError(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-server-streams", "a-terminal-stream-error-is-projected-as-a-typed-remote-error")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		// The flat terminal frame both providers write: the first-party error
		// envelope plus status, with `details` holding the declared body alone.
		_, _ = writer.Write([]byte("event: error\ndata: {\"status\":404,\"code\":\"errors.not_found\",\"error\":\"Not Found\",\"message\":\"missing\",\"details\":{\"reason\":\"missing\",\"secret\":\"drop\"}}\n\n"))
	}))
	defer server.Close()
	errorSchema := clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{
			"reason": {Type: "string"}, "secret": {Type: "string"},
		},
		Required: []string{"reason"}, AdditionalProperties: additionalForbidden(),
	}
	operation := serverStreamOperation(nil)
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: 404, Code: "errors.not_found", Schema: &errorSchema}}
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	var remote *RemoteError
	if !stderrors.As(stream.Err(), &remote) || remote.Code() != "errors.not_found" {
		t.Fatalf("terminal error = %T %v", stream.Err(), stream.Err())
	}
	if got := string(remote.Payload); got != `{"reason":"missing"}` || strings.Contains(got, "secret") {
		t.Fatalf("terminal payload = %s", got)
	}
	// The binding did not opt in, so the frame's prose stays off the error.
	if remote.Message != "" {
		t.Fatalf("terminal message = %q, want no provider prose without the opt-in", remote.Message)
	}
}

// TestOpenServerStreamCarriesTheTerminalMessageWhenTheBindingOptsIn proves the
// binding's opt-in reaches the SSE reader through the live stream path, not
// just decodeSSEError's argument.
func TestOpenServerStreamCarriesTheTerminalMessageWhenTheBindingOptsIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: error\ndata: " + sseTerminalFixture + "\n\n"))
	}))
	defer server.Close()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{CarryRemoteMessage: true, Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, sseTerminalOperation())
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	var remote *RemoteError
	if !stderrors.As(stream.Err(), &remote) || remote.Code() != "not_found" {
		t.Fatalf("terminal error = %T %v", stream.Err(), stream.Err())
	}
	if remote.Message != "widget 4f0c does not exist" || string(remote.Payload) != `{"resource":"widget"}` {
		t.Fatalf("terminal = message %q payload %s", remote.Message, remote.Payload)
	}
}

// sseTerminalFixture is the SSE terminal frame both generated clients read in
// the opt-in tests: the first-party error envelope plus `status`, flat,
// with `details` holding the declared body alone. It is what both providers
// write (go/framework/http/stream.go, stream-error.ts). The TypeScript half
// reads the same bytes in typescript/framework/client/test/runtime/sse-transport.test.ts.
const sseTerminalFixture = `{"status":404,"code":"not_found","error":"Not Found",` +
	`"message":"widget 4f0c does not exist","details":{"resource":"widget"}}`

func sseTerminalOperation() Operation {
	details := clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"resource": {Type: "string"}},
		Required: []string{"resource"}, AdditionalProperties: additionalForbidden(),
	}
	operation := serverStreamOperation(nil)
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: 404, Code: "not_found", Schema: &details}}
	return operation
}

// TestSSETerminalCarriesTheMessageUnderTheSameOptIn proves the SSE terminal
// frame follows the one consumer gate every other transport reads: nothing by
// default, the provider's prose on an opted-in binding, and the same inline
// redaction of this call's own bearer either way the prose echoes it. The
// declared `details` validation is unchanged by the option.
func TestSSETerminalCarriesTheMessageUnderTheSameOptIn(t *testing.T) {
	operation := sseTerminalOperation()
	var remote *RemoteError

	if err := decodeSSEError([]byte(sseTerminalFixture), operation, nil, nil, false); !stderrors.As(err, &remote) {
		t.Fatalf("default terminal = %T %v", err, err)
	}
	if remote.Code() != "not_found" || remote.StatusCode != 404 || string(remote.Payload) != `{"resource":"widget"}` {
		t.Fatalf("default terminal = %q/%d payload %s", remote.Code(), remote.StatusCode, remote.Payload)
	}
	if remote.Message != "" {
		t.Fatalf("default message = %q, want no provider prose without the opt-in", remote.Message)
	}

	if err := decodeSSEError([]byte(sseTerminalFixture), operation, nil, nil, true); !stderrors.As(err, &remote) {
		t.Fatalf("opted-in terminal = %T %v", err, err)
	}
	if remote.Message != "widget 4f0c does not exist" || string(remote.Payload) != `{"resource":"widget"}` {
		t.Fatalf("opted-in terminal = message %q payload %s", remote.Message, remote.Payload)
	}

	leaking := `{"status":400,"code":"errors.invalid","error":"Bad Request","message":"credential active-service-token expired"}`
	if err := decodeSSEError([]byte(leaking), operation, nil, []string{"active-service-token"}, true); !stderrors.As(err, &remote) {
		t.Fatalf("leaking terminal = %T %v", err, err)
	}
	if remote.Message != "credential [REDACTED] expired" {
		t.Fatalf("leaking message = %q, want the bearer substituted inline", remote.Message)
	}
}

func TestOpenServerStreamBoundsSlowErrorHandshakeAndDoesNotTripCircuitOnBusinessErrors(t *testing.T) {
	t.Run("slow error body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.WriteHeader(http.StatusUnauthorized)
			writer.(http.Flusher).Flush()
			<-request.Context().Done()
		}))
		defer server.Close()

		attempt := 30
		operation := serverStreamOperation(&clientcontract.ResiliencePolicy{AttemptTimeoutMs: &attempt})
		operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
		bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
		started := time.Now()
		_, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
		if err == nil || !perrors.Is(err, CodeClientDeadline) {
			t.Fatalf("handshake error = %T %v", err, err)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("slow error handshake took %s", elapsed)
		}
	})

	t.Run("business rejection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":"bad request"}`))
		}))
		defer server.Close()

		threshold := 1
		operation := serverStreamOperation(&clientcontract.ResiliencePolicy{Circuit: &clientcontract.CircuitPolicy{FailureThreshold: &threshold}})
		operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
		bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
		_, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
		if err == nil {
			t.Fatal("expected business rejection")
		}
		if state := bound.service.breakers[operation.ID].State(); state != CircuitClosed {
			t.Fatalf("circuit state = %s", state)
		}
	})
}

func TestOpenServerStreamSlowConsumerIsReleasedByIdleTimeout(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "declared-server-streams", "a-stalled-stream-is-released-by-the-declared-idle-timeout")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		for index := 0; index < 3; index++ {
			_, _ = fmt.Fprintf(writer, "data: {\"value\":\"%d\"}\n\n", index)
		}
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()

	idle := 40
	buffer := 1
	operation := serverStreamOperation(&clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{
		IdleTimeoutMs: &idle, MaxBufferedMessages: &buffer,
	}})
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(time.Second):
		t.Fatal("slow consumer stream did not terminate")
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("terminal error = %T %v", err, err)
	}
}

func TestOpenServerStreamReleasesHalfOpenProbeOnAdmissionAndCallerClose(t *testing.T) {
	t.Run("first message admits long lived stream", func(t *testing.T) {
		var calls atomic.Int32
		firstClosed := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			call := calls.Add(1)
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"value\":\"ready\"}\n\n"))
			writer.(http.Flusher).Flush()
			if call == 1 {
				select {
				case <-firstClosed:
				case <-request.Context().Done():
				}
			}
		}))
		defer server.Close()

		bound, operation, breaker := halfOpenStreamClient(t, server.URL)
		first, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
		if err != nil {
			t.Fatal(err)
		}
		if message := <-first.Messages(); message.Value != "ready" {
			t.Fatalf("first message = %#v", message)
		}
		if state := breaker.State(); state != CircuitClosed {
			t.Fatalf("circuit after stream admission = %s", state)
		}
		second, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
		if err != nil {
			t.Fatalf("second stream after admission: %v", err)
		}
		if message := <-second.Messages(); message.Value != "ready" {
			t.Fatalf("second message = %#v", message)
		}
		close(firstClosed)
		_ = first.Close()
		_ = second.Close()
	})

	// Admission is the handshake, so a provider that accepted the stream and
	// delivered nothing has still released the half-open probe with a success.
	// The caller's close afterwards is a session fact and writes nothing more.
	t.Run("caller close after admission leaves the circuit closed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			<-request.Context().Done()
		}))
		defer server.Close()

		bound, operation, breaker := halfOpenStreamClient(t, server.URL)
		stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-stream.Done():
		case <-time.After(time.Second):
			t.Fatal("closed stream did not terminate")
		}
		if state := breaker.State(); state != CircuitClosed {
			t.Fatalf("circuit after caller close = %s", state)
		}
		if err := breaker.AllowRequest(); err != nil {
			t.Fatalf("half-open probe remained occupied: %v", err)
		}
	})
}

func TestOpenServerStreamCallerCancellationDuringHandshakeReleasesProbe(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()

	bound, operation, breaker := halfOpenStreamClient(t, server.URL)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := OpenServerStream[streamItem](ctx, bound, &Request{}, operation)
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if err == nil || !perrors.Is(err, CodeClientCanceled) {
			t.Fatalf("handshake cancellation = %T %v", err, err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled handshake did not return")
	}
	if state := breaker.State(); state != CircuitHalfOpen {
		t.Fatalf("canceled handshake changed circuit to %s", state)
	}
	if err := breaker.AllowRequest(); err != nil {
		t.Fatalf("canceled handshake retained half-open probe: %v", err)
	}
	breaker.onIgnored()
}

func TestOpenServerStreamDeclaredTerminalBusinessErrorDoesNotOpenCircuit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: error\ndata: {\"status\":404,\"code\":\"not_found\"}\n\n"))
	}))
	defer server.Close()

	threshold := 1
	operation := serverStreamOperation(&clientcontract.ResiliencePolicy{Circuit: &clientcontract.CircuitPolicy{FailureThreshold: &threshold}})
	operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
	operation.Contract.Errors = []clientcontract.DeclaredError{{Status: http.StatusNotFound, Code: "not_found"}}
	bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	for attempt := 0; attempt < 2; attempt++ {
		stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
		if err != nil {
			t.Fatalf("open stream %d: %v", attempt+1, err)
		}
		for range stream.Messages() {
		}
		var remote *RemoteError
		if !stderrors.As(stream.Err(), &remote) || remote.StatusCode != http.StatusNotFound {
			t.Fatalf("terminal error %d = %T %v", attempt+1, stream.Err(), stream.Err())
		}
		if state := bound.service.breakers[operation.ID].State(); state != CircuitClosed {
			t.Fatalf("circuit after business terminal %d = %s", attempt+1, state)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", calls.Load())
	}
}

func halfOpenStreamClient(t *testing.T, endpoint string) (*Client, Operation, *CircuitBreaker) {
	t.Helper()
	operation := serverStreamOperation(nil)
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	bound := boundTestClient(t, endpoint, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	breaker := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1, ResetTimeout: time.Millisecond, SuccessThreshold: 1, HalfOpenMaxConcurrent: 1,
	})
	breaker.OnFailure()
	time.Sleep(2 * time.Millisecond)
	bound.service.breakers[operation.ID] = breaker
	return bound, operation, breaker
}

func TestReadSSEMessagesRejectsFrameBeforeUnboundedAllocation(t *testing.T) {
	operation := serverStreamOperation(nil)
	messages := make(chan streamItem, 1)
	payload := "data: " + strings.Repeat("x", 1<<20) + "\n\n"
	session, _ := newStreamSession(t.Context(), streamSessionConfig{Budgets: StreamBudgets{
		Idle: time.Second, MaxBufferedMessages: 1, MaxFrameBytes: 32,
	}})
	defer func() { _ = session.Close() }()
	session.Admit()
	err := readSSEMessages(t.Context(), strings.NewReader(payload), messages, operation, nil, session, nil, false)
	if err == nil || !perrors.Is(err, CodeClientResponse) {
		t.Fatalf("frame error = %T %v", err, err)
	}
}

// sseStreamRegistry builds a real application-scoped registry and the generated
// client bound to it, so a test can stop the application the way a deployment
// does instead of closing a stream by hand.
func sseStreamRegistry(t *testing.T, endpoint string, binding ServiceBinding) (*Client, *ServiceBindings) {
	t.Helper()
	binding.URL = endpoint
	binding.ClientID = "consumer.workload"
	bindings, err := newServiceBindings(ServicesOptions{Services: map[string]ServiceBinding{"inventory": binding}})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	bound, err := NewServiceClient(bindings, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	return bound, bindings
}

// writeSSE writes one already-formatted SSE chunk and flushes it, so the test
// controls exactly how the response is segmented on the wire.
func writeSSE(t *testing.T, writer http.ResponseWriter, chunk []byte) {
	t.Helper()
	if _, err := writer.Write(chunk); err != nil {
		return
	}
	writer.(http.Flusher).Flush()
}

func TestOpenServerStreamIsClosedByTheApplicationStopPhase(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "sse-admission-and-parsing", "stopping-the-application-closes-an-open-server-stream")
	observer := &recordingServiceTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writeSSE(t, writer, []byte("data: {\"value\":\"one\"}\n\n"))
		// The provider never ends the stream: only the application stop phase
		// can release this reader.
		<-request.Context().Done()
	}))
	defer server.Close()

	plugin := Services(ServicesOptions{ClientID: "consumer.workload", Services: map[string]ServiceBinding{
		"inventory": {URL: server.URL, Credentials: map[string]CredentialBinding{"service": {Provider: freshSource("stream-token", nil)}}},
	}})
	application := startedApplication(t, "consumer")
	application.Use(plugin)
	if err := application.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	registry := resolveRegistry(t, application.Container())
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	bound, err := NewServiceClient(registry, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, serverStreamOperation(nil))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case message, ok := <-stream.Messages():
		if !ok || message.Value != "one" {
			t.Fatalf("first message = %#v ok=%v", message, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider message never reached the caller")
	}

	if err := application.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("application stop left the stream open")
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientCanceled) {
		t.Fatalf("terminal after application stop = %T %v", err, err)
	}
	observer.mu.Lock()
	calls := append([]ServiceCallResult(nil), observer.calls...)
	observer.mu.Unlock()
	if len(calls) != 1 || calls[0].Code != string(CodeClientCanceled) {
		t.Fatalf("call measurements = %#v", calls)
	}
}

func TestOpenServerStreamRefusesToOpenOnAStoppedRegistry(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "sse-admission-and-parsing", "stopping-the-application-closes-an-open-server-stream")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	bound, bindings := sseStreamRegistry(t, server.URL, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	if err := bindings.Close(); err != nil {
		t.Fatal(err)
	}
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, serverStreamOperation(nil))
	if err == nil || !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("open on a stopped registry = %T %v", err, err)
	}
	if stream != nil {
		t.Fatal("a stopped registry still returned a stream")
	}
	// A stream nothing would ever stop is refused before it reaches the wire.
	if requests.Load() != 0 {
		t.Fatalf("requests received by the provider = %d", requests.Load())
	}
}

func TestOpenServerStreamRejectedAdmissionReachesTheCallerBeforeAnyMessage(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "sse-admission-and-parsing", "a-non-2xx-handshake-reaches-the-caller-before-any-message")
	spectest.Proves(t, "go/typed-service-clients", "sse-admission-and-parsing", "a-rejected-credential-is-reacquired-once-without-replaying-the-stream")
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			var tokens atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(status)
					_, _ = fmt.Fprintf(writer, "{\"code\":%q}", "denied")
					return
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.WriteHeader(http.StatusOK)
				writeSSE(t, writer, []byte("data: {\"value\":\"after refresh\"}\n\n"))
			}))
			defer server.Close()

			bound, _ := sseStreamRegistry(t, server.URL, ServiceBinding{Credentials: map[string]CredentialBinding{
				"service": {Provider: freshSource("stream-token", &tokens)},
			}})
			stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, serverStreamOperation(nil))
			if err == nil {
				t.Fatal("a rejected admission returned a stream")
			}
			var remote *RemoteError
			if !stderrors.As(err, &remote) || remote.StatusCode != status {
				t.Fatalf("admission error = %T %v", err, err)
			}
			if stream != nil {
				t.Fatal("a rejected admission returned a stream handle")
			}
			// One attempt only: a stream is never replayed, whatever the
			// declared idempotency says.
			if requests.Load() != 1 {
				t.Fatalf("requests after a rejected admission = %d", requests.Load())
			}

			// The refused credential was dropped exactly once, so the next open
			// reacquires instead of resending the value the provider rejected.
			acquired := tokens.Load()
			refreshed, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, serverStreamOperation(nil))
			if err != nil {
				t.Fatal(err)
			}
			defer refreshed.Close() //nolint:errcheck // Close always returns nil
			if got := tokens.Load(); got != acquired+1 {
				t.Fatalf("token acquisitions = %d, want %d", got, acquired+1)
			}
			select {
			case message, ok := <-refreshed.Messages():
				if !ok || message.Value != "after refresh" {
					t.Fatalf("message after refresh = %#v ok=%v", message, ok)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the refreshed stream delivered nothing")
			}
		})
	}
}

func TestOpenServerStreamParsesEventsAcrossChunkAndRuneBoundaries(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "sse-admission-and-parsing", "a-segmented-response-is-parsed-across-chunk-and-rune-boundaries")
	// Two CRLF-terminated events carrying multi-byte runes, written one byte at
	// a time: every rune, every CRLF pair and every event boundary is split.
	body := []byte("data: {\"value\":\"héllo→\"}\r\n\r\ndata: {\"value\":\"日本語\"}\r\n\r\n")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		for index := range body {
			writeSSE(t, writer, body[index:index+1])
		}
	}))
	defer server.Close()

	bound, _ := sseStreamRegistry(t, server.URL, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, serverStreamOperation(nil))
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
	if fmt.Sprint(values) != "[héllo→ 日本語]" {
		t.Fatalf("stream values = %v", values)
	}
}

func TestOpenServerStreamHeartbeatKeepsAnIdleStreamAliveAndTheHandshakeBudgetStopsAtAdmission(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "sse-admission-and-parsing", "a-heartbeat-comment-keeps-an-idle-stream-alive")
	spectest.Proves(t, "go/typed-service-clients", "stream-session-budgets", "the-handshake-budget-stops-bounding-the-stream-at-admission")
	const heartbeats = 6
	const heartbeatEvery = 20 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		for index := 0; index < heartbeats; index++ {
			select {
			case <-time.After(heartbeatEvery):
			case <-request.Context().Done():
				return
			}
			// A comment line is liveness, never a message.
			writeSSE(t, writer, []byte(": keep-alive\n\n"))
		}
		writeSSE(t, writer, []byte("data: {\"value\":\"late\"}\n\n"))
	}))
	defer server.Close()

	// The handshake budget is shorter than the time the provider takes to send
	// its first message, and the idle budget is shorter than the whole quiet
	// period the heartbeats cover. Neither may end this stream.
	attempt := 40
	idle := 90
	operation := serverStreamOperation(&clientcontract.ResiliencePolicy{
		AttemptTimeoutMs: &attempt,
		Stream:           &clientcontract.StreamPolicy{IdleTimeoutMs: &idle},
	})
	bound, _ := sseStreamRegistry(t, server.URL, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 1)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("heartbeats did not keep the stream alive: %T %v", err, err)
	}
	if fmt.Sprint(values) != "[late]" {
		t.Fatalf("stream values = %v", values)
	}
}

func TestOpenServerStreamDeclaredDurationBoundsAnAdmittedStream(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "stream-session-budgets", "the-declared-operation-duration-bounds-an-admitted-stream")
	// The provider keeps the stream perfectly healthy: it heartbeats faster
	// than the idle budget and never fails. Only the declared duration can end
	// it, and only when it is declared.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writeSSE(t, writer, []byte("data: {\"value\":\"one\"}\n\n"))
		for {
			select {
			case <-time.After(10 * time.Millisecond):
				writeSSE(t, writer, []byte(": keep-alive\n\n"))
			case <-request.Context().Done():
				return
			}
		}
	}))
	defer server.Close()

	bound, _ := sseStreamRegistry(t, server.URL, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	idle := 500
	total := 120
	declared := serverStreamOperation(&clientcontract.ResiliencePolicy{
		TimeoutMs: &total,
		Stream:    &clientcontract.StreamPolicy{IdleTimeoutMs: &idle},
	})
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, declared)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the declared duration did not bound the admitted stream")
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("terminal at the declared duration = %T %v", err, err)
	}

	// Without a declared duration the same provider is not cut: the stream is
	// still delivering when the caller closes it.
	undeclared := serverStreamOperation(&clientcontract.ResiliencePolicy{
		Stream: &clientcontract.StreamPolicy{IdleTimeoutMs: &idle},
	})
	open, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, undeclared)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case message, ok := <-open.Messages():
		if !ok || message.Value != "one" {
			t.Fatalf("first message = %#v ok=%v", message, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("undeclared duration stream delivered nothing")
	}
	select {
	case <-open.Done():
		t.Fatal("an undeclared duration bounded the stream anyway")
	case <-time.After(3 * time.Duration(total) * time.Millisecond):
	}
	if err := open.Close(); err != nil {
		t.Fatal(err)
	}
}
