package deliverycli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

const (
	testRunCacheToken   = "run-cache-token-0b8e"
	testConfiguredToken = "configured-cache-token"
)

// runCredentialEnv points the provider at a fake cache server that records the
// bearer of every run-marker lookup, and at a fake Delivery whose cache
// capability route answers with handler. It returns the recorded bearers.
func runCredentialEnv(t *testing.T, deliveryCalls *atomic.Int64, handler func(w http.ResponseWriter, r *http.Request)) func() []string {
	t.Helper()
	var mu sync.Mutex
	var bearers []string
	cacheSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cache.RunMarkerLookupPath {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		bearers = append(bearers, strings.TrimPrefix(r.Header.Get(cache.AuthorizationHeader), "Bearer "))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(cache.RunMarkerResponse{ProtocolVersion: cache.ProtocolVersion})
	}))
	t.Cleanup(cacheSrv.Close)

	delivery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deliveryCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/delivery/records/capabilities/cache" {
			t.Errorf("Delivery request = %s %s, want POST /v1/delivery/records/capabilities/cache", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testRunCredential {
			t.Errorf("Delivery authorization = %q, want the run credential", got)
		}
		if body, _ := io.ReadAll(r.Body); string(body) != "{}" {
			t.Errorf("Delivery body = %q, want {}", body)
		}
		handler(w, r)
	}))
	t.Cleanup(delivery.Close)

	t.Setenv("PUTNAMI_CACHE_URL", cacheSrv.URL)
	t.Setenv("PUTNAMI_CACHE_TOKEN", testConfiguredToken)
	t.Setenv(SessionReporterIngestURLEnv, delivery.URL+"/v1/delivery/records")
	stubCacheProviderGuards(t, nil)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bearers...)
	}
}

// stubCacheProviderGuards replaces the inspection guard and removes the retry
// delay for one test.
func stubCacheProviderGuards(t *testing.T, deny error) *atomic.Int64 {
	t.Helper()
	var denied atomic.Int64
	previousDeny, previousBackoff := cacheProviderDenyInspection, capabilityRetryBackoff
	cacheProviderDenyInspection = func() error {
		denied.Add(1)
		return deny
	}
	// The production number of retries, without the wait between them.
	capabilityRetryBackoff = make([]time.Duration, len(previousBackoff))
	t.Cleanup(func() {
		cacheProviderDenyInspection, capabilityRetryBackoff = previousDeny, previousBackoff
	})
	return &denied
}

// runCredentialSession drives a whole provider session the way core does on a
// hosted run: initialize listing run-credential, authenticate, one marker
// lookup, shutdown. It returns the validated responses and stderr.
func runCredentialSession(t *testing.T, authenticate string) ([]*cache.ProviderResponse, string) {
	t.Helper()
	initialize, _ := json.Marshal(map[string]any{
		"protocolVersion": 1, "id": 1, "op": "initialize",
		"payload": map[string]any{
			"protocolVersion": 1, "blobExchangeDir": t.TempDir(), "mode": "full",
			"capabilities": []string{cache.CapabilityRunCredential},
		},
	})
	lines := []string{
		string(initialize),
		authenticate,
		`{"protocolVersion":1,"id":3,"op":"marker-lookup","payload":{"workspace":"w","branch":"main","commands":["build"],"selection":"all"}}`,
		`{"protocolVersion":1,"id":4,"op":"shutdown"}`,
	}
	var out, stderr bytes.Buffer
	if err := runCacheProviderSession(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &stderr, t.TempDir()); err != nil {
		t.Fatalf("session: %v", err)
	}
	return decodeResponses(t, &out), stderr.String()
}

// authenticateLine is the authenticate request that carries testRunCredential.
func authenticateLine() string {
	encoded, _ := json.Marshal(map[string]any{
		"protocolVersion": 1, "id": 2, "op": "authenticate",
		"payload": map[string]any{"credential": testRunCredential},
	})
	return string(encoded)
}

func echoedRunCredential(t *testing.T, response *cache.ProviderResponse) bool {
	t.Helper()
	if !response.OK {
		t.Fatalf("initialize = %+v", response.Error)
	}
	result, diags := cache.ParseAndValidateInitializeResult(response.Payload)
	if result == nil {
		t.Fatalf("initialize result: %v", diags)
	}
	return slices.Contains(result.Capabilities, cache.CapabilityRunCredential)
}

func TestCacheProviderAuthenticateUsesTheRunCacheTokenOnTheNextRequest(t *testing.T) {
	var calls atomic.Int64
	bearers := runCredentialEnv(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"protocolVersion": 1, "token": testRunCacheToken, "expiresAt": "2026-10-02T06:00:00Z", "addedLater": true,
		})
	})
	responses, stderr := runCredentialSession(t, authenticateLine())
	if len(responses) != 4 {
		t.Fatalf("got %d responses, want 4", len(responses))
	}
	if !echoedRunCredential(t, responses[0]) {
		t.Fatal("initialize did not echo run-credential")
	}
	if !responses[1].OK || responses[1].ID != 2 {
		t.Fatalf("authenticate = %+v", responses[1])
	}
	if result, diags := cache.ParseAndValidateAuthenticateResult(responses[1].Payload); result == nil {
		t.Fatalf("authenticate result: %v", diags)
	}
	if got := bearers(); len(got) != 1 || got[0] != testRunCacheToken {
		t.Fatalf("marker lookup bearers = %v, want the run cache token", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("Delivery calls = %d, want 1", calls.Load())
	}
	for _, secret := range []string{testRunCredential, testRunCacheToken} {
		if strings.Contains(stderr, secret) {
			t.Fatalf("stderr carries a secret: %s", stderr)
		}
	}
}

func TestCacheProviderAuthenticate204KeepsTheConfiguredToken(t *testing.T) {
	var calls atomic.Int64
	bearers := runCredentialEnv(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	responses, stderr := runCredentialSession(t, authenticateLine())
	if !echoedRunCredential(t, responses[0]) || !responses[1].OK {
		t.Fatalf("initialize/authenticate = %+v / %+v", responses[0], responses[1])
	}
	if got := bearers(); len(got) != 1 || got[0] != testConfiguredToken {
		t.Fatalf("marker lookup bearers = %v, want the configured token", got)
	}
	if !strings.Contains(stderr, "the run has no cache token") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestCacheProviderAuthenticateRefusals pins that a coded 4xx refusal other
// than 404 is final and ends the session, whatever its code.
func TestCacheProviderAuthenticateRefusals(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     any
		wantCode string
		wantText string
	}{
		{
			name: "Delivery code passes through", status: http.StatusForbidden,
			body:     map[string]any{"error": "forbidden", "code": "cache_not_permitted", "message": "run " + testRunCredential + " may not use the cache"},
			wantCode: "cache_not_permitted", wantText: "run <redacted> may not use the cache",
		},
		{
			name: "a terminal run", status: http.StatusConflict,
			body:     map[string]any{"error": "the run has reached a terminal state", "message": "the run has reached a terminal state", "code": "run_terminal"},
			wantCode: "run_terminal", wantText: "terminal state",
		},
		{
			name: "an invalid run credential", status: http.StatusUnauthorized,
			body:     map[string]any{"error": "invalid or expired run credential", "message": "invalid or expired run credential", "code": "run_credential_invalid"},
			wantCode: "run_credential_invalid", wantText: "invalid or expired run credential",
		},
		{
			name: "invalid code becomes cache_refused", status: http.StatusUnauthorized,
			body:     map[string]any{"error": "unauthorized", "code": "BAD CODE"},
			wantCode: "cache_refused", wantText: "unauthorized",
		},
		{
			name: "an uncoded 4xx becomes cache_refused", status: http.StatusBadRequest,
			body:     "bad request",
			wantCode: "cache_refused", wantText: "HTTP 400",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			runCredentialEnv(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
				if text, ok := test.body.(string); ok {
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, text)
					return
				}
				writeJSON(w, test.status, test.body)
			})
			responses, stderr := runCredentialSession(t, authenticateLine())
			authenticate := responses[1]
			if authenticate.OK || authenticate.Error == nil || authenticate.Error.Code != test.wantCode {
				t.Fatalf("authenticate = %+v, want refusal %s", authenticate, test.wantCode)
			}
			if !strings.Contains(authenticate.Error.Message, test.wantText) {
				t.Fatalf("message = %q, want %q", authenticate.Error.Message, test.wantText)
			}
			if calls.Load() != 1 {
				t.Fatalf("Delivery calls = %d, want 1: a refusal is never retried", calls.Load())
			}
			for _, secret := range []string{testRunCredential, testRunCacheToken} {
				if strings.Contains(stderr, secret) || strings.Contains(authenticate.Error.Message, secret) {
					t.Fatalf("a secret leaked: stderr %q, message %q", stderr, authenticate.Error.Message)
				}
			}
		})
	}
}

// TestCacheProviderAuthenticateKeepsTheConfiguredTokenWhenDeliveryCannotAnswer
// pins that an answer saying nothing about the run (a 404 from a Delivery
// without the route, an unavailable Delivery, or an unusable grant) keeps the
// configured token source and the session.
func TestCacheProviderAuthenticateKeepsTheConfiguredTokenWhenDeliveryCannotAnswer(t *testing.T) {
	for _, test := range []struct {
		name      string
		handler   func(w http.ResponseWriter, r *http.Request)
		wantCalls int64
		wantText  string
	}{
		{
			name:      "a Delivery without the route",
			handler:   func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
			wantCalls: 1, wantText: "HTTP 404",
		},
		{
			name: "a coded 404",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "invalid_request"})
			},
			wantCalls: 1, wantText: "HTTP 404",
		},
		{
			name: "unavailable Delivery is retried once",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "busy " + testRunCredential, "code": "cache_capability_unavailable"})
			},
			wantCalls: 2, wantText: "last answer HTTP 503",
		},
		{
			name: "failing Delivery",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed", "code": "cache_capability_failed"})
			},
			wantCalls: 2, wantText: "last answer HTTP 502",
		},
		{
			name: "rate limited Delivery",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			},
			wantCalls: 2, wantText: "last answer HTTP 429",
		},
		{
			name: "a transport failure",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			},
			wantCalls: 2, wantText: "Delivery did not answer the cache capability;",
		},
		{
			name: "a token that cannot travel in a header",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusCreated, map[string]any{"protocolVersion": 1, "token": "two words", "expiresAt": "2026-10-02T06:00:00Z"})
			},
			wantCalls: 1, wantText: "cannot use",
		},
		{
			name: "another protocol version",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusCreated, map[string]any{"protocolVersion": 2, "token": testRunCacheToken, "expiresAt": "2026-10-02T06:00:00Z"})
			},
			wantCalls: 1, wantText: "cannot use",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			bearers := runCredentialEnv(t, &calls, test.handler)
			responses, stderr := runCredentialSession(t, authenticateLine())
			if len(responses) != 4 {
				t.Fatalf("got %d responses, want 4: the session goes on", len(responses))
			}
			if !responses[1].OK {
				t.Fatalf("authenticate = %+v, want ok", responses[1].Error)
			}
			if result, diags := cache.ParseAndValidateAuthenticateResult(responses[1].Payload); result == nil {
				t.Fatalf("authenticate result: %v", diags)
			}
			if got := bearers(); len(got) != 1 || got[0] != testConfiguredToken {
				t.Fatalf("marker lookup bearers = %v, want the configured token", got)
			}
			if calls.Load() != test.wantCalls {
				t.Fatalf("Delivery calls = %d, want %d", calls.Load(), test.wantCalls)
			}
			var lines []string
			for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
				if strings.Contains(line, "run credential:") {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 || !strings.Contains(lines[0], test.wantText) || !strings.HasSuffix(lines[0], "using the configured cache token source") {
				t.Fatalf("run credential lines = %q, want one naming %q", lines, test.wantText)
			}
			if len(lines[0]) > len("cache-provider: run credential: ")+cacheRefusalMessageBytes {
				t.Fatalf("the diagnostic has %d bytes, want it bounded", len(lines[0]))
			}
			for _, secret := range []string{testRunCredential, testRunCacheToken} {
				if strings.Contains(stderr, secret) {
					t.Fatalf("stderr carries a secret: %q", stderr)
				}
			}
		})
	}
}

func TestCacheProviderEchoesRunCredentialOnlyWhenItCanUseIt(t *testing.T) {
	for _, test := range []struct {
		name       string
		cacheURL   string
		ingest     string
		deny       error
		listed     bool
		wantEcho   bool
		wantDenied int64
	}{
		{name: "everything in place", cacheURL: "https://cache.example", ingest: "https://api.example/v1/delivery/records", listed: true, wantEcho: true, wantDenied: 1},
		{name: "core holds no run credential", cacheURL: "https://cache.example", ingest: "https://api.example/v1/delivery/records"},
		{name: "no remote cache", ingest: "https://api.example/v1/delivery/records", listed: true},
		{name: "no ingest base", cacheURL: "https://cache.example", listed: true},
		{name: "ingest base over plain http", cacheURL: "https://cache.example", ingest: "http://api.example/v1/delivery/records", listed: true},
		{name: "inspection cannot be denied", cacheURL: "https://cache.example", ingest: "https://api.example/v1/delivery/records", deny: errors.New("prctl refused"), listed: true, wantDenied: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PUTNAMI_CACHE_URL", test.cacheURL)
			t.Setenv("PUTNAMI_CACHE_TOKEN", testConfiguredToken)
			t.Setenv(SessionReporterIngestURLEnv, test.ingest)
			denied := stubCacheProviderGuards(t, test.deny)
			var stderr bytes.Buffer
			s := newProviderSession(t.TempDir(), &stderr)
			defer s.close()
			params := &cache.InitializeParams{ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir()}
			if test.listed {
				params.Capabilities = []string{cache.CapabilityRunCredential}
			}
			result := mustInit(t, s, params)
			if got := slices.Contains(result.Capabilities, cache.CapabilityRunCredential); got != test.wantEcho {
				t.Fatalf("echoed run-credential = %v, want %v (stderr %q)", got, test.wantEcho, stderr.String())
			}
			if denied.Load() != test.wantDenied {
				t.Fatalf("inspection guard ran %d times, want %d", denied.Load(), test.wantDenied)
			}
			if !test.wantEcho {
				// Without the echo core never sends authenticate; one that
				// arrives anyway is refused without a Delivery call.
				response := s.handle(&cache.ProviderRequest{
					ProtocolVersion: cache.ProviderProtocolMinVersion, ID: 2, Op: cache.OpAuthenticate,
					Payload: json.RawMessage(`{"credential":"` + testRunCredential + `"}`),
				})
				if response.OK || response.Error.Code != "unexpected_op" {
					t.Fatalf("authenticate without the echo = %+v", response)
				}
			}
		})
	}
}

func TestCacheProviderAuthenticateRunsOnceAndStrictly(t *testing.T) {
	var calls atomic.Int64
	runCredentialEnv(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"protocolVersion": 1, "token": testRunCacheToken, "expiresAt": "2026-10-02T06:00:00Z"})
	})
	s := newProviderSession(t.TempDir(), io.Discard)
	defer s.close()
	mustInit(t, s, &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolMinVersion, BlobExchangeDir: t.TempDir(),
		Capabilities: []string{cache.CapabilityRunCredential},
	})
	authenticate := func(payload string) *cache.ProviderResponse {
		return s.handle(&cache.ProviderRequest{
			ProtocolVersion: cache.ProviderProtocolMinVersion, ID: 2, Op: cache.OpAuthenticate, Payload: json.RawMessage(payload),
		})
	}

	// Strict params: an unknown member or an invalid credential is refused,
	// and the refusal never quotes the credential.
	for _, payload := range []string{
		`{"credential":"` + testRunCredential + `","extra":1}`,
		`{"credential":"has a space ` + testRunCredential + `"}`,
	} {
		response := authenticate(payload)
		if response.OK || response.Error.Code != "invalid_params" || strings.Contains(response.Error.Message, testRunCredential) {
			t.Fatalf("authenticate %s = %+v", payload, response)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("Delivery calls = %d after invalid params, want 0", calls.Load())
	}

	if response := authenticate(`{"credential":"` + testRunCredential + `"}`); !response.OK {
		t.Fatalf("authenticate = %+v", response.Error)
	}
	if response := authenticate(`{"credential":"` + testRunCredential + `"}`); response.OK || response.Error.Code != "unexpected_op" {
		t.Fatalf("second authenticate = %+v, want unexpected_op", response)
	}
	if calls.Load() != 1 {
		t.Fatalf("Delivery calls = %d, want 1", calls.Load())
	}
}
