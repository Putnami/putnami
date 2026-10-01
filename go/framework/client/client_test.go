package client

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/errors"
)

// mockTransport is a test transport that returns configured responses.
type mockTransport struct {
	handler func(ctx context.Context, req *Request) (*Response, error)
}

func (m *mockTransport) Do(ctx context.Context, req *Request) (*Response, error) {
	return m.handler(ctx, req)
}

func TestClientBuilder(t *testing.T) {
	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			return &Response{StatusCode: 200, Body: []byte(`{"ok":true}`)}, nil
		},
	}

	client, err := NewBuilder().
		BaseURL("http://example.com").
		ClientID("test-service").
		Timeout(5 * time.Second).
		Transport(transport).
		Build()

	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer client.Close()

	resp, err := client.Do(context.Background(), &Request{
		Method: "GET",
		Path:   "/api/health",
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestClientBuilderRequiresBaseURL(t *testing.T) {
	_, err := NewBuilder().Build()
	if err == nil {
		t.Fatal("expected error without baseURL")
	}
}

func TestClientIDInterceptor(t *testing.T) {
	var gotClientID string
	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			gotClientID = req.Headers.Get("X-Client-Id")
			return &Response{StatusCode: 200}, nil
		},
	}

	client, _ := NewBuilder().
		BaseURL("http://example.com").
		ClientID("my-service").
		Transport(transport).
		Build()

	client.Do(context.Background(), &Request{Path: "/test"})

	if gotClientID != "my-service" {
		t.Errorf("expected client ID 'my-service', got %q", gotClientID)
	}
}

func TestRetryOnError(t *testing.T) {
	var attempts atomic.Int32

	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			n := attempts.Add(1)
			if n < 3 {
				return nil, fmt.Errorf("connection refused")
			}
			return &Response{StatusCode: 200, Body: []byte("ok")}, nil
		},
	}

	client, _ := NewBuilder().
		BaseURL("http://example.com").
		Retry(RetryConfig{
			MaxRetries: 3,
			BaseDelay:  10 * time.Millisecond,
			MaxDelay:   50 * time.Millisecond,
		}).
		Transport(transport).
		Build()

	resp, err := client.Do(context.Background(), &Request{Path: "/api"})
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

func TestTotalTimeoutBoundsRetryLoop(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "bounded-retries", "the-total-timeout-bounds-the-whole-retry-loop")
	var attempts atomic.Int32
	transport := &mockTransport{
		handler: func(_ context.Context, _ *Request) (*Response, error) {
			attempts.Add(1)
			return nil, fmt.Errorf("connection refused")
		},
	}

	client, _ := NewBuilder().
		BaseURL("http://example.com").
		TotalTimeout(60 * time.Millisecond).
		Retry(RetryConfig{
			MaxRetries: 10,
			BaseDelay:  40 * time.Millisecond,
			MaxDelay:   40 * time.Millisecond,
		}).
		Transport(transport).
		Build()

	start := time.Now()
	_, err := client.Do(context.Background(), &Request{Path: "/api"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error once the total timeout elapses")
	}
	// Without the total bound, 10 retries × 40ms backoff would run ~400ms+.
	if elapsed > 300*time.Millisecond {
		t.Errorf("total timeout not enforced: elapsed %v", elapsed)
	}
	if got := attempts.Load(); got >= 11 {
		t.Errorf("expected the loop to be cut short, but ran all %d attempts", got)
	}
}

func TestCalculateBackoffCapsAtHighAttempts(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "bounded-retries", "backoff-stays-positive-at-high-attempt-counts")
	base := 200 * time.Millisecond
	maxDelay := 5 * time.Second
	// At extreme attempt counts float64(base)*2^(attempt-1) overflows int64 (or
	// reaches +Inf); the result must still clamp to [MaxDelay, MaxDelay+25%
	// jitter], never a negative/near-zero immediate retry.
	for _, attempt := range []int{37, 64, 128, 1000} {
		d := calculateBackoff(attempt, base, maxDelay)
		if d < maxDelay {
			t.Errorf("attempt %d: backoff %v < MaxDelay %v (cap defeated by overflow)", attempt, d, maxDelay)
		}
		if d > maxDelay+maxDelay/4 {
			t.Errorf("attempt %d: backoff %v exceeds MaxDelay + jitter", attempt, d)
		}
	}
}

func TestRetryOnRetryableStatus(t *testing.T) {
	var attempts atomic.Int32

	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			n := attempts.Add(1)
			if n < 2 {
				return &Response{StatusCode: 503}, nil
			}
			return &Response{StatusCode: 200}, nil
		},
	}

	client, _ := NewBuilder().
		BaseURL("http://example.com").
		Retry(RetryConfig{
			MaxRetries: 3,
			BaseDelay:  10 * time.Millisecond,
			MaxDelay:   50 * time.Millisecond,
		}).
		Transport(transport).
		Build()

	resp, err := client.Do(context.Background(), &Request{Path: "/api"})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestNoRetryOnNonRetryableStatus(t *testing.T) {
	var attempts atomic.Int32

	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			attempts.Add(1)
			return &Response{StatusCode: 404}, nil
		},
	}

	client, _ := NewBuilder().
		BaseURL("http://example.com").
		Retry(RetryConfig{
			MaxRetries: 3,
			BaseDelay:  10 * time.Millisecond,
			MaxDelay:   50 * time.Millisecond,
		}).
		Transport(transport).
		Build()

	resp, _ := client.Do(context.Background(), &Request{Path: "/api"})
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 attempt (no retry for 404), got %d", got)
	}
}

func TestRetryFiresOnRetryHook(t *testing.T) {
	var attempts atomic.Int32
	transport := &mockTransport{
		handler: func(_ context.Context, _ *Request) (*Response, error) {
			if attempts.Add(1) < 3 {
				return nil, fmt.Errorf("connection refused")
			}
			return &Response{StatusCode: 200}, nil
		},
	}

	var retried []int
	client, _ := NewBuilder().
		BaseURL("http://example.com").
		Retry(RetryConfig{
			MaxRetries: 3,
			BaseDelay:  time.Millisecond,
			MaxDelay:   5 * time.Millisecond,
			OnRetry: func(attempt int, _ time.Duration, _ *Response, err error) {
				retried = append(retried, attempt)
				if err == nil {
					t.Errorf("OnRetry attempt %d received nil triggering error", attempt)
				}
			},
		}).
		Transport(transport).
		Build()

	if _, err := client.Do(context.Background(), &Request{Path: "/x"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 transport calls => 2 retries => OnRetry fired for attempts 1 and 2.
	if len(retried) != 2 || retried[0] != 1 || retried[1] != 2 {
		t.Errorf("OnRetry attempts = %v, want [1 2]", retried)
	}
}

func TestCircuitBreaker(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "circuit-states", "the-breaker-opens-after-consecutive-failures-and-fails-fast")
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 3,
		ResetTimeout:     100 * time.Millisecond,
		SuccessThreshold: 1,
	})

	// Initially closed.
	if cb.State() != CircuitClosed {
		t.Errorf("expected closed, got %v", cb.State())
	}

	// Record failures to open the circuit.
	for range 3 {
		cb.OnFailure()
	}
	if cb.State() != CircuitOpen {
		t.Errorf("expected open after 3 failures, got %v", cb.State())
	}

	// Should reject requests.
	if err := cb.AllowRequest(); err == nil {
		t.Error("expected error from open circuit")
	}

	// Wait for reset timeout.
	time.Sleep(150 * time.Millisecond)

	// Should transition to half-open.
	if cb.State() != CircuitHalfOpen {
		t.Errorf("expected half-open after reset timeout, got %v", cb.State())
	}

	// Success should close the circuit.
	cb.OnSuccess()
	if cb.State() != CircuitClosed {
		t.Errorf("expected closed after success in half-open, got %v", cb.State())
	}
}

func TestCircuitBreakerFiresOnStateChange(t *testing.T) {
	var transitions [][2]CircuitState
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		ResetTimeout:     20 * time.Millisecond,
		SuccessThreshold: 1,
		OnStateChange: func(from, to CircuitState) {
			transitions = append(transitions, [2]CircuitState{from, to})
		},
	})

	cb.OnFailure() // closed, 1 failure — no transition
	cb.OnFailure() // closed -> open
	time.Sleep(40 * time.Millisecond)
	if cb.State() != CircuitHalfOpen { // open -> half-open
		t.Fatal("expected half-open after reset timeout")
	}
	cb.OnSuccess() // half-open -> closed

	want := [][2]CircuitState{
		{CircuitClosed, CircuitOpen},
		{CircuitOpen, CircuitHalfOpen},
		{CircuitHalfOpen, CircuitClosed},
	}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Errorf("transition %d = %v, want %v", i, transitions[i], want[i])
		}
	}
}

func TestCircuitBreakerInterceptor(t *testing.T) {
	var attempts atomic.Int32

	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			attempts.Add(1)
			return &Response{StatusCode: 500}, nil
		},
	}

	client, _ := NewBuilder().
		BaseURL("http://example.com").
		CircuitBreaker(CircuitBreakerConfig{
			FailureThreshold: 2,
			ResetTimeout:     1 * time.Second,
			SuccessThreshold: 1,
		}).
		Retry(RetryConfig{MaxRetries: 0}).
		Transport(transport).
		Build()

	// First two requests should go through (and fail).
	for range 2 {
		client.Do(context.Background(), &Request{Path: "/api"})
	}

	// Third request should be rejected by circuit breaker.
	_, err := client.Do(context.Background(), &Request{Path: "/api"})
	if err == nil {
		t.Fatal("expected circuit breaker error")
	}
	if !errors.Is(err, CodeCircuitOpen) {
		t.Errorf("expected CodeCircuitOpen error, got %T: %v", err, err)
	}
}

func TestHTTPTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"method": r.Method,
			"path":   r.URL.Path,
		})
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: server.URL,
		Timeout: 5 * time.Second,
	})

	resp, err := transport.Do(context.Background(), &Request{
		Method: "GET",
		Path:   "/api/test",
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]string
	json.Unmarshal(resp.Body, &body)
	if body["method"] != "GET" {
		t.Errorf("expected GET method, got %q", body["method"])
	}
	if body["path"] != "/api/test" {
		t.Errorf("expected path '/api/test', got %q", body["path"])
	}
}

func TestHTTPTransportWithQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"name": r.URL.Query().Get("name"),
		})
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportConfig{BaseURL: server.URL})

	resp, err := transport.Do(context.Background(), &Request{
		Path:  "/search",
		Query: map[string]string{"name": "test"},
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}

	var body map[string]string
	json.Unmarshal(resp.Body, &body)
	if body["name"] != "test" {
		t.Errorf("expected name 'test', got %q", body["name"])
	}
}

func TestHTTPTransportWithBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportConfig{BaseURL: server.URL})

	reqBody, _ := json.Marshal(map[string]string{"key": "value"})
	resp, err := transport.Do(context.Background(), &Request{
		Method: "POST",
		Path:   "/create",
		Body:   reqBody,
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}
}

func TestInterceptorChain(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "resilience-order", "the-chain-runs-in-the-specified-order")
	var order []string

	interceptorA := func(ctx context.Context, req *Request, next InterceptorFunc) (*Response, error) {
		order = append(order, "A-before")
		resp, err := next(ctx, req)
		order = append(order, "A-after")
		return resp, err
	}

	interceptorB := func(ctx context.Context, req *Request, next InterceptorFunc) (*Response, error) {
		order = append(order, "B-before")
		resp, err := next(ctx, req)
		order = append(order, "B-after")
		return resp, err
	}

	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			order = append(order, "transport")
			return &Response{StatusCode: 200}, nil
		},
	}

	client, _ := NewBuilder().
		BaseURL("http://example.com").
		Interceptors(interceptorA, interceptorB).
		Retry(RetryConfig{MaxRetries: 0}).
		Transport(transport).
		Build()

	client.Do(context.Background(), &Request{Path: "/"})

	expected := []string{"A-before", "B-before", "transport", "B-after", "A-after"}
	if len(order) != len(expected) {
		t.Fatalf("expected %d steps, got %d: %v", len(expected), len(order), order)
	}
	for i, exp := range expected {
		if order[i] != exp {
			t.Errorf("step %d: expected %q, got %q", i, exp, order[i])
		}
	}
}

func TestResponseHelpers(t *testing.T) {
	tests := []struct {
		status      int
		isSuccess   bool
		isRetryable bool
	}{
		{200, true, false},
		{201, true, false},
		{204, true, false},
		{301, false, false},
		{400, false, false},
		{404, false, false},
		{408, false, true},
		{429, false, true},
		{500, false, true},
		{502, false, true},
		{503, false, true},
	}

	for _, tt := range tests {
		resp := &Response{StatusCode: tt.status}
		if got := resp.IsSuccess(); got != tt.isSuccess {
			t.Errorf("status %d: IsSuccess() = %v, want %v", tt.status, got, tt.isSuccess)
		}
		if got := resp.IsRetryable(); got != tt.isRetryable {
			t.Errorf("status %d: IsRetryable() = %v, want %v", tt.status, got, tt.isRetryable)
		}
	}
}

func TestCircuitBreakerFailureInHalfOpen(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		ResetTimeout:     50 * time.Millisecond,
		SuccessThreshold: 2,
	})

	// Open the circuit.
	cb.OnFailure()
	if cb.State() != CircuitOpen {
		t.Fatalf("expected open, got %v", cb.State())
	}

	// Wait for half-open.
	time.Sleep(75 * time.Millisecond)
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("expected half-open, got %v", cb.State())
	}

	// A failure in half-open should re-open.
	cb.OnFailure()
	if cb.State() != CircuitOpen {
		t.Errorf("expected open after half-open failure, got %v", cb.State())
	}
}

func TestConnectTransportCompileCheck(t *testing.T) {
	transport := NewConnectTransport(ConnectTransportConfig{
		BaseURL: "http://localhost:8080",
		Timeout: 5 * time.Second,
	})
	if transport == nil {
		t.Fatal("expected non-nil ConnectTransport")
	}
	defer transport.Close()
}

func TestCircuitStateString(t *testing.T) {
	tests := []struct {
		state CircuitState
		want  string
	}{
		{CircuitClosed, "closed"},
		{CircuitOpen, "open"},
		{CircuitHalfOpen, "half-open"},
		{CircuitState(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("CircuitState(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestCircuitBreakerDefaults(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{})
	if cb.config.FailureThreshold != 5 {
		t.Errorf("default FailureThreshold = %d, want 5", cb.config.FailureThreshold)
	}
	if cb.config.ResetTimeout != 30*time.Second {
		t.Errorf("default ResetTimeout = %v, want 30s", cb.config.ResetTimeout)
	}
	if cb.config.SuccessThreshold != 2 {
		t.Errorf("default SuccessThreshold = %d, want 2", cb.config.SuccessThreshold)
	}
	if cb.config.HalfOpenMaxConcurrent != 2 {
		t.Errorf("default HalfOpenMaxConcurrent = %d, want 2 (SuccessThreshold)", cb.config.HalfOpenMaxConcurrent)
	}
	if len(cb.config.FailureStatuses) != 4 {
		t.Errorf("default FailureStatuses count = %d, want 4", len(cb.config.FailureStatuses))
	}
}

func TestCircuitBreakerIsFailureStatus(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{})
	if !cb.isFailureStatus(500) {
		t.Error("expected 500 to be a failure status")
	}
	if cb.isFailureStatus(200) {
		t.Error("expected 200 to not be a failure status")
	}
	if cb.isFailureStatus(404) {
		t.Error("expected 404 to not be a failure status")
	}
}

func TestCircuitBreakerSuccessResetsClosed(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "circuit-states", "the-breaker-closes-only-after-the-configured-successes")
	cb := NewCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 3})
	cb.OnFailure()
	cb.OnFailure()
	// Success in closed state resets failure count
	cb.OnSuccess()
	cb.OnFailure()
	cb.OnFailure()
	// Should still be closed (failures reset to 0 after success, so only 2 failures)
	if cb.State() != CircuitClosed {
		t.Errorf("expected closed after reset, got %v", cb.State())
	}
}

func TestRetryContextCancellation(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "bounded-retries", "retries-abort-when-the-callers-context-is-done")
	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			return nil, fmt.Errorf("always fails")
		},
	}

	client, _ := NewBuilder().
		BaseURL("http://example.com").
		Retry(RetryConfig{
			MaxRetries: 10,
			BaseDelay:  1 * time.Second,
			MaxDelay:   5 * time.Second,
		}).
		Transport(transport).
		Build()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.Do(ctx, &Request{Path: "/api"})
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
}

func TestRetryCustomRetryableFunc(t *testing.T) {
	var attempts atomic.Int32
	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			attempts.Add(1)
			return &Response{StatusCode: 418}, nil // I'm a teapot
		},
	}

	client, _ := NewBuilder().
		BaseURL("http://example.com").
		Retry(RetryConfig{
			MaxRetries: 2,
			BaseDelay:  10 * time.Millisecond,
			MaxDelay:   50 * time.Millisecond,
			RetryableFunc: func(resp *Response, err error) bool {
				return resp != nil && resp.StatusCode == 418
			},
		}).
		Transport(transport).
		Build()

	resp, _ := client.Do(context.Background(), &Request{Path: "/"})
	if resp.StatusCode != 418 {
		t.Errorf("expected 418, got %d", resp.StatusCode)
	}
	if got := attempts.Load(); got != 3 { // 1 initial + 2 retries
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

func TestCalculateBackoff(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "bounded-retries", "backoff-is-exponential-with-jitter-capped-at-the-maximum")
	base := 100 * time.Millisecond
	max := 1 * time.Second

	// First attempt: ~100ms + jitter
	d1 := calculateBackoff(1, base, max)
	if d1 < base || d1 > base+base/4+1 {
		t.Errorf("attempt 1 backoff %v out of expected range", d1)
	}

	// Large attempt: should be capped at max + jitter
	d10 := calculateBackoff(10, base, max)
	if d10 > max+max/4+1 {
		t.Errorf("attempt 10 backoff %v exceeds max + jitter", d10)
	}
}

func TestBuildChainEmpty(t *testing.T) {
	called := false
	transport := func(ctx context.Context, req *Request) (*Response, error) {
		called = true
		return &Response{StatusCode: 200}, nil
	}

	chain := buildChain(nil, transport)
	resp, err := chain(context.Background(), &Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("transport should have been called")
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestNewCircuitOpenError(t *testing.T) {
	resetAt := time.Now().Add(30 * time.Second)
	err := newCircuitOpenError(resetAt)
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(err, CodeCircuitOpen) {
		t.Error("error should match CodeCircuitOpen")
	}
}

// TestClientBuildDefaultTransport verifies that Build() without Transport
// creates a working client backed by the default HTTPTransport
// (client.go:102-107 — the transport == nil branch).
func TestClientBuildDefaultTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewBuilder().
		BaseURL(server.URL).
		Retry(RetryConfig{MaxRetries: 0}).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer client.Close()

	resp, err := client.Do(context.Background(), &Request{Method: "GET", Path: "/probe"})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// TestClientCloseWithRealTransport exercises the client.Close() path that
// delegates to the transport's Close method (client.go:157 — the
// closer.Close() branch). It uses a real HTTPTransport which implements Close.
func TestClientCloseWithRealTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Build without Transport so the client owns an HTTPTransport.
	client, err := NewBuilder().
		BaseURL(server.URL).
		Retry(RetryConfig{MaxRetries: 0}).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Close must not return an error and must invoke the transport's closer.
	if err := client.Close(); err != nil {
		t.Errorf("client.Close() returned error: %v", err)
	}
}

// Ensure transports implement the Transport interface.
var _ Transport = (*HTTPTransport)(nil)
var _ Transport = (*ConnectTransport)(nil)
var _ Transport = (*mockTransport)(nil)

// The resilience-order requirement also promises the chain is composed ONCE
// per client rather than per request. That is not observable through slice
// mutation — Build assembles its own slice — but it is observable as
// allocation behavior: a cached chain does not re-allocate one closure per
// interceptor on every Do, while a per-request composition must.
func TestInterceptorChainIsComposedOncePerClient(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "resilience-order", "the-chain-is-composed-once-per-client")
	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			return &Response{StatusCode: 200}, nil
		},
	}
	passthrough := func(ctx context.Context, req *Request, next InterceptorFunc) (*Response, error) {
		return next(ctx, req)
	}
	client, err := NewBuilder().
		BaseURL("http://example.com").
		Transport(transport).
		Interceptors(passthrough, passthrough, passthrough, passthrough).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer client.Close()

	req := &Request{Method: "GET", Path: "/x"}
	allocs := testing.AllocsPerRun(200, func() {
		if _, err := client.Do(context.Background(), req); err != nil {
			t.Fatalf("do: %v", err)
		}
	})
	// A cached chain allocates only per-request incidentals (a response, a
	// context value or two). Re-composing four interceptors plus the terminal
	// closure adds at least five closure allocations per call; the bound sits
	// well between the two.
	if allocs > 4 {
		t.Fatalf("Do allocates %.0f objects per request with 4 interceptors; the chain looks re-composed per call rather than once per client", allocs)
	}
}
