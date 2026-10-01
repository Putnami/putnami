package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	connectwire "go.putnami.dev/protocol/clientcontract/connect"
	"go.putnami.dev/protocol/features/spectest"
)

func endpointOperation() Operation {
	return testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
}

func endpointCall(ctx context.Context, client *Client, operation Operation) (string, error) {
	result, err := CallOperation[struct {
		Value string `json:"value"`
	}](ctx, client,
		&OperationCall{Request: &Request{Method: http.MethodGet, Path: "/items"}}, operation)
	return result.Value, err
}

func TestEndpointSelectionNamesOneServiceOnASharedContext(t *testing.T) {
	var usersDefaultCalls atomic.Int32
	usersDefault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		usersDefaultCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"users-default"}`))
	}))
	defer usersDefault.Close()
	var usersFleetCalls atomic.Int32
	usersFleet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usersFleetCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer users-token" {
			t.Errorf("users fleet credential = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"users-fleet"}`))
	}))
	defer usersFleet.Close()
	var auditCalls atomic.Int32
	auditDefault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auditCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer audit-token" {
			t.Errorf("audit credential = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"audit-default"}`))
	}))
	defer auditDefault.Close()

	bindings, err := newServiceBindings(ServicesOptions{ClientID: "consumer", Services: map[string]ServiceBinding{
		"users": {
			URL: usersDefault.URL,
			Credentials: map[string]CredentialBinding{"service": {
				Provider: freshSource("users-token", nil),
			}},
		},
		"audit": {
			URL: auditDefault.URL,
			Credentials: map[string]CredentialBinding{"service": {
				Provider: freshSource("audit-token", nil),
			}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bindings.Close() })
	profiles := map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	}
	usersDescriptor := testDescriptor(profiles)
	usersDescriptor.Contract.Service.ID = "users"
	auditDescriptor := testDescriptor(profiles)
	auditDescriptor.Contract.Service.ID = "audit"
	users, err := NewServiceClient(bindings, usersDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := NewServiceClient(bindings, auditDescriptor)
	if err != nil {
		t.Fatal(err)
	}

	ctx := WithEndpoint(t.Context(), "users", usersFleet.URL)
	if got, callErr := endpointCall(ctx, users, endpointOperation()); callErr != nil || got != "users-fleet" {
		t.Fatalf("users call = %q, %v", got, callErr)
	}
	if got, callErr := endpointCall(ctx, audit, endpointOperation()); callErr != nil || got != "audit-default" {
		t.Fatalf("audit call = %q, %v", got, callErr)
	}
	if usersDefaultCalls.Load() != 0 || usersFleetCalls.Load() != 1 || auditCalls.Load() != 1 {
		t.Fatalf("calls users-default=%d users-fleet=%d audit=%d", usersDefaultCalls.Load(), usersFleetCalls.Load(), auditCalls.Load())
	}
}

func TestEndpointSelectionDoesNotRedirectANestedGeneratedCall(t *testing.T) {
	var defaultCalls atomic.Int32
	defaultServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("nested anonymous credential = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"default"}`))
	}))
	defer defaultServer.Close()
	var fleetCalls atomic.Int32
	fleetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fleetCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer outer-token" {
			t.Errorf("outer credential = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"fleet"}`))
	}))
	defer fleetServer.Close()

	var bound *Client
	provider := TokenSourceFunc(func(ctx context.Context, _ CredentialRequest) (Credential, error) {
		anonymous := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
		value, err := endpointCall(ctx, bound, anonymous)
		if err != nil {
			return Credential{}, err
		}
		if value != "default" {
			return Credential{}, fmt.Errorf("nested call reached %q", value)
		}
		return Credential{Value: "outer-token", Expiry: time.Now().Add(time.Hour)}, nil
	})
	bound = boundTestClient(t, defaultServer.URL, testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	}), ServiceBinding{Credentials: map[string]CredentialBinding{"service": {Provider: provider}}})
	t.Cleanup(func() { _ = bound.service.registry.Close() })

	ctx := WithEndpoint(t.Context(), "inventory", fleetServer.URL)
	if got, err := endpointCall(ctx, bound, endpointOperation()); err != nil || got != "fleet" {
		t.Fatalf("outer call = %q, %v", got, err)
	}
	if defaultCalls.Load() != 1 || fleetCalls.Load() != 1 {
		t.Fatalf("calls default=%d fleet=%d", defaultCalls.Load(), fleetCalls.Load())
	}
}

func TestEndpointViewsUseCanonicalURLCase(t *testing.T) {
	client := boundTestClient(t, "https://default.example", testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	t.Cleanup(func() { _ = client.service.registry.Close() })
	first, _, err := clientForEndpoint(WithEndpoint(t.Context(), "inventory", "https://TARGET.example"), client)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := clientForEndpoint(WithEndpoint(t.Context(), "inventory", "HTTPS://target.EXAMPLE/"), client)
	if err != nil {
		t.Fatal(err)
	}
	if first.service != second.service {
		t.Fatal("URL case variants created separate endpoint views")
	}
}

func TestEndpointConcurrentCallsIsolateAudiencesResponsesAndFlights(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "per-call-endpoint", "concurrent-targets-isolate-credentials-circuits-and-cached-responses")
	var calls [2]atomic.Int32
	servers := make([]*httptest.Server, 2)
	for i := range servers {
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[i].Add(1)
			if r.Header.Get("Authorization") != "Bearer token-http://"+r.Host {
				t.Errorf("target %d received another target's credential", i)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"value":"target-%d"}`, i)
		}))
		defer servers[i].Close()
	}
	var credentials atomic.Int32
	client := boundTestClient(t, servers[0].URL, testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken, Audience: "ignored-provider-audience"},
	}), ServiceBinding{Credentials: map[string]CredentialBinding{"service": {
		Source: CredentialSourceGCPIDToken,
		Provider: TokenSourceFunc(func(_ context.Context, request CredentialRequest) (Credential, error) {
			credentials.Add(1)
			return Credential{Value: "token-" + request.Audience, Expiry: time.Now().Add(time.Hour)}, nil
		}),
	}}})
	t.Cleanup(func() { _ = client.service.registry.Close() })
	operation := endpointOperation()
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{Cache: &clientcontract.CachePolicy{FreshMs: 60_000}}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			target := i % 2
			ctx := WithEndpoint(t.Context(), "inventory", servers[target].URL+"/")
			got, err := endpointCall(ctx, client, operation)
			if err != nil || got != fmt.Sprintf("target-%d", target) {
				t.Errorf("target %d: got %q, %v", target, got, err)
			}
		})
	}
	wg.Wait()
	if got, err := endpointCall(t.Context(), client, operation); err != nil || got != "target-0" {
		t.Fatalf("default binding changed: %q, %v", got, err)
	}
	if credentials.Load() != 2 || calls[0].Load() != 1 || calls[1].Load() != 1 {
		t.Fatalf("credentials=%d upstream=(%d,%d)", credentials.Load(), calls[0].Load(), calls[1].Load())
	}
	if got := client.InvalidateResponses("putItem?"); got != 2 {
		t.Fatalf("invalidation across endpoints dropped %d responses", got)
	}
}

func TestEndpointFailureRetryCircuitAndDeadlineStayWithTheirTarget(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "per-call-endpoint", "concurrent-targets-isolate-credentials-circuits-and-cached-responses")
	var failedCalls atomic.Int32
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		failedCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"unavailable"}`))
	}))
	defer failed.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"healthy"}`))
	}))
	defer good.Close()
	client := boundTestClient(t, good.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{
		Retry:   &clientcontract.RetryPolicy{MaxAttempts: intPointer(2)},
		Circuit: &clientcontract.CircuitPolicy{FailureThreshold: intPointer(1)},
	}
	if _, err := endpointCall(WithEndpoint(t.Context(), "inventory", failed.URL), client, operation); !perrors.Is(err, CodeClientRemote) {
		t.Fatalf("failed target: %v", err)
	}
	if _, err := endpointCall(WithEndpoint(t.Context(), "inventory", failed.URL+"/"), client, operation); !perrors.Is(err, CodeCircuitOpen) {
		t.Fatalf("target circuit did not stay open: %v", err)
	}
	if failedCalls.Load() != 2 {
		t.Fatalf("retry attempts=%d", failedCalls.Load())
	}
	if got, err := endpointCall(t.Context(), client, operation); err != nil || got != "healthy" {
		t.Fatalf("failed target poisoned default: %q, %v", got, err)
	}
	stalled := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer stalled.Close()
	ctx, cancel := context.WithTimeout(WithEndpoint(t.Context(), "inventory", stalled.URL), 30*time.Millisecond)
	defer cancel()
	if _, err := endpointCall(ctx, client, operation); !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("target deadline: %v", err)
	}
	if _, err := endpointCall(t.Context(), client, operation); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointValidationPrecedesCredentialsAndDispatch(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "per-call-endpoint", "an-invalid-override-fails-before-credentials-or-network")
	var credentials atomic.Int32
	client := boundTestClient(t, "https://default.example", testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	}), ServiceBinding{Credentials: map[string]CredentialBinding{"service": {Provider: freshSource("secret", &credentials)}}})
	for _, endpoint := range []string{"", "/relative", "https://user:password@target.example", "https://target.example?secret=x", "https://target.example#x", "http://target.example", "file:///tmp/secret"} {
		if _, err := endpointCall(WithEndpoint(t.Context(), "inventory", endpoint), client, endpointOperation()); !perrors.Is(err, CodeClientConfig) {
			t.Errorf("endpoint %q: %v", endpoint, err)
		}
	}
	if credentials.Load() != 0 {
		t.Fatal("invalid target acquired credentials")
	}
	if _, _, err := clientForEndpoint(WithEndpoint(t.Context(), "inventory", "https://target.example"), &Client{}); !perrors.Is(err, CodeClientConfig) {
		t.Fatal(err)
	}
	client.service.binding.AllowInsecure = true
	if _, _, err := clientForEndpoint(WithEndpoint(t.Context(), "inventory", "http://target.example"), client); err != nil {
		t.Fatal(err)
	}
}

func TestEndpointPreservesExplicitAudienceAndSchemaValidation(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "per-call-endpoint", "an-override-preserves-explicit-audiences-and-declared-schemas")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":42}`))
	}))
	defer server.Close()
	client := boundTestClient(t, "https://default.example", testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken, Audience: "provider-audience"},
	}), ServiceBinding{Credentials: map[string]CredentialBinding{"service": {
		Source: CredentialSourceGCPIDToken, Audience: "explicit-audience",
		Provider: TokenSourceFunc(func(_ context.Context, request CredentialRequest) (Credential, error) {
			if request.Audience != "explicit-audience" {
				t.Errorf("audience=%s", request.Audience)
			}
			return Credential{Value: "secret", Expiry: time.Now().Add(time.Hour)}, nil
		}),
	}}})
	operation := endpointOperation()
	ctx := WithEndpoint(t.Context(), "inventory", server.URL)
	if _, err := endpointCall(ctx, client, operation); !perrors.Is(err, CodeClientResponse) {
		t.Fatalf("response schema: %v", err)
	}
	operation.Request = &OperationRequest{Required: true, Content: []OperationContent{{MediaType: "application/json", Schema: &clientcontract.Schema{Type: "string"}}}}
	if _, err := endpointCall(ctx, client, operation); !perrors.Is(err, CodeClientRequest) {
		t.Fatalf("request schema: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid request reached target: %d", calls.Load())
	}
	_ = client.service.registry.Close()
	if _, err := endpointCall(ctx, client, endpointOperation()); !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("closed registry: %v", err)
	}
}

func TestEndpointConnectKeepsTheDeclaredWire(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inventory.v1.ApiService/GetItems" || r.Method != http.MethodPost {
			t.Errorf("Connect dispatched %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer connect-token" {
			t.Error("missing credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"target"}`))
	}))
	defer server.Close()
	bound := connectBoundClient(t, "https://default.example", connectServiceProfiles(), connectServiceBinding())
	operation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)}, nil, nil)
	value, err := CallOperation[connectItem](WithEndpoint(t.Context(), "inventory", server.URL), bound, newConnectCallInput(), operation)
	if err != nil || value.Value != "target" {
		t.Fatalf("Connect target: %#v, %v", value, err)
	}
}

func TestEndpointStreamsKeepTheirWireAndRegistryLifetime(t *testing.T) {
	t.Run("SSE", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"value\":\"target\"}\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer server.Close()
		bound := boundTestClient(t, "https://default.example", testDescriptor(connectServiceProfiles()), connectServiceBinding())
		stream, err := OpenServerStream[streamItem](WithEndpoint(t.Context(), "inventory", server.URL), bound, &Request{}, serverStreamOperation(nil))
		if err != nil {
			t.Fatal(err)
		}
		if value := <-stream.Messages(); value.Value != "target" {
			t.Fatalf("value=%#v", value)
		}
		if err := bound.service.registry.Close(); err != nil {
			t.Fatal(err)
		}
		for range stream.Messages() {
		}
	})
	t.Run("Connect", func(t *testing.T) {
		server := connectStreamServer(t, connectwire.StreamJSONContentType, func() []byte {
			return connectStreamBody([]string{`{"value":"target"}`})
		}, false)
		defer server.Close()
		bound := connectBoundClient(t, "https://default.example", connectServiceProfiles(), connectServiceBinding())
		operation := connectStreamOperation([]clientcontract.Transport{connectStreamTransport(clientcontract.EncodingJSON)}, nil)
		stream, err := OpenOperationServerStream[connectItem](WithEndpoint(t.Context(), "inventory", server.URL), bound, newConnectCallInput(), operation)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for value := range stream.Messages() {
			count++
			if value.Value != "target" {
				t.Fatalf("value=%#v", value)
			}
		}
		if stream.Err() != nil || count != 1 {
			t.Fatalf("count=%d, err=%v", count, stream.Err())
		}
	})
	t.Run("WebSocket", func(t *testing.T) {
		server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
			peer.expect(clientcontract.WebSocketFrameInit)
			peer.send(webSocketReadyFrame(false, ""))
			peer.send(webSocketMessageFrame("1", `{"value":"target"}`))
			peer.send(webSocketResultFrame(""))
			peer.drain()
		}})
		bound := webSocketTestClient(t, "https://default.example")
		stream, err := OpenServerStreamWS[wsItem](WithEndpoint(t.Context(), "inventory", server.URL), bound, &Request{}, webSocketServerStreamOperation(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for value := range stream.Messages() {
			count++
			if value.Value != "target" {
				t.Fatalf("value=%#v", value)
			}
		}
		if stream.Err() != nil || count != 1 {
			t.Fatalf("count=%d, err=%v", count, stream.Err())
		}
		server.waitForPlay(t)
		server.assertNoFailures(t)
	})
}
