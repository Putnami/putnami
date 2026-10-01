package client

import (
	"context"
	"sync"
	"time"

	"go.putnami.dev/errors"
)

// CircuitState represents the current state of the circuit breaker.
type CircuitState int

const (
	// CircuitClosed is the normal state where requests flow through.
	CircuitClosed CircuitState = iota
	// CircuitOpen rejects all requests immediately.
	CircuitOpen
	// CircuitHalfOpen allows limited requests to test recovery.
	CircuitHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitBreakerConfig configures the circuit breaker.
type CircuitBreakerConfig struct {
	FailureThreshold int           // Consecutive failures before opening. Default: 5.
	ResetTimeout     time.Duration // Time before transitioning to half-open. Default: 30s.
	SuccessThreshold int           // Successes in half-open before closing. Default: 2.
	// HalfOpenMaxConcurrent caps how many probe requests may be in flight at
	// once while half-open. Excess callers are rejected with the circuit-open
	// error so a burst arriving after the reset timeout cannot re-flood a
	// downstream that has only just maybe-recovered. Defaults to SuccessThreshold.
	HalfOpenMaxConcurrent int
	FailureStatuses       []int // HTTP status codes considered failures. Default: [500,502,503,504].
	// OnStateChange is called whenever the breaker transitions between states
	// (closed→open→half-open→closed), with the previous and new state. Optional
	// — wire it to a logger or metric so an open breaker is observable instead
	// of silent. It is invoked after the internal lock is released, so it may
	// safely call back into the breaker; it must be non-blocking.
	OnStateChange func(from, to CircuitState)
}

func (c CircuitBreakerConfig) withDefaults() CircuitBreakerConfig {
	if c.FailureThreshold == 0 {
		c.FailureThreshold = 5
	}
	if c.ResetTimeout == 0 {
		c.ResetTimeout = 30 * time.Second
	}
	if c.SuccessThreshold == 0 {
		c.SuccessThreshold = 2
	}
	if c.HalfOpenMaxConcurrent <= 0 {
		// Allow enough in-flight probes to close the breaker in one round while
		// still capping the burst.
		c.HalfOpenMaxConcurrent = c.SuccessThreshold
	}
	if len(c.FailureStatuses) == 0 {
		c.FailureStatuses = []int{500, 502, 503, 504}
	}
	return c
}

// CodeCircuitOpen is the error code returned when the circuit breaker is open.
const CodeCircuitOpen errors.Code = "client.circuit_open"

// newCircuitOpenError creates a structured error for an open circuit breaker.
func newCircuitOpenError(resetAt time.Time) *errors.Error {
	return errors.New(CodeCircuitOpen, "circuit breaker is open",
		errors.String("reset_at", resetAt.Format(time.RFC3339)),
	).WithCategory(errors.CategoryTransient).WithRetryable(true)
}

// CircuitBreaker implements the circuit breaker pattern.
type CircuitBreaker struct {
	config           CircuitBreakerConfig
	mu               sync.Mutex
	state            CircuitState
	failures         int
	successes        int
	halfOpenInFlight int // probes admitted and not yet completed while half-open
	lastFailureTime  time.Time
}

// NewCircuitBreaker creates a new circuit breaker.
func NewCircuitBreaker(config CircuitBreakerConfig) *CircuitBreaker {
	return &CircuitBreaker{
		config: config.withDefaults(),
		state:  CircuitClosed,
	}
}

// State returns the current circuit state.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	from := cb.state
	cb.checkTransition()
	to := cb.state
	cb.mu.Unlock()
	cb.notify(from, to)
	return to
}

// AllowRequest checks whether a request should be allowed through.
func (cb *CircuitBreaker) AllowRequest() error {
	cb.mu.Lock()
	from := cb.state
	cb.checkTransition()
	to := cb.state
	var err error
	switch cb.state {
	case CircuitOpen:
		err = newCircuitOpenError(cb.lastFailureTime.Add(cb.config.ResetTimeout))
	case CircuitHalfOpen:
		// Admit at most HalfOpenMaxConcurrent probes; reject the rest so a burst
		// after the reset timeout cannot re-flood the downstream. The slot is
		// released in OnSuccess/OnFailure.
		if cb.halfOpenInFlight >= cb.config.HalfOpenMaxConcurrent {
			err = newCircuitOpenError(cb.lastFailureTime.Add(cb.config.ResetTimeout))
		} else {
			cb.halfOpenInFlight++
		}
	}
	cb.mu.Unlock()
	cb.notify(from, to)
	return err
}

// OnSuccess records a successful request.
func (cb *CircuitBreaker) OnSuccess() {
	cb.mu.Lock()
	from := cb.state

	switch cb.state {
	case CircuitHalfOpen:
		cb.releaseProbe()
		cb.successes++
		if cb.successes >= cb.config.SuccessThreshold {
			cb.state = CircuitClosed
			cb.failures = 0
			cb.successes = 0
			cb.halfOpenInFlight = 0
		}
	case CircuitClosed:
		cb.failures = 0
	}
	to := cb.state
	cb.mu.Unlock()
	cb.notify(from, to)
}

// OnFailure records a failed request.
func (cb *CircuitBreaker) OnFailure() {
	cb.mu.Lock()
	from := cb.state

	cb.lastFailureTime = time.Now()

	switch cb.state {
	case CircuitClosed:
		cb.failures++
		if cb.failures >= cb.config.FailureThreshold {
			cb.state = CircuitOpen
		}
	case CircuitHalfOpen:
		cb.state = CircuitOpen
		cb.successes = 0
		cb.halfOpenInFlight = 0
	}
	to := cb.state
	cb.mu.Unlock()
	cb.notify(from, to)
}

// onIgnored releases a half-open probe without treating a local configuration,
// credential, or caller-cancellation failure as a downstream result.
func (cb *CircuitBreaker) onIgnored() {
	cb.mu.Lock()
	if cb.state == CircuitHalfOpen {
		cb.releaseProbe()
	}
	cb.mu.Unlock()
}

// notify fires the OnStateChange hook when the state actually changed. It runs
// without the breaker lock held, so a hook may safely call back into the
// breaker (e.g. State) without deadlocking. config is set once at construction
// and never mutated, so reading OnStateChange here needs no lock.
func (cb *CircuitBreaker) notify(from, to CircuitState) {
	if from != to && cb.config.OnStateChange != nil {
		cb.config.OnStateChange(from, to)
	}
}

// checkTransition checks if the circuit should transition to half-open.
// Must be called with mu held.
func (cb *CircuitBreaker) checkTransition() {
	if cb.state == CircuitOpen && time.Since(cb.lastFailureTime) >= cb.config.ResetTimeout {
		cb.state = CircuitHalfOpen
		cb.successes = 0
		cb.halfOpenInFlight = 0
	}
}

// releaseProbe frees a half-open probe slot acquired by AllowRequest, clamping
// at zero so a stray completion never drives the counter negative. Must be
// called with mu held.
func (cb *CircuitBreaker) releaseProbe() {
	if cb.halfOpenInFlight > 0 {
		cb.halfOpenInFlight--
	}
}

// isFailureStatus checks if a status code is considered a failure.
func (cb *CircuitBreaker) isFailureStatus(status int) bool {
	for _, s := range cb.config.FailureStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// Interceptor returns an interceptor that enforces the circuit breaker.
func (cb *CircuitBreaker) Interceptor() Interceptor {
	return func(ctx context.Context, req *Request, next InterceptorFunc) (*Response, error) {
		if err := cb.AllowRequest(); err != nil {
			return nil, err
		}

		resp, err := next(ctx, req)
		if err != nil {
			cb.OnFailure()
			return resp, err
		}

		if resp != nil && cb.isFailureStatus(resp.StatusCode) {
			cb.OnFailure()
		} else {
			cb.OnSuccess()
		}

		return resp, nil
	}
}
