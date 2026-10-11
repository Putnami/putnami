package remotecache

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// WriteRetry bounds what the batched write path does about a failure that may
// not last. The zero value sends every exchange once and never splits a
// window, which is what StoreObjects and a caller without a policy get.
type WriteRetry struct {
	// Attempts is how many times one control exchange (find-missing,
	// upload-batch or commit-batch) is sent while it fails for a transient
	// reason (see transientFailure). A value below 2 sends it once.
	Attempts int
	// Backoff is the wait before the second attempt. Each later wait is
	// retryBackoffFactor times the one before.
	Backoff time.Duration
	// SplitWindow makes StoreBatch store the entries of a window one at a time,
	// once, when the window still fails as a whole.
	SplitWindow bool
}

// retryBackoffFactor multiplies the wait between two attempts: a Backoff of
// 500 ms waits 500 ms, then 2 s.
const retryBackoffFactor = 4

// splitStopAfter ends a split after this many entries in a row failed for a
// transient reason: the server is failing, not one entry, and the entries left
// would meet the same answer.
const splitStopAfter = 3

// bearerError marks a failure to resolve the session's bearer. The client keeps
// that failure for the rest of the session (see Client.bearer), so sending the
// exchange again cannot succeed.
type bearerError struct{ err error }

func (e *bearerError) Error() string { return e.err.Error() }
func (e *bearerError) Unwrap() error { return e.err }

// transientFailure reports whether an exchange may succeed when it is sent
// again: the server answered 5xx, 429 or 408, or no answer arrived (a dial or
// read failure, a reset connection, the attempt's own timeout).
//
// It is false for every other answer. A 401 or 403 is a verdict on the
// credential, and the 403 closes the caller's write path at once. Another 4xx
// is a verdict on the request. A 501 says the route is not served. A request
// the client itself refused to send, an answer it could not read, and a bearer
// it could not resolve do not change on a second attempt either.
func transientFailure(err error) bool {
	var bearer *bearerError
	if errors.As(err, &bearer) {
		return false
	}
	var status *StatusError
	if errors.As(err, &status) {
		switch {
		case status.Status == http.StatusNotImplemented:
			return false
		case status.Status >= 500:
			return true
		default:
			return status.Status == http.StatusTooManyRequests || status.Status == http.StatusRequestTimeout
		}
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF)
}

// authRefused reports a 401 or 403: the server refused the credential, so
// every entry of the window meets the same answer.
func authRefused(err error) bool {
	return IsStatus(err, http.StatusUnauthorized) || IsStatus(err, http.StatusForbidden)
}

// retryWrite sends one control exchange of the batched write path, and sends
// it again while it fails for a transient reason, at most policy.Attempts
// times in all. It waits policy.Backoff before the second attempt and
// retryBackoffFactor times longer before each later one. It stops at once when
// ctx is done, and returns the exchange's own error, which names the cause.
//
// Sending again is safe: find-missing is a query, upload-batch writes
// content-addressed blobs, and commit-batch answers committed for a key that
// is already there (see doAuthed).
func retryWrite[T any](ctx context.Context, policy WriteRetry, call func() (T, error)) (T, error) {
	wait := policy.Backoff
	for attempt := 1; ; attempt++ {
		out, err := call()
		if err == nil {
			reportUploadProgress(ctx)
			return out, nil
		}
		if attempt >= policy.Attempts || !transientFailure(err) || ctx.Err() != nil {
			return out, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, err
		case <-timer.C:
		}
		wait *= retryBackoffFactor
	}
}
