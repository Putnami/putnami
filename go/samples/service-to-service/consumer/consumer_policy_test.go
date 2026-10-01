package consumer

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"time"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	"go.putnami.dev/examples/service-to-service/service"
)

// Policy cell of the client matrix: the request policy the create operation
// declares — an identity header, bounded attempts, a per-attempt and a total
// budget, and a circuit — decided by the contract and applied by the generated
// client. No consumer here writes a retry loop, a header or a deadline.

// policyProvider answers /items with the statuses `plan` lists, one per
// attempt, and records what each attempt carried. A plan shorter than the
// attempts made repeats its last entry.
func policyProvider(t *testing.T, plan []int, delay time.Duration) (string, func() []http.Header) {
	t.Helper()
	var mu sync.Mutex
	var received []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempt := len(received)
		received = append(received, r.Header.Clone())
		mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		status := plan[len(plan)-1]
		if attempt < len(plan) {
			status = plan[attempt]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status < 300 {
			_, _ = w.Write([]byte(`{"id":"3","name":"Sprocket","price":75}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"try again"}`))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return append([]http.Header(nil), received...)
	}
}

// A declared retryable status is retried up to the declared attempt count, and
// every attempt carries the same request identity — which is what lets the
// provider recognize a repeat instead of creating a second item.
func TestDeclaredRetryRepeatsTheSameRequestIdentity(t *testing.T) {
	spectest.Proves(t, matrixFeature, "the-declared-request-policy-is-the-one-applied", "a-declared-retry-repeats-one-request-identity")
	url, attempts := policyProvider(t, []int{503, 503, 201}, 0)
	item, err := CreateItem(t.Context(), boundClient(t, sampleBinding(url)), "Sprocket", 75)
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if item.Id != "3" {
		t.Fatalf("item = %+v", item)
	}

	got := attempts()
	if len(got) != 3 {
		t.Fatalf("the provider saw %d attempts, want the 3 the contract declares", len(got))
	}
	key := got[0].Get(service.IdempotencyKeyHeader)
	if key == "" {
		t.Fatal("the first attempt carried no request identity")
	}
	for index, header := range got {
		if header.Get(service.IdempotencyKeyHeader) != key {
			t.Fatalf("attempt %d carried a different request identity", index)
		}
	}
}

// The declared attempt count is a bound, not a suggestion: a provider that
// keeps failing sees exactly that many attempts and no more, and the consumer
// reads the last failure.
func TestDeclaredAttemptsAreABound(t *testing.T) {
	spectest.Proves(t, matrixFeature, "the-declared-request-policy-is-the-one-applied", "declared-attempts-are-a-bound")
	url, attempts := policyProvider(t, []int{503}, 0)
	_, err := CreateItem(t.Context(), boundClient(t, sampleBinding(url)), "Sprocket", 75)
	if err == nil {
		t.Fatal("a call that never succeeded returned no error")
	}
	if got := len(attempts()); got != 3 {
		t.Fatalf("the provider saw %d attempts, want the 3 the contract declares", got)
	}
}

// The declared per-attempt budget bounds an attempt that never answers: the
// consumer gives up on its own deadline rather than waiting for the provider.
func TestDeclaredBudgetEndsAnAttemptThatNeverAnswers(t *testing.T) {
	spectest.Proves(t, matrixFeature, "the-declared-request-policy-is-the-one-applied", "a-declared-budget-ends-an-attempt-that-never-answers")
	// Each attempt is bounded at 250ms and the whole call at 2s, so a provider
	// that takes a second per attempt can only end one way.
	url, attempts := policyProvider(t, []int{201}, time.Second)
	started := time.Now()
	_, err := CreateItem(t.Context(), boundClient(t, sampleBinding(url)), "Sprocket", 75)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a call past its declared budget succeeded")
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("the call took %s, longer than the declared total budget", elapsed)
	}
	if got := len(attempts()); got == 0 {
		t.Fatal("the provider saw no attempt at all")
	}
}

// After the declared consecutive failures the circuit opens: the next call is
// refused in the consumer, and the provider sees nothing.
func TestDeclaredCircuitStopsCallingAfterItsFailureThreshold(t *testing.T) {
	spectest.Proves(t, matrixFeature, "the-declared-request-policy-is-the-one-applied", "a-declared-circuit-stops-calling")
	url, attempts := policyProvider(t, []int{503}, 0)
	generated := boundClient(t, sampleBinding(url))

	// The circuit counts calls, not attempts: two failed calls reach the
	// declared threshold.
	for range 2 {
		if _, err := CreateItem(t.Context(), generated, "Sprocket", 75); err == nil {
			t.Fatal("a failing call succeeded")
		}
	}
	before := len(attempts())

	_, err := CreateItem(t.Context(), generated, "Sprocket", 75)
	if err == nil {
		t.Fatal("a call through an open circuit succeeded")
	}
	if !perrors.Is(err, client.CodeCircuitOpen) {
		t.Fatalf("error = %v, want %s", err, client.CodeCircuitOpen)
	}
	if got := len(attempts()); got != before {
		t.Fatalf("an open circuit still sent %d attempt(s)", got-before)
	}
}
