// Package freshbearer sets the bearer of each request to one registry host
// from a source asked at that request.
//
// A publish bearer is short-lived, and an upload can outlast it: an OCI image
// is many blob requests, and an archive is one request per platform. A client
// that reads the bearer once sends a value that may expire before its last
// request. Transport asks its source again before every request that carries
// a bearer, so each request sends the value its caller holds at that moment.
// The caller decides when that value is renewed.
//
// Transport also records whether the registry's last answer to a bearer it
// set refused it (401 or 403), so a failed upload can say so, and every value
// it asked, so an error can be redacted of each one.
package freshbearer

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
)

// ErrRefused marks an upload error after the registry refused a bearer the
// Transport set: it answered 401 Unauthorized or 403 Forbidden.
var ErrRefused = errors.New("the registry refused the publish bearer")

// Source returns the bearer to send now. An error, or an empty bearer, ends
// the request before it is sent.
type Source func(context.Context) (string, error)

// Transport replaces the bearer of each request to its host with the value
// its source returns at that request. A request to another host, or one that
// carries no bearer, goes through unchanged: the bearer reaches no other host,
// and an anonymous request stays anonymous.
type Transport struct {
	base   http.RoundTripper
	host   string
	source Source

	mu      sync.Mutex
	issued  []string
	refused bool
}

// New returns a Transport over base for the requests to host, the URL's
// host[:port] as a request names it.
func New(base http.RoundTripper, host string, source Source) *Transport {
	return &Transport{base: base, host: host, source: source}
}

// RoundTrip sends req through the base transport, with the source's current
// bearer when req goes to the host with a bearer.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Host != t.host || !hasBearer(req.Header.Get("Authorization")) {
		return t.base.RoundTrip(req)
	}
	token, err := t.Ask(req.Context())
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	fresh := req.Clone(req.Context())
	fresh.Header.Set("Authorization", "Bearer "+token)
	resp, err := t.base.RoundTrip(fresh)
	if err == nil {
		t.answered(resp.StatusCode)
	}
	return resp, err
}

// Ask returns the source's current bearer and records it among the issued
// values, so an error can be redacted of it even when no request carried it.
// An empty bearer is an error.
func (t *Transport) Ask(ctx context.Context) (string, error) {
	token, err := t.source(ctx)
	if err == nil && token == "" {
		err = errors.New("the bearer source answered no bearer")
	}
	if err != nil {
		return "", err
	}
	t.record(token)
	return token, nil
}

// answered records the status of an answer to a request that carried a
// bearer: 401 or 403 marks the bearer refused, and a later success clears the
// mark, since a client that refreshed and retried was then accepted.
func (t *Transport) answered(status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		t.refused = true
	case status >= 200 && status < 300:
		t.refused = false
	}
}

// Refused reports whether the registry answered 401 or 403 to a request this
// Transport set the bearer of, with no success since.
func (t *Transport) Refused() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refused
}

// Issued returns every bearer this Transport asked its source, once each.
func (t *Transport) Issued() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.issued...)
}

func (t *Transport) record(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, known := range t.issued {
		if known == token {
			return
		}
	}
	t.issued = append(t.issued, token)
}

// hasBearer reports whether an Authorization value is a bearer credential.
func hasBearer(value string) bool {
	scheme, _, found := strings.Cut(value, " ")
	return found && strings.EqualFold(scheme, "bearer")
}

// Mark returns err marked with ErrRefused when t saw the registry refuse a
// bearer, and err itself otherwise. The text of err is kept.
func Mark(err error, t *Transport) error {
	if err == nil || t == nil || !t.Refused() || errors.Is(err, ErrRefused) {
		return err
	}
	return &refusedError{err: err}
}

type refusedError struct{ err error }

func (e *refusedError) Error() string        { return e.err.Error() }
func (e *refusedError) Unwrap() error        { return e.err }
func (e *refusedError) Is(target error) bool { return target == ErrRefused }
