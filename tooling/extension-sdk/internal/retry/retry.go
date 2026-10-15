// Package retry sends a registry request again after a transient failure.
// A transient failure is one of:
//
//   - an answer of 408, 429, 500, 502, 503 or 504, the statuses
//     go-containerregistry retries for an image push;
//   - a failure before the request obtained a connection, a dial or TLS
//     handshake timeout included, since nothing reached the registry;
//   - a failure after the request obtained a connection that is not a
//     timeout, such as a connection reset before the answer or during its
//     body.
//
// Every other failure is final: a timeout after the request obtained a
// connection, because the client timeout bounds one attempt; a host the
// resolver does not know; a registry certificate the client refuses; and a
// caller context that ended. Between two attempts Do waits a doubling,
// randomized delay, or the longer wait a Retry-After header asks for. An
// answer that asks for more than MaxRetryAfter ends the retries. A caller
// retries only a request that is safe to send again.
package retry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// Attempts is the most times Default sends one request.
	Attempts = 4
	// BaseDelay is the base of the wait before the second attempt. Do adds up
	// to half of the base at random, and doubles the base before each later
	// wait.
	BaseDelay = time.Second
	// MaxRetryAfter is the longest wait Do takes. One request makes at most
	// Attempts-1 waits, each at most MaxRetryAfter, and the client timeout
	// bounds each attempt.
	MaxRetryAfter = 30 * time.Second
)

// Answer is the response one attempt received.
type Answer struct {
	// Status is the response status code, or 0 without a response.
	Status int
	// Header is the response header.
	Header http.Header
	// stop is why Do ended before the attempt bound, for Note.
	stop string
}

// Policy is how many times Do sends a request and how long it waits between
// two attempts.
type Policy struct {
	// Attempts is the most times Do sends the request, at least 1.
	Attempts int
	// BaseDelay is the base of the wait before the second attempt.
	BaseDelay time.Duration
	// MaxRetryAfter is the longest wait Do takes.
	MaxRetryAfter time.Duration
	// Wait waits d, or returns ctx.Err() as soon as ctx ends.
	Wait func(ctx context.Context, d time.Duration) error
	// Jitter returns a random duration from 0 up to, not including, d.
	Jitter func(d time.Duration) time.Duration
	// Now is the current time, which an HTTP-date Retry-After is read
	// against.
	Now func() time.Time
}

// Default is the policy a registry request uses: Attempts attempts,
// BaseDelay, MaxRetryAfter, a timer wait, a uniform jitter, and time.Now.
func Default() Policy {
	return Policy{Attempts: Attempts, BaseDelay: BaseDelay, MaxRetryAfter: MaxRetryAfter, Wait: wait, Jitter: jitter, Now: time.Now}
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
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// again is a failure worth another attempt.
type again struct{ err error }

func (e again) Error() string { return e.err.Error() }
func (e again) Unwrap() error { return e.err }

// Again marks err as a failure worth another attempt, such as the error a
// caller reports for a transient answer. It returns nil for a nil err.
func Again(err error) error {
	if err == nil {
		return nil
	}
	return again{err: err}
}

// Exchange is one attempt of a request. It records whether the request
// obtained a connection.
type Exchange struct {
	ctx       context.Context
	connected atomic.Bool
}

// Watch returns the context to send one attempt with, and the Exchange that
// classifies its failure. ctx is the caller's context.
func Watch(ctx context.Context) (context.Context, *Exchange) {
	exchange := &Exchange{ctx: ctx}
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { exchange.connected.Store(true) }}
	return httptrace.WithClientTrace(ctx, trace), exchange
}

// Failed returns reported, the error a caller reports for raw, marked with
// Again when raw is a transient failure. raw is the error the attempt failed
// with: the error of the client's Do or of reading the answer body.
func (e *Exchange) Failed(raw, reported error) error {
	if reported == nil {
		return nil
	}
	if e.ctx.Err() != nil || final(raw) || (e.connected.Load() && timeout(raw)) {
		return reported
	}
	return Again(reported)
}

// final reports a transport failure another attempt would repeat: an ended
// context, a host the resolver does not know, and a refused certificate.
func final(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return true
	}
	return CertificateRefused(err)
}

// CertificateRefused reports whether err is the client refusing the
// certificate a server presented.
func CertificateRefused(err error) bool {
	var verification *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &verification) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid)
}

func timeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timed interface{ Timeout() bool }
	return errors.As(err, &timed) && timed.Timeout()
}

// Do calls attempt until it returns an answer whose status Transient refuses
// with a nil error, an error Again did not mark, or p.Attempts have run. It
// also stops when the answer's Retry-After asks for more than
// p.MaxRetryAfter, which Note then names. It returns the last answer, the
// number of attempts it made, and the last error. When ctx ends before the
// next attempt, it sends nothing more and returns ctx.Err(), followed by the
// last answer's status when there is one.
func (p Policy) Do(ctx context.Context, attempt func() (Answer, error)) (Answer, int, error) {
	for made := 1; ; made++ {
		answer, err := attempt()
		if made >= p.Attempts || !retryable(answer, err) {
			return answer, made, err
		}
		delay, stop := p.delay(made, answer.Header)
		if stop != "" {
			answer.stop = stop
			return answer, made, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return answer, made, withLastAnswer(ctxErr, answer)
		}
		if waitErr := p.Wait(ctx, delay); waitErr != nil {
			return answer, made, withLastAnswer(waitErr, answer)
		}
	}
}

func retryable(answer Answer, err error) bool {
	if err != nil {
		var marked again
		return errors.As(err, &marked)
	}
	return Transient(answer.Status)
}

func withLastAnswer(err error, answer Answer) error {
	if answer.Status == 0 {
		return err
	}
	return fmt.Errorf("%w; the last answer was %d", err, answer.Status)
}

// delay is the wait after attempt made: BaseDelay doubled made-1 times plus
// a jitter of up to half of it, or the Retry-After of header when it is
// longer, and at most MaxRetryAfter. When the Retry-After asks for more than
// MaxRetryAfter, delay returns the reason to stop instead.
func (p Policy) delay(made int, header http.Header) (time.Duration, string) {
	backoff := p.BaseDelay << (made - 1)
	if p.Jitter != nil {
		backoff += p.Jitter(backoff / 2)
	}
	asked, label, ok := p.retryAfter(header.Get("Retry-After"))
	if !ok {
		return min(backoff, p.MaxRetryAfter), ""
	}
	if asked > p.MaxRetryAfter {
		return 0, fmt.Sprintf("; the registry asked to wait %s, more than %s", label, p.MaxRetryAfter)
	}
	return min(max(backoff, asked), p.MaxRetryAfter), ""
}

// retryAfter reads a Retry-After value: a number of seconds or an HTTP date.
// A number too large for the parser asks for longer than any bound. It
// returns the asked wait, the text that names it, and whether value asks for
// a wait at all.
func (p Policy) retryAfter(value string) (time.Duration, string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, "", false
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err == nil || errors.Is(err, strconv.ErrRange) {
		if seconds < 0 {
			return 0, "", false
		}
		if seconds > int64(math.MaxInt64/time.Second) {
			return time.Duration(math.MaxInt64), value + "s", true
		}
		asked := time.Duration(seconds) * time.Second
		return asked, asked.String(), true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, "", false
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	asked := max(when.Sub(now()), 0).Round(time.Second)
	return asked, asked.String(), true
}

// After is " after N attempts" when attempts is more than one, and empty
// otherwise, for the end of an error message.
func After(attempts int) string {
	if attempts <= 1 {
		return ""
	}
	return fmt.Sprintf(" after %d attempts", attempts)
}

// Note is After(attempts), followed by the reason Do stopped before the
// attempt bound when answer asked for a wait above MaxRetryAfter.
func Note(answer Answer, attempts int) string {
	return After(attempts) + answer.stop
}
