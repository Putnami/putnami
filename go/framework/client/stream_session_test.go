package client

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// streamSessionBreaker is the breaker shape the phase rules are asserted
// against: one failure opens it, and two successes are needed to close it from
// half-open, so "exactly one success" is observable as "still half-open".
func streamSessionBreaker(t *testing.T) *CircuitBreaker {
	t.Helper()
	return NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1, ResetTimeout: time.Hour, SuccessThreshold: 2, HalfOpenMaxConcurrent: 2,
	})
}

func TestStreamSessionWalksTheFivePhasesInOrder(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "stream-session-lifecycle", "a-session-walks-connecting-admitted-active-terminal-closed")
	session, sessionCtx := newStreamSession(t.Context(), streamSessionConfig{ServiceID: "inventory", OperationID: "watchItems"})
	if session.Phase() != StreamPhaseConnecting || session.Phase().String() != "connecting" {
		t.Fatalf("initial phase = %s", session.Phase())
	}
	session.Admit()
	if session.Phase() != StreamPhaseAdmitted {
		t.Fatalf("phase after admission = %s", session.Phase())
	}
	session.Activate()
	if session.Phase() != StreamPhaseActive {
		t.Fatalf("phase after first message = %s", session.Phase())
	}
	// Admission never runs backwards: a second admission after activation is a
	// no-op rather than a return to the admitted phase.
	session.Admit()
	if session.Phase() != StreamPhaseActive {
		t.Fatalf("phase after repeated admission = %s", session.Phase())
	}
	session.Complete()
	if session.Phase() != StreamPhaseTerminal {
		t.Fatalf("phase after completion = %s", session.Phase())
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if session.Phase() != StreamPhaseClosed || session.Phase().String() != "closed" {
		t.Fatalf("phase after close = %s", session.Phase())
	}
	if sessionCtx.Err() == nil {
		t.Fatal("closing the session left its context live")
	}
}

func TestStreamSessionRecordsOneBreakerFactPerPhaseRule(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "stream-session-phase-rules", "the-breaker-records-admission-once-and-nothing-after-it")
	t.Run("admission records exactly one success", func(t *testing.T) {
		breaker := streamSessionBreaker(t)
		breaker.OnFailure()
		breaker.state = CircuitHalfOpen
		session, _ := newStreamSession(t.Context(), streamSessionConfig{Breaker: breaker})
		session.Admit()
		session.Admit()
		if state := breaker.State(); state != CircuitHalfOpen {
			t.Fatalf("two admissions recorded more than one success: %s", state)
		}
	})

	t.Run("a break after admission writes nothing", func(t *testing.T) {
		breaker := streamSessionBreaker(t)
		session, _ := newStreamSession(t.Context(), streamSessionConfig{Breaker: breaker})
		session.Dispatch()
		session.Admit()
		session.Fail(perrors.New(CodeClientResponse, "provider cut the stream"))
		if state := breaker.State(); state != CircuitClosed {
			t.Fatalf("post-admission break changed the circuit to %s", state)
		}
	})

	t.Run("a dispatched pre-admission failure records a failure", func(t *testing.T) {
		breaker := streamSessionBreaker(t)
		session, _ := newStreamSession(t.Context(), streamSessionConfig{Breaker: breaker})
		session.Dispatch()
		session.Fail(perrors.New(CodeClientRequest, "handshake refused"))
		if state := breaker.State(); state != CircuitOpen {
			t.Fatalf("refused handshake left the circuit %s", state)
		}
	})

	t.Run("a failure that never reached the provider records nothing", func(t *testing.T) {
		breaker := streamSessionBreaker(t)
		session, _ := newStreamSession(t.Context(), streamSessionConfig{Breaker: breaker})
		session.Fail(perrors.New(CodeClientCredential, "credential acquisition failed"))
		if state := breaker.State(); state != CircuitClosed {
			t.Fatalf("local credential failure changed the circuit to %s", state)
		}
	})

	t.Run("a caller cancellation records nothing", func(t *testing.T) {
		breaker := streamSessionBreaker(t)
		ctx, cancel := context.WithCancel(t.Context())
		session, _ := newStreamSession(ctx, streamSessionConfig{Breaker: breaker})
		session.Dispatch()
		cancel()
		session.Fail(perrors.New(CodeClientDeadline, "caller gave up"))
		if state := breaker.State(); state != CircuitClosed {
			t.Fatalf("caller cancellation changed the circuit to %s", state)
		}
	})

	t.Run("a rejected request neither records nor releases a probe", func(t *testing.T) {
		breaker := NewCircuitBreaker(CircuitBreakerConfig{
			FailureThreshold: 1, ResetTimeout: time.Hour, SuccessThreshold: 2, HalfOpenMaxConcurrent: 1,
		})
		breaker.OnFailure()
		breaker.state = CircuitHalfOpen
		if err := breaker.AllowRequest(); err != nil {
			t.Fatalf("first probe rejected: %v", err)
		}
		session, _ := newStreamSession(t.Context(), streamSessionConfig{Breaker: breaker})
		session.Fail(newCircuitOpenError(time.Now()))
		if breaker.AllowRequest() == nil {
			t.Fatal("a rejected request released a probe it never held")
		}
	})
}

func TestStreamSessionKeepsOneTerminalAndOneMeasurementUnderInterleaving(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "stream-session-lifecycle", "one-terminal-and-one-call-measurement-survive-concurrent-stop-and-terminal")
	for range 64 {
		observer := &recordingServiceTelemetry{}
		breaker := streamSessionBreaker(t)
		session, _ := newStreamSession(t.Context(), streamSessionConfig{
			Breaker: breaker, Telemetry: observer, Budgets: StreamBudgets{Idle: time.Hour},
		})
		session.Dispatch()
		session.Admit()
		// Registry stop, caller close, reader terminal and cancel all race for
		// the same session. The gate releases them together so the ordering is
		// decided by the runtime, not by a sleep.
		start := make(chan struct{})
		var group sync.WaitGroup
		actions := []func(){
			func() { _ = session.Close() },
			func() { _ = session.Close() },
			func() { session.Fail(perrors.New(CodeClientResponse, "provider cut the stream")) },
			func() { session.Complete() },
			func() { session.Cancel() },
		}
		group.Add(len(actions))
		for _, action := range actions {
			go func() {
				defer group.Done()
				<-start
				action()
			}()
		}
		close(start)
		group.Wait()

		observer.mu.Lock()
		calls := append([]ServiceCallResult(nil), observer.calls...)
		observer.mu.Unlock()
		if len(calls) != 1 {
			t.Fatalf("call measurements = %#v", calls)
		}
		if session.Phase() != StreamPhaseClosed {
			t.Fatalf("phase after the race = %s", session.Phase())
		}
		if state := breaker.State(); state != CircuitClosed {
			t.Fatalf("the race wrote to the breaker after admission: %s", state)
		}
	}
}

func TestStreamSessionCredentialsAreRecheckedAtTheSendPoint(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "stream-session-phase-rules", "a-credential-that-expired-during-its-acquisition-is-re-resolved-before-the-first-frame")
	var sourceCalls atomic.Int32
	var seen atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen.Store(request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("data: {\"value\":\"one\"}\n\n"))
	}))
	defer server.Close()

	// The first acquisition returns a value that is valid at the instant it is
	// fetched and already past the freshness leeway, which is what an
	// asynchronous acquisition that outlived its own credential looks like.
	source := TokenSourceFunc(func(context.Context, CredentialRequest) (Credential, error) {
		if sourceCalls.Add(1) == 1 {
			return Credential{Value: "expiring-token", Expiry: time.Now().Add(500 * time.Millisecond)}, nil
		}
		return Credential{Value: "fresh-token", Expiry: time.Now().Add(time.Hour)}, nil
	})
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: source},
	}})
	stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, serverStreamOperation(nil))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if got, _ := seen.Load().(string); got != "Bearer fresh-token" {
		t.Fatalf("the expired credential reached the provider: %q", got)
	}
	if calls := sourceCalls.Load(); calls != 2 {
		t.Fatalf("credential acquisitions = %d, want 2", calls)
	}
}

func TestStreamSessionCredentialsRunTheSendPointRecheck(t *testing.T) {
	var resolutions atomic.Int32
	session, _ := newStreamSession(t.Context(), streamSessionConfig{
		Credentials: func(context.Context) (appliedCredentials, error) {
			return appliedCredentials{secrets: []string{fmt.Sprintf("token-%d", resolutions.Add(1))}}, nil
		},
	})
	defer func() { _ = session.Close() }()
	applied, err := session.Credentials(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The acquisition is followed by the send-point state re-check, and it is
	// the re-checked value that the transport emits.
	if resolutions.Load() != 2 {
		t.Fatalf("resolutions = %d, want 2", resolutions.Load())
	}
	if len(applied.secrets) != 1 || applied.secrets[0] != "token-2" {
		t.Fatalf("applied credentials = %#v", applied.secrets)
	}
	if session.AuthDuration() < 0 {
		t.Fatal("credential resolution reported a negative duration")
	}
}

func TestStreamSessionInvalidatesTheResolvedCredentialExactlyOnce(t *testing.T) {
	var invalidations atomic.Int32
	session, _ := newStreamSession(t.Context(), streamSessionConfig{
		Credentials: func(context.Context) (appliedCredentials, error) {
			return appliedCredentials{secrets: []string{"token"}}, nil
		},
		Invalidate: func(appliedCredentials) { invalidations.Add(1) },
	})
	if _, err := session.Credentials(t.Context()); err != nil {
		t.Fatal(err)
	}
	session.invalidateCredentials()
	session.invalidateCredentials()
	if invalidations.Load() != 1 {
		t.Fatalf("credential invalidations = %d, want 1", invalidations.Load())
	}
}

func TestOpenServerStreamEmitsOneCallMeasurementOnEveryExitPath(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "stream-session-lifecycle", "one-terminal-and-one-call-measurement-survive-concurrent-stop-and-terminal")
	held := make(chan struct{})
	defer close(held)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/refused":
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"code":"errors.unavailable"}`))
		case "/terminal":
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("event: error\ndata: {\"status\":404,\"code\":\"errors.not_found\"}\n\n"))
		case "/held":
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			select {
			case <-held:
			case <-request.Context().Done():
			}
		default:
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"value\":\"one\"}\n\n"))
		}
	}))
	defer server.Close()

	tests := []struct {
		name   string
		path   string
		code   string
		status int
		close  bool
	}{
		{name: "success", path: "/ok", status: http.StatusOK},
		{name: "handshake failure", path: "/refused", code: "errors.unavailable"},
		{name: "terminal error", path: "/terminal", code: "errors.not_found", status: http.StatusOK},
		{name: "caller close", path: "/held", code: string(CodeClientCanceled), status: http.StatusOK, close: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observer := &recordingServiceTelemetry{}
			restore := InstallServiceCallTelemetry(observer)
			defer restore()

			operation := serverStreamOperation(nil)
			operation.Contract.Errors = []clientcontract.DeclaredError{
				{Status: http.StatusServiceUnavailable, Code: "errors.unavailable"},
				{Status: http.StatusNotFound, Code: "errors.not_found"},
			}
			descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}})
			bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
				"service": {Provider: freshSource("stream-token", nil)},
			}})
			stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{Path: test.path}, operation)
			switch {
			case test.code != "" && !test.close && stream == nil:
				if err == nil {
					t.Fatal("expected a refused handshake")
				}
			case err != nil:
				t.Fatal(err)
			}
			if stream != nil {
				if test.close {
					_ = stream.Close()
				}
				for range stream.Messages() {
				}
				<-stream.Done()
			}

			observer.mu.Lock()
			calls := append([]ServiceCallResult(nil), observer.calls...)
			observer.mu.Unlock()
			if len(calls) != 1 {
				t.Fatalf("call measurements = %#v", calls)
			}
			if calls[0].Code != test.code || calls[0].StatusCode != test.status || calls[0].Attempts != 1 {
				t.Fatalf("call measurement = %#v", calls[0])
			}
		})
	}
}

func TestOpenServerStreamCircuitFollowsAdmissionNotDelivery(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "stream-session-phase-rules", "the-breaker-records-admission-once-and-nothing-after-it")
	t.Run("accepted then cut mid-stream", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			// A half-written event followed by end of body: the provider
			// accepted the operation and then broke the stream.
			_, _ = writer.Write([]byte("data: {\"value\":\"one\"}\n"))
		}))
		defer server.Close()

		threshold := 1
		operation := serverStreamOperation(&clientcontract.ResiliencePolicy{Circuit: &clientcontract.CircuitPolicy{FailureThreshold: &threshold}})
		operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
		bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
		stream, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
		if err != nil {
			t.Fatal(err)
		}
		for range stream.Messages() {
		}
		if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientResponse) {
			t.Fatalf("terminal error = %T %v", err, err)
		}
		if state := bound.service.breakers[operation.ID].State(); state != CircuitClosed {
			t.Fatalf("a mid-stream break opened the circuit: %s", state)
		}
	})

	t.Run("refused handshake", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		threshold := 1
		operation := serverStreamOperation(&clientcontract.ResiliencePolicy{Circuit: &clientcontract.CircuitPolicy{FailureThreshold: &threshold}})
		operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
		bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
		if _, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation); err == nil {
			t.Fatal("expected a refused handshake")
		}
		if state := bound.service.breakers[operation.ID].State(); state != CircuitOpen {
			t.Fatalf("a refused handshake left the circuit %s", state)
		}
	})

	t.Run("caller cancellation before admission", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			close(started)
			<-request.Context().Done()
		}))
		defer server.Close()

		threshold := 1
		operation := serverStreamOperation(&clientcontract.ResiliencePolicy{Circuit: &clientcontract.CircuitPolicy{FailureThreshold: &threshold}})
		operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
		bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
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
				t.Fatalf("canceled handshake = %T %v", err, err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled handshake did not return")
		}
		if state := bound.service.breakers[operation.ID].State(); state != CircuitClosed {
			t.Fatalf("a canceled handshake opened the circuit: %s", state)
		}
	})
}

func TestStreamBudgetsAreDistinctFromTheDeclaredDuration(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "stream-session-budgets", "the-handshake-and-idle-budgets-are-distinct-from-the-declared-operation-duration")
	t.Run("handshake is bounded below the declared duration", func(t *testing.T) {
		session := 5_000
		handshake := 30
		budgets := resolveStreamBudgets(nil, &clientcontract.ResiliencePolicy{
			TimeoutMs: &session, AttemptTimeoutMs: &handshake,
		})
		if budgets.Handshake != 30*time.Millisecond || budgets.Session != 5*time.Second {
			t.Fatalf("budgets = %#v", budgets)
		}

		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			<-request.Context().Done()
		}))
		defer server.Close()
		operation := serverStreamOperation(&clientcontract.ResiliencePolicy{TimeoutMs: &session, AttemptTimeoutMs: &handshake})
		operation.Contract.Security = clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
		bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
		started := time.Now()
		_, err := OpenServerStream[streamItem](t.Context(), bound, &Request{}, operation)
		if err == nil || !perrors.Is(err, CodeClientDeadline) {
			t.Fatalf("slow handshake = %T %v", err, err)
		}
		if elapsed := time.Since(started); elapsed >= time.Duration(session)*time.Millisecond {
			t.Fatalf("the handshake consumed the declared duration: %s", elapsed)
		}
	})

	t.Run("an undeclared duration leaves the session unbounded but idle applies", func(t *testing.T) {
		budgets := resolveStreamBudgets(nil, nil)
		if budgets.Session != 0 || budgets.Idle != defaultStreamIdleTimeout || budgets.Handshake != defaultAttemptTimeout {
			t.Fatalf("default budgets = %#v", budgets)
		}
		session, sessionCtx := newStreamSession(t.Context(), streamSessionConfig{Budgets: StreamBudgets{Idle: 20 * time.Millisecond}})
		defer func() { _ = session.Close() }()
		if _, bounded := sessionCtx.Deadline(); bounded {
			t.Fatal("an undeclared duration bounded the session in time")
		}
		session.Admit()
		select {
		case <-session.idleExpired():
		case <-time.After(2 * time.Second):
			t.Fatal("the idle budget never expired")
		}
		if !session.idleBudgetExpired() || sessionCtx.Err() == nil {
			t.Fatal("an expired idle budget did not release the session")
		}
	})
}

// TestStreamTransportsDoNotWriteToTheBreakerThemselves is the guard D0.7 asks
// for: every first-party stream transport drives StreamSession, so none of them
// may record a breaker outcome of its own. A WebSocket or Connect transport
// added later fails here the moment it restates a phase rule.
func TestStreamTransportsDoNotWriteToTheBreakerThemselves(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	transports := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "stream_session.go" || !isStreamTransportFile(name) {
			continue
		}
		transports++
		source, readErr := os.ReadFile(filepath.Clean(name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, write := range []string{"OnSuccess(", "OnFailure(", "onIgnored("} {
			if strings.Contains(string(source), write) {
				t.Errorf("%s records %s itself instead of letting the stream session own the phase rule", name, write)
			}
		}
	}
	if transports == 0 {
		t.Fatal("the guard scanned no stream transport file")
	}
}

func isStreamTransportFile(name string) bool {
	return name == "server_stream.go" || strings.HasPrefix(name, "ws_") || strings.HasPrefix(name, "connect_stream")
}

func TestStreamSessionAttemptFinisherRunsOnce(t *testing.T) {
	observer := &recordingServiceTelemetry{}
	session, _ := newStreamSession(t.Context(), streamSessionConfig{
		Telemetry: observer, Budgets: StreamBudgets{Handshake: time.Minute},
	})
	defer func() { _ = session.Close() }()
	attemptCtx, finish := session.beginAttempt()
	deadline, bounded := attemptCtx.Deadline()
	if !bounded || !deadline.Equal(session.handshakeDeadline()) {
		t.Fatalf("attempt deadline = %v (%t), handshake deadline = %v", deadline, bounded, session.handshakeDeadline())
	}
	finish(ServiceAttemptResult{StatusCode: http.StatusOK})
	finish(ServiceAttemptResult{StatusCode: http.StatusTeapot})
	if attemptCtx.Err() == nil {
		t.Fatal("finishing an attempt left its context live")
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.attempts) != 1 || observer.attempts[0].StatusCode != http.StatusOK {
		t.Fatalf("attempt results = %#v", observer.attempts)
	}
}

func TestStreamSessionFailKeepsTheFirstTerminalAndMapsIt(t *testing.T) {
	mapped := perrors.New(CodeClientRemote, "projected by the generated mapper")
	session, _ := newStreamSession(t.Context(), streamSessionConfig{
		MapError: func(error) error { return mapped },
	})
	if surfaced := session.Fail(perrors.New(CodeClientResponse, "first")); !stderrors.Is(surfaced, mapped) {
		t.Fatalf("first terminal = %v", surfaced)
	}
	if surfaced := session.Fail(perrors.New(CodeClientDeadline, "second")); !stderrors.Is(surfaced, mapped) {
		t.Fatalf("second terminal replaced the first: %v", surfaced)
	}
	if !stderrors.Is(session.Err(), mapped) {
		t.Fatalf("recorded terminal = %v", session.Err())
	}
	if session.Fail(nil) != nil {
		t.Fatal("a nil terminal was recorded")
	}
	session.Complete()
	if !stderrors.Is(session.Err(), mapped) {
		t.Fatalf("completion overwrote the terminal error: %v", session.Err())
	}
}
