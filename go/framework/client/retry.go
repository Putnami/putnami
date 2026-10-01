package client

import (
	"context"
	"math"
	"math/rand"
	"time"
)

// RetryConfig configures retry behavior with exponential backoff.
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts. Default: 3.
	MaxRetries int
	// BaseDelay is the initial delay before the first retry. Default: 200ms.
	// Subsequent retries use exponential backoff: BaseDelay * 2^attempt.
	BaseDelay time.Duration
	// MaxDelay caps the backoff delay between retries. Default: 5s.
	MaxDelay time.Duration
	// RetryableFunc determines whether a failed request should be retried.
	// When nil, retries on network errors and 5xx status codes.
	RetryableFunc func(resp *Response, err error) bool
	// OnRetry is called before each retry attempt, after the triggering try
	// failed and before the backoff sleep. It receives the upcoming attempt
	// number (1-based), the planned backoff delay, and the response/error that
	// triggered the retry. Optional — wire it to a logger or metric so retry
	// storms are observable instead of silent. Must be non-blocking.
	OnRetry func(attempt int, delay time.Duration, resp *Response, err error)
}

// defaultRetryConfig returns retry defaults.
func defaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries: 3,
		BaseDelay:  200 * time.Millisecond,
		MaxDelay:   5 * time.Second,
	}
}

// totalTimeoutInterceptor bounds the whole request — including every retry
// attempt and backoff — with a single deadline derived from now. Placed outside
// the retry interceptor, its context deadline both caps the total wall-clock and
// flows into each attempt, so an attempt is bounded by the smaller of the
// per-attempt Timeout and the remaining total budget.
func totalTimeoutInterceptor(d time.Duration) Interceptor {
	return func(ctx context.Context, req *Request, next InterceptorFunc) (*Response, error) {
		ctx, cancel := context.WithTimeout(ctx, d)
		response, err := next(ctx, req)
		if err == nil && response != nil && response.BodyStream != nil {
			response.BodyStream = ownResponseStream(ctx, response.BodyStream, cancel)
		} else {
			cancel()
		}
		return response, err
	}
}

// retryInterceptor creates an interceptor that retries failed requests.
func retryInterceptor(config RetryConfig) Interceptor {
	return func(ctx context.Context, req *Request, next InterceptorFunc) (*Response, error) {
		var lastResp *Response
		var lastErr error

		for attempt := 0; attempt <= config.MaxRetries; attempt++ {
			if attempt > 0 {
				delay := calculateBackoff(attempt, config.BaseDelay, config.MaxDelay)
				// Signal the retry (with the result that triggered it) before
				// sleeping, so attempts and backoff are observable.
				if config.OnRetry != nil {
					config.OnRetry(attempt, delay, lastResp, lastErr)
				}
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}

			lastResp, lastErr = next(ctx, req)
			if req.BodyStream != nil || (lastResp != nil && lastResp.BodyStream != nil) {
				return lastResp, lastErr
			}

			if shouldRetry(lastResp, lastErr, config) {
				continue
			}
			return lastResp, lastErr
		}

		return lastResp, lastErr
	}
}

// shouldRetry determines whether to retry a request.
func shouldRetry(resp *Response, err error, config RetryConfig) bool {
	if config.RetryableFunc != nil {
		return config.RetryableFunc(resp, err)
	}
	if err != nil {
		return true // Network errors are retryable.
	}
	if resp != nil {
		return resp.IsRetryable()
	}
	return false
}

// calculateBackoff computes exponential backoff with jitter.
func calculateBackoff(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	// Cap in float64 before converting to time.Duration (int64). At high attempt
	// counts float64(baseDelay)*2^(attempt-1) exceeds math.MaxInt64 (or reaches
	// +Inf), so converting first would overflow to a negative duration and
	// defeat the MaxDelay cap, yielding an immediate retry.
	backoff := float64(baseDelay) * math.Pow(2, float64(attempt-1))
	var delay time.Duration
	if backoff >= float64(maxDelay) {
		delay = maxDelay
	} else {
		delay = time.Duration(backoff)
	}
	// Add 0-25% jitter.
	jitter := time.Duration(float64(delay) * 0.25 * rand.Float64())
	return delay + jitter
}
