package events

import (
	"math/rand"
	"time"
)

// retryBackoff returns the delay before the next retry attempt. It is the single
// retry-backoff policy shared by every transport: an exponential curve
// (base * 2^(attempt-1)) capped at maxBackoff, plus 0-25% jitter to avoid
// synchronized retry storms (thundering herd) when many consumers fail at once.
func retryBackoff(attempt int, base, maxBackoff time.Duration) time.Duration {
	delay := baseBackoff(attempt, base, maxBackoff)
	// Add 0-25% jitter.
	jitter := time.Duration(float64(delay) * 0.25 * rand.Float64())
	return delay + jitter
}

// baseBackoff returns the deterministic exponential backoff for attempt, capped
// at maxBackoff. attempt is 1-based: attempt 1 yields the base delay. A
// non-positive base or maxBackoff falls back to the default handler values.
func baseBackoff(attempt int, base, maxBackoff time.Duration) time.Duration {
	if base <= 0 {
		base = DefaultHandlerOptions().BaseBackoff
	}
	if maxBackoff <= 0 {
		maxBackoff = DefaultHandlerOptions().MaxBackoff
	}
	for i := 1; i < attempt; i++ {
		base *= 2
		if base >= maxBackoff {
			return maxBackoff
		}
	}
	if base > maxBackoff {
		return maxBackoff
	}
	return base
}
