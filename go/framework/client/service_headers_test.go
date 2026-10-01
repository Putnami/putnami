package client

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	connectwire "go.putnami.dev/protocol/clientcontract/connect"
	"go.putnami.dev/protocol/features/spectest"
)

func TestBindingHeadersAccompanyEachForwardedUser(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binding-headers", "static-headers-accompany-each-calls-credentials")
	seen := make(chan string, 3)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Putnami-Observed-Revision"); got != "revision-one" {
			t.Errorf("observed revision = %q, want revision-one", got)
		}
		seen <- r.Header.Get("Authorization")
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	var binding ServiceBinding
	if err := json.Unmarshal([]byte(`{"headers":{"X-Putnami-Observed-Revision":"revision-one"},"credentials":{"user":{"source":"forwarded-user"}}}`), &binding); err != nil {
		t.Fatal(err)
	}
	bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{
		"user": {Kind: clientcontract.CredentialForwardedUserToken},
	}), binding)
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "user"}}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	operation.Contract.Resilience = &clientcontract.ResiliencePolicy{Retry: &clientcontract.RetryPolicy{MaxAttempts: intPointer(2)}}
	for _, token := range []string{"first-user", "second-user"} {
		ctx := WithForwardedUserToken(t.Context(), token)
		if _, err := CallOperation[account](ctx, bound, &OperationCall{Request: &Request{Method: "GET", Path: "/items"}}, operation); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 3 || <-seen != "Bearer first-user" || <-seen != "Bearer first-user" || <-seen != "Bearer second-user" {
		t.Fatal("retry or subsequent call changed the forwarded user")
	}
	if _, err := CallOperation[account](t.Context(), bound, &OperationCall{Request: &Request{Path: "/items"}}, operation); !perrors.Is(err, CodeClientCredential) {
		t.Fatalf("static header replaced a missing credential: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatal("call with missing credentials reached the provider")
	}
}

func TestBindingHeadersAreValidatedWithoutEchoingValues(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binding-headers", "invalid-or-reserved-headers-fail-before-dispatch")
	reserved := []string{"authorization", "Cookie", "Set-Cookie", "Host", "Connection", "Keep-Alive", "Upgrade", "Content-Type", "Content-Length", "Content-Encoding", "Transfer-Encoding", "TE", "Trailer", "Accept", "Accept-Encoding", "Traceparent", "Tracestate", "Baggage", "X-Client-ID", "X-Request-ID", "X-Putnami-Client-ID", "X-Putnami-Service", "Sec-WebSocket-Protocol", "Proxy-Authorization", "Connect-Timeout-Ms", "Grpc-Timeout"}
	cases := make([]map[string]string, 0, 8+len(reserved))
	cases = append(cases, []map[string]string{
		{"": "private-value"}, {"bad name": "private-value"}, {"bad:name": "private-value"},
		{"X-ä": "private-value"}, {"X-Hint": "private-value\r\ninjected: yes"},
		{"X-Hint": "private-value\x00"}, {"X-Hint": "private-value\x7f"},
		{"X-Hint": "private-value", "x-hint": "private-value"},
	}...)
	for _, name := range reserved {
		cases = append(cases, map[string]string{name: "private-value"})
	}
	for _, headers := range cases {
		_, err := NewServiceClientBinding(ServiceBinding{URL: "https://service.example", ClientID: "consumer", Headers: headers}, testDescriptor(map[string]clientcontract.CredentialProfile{}))
		if !perrors.Is(err, CodeClientConfig) || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("invalid headers returned %v", err)
		}
	}
	for _, kind := range []clientcontract.CredentialKind{clientcontract.CredentialAPIKey, clientcontract.CredentialNamedHeader} {
		_, err := NewServiceClientBinding(ServiceBinding{URL: "https://service.example", ClientID: "consumer", Headers: map[string]string{"x-key": "private-value"}}, testDescriptor(map[string]clientcontract.CredentialProfile{"key": {Kind: kind, Header: "X-Key"}}))
		if !perrors.Is(err, CodeClientConfig) || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("credential collision returned %v", err)
		}
	}
}

// bindingHeaderCorpus is the shared corpus the TypeScript runtime reads too, so
// both runtimes accept, canonicalize and refuse the same static header maps.
type bindingHeaderCorpus struct {
	ReservedNames    []string `json:"reservedNames"`
	ReservedPrefixes []string `json:"reservedPrefixes"`
	Accepted         []struct {
		Name      string            `json:"name"`
		Headers   map[string]string `json:"headers"`
		Canonical map[string]string `json:"canonical"`
	} `json:"accepted"`
	Refused []struct {
		Name                      string            `json:"name"`
		Headers                   map[string]string `json:"headers"`
		DeclaredCredentialHeaders []string          `json:"declaredCredentialHeaders"`
	} `json:"refused"`
}

func TestBindingHeadersMatchTheSharedCorpus(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binding-headers", "binding-headers-match-the-shared-corpus")
	raw, err := os.ReadFile("../../../protocols/clientcontract/fixtures/binding/headers.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus bindingHeaderCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.ReservedNames) == 0 || len(corpus.ReservedPrefixes) == 0 || len(corpus.Accepted) == 0 || len(corpus.Refused) == 0 {
		t.Fatal("the shared corpus is empty, so this test would pass vacuously")
	}
	// This runtime reserves exactly the shared set: a name added to the runtime
	// without a corpus entry, or the reverse, is a parity drift.
	lowerSorted := func(values []string) []string {
		lowered := make([]string, 0, len(values))
		for _, value := range values {
			lowered = append(lowered, strings.ToLower(value))
		}
		slices.Sort(lowered)
		return lowered
	}
	if got, want := lowerSorted(reservedBindingHeaderNames), lowerSorted(corpus.ReservedNames); !slices.Equal(got, want) {
		t.Fatalf("reserved names %v, corpus %v", got, want)
	}
	if got, want := lowerSorted(reservedBindingHeaderPrefixes), lowerSorted(corpus.ReservedPrefixes); !slices.Equal(got, want) {
		t.Fatalf("reserved prefixes %v, corpus %v", got, want)
	}
	bind := func(headers map[string]string, credentialHeaders []string) (*Client, error) {
		profiles := map[string]clientcontract.CredentialProfile{}
		for index, header := range credentialHeaders {
			profiles["key-"+string(rune('a'+index))] = clientcontract.CredentialProfile{Kind: clientcontract.CredentialAPIKey, Header: header}
		}
		return NewServiceClientBinding(ServiceBinding{URL: "https://service.example", ClientID: "consumer", Headers: headers}, testDescriptor(profiles))
	}
	for _, accepted := range corpus.Accepted {
		bound, err := bind(accepted.Headers, nil)
		if err != nil {
			t.Fatalf("%s: refused: %v", accepted.Name, err)
		}
		if !maps.Equal(bound.service.binding.Headers, accepted.Canonical) {
			t.Fatalf("%s: snapshot %v, want %v", accepted.Name, bound.service.binding.Headers, accepted.Canonical)
		}
		if err := bound.Close(); err != nil {
			t.Fatal(err)
		}
	}
	refuse := func(name string, headers map[string]string, credentialHeaders []string) {
		t.Helper()
		_, err := bind(headers, credentialHeaders)
		if !perrors.Is(err, CodeClientConfig) || strings.Contains(err.Error(), "private") {
			t.Fatalf("%s: returned %v, want client.config without the value", name, err)
		}
	}
	for _, refused := range corpus.Refused {
		refuse(refused.Name, refused.Headers, refused.DeclaredCredentialHeaders)
	}
	for _, reserved := range corpus.ReservedNames {
		for _, name := range []string{reserved, strings.ToLower(reserved), strings.ToUpper(reserved)} {
			refuse(name, map[string]string{name: "private-value"}, nil)
		}
	}
	for _, prefix := range corpus.ReservedPrefixes {
		for _, name := range []string{prefix + "Hint", strings.ToLower(prefix) + "hint", strings.ToUpper(prefix) + "HINT"} {
			refuse(name, map[string]string{name: "private-value"}, nil)
		}
	}
}

func TestBindingHeadersRejectForwardedIdentityAndOrigin(t *testing.T) {
	for _, name := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP", "X-Cloud-Trace-Context", "Origin"} {
		for _, variant := range []string{name, strings.ToLower(name), strings.ToUpper(name)} {
			t.Run(variant, func(t *testing.T) {
				_, err := NewServiceClientBinding(ServiceBinding{
					URL: "https://service.example", ClientID: "consumer",
					Headers: map[string]string{variant: "caller-supplied-value"},
				}, testDescriptor(map[string]clientcontract.CredentialProfile{}))
				if !perrors.Is(err, CodeClientConfig) {
					t.Fatalf("identity or origin header %q returned %v, want client.config", variant, err)
				}
			})
		}
	}
}

func TestBindingHeadersRemainImmutableAcrossConcurrentCalls(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binding-headers", "binding-headers-are-an-immutable-snapshot")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hint") != "original" {
			t.Errorf("caller changed the bound header: %q", r.Header.Get("X-Hint"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	headers := map[string]string{"x-hint": "original"}
	registry, err := newServiceBindings(ServicesOptions{ClientID: "consumer", Services: map[string]ServiceBinding{"inventory": {URL: server.URL, Headers: headers}}})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := NewServiceClient(registry, testDescriptor(map[string]clientcontract.CredentialProfile{}))
	if err != nil {
		t.Fatal(err)
	}
	exposed, err := registry.For("inventory")
	if err != nil {
		t.Fatal(err)
	}
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	var callers sync.WaitGroup
	for range 10 {
		callers.Go(func() {
			if _, err := Call[account](t.Context(), bound, &Request{Path: "/items"}, operation); err != nil {
				t.Error(err)
			}
		})
		headers["x-hint"] = "changed"
		exposed.Headers["X-Hint"] = "changed"
	}
	callers.Wait()
}

func TestBindingHeadersRespectOperationValuesAndCacheKeys(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binding-headers", "operation-headers-win-and-cache-keys-see-the-sent-value")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(account{Value: r.Header.Get("X-Revision")})
	}))
	defer server.Close()
	bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{Headers: map[string]string{"X-Revision": "instance"}})
	policy := freshStalePolicy()
	policy.KeyFields = []string{"header.x-revision"}
	operation := cachedAccountOperation(policy, nil)
	for _, value := range []string{"", "operation", "", "operation"} {
		request := &Request{Method: "GET", Path: "/accounts/1", Headers: http.Header{}}
		want := "instance"
		if value != "" {
			request.Headers.Set("X-Revision", value)
			want = value
		}
		got, err := CallOperation[account](t.Context(), bound, &OperationCall{Request: request}, operation)
		if err != nil || got.Value != want {
			t.Fatalf("header %q: %+v, %v", value, got, err)
		}
		if request.Headers.Get("X-Revision") != value {
			t.Fatal("call mutated the supplied request")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want two distinct cached variants", calls.Load())
	}
	request, err := bound.service.requestWithBindingHeaders(&Request{Headers: http.Header{"x-revision": []string{""}}}, operation)
	//lint:ignore SA1008 Deliberately inspect the caller's noncanonical key to prove the merge preserves it.
	if err != nil || len(request.Headers) != 1 || request.Headers["x-revision"][0] != "" {
		t.Fatalf("explicit empty case-insensitive operation header lost: %+v, %v", request, err)
	}
	operation.Contract.Idempotency = clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent, KeyHeader: "x-revision"}
	if _, err := CallOperation[account](t.Context(), bound, &OperationCall{Request: &Request{Path: "/accounts/1"}}, operation); !perrors.Is(err, CodeClientConfig) {
		t.Fatalf("static idempotency key returned %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("invalid idempotency binding reached the provider")
	}
}

func TestBindingHeadersReachConnectUnaryWithAuthAndCacheOverrides(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/inventory.v1.ApiService/GetItems" ||
			r.Header.Get("Content-Type") != connectwire.UnaryJSONContentType || r.Header.Get("Authorization") != "Bearer connect-token" {
			t.Error("the Connect unary request lost its declared transport or credential")
		}
		if len(r.Header.Values("X-Revision")) != 1 {
			t.Error("the Connect unary request duplicated the static header")
		}
		w.Header().Set("Content-Type", connectwire.UnaryJSONContentType)
		_ = json.NewEncoder(w).Encode(connectItem{Value: r.Header.Get("X-Revision")})
	}))
	defer server.Close()
	binding := connectServiceBinding()
	binding.Headers = map[string]string{"X-Revision": "instance"}
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), binding)
	cache := freshStalePolicy()
	cache.KeyFields = []string{"path.id", "header.x-revision"}
	operation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)}, nil,
		&clientcontract.ResiliencePolicy{Cache: cache})
	for _, value := range []string{"", "operation", "", "operation"} {
		call := newConnectCallInput()
		want := "instance"
		if value != "" {
			call.Request.Headers = http.Header{"X-Revision": []string{value}}
			want = value
		}
		original := cloneRequest(call.Request)
		got, err := CallOperation[connectItem](t.Context(), bound, call, operation)
		if err != nil || got.Value != want {
			t.Fatalf("Connect header %q: %+v, %v", value, got, err)
		}
		if !reflect.DeepEqual(call.Request, original) {
			t.Fatal("Connect dispatch or cache lookup mutated the caller's request")
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("Connect upstream calls = %d, want two cached header variants", calls.Load())
	}
	if _, err := CallOperation[connectItem](WithoutResponseCache(t.Context()), bound, newConnectCallInput(), operation); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("Connect cache bypass did not make a bound call")
	}
}

func TestBindingHeadersDirectOperationsKeepIndependentRequestState(t *testing.T) {
	keys := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Revision") != "instance" || r.Header.Get("Authorization") != "Bearer connect-token" {
			t.Error("direct operation lost static headers or credentials")
		}
		keys <- r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	binding := connectServiceBinding()
	binding.Headers = map[string]string{"X-Revision": "instance"}
	bound := boundTestClient(t, server.URL, testDescriptor(connectServiceProfiles()), binding)
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent, KeyHeader: "Idempotency-Key"})
	request := &Request{Method: http.MethodPut, Path: "/items", Headers: http.Header{"X-Input": []string{"operation"}}, Body: []byte(`{"value":"input"}`)}
	original := cloneRequest(request)
	for range 2 {
		if _, err := bound.DoOperation(t.Context(), request, operation); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(request, original) {
			t.Fatal("direct operation changed caller-owned request state")
		}
	}
	first, second := <-keys, <-keys
	if first == "" || second == "" || first == second {
		t.Fatal("separate direct operations reused an idempotency key")
	}
}

func TestBindingHeadersReachEveryStreamTransport(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binding-headers", "stream-headers-use-each-transports-declared-carrier")
	binding := ServiceBinding{Headers: map[string]string{"X-Revision": "instance"}, Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}}
	for _, protocol := range []string{"sse", "connect"} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Revision") != "instance" || r.Header.Get("Authorization") != "Bearer stream-token" {
					t.Error("static headers or credentials missing from stream")
				}
				if protocol == "sse" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"value\":\"one\"}\n\n"))
				} else {
					w.Header().Set("Content-Type", connectwire.StreamJSONContentType)
					_, _ = w.Write(connectStreamBody([]string{`{"value":"one"}`}))
				}
			}))
			defer server.Close()
			bound := connectBoundClient(t, server.URL, connectServiceProfiles(), binding)
			operation := serverStreamOperation(nil)
			if protocol == "connect" {
				operation = connectStreamOperation([]clientcontract.Transport{connectStreamTransport(clientcontract.EncodingJSON)}, nil)
			}
			stream, err := OpenOperationServerStream[streamItem](t.Context(), bound, newConnectCallInput(), operation)
			if err != nil {
				t.Fatal(err)
			}
			for range stream.Messages() {
			}
			if err := stream.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, providerOwned := range []bool{false, true} {
		name := "first-party-websocket"
		if providerOwned {
			name = "provider-websocket"
		}
		t.Run(name, func(t *testing.T) {
			server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
				if providerOwned {
					if peer.request.Header.Get("X-Revision") != "instance" || peer.request.Header.Get("Authorization") != "Bearer stream-token" {
						peer.server.fail("static header or credential missing from upgrade")
					}
					peer.readClose()
					return
				}
				if peer.request.Header.Get("X-Revision") != "" {
					peer.server.fail("first-party header leaked onto upgrade")
				}
				peer.expect(clientcontract.WebSocketFrameInit)
				peer.send(webSocketReadyFrame(false, ""))
				peer.send(webSocketResultFrame(""))
				peer.drain()
			}})
			descriptor := testDescriptor(connectServiceProfiles())
			descriptor.Contract.Protobuf = webSocketProtoProjection()
			bound := boundTestClient(t, server.URL, descriptor, binding)
			if providerOwned {
				stream, err := OpenByteStream(t.Context(), bound, &Request{}, providerWireTestOperation(true, nil, nil))
				if err != nil {
					t.Fatal(err)
				}
				if err := stream.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, webSocketServerStreamOperation(nil, nil))
				if err != nil {
					t.Fatal(err)
				}
				for range stream.Messages() {
				}
				if err := stream.Err(); err != nil {
					t.Fatal(err)
				}
			}
			server.waitForPlay(t)
			server.assertNoFailures(t)
			if !providerOwned {
				init := server.clientFrames()[0].(*clientcontract.WebSocketInitFrameV1)
				if len(init.Headers) != 1 || init.Headers[0].Name != "X-Revision" || init.Headers[0].Values[0] != "instance" || len(init.Credentials) != 1 {
					t.Fatalf("init did not carry ordinary headers beside credentials: %+v", init)
				}
			}
		})
	}
}
