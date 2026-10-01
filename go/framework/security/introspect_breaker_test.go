package security

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

// --- Breaker state machine (white-box) ---

func TestCircuitBreaker_DisabledIsNil(t *testing.T) {
	if b := newCircuitBreaker(BreakerConfig{}); b != nil {
		t.Fatalf("newCircuitBreaker(zero) = %v, want nil (disabled)", b)
	}
	if b := newCircuitBreaker(BreakerConfig{FailureThreshold: -1}); b != nil {
		t.Fatalf("newCircuitBreaker(negative) = %v, want nil", b)
	}
	// nil breaker admits everything and record is a no-op.
	var nilB *circuitBreaker
	if allowed, trial := nilB.allow(); !allowed || trial {
		t.Errorf("nil.allow() = (%v,%v), want (true,false)", allowed, trial)
	}
	nilB.record(false, false) // must not panic
	nilB.release(true)        // must not panic
}

func TestCircuitBreaker_DefaultsOpenDuration(t *testing.T) {
	b := newCircuitBreaker(BreakerConfig{FailureThreshold: 1})
	if b.openFor != defaultBreakerOpenDuration {
		t.Errorf("openFor = %v, want default %v", b.openFor, defaultBreakerOpenDuration)
	}
}

func TestCircuitBreaker_OpensAfterThreshold(t *testing.T) {
	b := newCircuitBreaker(BreakerConfig{FailureThreshold: 3, OpenDuration: time.Hour})

	for i := 0; i < 2; i++ {
		if allowed, _ := b.allow(); !allowed {
			t.Fatalf("call %d denied before threshold", i)
		}
		b.record(false, false)
	}
	// Still closed at 2 failures.
	if allowed, _ := b.allow(); !allowed {
		t.Fatal("breaker opened before threshold")
	}
	b.record(false, false) // third failure crosses threshold
	if allowed, _ := b.allow(); allowed {
		t.Fatal("breaker did not open at threshold")
	}
}

func TestCircuitBreaker_SuccessResetsFailureRun(t *testing.T) {
	b := newCircuitBreaker(BreakerConfig{FailureThreshold: 3, OpenDuration: time.Hour})
	b.record(false, false)
	b.record(false, false)
	b.record(true, false) // reset
	b.record(false, false)
	b.record(false, false)
	// Only two failures since the reset — must still be closed.
	if allowed, _ := b.allow(); !allowed {
		t.Fatal("breaker opened despite an intervening success resetting the run")
	}
}

func TestCircuitBreaker_HalfOpenTrialClosesOnSuccess(t *testing.T) {
	b := newCircuitBreaker(BreakerConfig{FailureThreshold: 2, OpenDuration: time.Hour})
	b.record(false, false)
	b.record(false, false) // open
	// Force the open window to have elapsed (deterministic, no sleep).
	b.mu.Lock()
	b.openedAt = time.Now().Add(-2 * time.Hour)
	b.mu.Unlock()

	allowed, trial := b.allow()
	if !allowed || !trial {
		t.Fatalf("half-open allow() = (%v,%v), want (true,true)", allowed, trial)
	}
	// A concurrent caller during the probe is denied (only one trial in flight).
	if a2, _ := b.allow(); a2 {
		t.Fatal("second caller admitted during half-open probe")
	}
	b.record(true, trial) // probe succeeded → closed
	if allowed, _ := b.allow(); !allowed {
		t.Fatal("breaker did not close after a successful trial")
	}
}

func TestCircuitBreaker_HalfOpenTrialReopensOnFailure(t *testing.T) {
	b := newCircuitBreaker(BreakerConfig{FailureThreshold: 2, OpenDuration: time.Hour})
	b.record(false, false)
	b.record(false, false) // open
	b.mu.Lock()
	b.openedAt = time.Now().Add(-2 * time.Hour)
	b.mu.Unlock()

	_, trial := b.allow()
	b.record(false, trial) // probe failed → re-open, and free the probe slot
	if allowed, _ := b.allow(); allowed {
		t.Fatal("breaker did not re-open after a failed trial")
	}
	// The probe slot was released, so a later window admits a fresh trial.
	b.mu.Lock()
	b.openedAt = time.Now().Add(-2 * time.Hour)
	b.mu.Unlock()
	if allowed, trial2 := b.allow(); !allowed || !trial2 {
		t.Fatal("no fresh trial admitted after the failed probe freed the slot")
	}
}

// TestCircuitBreaker_ReleaseIsNeutralWhileClosed pins the 429 outcome in the
// closed state: release neither counts as a failure nor resets the run.
func TestCircuitBreaker_ReleaseIsNeutralWhileClosed(t *testing.T) {
	b := newCircuitBreaker(BreakerConfig{FailureThreshold: 2, OpenDuration: time.Hour})
	b.record(false, false) // one genuine failure
	for i := 0; i < 10; i++ {
		b.release(false) // a run of throttled calls, far past the threshold
	}

	b.mu.Lock()
	failures := b.failures
	b.mu.Unlock()
	if failures != 1 {
		t.Fatalf("failures = %d after throttled calls, want 1 (neither reset nor extended)", failures)
	}
	if allowed, _ := b.allow(); !allowed {
		t.Fatal("throttled calls opened the breaker")
	}
	b.record(false, false) // second genuine failure: the run survived, so it opens
	if allowed, _ := b.allow(); allowed {
		t.Fatal("throttled calls reset the failure run: the second genuine failure did not open the breaker")
	}
}

// TestCircuitBreaker_HalfOpenTrialReleaseFreesProbeSlot pins the wedge guard: a
// throttled half-open trial frees its probe slot without closing the breaker
// or re-arming the open window, so the next caller probes at once.
func TestCircuitBreaker_HalfOpenTrialReleaseFreesProbeSlot(t *testing.T) {
	b := newCircuitBreaker(BreakerConfig{FailureThreshold: 2, OpenDuration: time.Hour})
	b.record(false, false)
	b.record(false, false) // open
	elapsed := time.Now().Add(-2 * time.Hour)
	b.mu.Lock()
	b.openedAt = elapsed
	b.mu.Unlock()

	allowed, trial := b.allow()
	if !allowed || !trial {
		t.Fatalf("half-open allow() = (%v,%v), want (true,true)", allowed, trial)
	}
	b.release(false) // a caller that holds no probe slot must not free this one
	if a2, _ := b.allow(); a2 {
		t.Fatal("release(false) freed a probe slot it does not hold")
	}
	b.release(trial) // the probe was throttled

	b.mu.Lock()
	failures, openedAt, inFlight := b.failures, b.openedAt, b.trialInFlight
	b.mu.Unlock()
	if inFlight {
		t.Fatal("release did not free the half-open probe slot: the breaker would wedge")
	}
	if failures != 2 {
		t.Errorf("failures = %d after a throttled trial, want 2 (release must not close the breaker or extend the run)", failures)
	}
	if !openedAt.Equal(elapsed) {
		t.Errorf("openedAt = %v after a throttled trial, want %v (release must not re-arm the open window)", openedAt, elapsed)
	}
	if allowed, trial2 := b.allow(); !allowed || !trial2 {
		t.Fatal("no fresh trial admitted after a throttled probe released the slot")
	}
}

// --- Breaker through the Introspect middleware ---

// switchableIntrospectServer serves active tokens until down is set, then fails
// every request with 503 — a stand-in for a /introspect outage.
func switchableIntrospectServer(t *testing.T) (url string, down *atomic.Bool, calls *atomic.Int64) {
	t.Helper()
	down = &atomic.Bool{}
	calls = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeIntrospectJSON(t, w, activeJSON("svc"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, down, calls
}

func TestIntrospect_BreakerShortCircuitsUncachedAfterOutage(t *testing.T) {
	url, down, calls := switchableIntrospectServer(t)
	down.Store(true) // endpoint is out from the start

	mw := Introspect(IntrospectConfig{
		Endpoint: url,
		Breaker:  BreakerConfig{FailureThreshold: 2, OpenDuration: time.Hour},
	})

	beforeOpen := introspectCounterValue(t, introspectBreakerOpen)

	// First two uncached tokens each reach the (failing) endpoint; the second
	// crosses the threshold and opens the breaker.
	runIntrospect(mw, "a")
	runIntrospect(mw, "b")
	if got := calls.Load(); got != 2 {
		t.Fatalf("endpoint calls before open = %d, want 2", got)
	}

	// Subsequent uncached tokens are short-circuited: no more endpoint calls.
	for i := 0; i < 5; i++ {
		if ctx := runIntrospect(mw, "later"+string(rune('0'+i))); ctx.User != nil {
			t.Fatalf("token resolved while breaker open, want fail closed")
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("endpoint calls after open = %d, want still 2 (short-circuited)", got)
	}
	if got := introspectCounterValue(t, introspectBreakerOpen) - beforeOpen; got != 5 {
		t.Errorf("breaker_open delta = %d, want 5", got)
	}
}

func TestIntrospect_BreakerServesCacheWhileOpen(t *testing.T) {
	url, down, calls := switchableIntrospectServer(t)

	mw := Introspect(IntrospectConfig{
		Endpoint: url,
		Cache:    newTestCache(t),
		Breaker:  BreakerConfig{FailureThreshold: 2, OpenDuration: time.Hour},
	})

	// 1. Prime the cache with token X while the endpoint is healthy.
	if runIntrospect(mw, "X").User == nil {
		t.Fatal("expected X to resolve while healthy")
	}
	callsAfterPrime := calls.Load()

	// 2. Endpoint goes down; trip the breaker with uncached tokens.
	down.Store(true)
	runIntrospect(mw, "trip-0")
	runIntrospect(mw, "trip-1") // opens the breaker

	beforeOpen := introspectCounterValue(t, introspectBreakerOpen)
	callsAfterTrip := calls.Load()

	// 3. The recently introspected token keeps working — from cache, no network,
	//    not counted as breaker_open.
	if runIntrospect(mw, "X").User == nil {
		t.Error("cached token X failed while breaker open, want it to keep serving")
	}
	// 4. An uncached token is rejected without touching the network.
	if runIntrospect(mw, "Y").User != nil {
		t.Error("uncached token Y resolved while breaker open, want fail closed")
	}

	if got := calls.Load() - callsAfterTrip; got != 0 {
		t.Errorf("endpoint calls during open serve = %d, want 0", got)
	}
	if got := introspectCounterValue(t, introspectBreakerOpen) - beforeOpen; got != 1 {
		t.Errorf("breaker_open delta = %d, want 1 (only the uncached Y)", got)
	}
	_ = callsAfterPrime
}

func TestIntrospect_HealthyEndpointNeverOpensBreaker(t *testing.T) {
	url, _, _ := switchableIntrospectServer(t) // stays healthy

	mw := Introspect(IntrospectConfig{
		Endpoint: url,
		Breaker:  BreakerConfig{FailureThreshold: 2, OpenDuration: time.Hour},
	})

	beforeOpen := introspectCounterValue(t, introspectBreakerOpen)
	for i := 0; i < 10; i++ {
		if runIntrospect(mw, "tok"+string(rune('0'+i))).User == nil {
			t.Fatalf("healthy introspection %d failed", i)
		}
	}
	if got := introspectCounterValue(t, introspectBreakerOpen) - beforeOpen; got != 0 {
		t.Errorf("breaker_open delta = %d on a healthy endpoint, want 0", got)
	}
}

// --- 429 backpressure through the Introspect middleware ---

// writeThrottled answers 429 Too Many Requests, with a Retry-After header when
// retryAfter is non-empty.
func writeThrottled(w http.ResponseWriter, retryAfter string) {
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(http.StatusTooManyRequests)
}

// scriptedIntrospectServer answers every introspection with the status the test
// stored last: 200 serves an active token, 429 throttles with "Retry-After: 0"
// (so the retry runs at once), anything else is written bare. It lets a test
// walk the endpoint through outage, throttling, and recovery between requests.
func scriptedIntrospectServer(t *testing.T, initial int) (url string, status, calls *atomic.Int64) {
	t.Helper()
	status = &atomic.Int64{}
	status.Store(int64(initial))
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		switch code := int(status.Load()); code {
		case http.StatusOK:
			writeIntrospectJSON(t, w, activeJSON("svc"))
		case http.StatusTooManyRequests:
			writeThrottled(w, "0")
		default:
			w.WriteHeader(code)
		}
	})
	return srv.URL, status, calls
}

// TestIntrospect_ThrottleBurstNeverOpensBreaker is the issue's acceptance
// check: a burst of 429s — each surviving its single retry — fails every
// request closed but cannot open the breaker, even at FailureThreshold 1 where
// one counted failure would open it for an hour.
func TestIntrospect_ThrottleBurstNeverOpensBreaker(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "credential-validation", "introspection-throttling-fails-closed-without-opening-the-breaker")
	url, status, calls := scriptedIntrospectServer(t, http.StatusTooManyRequests)

	mw := Introspect(IntrospectConfig{
		Endpoint: url,
		Breaker:  BreakerConfig{FailureThreshold: 1, OpenDuration: time.Hour},
	})

	beforeThrottled := introspectCounterValue(t, introspectThrottled)
	beforeErrors := introspectCounterValue(t, introspectEndpointError)
	beforeOpen := introspectCounterValue(t, introspectBreakerOpen)
	beforeCalls := introspectCounterValue(t, introspectEndpointCall)

	// Distinct uncached tokens, so every request is its own singleflight cohort
	// and folds its own outcome into the breaker.
	const burst = 12
	var wg sync.WaitGroup
	results := make([]*phttp.Context, burst)
	for i := range burst {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = runIntrospect(mw, "burst-"+strconv.Itoa(i))
		}(i)
	}
	wg.Wait()

	for i, ctx := range results {
		if ctx.User != nil {
			t.Fatalf("request %d: a 429 authenticated %+v, want fail closed", i, ctx.User)
		}
	}
	// Every request reached the endpoint (first call + one retry): none was
	// short-circuited by an open breaker.
	if got := calls.Load(); got != 2*burst {
		t.Errorf("endpoint calls = %d, want %d (a first call and one retry per request)", got, 2*burst)
	}
	if got := introspectCounterValue(t, introspectEndpointCall) - beforeCalls; got != 2*burst {
		t.Errorf("endpoint_call delta = %d, want %d (the retry is an upstream call too)", got, 2*burst)
	}
	if got := introspectCounterValue(t, introspectThrottled) - beforeThrottled; got != burst {
		t.Errorf("throttled delta = %d, want %d (one per failed-closed request)", got, burst)
	}
	if got := introspectCounterValue(t, introspectEndpointError) - beforeErrors; got != 0 {
		t.Errorf("endpoint_error delta = %d, want 0 (a 429 is not an endpoint failure)", got)
	}

	// The breaker is still closed: once the endpoint stops throttling, an
	// uncached token reaches it and resolves.
	status.Store(http.StatusOK)
	if ctx := runIntrospect(mw, "after-burst"); ctx.User == nil {
		t.Fatal("token failed after the burst: the 429s opened the breaker")
	}
	if got := calls.Load(); got != 2*burst+1 {
		t.Errorf("endpoint calls after the burst = %d, want %d", got, 2*burst+1)
	}
	if got := introspectCounterValue(t, introspectBreakerOpen) - beforeOpen; got != 0 {
		t.Errorf("breaker_open delta = %d, want 0", got)
	}
}

// TestIntrospect_ThrottleNeitherResetsNorExtendsFailureRun interleaves a 429
// between two genuine failures: had it counted, the breaker would open one
// request early; had it reset the run, the second failure would not open it.
func TestIntrospect_ThrottleNeitherResetsNorExtendsFailureRun(t *testing.T) {
	url, status, calls := scriptedIntrospectServer(t, http.StatusServiceUnavailable)

	mw := Introspect(IntrospectConfig{
		Endpoint: url,
		Breaker:  BreakerConfig{FailureThreshold: 2, OpenDuration: time.Hour},
	})
	beforeOpen := introspectCounterValue(t, introspectBreakerOpen)

	runIntrospect(mw, "a") // 503: failure 1 of 2
	status.Store(http.StatusTooManyRequests)
	runIntrospect(mw, "b") // 429 twice: neutral
	status.Store(http.StatusServiceUnavailable)
	runIntrospect(mw, "c") // 503: failure 2 of 2, opens
	if got := calls.Load(); got != 4 {
		t.Fatalf("endpoint calls = %d, want 4: the 429 must not open the breaker early", got)
	}

	status.Store(http.StatusOK)
	if ctx := runIntrospect(mw, "d"); ctx.User != nil {
		t.Fatal("token resolved: the 429 reset the failure run, so the second 503 did not open the breaker")
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("endpoint calls = %d, want still 4 (short-circuited)", got)
	}
	if got := introspectCounterValue(t, introspectBreakerOpen) - beforeOpen; got != 1 {
		t.Errorf("breaker_open delta = %d, want 1", got)
	}
}

// TestIntrospect_ThrottledRetryServerErrorOpensBreaker proves the retry keeps
// its own classification: a 429 whose retry answers 5xx is an endpoint failure
// and counts toward the breaker like any other 5xx.
func TestIntrospect_ThrottledRetryServerErrorOpensBreaker(t *testing.T) {
	var seen atomic.Int64
	srv, calls := newIntrospectServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) == 1 {
			writeThrottled(w, "0")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	mw := Introspect(IntrospectConfig{
		Endpoint: srv.URL,
		Breaker:  BreakerConfig{FailureThreshold: 1, OpenDuration: time.Hour},
	})
	beforeErrors := introspectCounterValue(t, introspectEndpointError)
	beforeThrottled := introspectCounterValue(t, introspectThrottled)
	beforeOpen := introspectCounterValue(t, introspectBreakerOpen)

	if ctx := runIntrospect(mw, "a"); ctx.User != nil {
		t.Fatalf("a 429 followed by a 503 authenticated %+v, want fail closed", ctx.User)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("endpoint calls = %d, want 2 (first call + retry)", got)
	}
	if got := introspectCounterValue(t, introspectEndpointError) - beforeErrors; got != 1 {
		t.Errorf("endpoint_error delta = %d, want 1 (the retry's 503)", got)
	}
	if got := introspectCounterValue(t, introspectThrottled) - beforeThrottled; got != 0 {
		t.Errorf("throttled delta = %d, want 0 (the final outcome was a 503)", got)
	}

	// The 503 opened the breaker (threshold 1): the next uncached token is
	// short-circuited without an upstream call.
	runIntrospect(mw, "b")
	if got := calls.Load(); got != 2 {
		t.Errorf("endpoint calls = %d, want still 2 (short-circuited)", got)
	}
	if got := introspectCounterValue(t, introspectBreakerOpen) - beforeOpen; got != 1 {
		t.Errorf("breaker_open delta = %d, want 1", got)
	}
}

// TestIntrospect_ThrottledTrialDoesNotWedgeBreaker drives a real half-open
// trial into a 429: the trial must free its probe slot, so the next caller
// probes the recovered endpoint and closes the breaker instead of being denied
// forever.
func TestIntrospect_ThrottledTrialDoesNotWedgeBreaker(t *testing.T) {
	const openFor = 30 * time.Millisecond
	url, status, calls := scriptedIntrospectServer(t, http.StatusServiceUnavailable)

	mw := Introspect(IntrospectConfig{
		Endpoint: url,
		Breaker:  BreakerConfig{FailureThreshold: 1, OpenDuration: openFor},
	})
	beforeThrottled := introspectCounterValue(t, introspectThrottled)
	beforeOpen := introspectCounterValue(t, introspectBreakerOpen)

	runIntrospect(mw, "a") // 503: opens
	time.Sleep(2 * openFor)

	status.Store(http.StatusTooManyRequests)
	if ctx := runIntrospect(mw, "trial"); ctx.User != nil {
		t.Fatalf("throttled trial authenticated %+v, want fail closed", ctx.User)
	}
	if got := introspectCounterValue(t, introspectThrottled) - beforeThrottled; got != 1 {
		t.Fatalf("throttled delta = %d, want 1 (the trial reached the endpoint and was throttled)", got)
	}

	status.Store(http.StatusOK)
	if ctx := runIntrospect(mw, "next"); ctx.User == nil {
		t.Fatal("next caller denied after a throttled trial: the probe slot was stranded and the breaker wedged")
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("endpoint calls = %d, want 4 (503, trial 429 + retry, recovery probe)", got)
	}
	if got := introspectCounterValue(t, introspectBreakerOpen) - beforeOpen; got != 0 {
		t.Errorf("breaker_open delta = %d, want 0", got)
	}
}
