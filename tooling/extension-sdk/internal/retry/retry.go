// Package retry sends a registry request again after a transient failure: a
// failure before any response, or a 429, 500, 502, 503 or 504 answer. Between
// attempts it waits a doubling, randomized delay, or the longer delay a
// Retry-After header asks for, and it stops at once when the context ends.
// A caller retries only a request that is safe to send again.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// Attempts is the most times Default sends one request.
	Attempts = 4
	// BaseDelay is the wait before the second attempt. Each later wait
	// doubles it, plus up to half of it at random.
	BaseDelay = time.Second
	// MaxRetryAfter bounds the wait a Retry-After header asks for. Three
	// waits at the bound add 90 seconds to a publication, less than the
	// five-minute timeout of a single upload.
	MaxRetryAfter = 30 * time.Second
)

// Answer is the response one attempt received.
type Answer struct {
	// Status is the response status code.
	Status int
	// Header is the response header.
	Header http.Header
}

// Policy is how many times Do sends a request and how long it waits between
// two attempts.
type Policy struct {
	// Attempts is the most times Do sends the request, at least 1.
	Attempts int
	// BaseDelay is the wait before the second attempt.
	BaseDelay time.Duration
	// MaxRetryAfter bounds the wait a Retry-After header asks for.
	MaxRetryAfter time.Duration
	// Wait waits d, or returns ctx.Err() as soon as ctx ends.
	Wait func(ctx context.Context, d time.Duration) error
	// Jitter returns a random duration from 0 up to, not including, d.
	Jitter func(d time.Duration) time.Duration
}

// Default is the policy a registry upload uses: Attempts attempts, BaseDelay,
// MaxRetryAfter, a timer wait, and a uniform jitter.
func Default() Policy {
	return Policy{Attempts: Attempts, BaseDelay: BaseDelay, MaxRetryAfter: MaxRetryAfter, Wait: wait, Jitter: jitter}
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return rand.N(d)
}

// Transient reports whether an answer with status is worth another attempt.
func Transient(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// noResponse is a request that failed before any response arrived.
type noResponse struct{ err error }

func (e noResponse) Error() string { return e.err.Error() }
func (e noResponse) Unwrap() error { return e.err }

// NoResponse marks err, the failure of a request that received no response,
// as worth another attempt. It returns err unmarked when ctx has ended or err
// is a timeout: a request that waited out its timeout is not sent again.
func NoResponse(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return err
	}
	return noResponse{err: err}
}

// Do calls attempt until it returns an answer whose status Transient refuses,
// an error NoResponse did not mark, or p.Attempts have run. It returns the
// last answer, the number of attempts it made, and the last error. When ctx
// ends before the next attempt, it returns ctx.Err() and sends nothing more.
func (p Policy) Do(ctx context.Context, attempt func() (Answer, error)) (Answer, int, error) {
	for made := 1; ; made++ {
		answer, err := attempt()
		if made >= p.Attempts || !retryable(answer, err) {
			return answer, made, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return answer, made, ctxErr
		}
		if waitErr := p.Wait(ctx, p.delay(made, answer.Header)); waitErr != nil {
			return answer, made, waitErr
		}
	}
}

func retryable(answer Answer, err error) bool {
	if err != nil {
		var marked noResponse
		return errors.As(err, &marked)
	}
	return Transient(answer.Status)
}

// delay is the wait after attempt made: BaseDelay doubled made-1 times plus
// a jitter of up to half of it, or the Retry-After seconds of header, bounded
// by MaxRetryAfter, when they are longer.
func (p Policy) delay(made int, header http.Header) time.Duration {
	backoff := p.BaseDelay << (made - 1)
	if p.Jitter != nil {
		backoff += p.Jitter(backoff / 2)
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After")))
	if err != nil || seconds < 0 {
		return backoff
	}
	asked := p.MaxRetryAfter
	if seconds < int(p.MaxRetryAfter/time.Second) {
		asked = time.Duration(seconds) * time.Second
	}
	return max(backoff, asked)
}

// After is " after N attempts" when attempts is more than one, and empty
// otherwise, for the end of an error message.
func After(attempts int) string {
	if attempts <= 1 {
		return ""
	}
	return fmt.Sprintf(" after %d attempts", attempts)
}
