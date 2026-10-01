package client

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/errors"
)

// TestCircuitBreakerConcurrentAccess hammers AllowRequest / OnSuccess /
// OnFailure from N goroutines on a single CircuitBreaker.  The test is
// designed to surface data races under -race; it also verifies that the
// breaker is still in a legal state when all goroutines finish.
func TestCircuitBreakerConcurrentAccess(t *testing.T) {
	const (
		numGoroutines     = 50
		itersPerGoroutine = 200
	)

	cb := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 10,
		ResetTimeout:     10 * time.Millisecond,
		SuccessThreshold: 3,
	})

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := range numGoroutines {
		go func(id int) {
			defer wg.Done()
			for i := range itersPerGoroutine {
				// Alternate between success and failure based on iteration parity.
				cb.AllowRequest() // nolint: errcheck — open-circuit errors are expected and fine here
				if (id+i)%3 == 0 {
					cb.OnFailure()
				} else {
					cb.OnSuccess()
				}
				// Occasionally read the state to exercise State().
				if i%20 == 0 {
					s := cb.State()
					// State must be one of the three legal values.
					if s != CircuitClosed && s != CircuitOpen && s != CircuitHalfOpen {
						t.Errorf("goroutine %d: unexpected circuit state %v", id, s)
					}
				}
			}
		}(g)
	}

	wg.Wait()

	// The breaker must end in a legal state.
	finalState := cb.State()
	if finalState != CircuitClosed && finalState != CircuitOpen && finalState != CircuitHalfOpen {
		t.Errorf("unexpected final circuit state: %v", finalState)
	}
}

// TestCircuitBreakerInterceptorConcurrent runs Client.Do concurrently through
// the circuit-breaker interceptor on a single shared breaker (via
// CircuitBreaker, mirroring client.go:122-125).  The test ensures no data
// races occur and that total request outcomes are consistent.
func TestCircuitBreakerInterceptorConcurrent(t *testing.T) {
	const numGoroutines = 30

	var servedRequests atomic.Int64

	server := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			servedRequests.Add(1)
			// Alternate between success (200) and server error (500) responses
			// so the circuit sees both success and failure paths.
			n := servedRequests.Load()
			if n%4 == 0 {
				return &Response{StatusCode: 500}, nil
			}
			return &Response{StatusCode: 200}, nil
		},
	}

	// Build a client with a circuit breaker shared across all goroutines.
	client, err := NewBuilder().
		BaseURL("http://example.com").
		CircuitBreaker(CircuitBreakerConfig{
			FailureThreshold: 5,
			ResetTimeout:     5 * time.Millisecond,
			SuccessThreshold: 2,
		}).
		Retry(RetryConfig{MaxRetries: 0}).
		Transport(server).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer client.Close()

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := range numGoroutines {
		go func(id int) {
			defer wg.Done()
			for i := range 50 {
				_, _ = client.Do(context.Background(), &Request{
					Method: "GET",
					Path:   "/concurrent",
					// Vary the path slightly so requests are distinguishable in logs.
					Headers: http.Header{"X-Goroutine": {string(rune('A' + id%26))}},
				})
				// Give the reset-timeout a chance to trigger so we exercise
				// the half-open → closed transition as well.
				if i%10 == 0 {
					time.Sleep(6 * time.Millisecond)
				}
			}
		}(g)
	}

	wg.Wait()
	// No assertion on the counts — the goal is a clean -race run.
}

// TestCircuitBreakerInterceptorOnFailureViaError covers the
// circuit_breaker.go:174-176 branch where next() returns an error (not just a
// failure status code). The circuit breaker must call OnFailure() in that case.
func TestCircuitBreakerInterceptorOnFailureViaError(t *testing.T) {
	var attempts atomic.Int32

	transport := &mockTransport{
		handler: func(ctx context.Context, req *Request) (*Response, error) {
			n := attempts.Add(1)
			// Return an error (not a response) to exercise the err != nil path.
			return nil, fmt.Errorf("simulated transport error (attempt %d)", n)
		},
	}

	cb := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 3,
		ResetTimeout:     1 * time.Second,
		SuccessThreshold: 1,
	})

	client, err := NewBuilder().
		BaseURL("http://example.com").
		Interceptors(cb.Interceptor()).
		Retry(RetryConfig{MaxRetries: 0}).
		Transport(transport).
		Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer client.Close()

	// Three failing calls should open the circuit via the err != nil path.
	for range 3 {
		_, callErr := client.Do(context.Background(), &Request{Path: "/api"})
		if callErr == nil {
			t.Error("expected transport error, got nil")
		}
	}

	if cb.State() != CircuitOpen {
		t.Errorf("expected CircuitOpen after 3 transport errors, got %v", cb.State())
	}

	// The 4th call must be short-circuited by the breaker (CodeCircuitOpen).
	_, circErr := client.Do(context.Background(), &Request{Path: "/api"})
	if !errors.Is(circErr, CodeCircuitOpen) {
		t.Errorf("expected CodeCircuitOpen after circuit opened, got %T: %v", circErr, circErr)
	}
}

// TestCircuitBreakerHalfOpenLimitsConcurrentProbes asserts that, while half-open,
// no more than HalfOpenMaxConcurrent probe requests reach the transport at once —
// excess callers are rejected with CodeCircuitOpen so a burst arriving after the
// reset timeout cannot re-flood a downstream that has only just maybe-recovered.
func TestCircuitBreakerHalfOpenLimitsConcurrentProbes(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "circuit-states", "half-open-admits-a-bounded-number-of-trial-requests")
	const (
		goroutines = 12
		maxProbes  = 2
	)

	release := make(chan struct{})
	var reached atomic.Int32     // total requests that reached the transport
	var inTransport atomic.Int32 // currently inside the transport
	var maxInTransport atomic.Int32
	var rejected atomic.Int32 // callers short-circuited by the breaker

	next := InterceptorFunc(func(_ context.Context, _ *Request) (*Response, error) {
		reached.Add(1)
		cur := inTransport.Add(1)
		for {
			m := maxInTransport.Load()
			if cur <= m || maxInTransport.CompareAndSwap(m, cur) {
				break
			}
		}
		<-release // hold the probe in flight so its slot stays occupied
		inTransport.Add(-1)
		return &Response{StatusCode: 200}, nil
	})

	cb := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold:      1,
		ResetTimeout:          10 * time.Millisecond,
		SuccessThreshold:      goroutines, // high, so probe successes don't close the breaker mid-test
		HalfOpenMaxConcurrent: maxProbes,
	})
	interceptor := cb.Interceptor()

	// Open the breaker, then let it transition to half-open.
	cb.OnFailure()
	time.Sleep(20 * time.Millisecond)
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("expected half-open, got %v", cb.State())
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_, err := interceptor(context.Background(), &Request{Path: "/probe"}, next)
			if errors.Is(err, CodeCircuitOpen) {
				rejected.Add(1)
			}
		}()
	}

	// Wait until the permitted probes are blocked in the transport and every
	// other caller has been rejected.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reached.Load() == maxProbes && rejected.Load() == goroutines-maxProbes {
			break
		}
		time.Sleep(time.Millisecond)
	}

	gotReached, gotRejected := reached.Load(), rejected.Load()
	close(release) // unblock the held probes before any assertion can abort the test
	wg.Wait()

	if gotReached != maxProbes {
		t.Fatalf("requests reaching transport = %d, want %d (half-open must cap concurrent probes)", gotReached, maxProbes)
	}
	if gotRejected != goroutines-maxProbes {
		t.Fatalf("rejected callers = %d, want %d", gotRejected, goroutines-maxProbes)
	}
	if got := maxInTransport.Load(); got > maxProbes {
		t.Errorf("max concurrent probes in transport = %d, want <= %d", got, maxProbes)
	}
}
