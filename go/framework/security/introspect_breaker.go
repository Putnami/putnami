package security

import (
	"sync"
	"time"
)

// defaultBreakerOpenDuration is how long the introspection circuit breaker stays
// open before probing the endpoint with a single trial request, when the caller
// enables the breaker without setting OpenDuration.
const defaultBreakerOpenDuration = 30 * time.Second

// circuitBreaker is the introspection circuit breaker. It is a classic
// closed → open → half-open breaker guarding the upstream /introspect call:
//
//   - closed: fewer than threshold consecutive failures; calls pass through.
//   - open: threshold reached; allow() denies calls for openFor, so an uncached
//     token fails closed immediately instead of stalling on a Timeout against a
//     dead endpoint. Cached results are unaffected — the breaker only gates the
//     network, and the cache lookup happens before allow() is consulted.
//   - half-open: after openFor, exactly one trial call is admitted (trialInFlight
//     serializes it). Its record() closes the breaker on success or re-opens it
//     on failure; a throttled trial release()s the slot instead, so the next
//     caller probes again.
//
// A 429 Too Many Requests is neither a success nor a failure: release() frees a
// half-open probe slot without touching the failure run, so backpressure from a
// live endpoint can never open the breaker, reset a genuine failure run, or
// wedge it half-open.
//
// A nil breaker, or one with threshold <= 0, is disabled: allow() always admits
// and record() and release() are no-ops, so an unconfigured breaker adds zero
// behavior change.
type circuitBreaker struct {
	threshold int
	openFor   time.Duration

	mu            sync.Mutex
	failures      int
	openedAt      time.Time
	trialInFlight bool
}

// newCircuitBreaker returns a breaker for the config, or nil when the breaker is
// disabled (FailureThreshold <= 0) so the hot path can skip it entirely.
func newCircuitBreaker(cfg BreakerConfig) *circuitBreaker {
	if cfg.FailureThreshold <= 0 {
		return nil
	}
	openFor := cfg.OpenDuration
	if openFor <= 0 {
		openFor = defaultBreakerOpenDuration
	}
	return &circuitBreaker{threshold: cfg.FailureThreshold, openFor: openFor}
}

// allow reports whether an upstream introspection call may proceed now. When the
// breaker is open it denies until openFor has elapsed, then admits a single trial
// (returning trial=true). The caller MUST call record or release with the same
// trial value once the admitted call completes, so a granted trial slot is
// always released.
func (b *circuitBreaker) allow() (allowed, trial bool) {
	if b == nil {
		return true, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures < b.threshold {
		return true, false // closed
	}
	if time.Since(b.openedAt) < b.openFor {
		return false, false // open: fail closed without a network call
	}
	if b.trialInFlight {
		return false, false // a trial is already probing the endpoint
	}
	b.trialInFlight = true
	return true, true // half-open: this caller probes the endpoint
}

// record folds one upstream call's outcome into the breaker state. success means
// the endpoint responded (a 200, active or inactive — the endpoint is healthy);
// any transport error, timeout, or non-200 status other than 429 is a failure
// (a 429 goes to release instead). trial must be the value allow returned for
// this call, so the half-open probe slot is freed.
func (b *circuitBreaker) record(success, trial bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if trial {
		b.trialInFlight = false
	}
	if success {
		b.failures = 0
		b.openedAt = time.Time{}
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		// Cross (or re-cross) the threshold: arm the full openFor window. A trial
		// failure lands here too, so a still-dead endpoint re-opens for openFor.
		b.openedAt = time.Now()
	}
}

// release frees a granted half-open probe slot WITHOUT folding an outcome into
// the breaker: failures and openedAt stay untouched, so the call neither resets
// nor extends a failure run, and an open window is neither closed nor re-armed.
// It is the completion for a 429 Too Many Requests — backpressure from a live
// endpoint, not an outage. trial must be the value allow returned for this
// call; release(false) is a no-op.
func (b *circuitBreaker) release(trial bool) {
	if b == nil || !trial {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trialInFlight = false
}
