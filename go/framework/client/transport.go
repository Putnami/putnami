package client

import (
	"context"
	"io"
	"net/http"
	"net/url"
)

// Transport defines the interface for making service calls.
type Transport interface {
	// Do executes a request and returns a response.
	Do(ctx context.Context, req *Request) (*Response, error)
}

// Request represents an outgoing service request.
type Request struct {
	Method string
	Path   string
	// FeatureTrace is populated automatically by generated clients. It records
	// the invoked operation and the feature that produced it, without relying on
	// the base URL. It is nil for a call the descriptor does not describe.
	FeatureTrace *FeatureTrace
	// OperationID is the stable provider operation identity populated by a
	// generated client.
	OperationID string
	// MaxResponseBytes is a per-operation response cap. Zero uses the transport
	// default.
	MaxResponseBytes int64
	// MaxPayloadBytes bounds a declared success payload specifically, and only
	// a successful one. It carries the contract's own bound — today, a raw
	// octet declaration — which is usually far smaller than the resilience cap.
	// Applying it to an error body too would make a provider's own refusal
	// unreadable, so it narrows the read on 2xx and nowhere else. Zero leaves
	// MaxResponseBytes alone.
	MaxPayloadBytes int64
	// MaxRequestBytes bounds a streamed raw-octet request incrementally. Zero
	// leaves an ordinary request body unchanged.
	MaxRequestBytes int64
	// Headers carries request headers. It is an http.Header so a key may hold
	// repeated values; use Set/Add or the SetHeader helper. A map[string]string
	// could not represent multi-valued headers.
	Headers http.Header
	Body    []byte
	// BodyStream supplies an unbuffered, single-use request body. It is never
	// replayed automatically and must not be combined with Body.
	BodyStream io.Reader
	// StreamResponse transfers ownership of a successful response body to the
	// caller. Non-success responses retain the ordinary bounded read.
	StreamResponse bool
	Query          map[string]string
	// QueryValues preserves repeated parameters emitted from array-valued
	// OpenAPI query parameters. It is merged after Query.
	QueryValues url.Values
}

func cloneURLValues(values url.Values) url.Values {
	if values == nil {
		return nil
	}
	cloned := make(url.Values, len(values))
	for key, entries := range values {
		cloned[key] = append([]string(nil), entries...)
	}
	return cloned
}

// SetHeader sets a request header, replacing any existing values for key. It
// lazily allocates the header map so callers need not pre-initialize it.
func (r *Request) SetHeader(key, value string) {
	if r.Headers == nil {
		r.Headers = make(http.Header)
	}
	r.Headers.Set(key, value)
}

// Response represents a service response.
type Response struct {
	StatusCode int
	// Headers carries all response header values. Using http.Header (rather than
	// map[string]string) preserves repeated headers such as Set-Cookie, which a
	// single-value map would silently truncate to the first value.
	Headers http.Header
	Body    []byte
	// BodyStream is an unbuffered successful response. The caller must close it.
	BodyStream io.ReadCloser
}

// IsSuccess returns true if the response status code is in the 2xx range.
func (r *Response) IsSuccess() bool {
	return r.StatusCode >= 200 && r.StatusCode < 300
}

// IsRetryable returns true if the status code indicates a transient failure.
func (r *Response) IsRetryable() bool {
	switch r.StatusCode {
	case 408, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}
