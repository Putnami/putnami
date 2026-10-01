package client

import (
	"go.putnami.dev/protocol/features/spectest"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/errors"
)

// errOnCloseReader wraps a byte reader but returns an error on Close.
// Used to exercise the deferred resp.Body.Close() error path.
type errOnCloseReader struct {
	*bytes.Reader
}

func (e *errOnCloseReader) Close() error {
	return fmt.Errorf("simulated body close error")
}

// fakeRoundTripper is an http.RoundTripper that always returns a response
// whose body errors on Close, letting us cover the deferred-close error path.
type fakeRoundTripper struct {
	body string
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       &errOnCloseReader{bytes.NewReader([]byte(f.body))},
	}, nil
}

// TestHTTPTransportResponseSizeLimit verifies the LimitReader / size-exceeded
// rejection in HTTPTransport.Do (http_transport.go:107-113).
func TestHTTPTransportResponseSizeLimit(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "bounded-responses", "the-http-transport-caps-the-response-body-it-reads")
	const maxSize = 128

	t.Run("body exactly at limit is accepted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(strings.Repeat("a", maxSize)))
		}))
		defer server.Close()

		transport := NewHTTPTransport(HTTPTransportConfig{
			BaseURL:         server.URL,
			Timeout:         5 * time.Second,
			MaxResponseSize: maxSize,
		})

		resp, err := transport.Do(context.Background(), &Request{
			Method: http.MethodGet,
			Path:   "/data",
		})
		if err != nil {
			t.Fatalf("expected success for body at limit, got: %v", err)
		}
		if len(resp.Body) != maxSize {
			t.Errorf("expected body length %d, got %d", maxSize, len(resp.Body))
		}
	})

	t.Run("body exceeding limit is rejected with CodeClientResponse", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(strings.Repeat("a", maxSize+1)))
		}))
		defer server.Close()

		transport := NewHTTPTransport(HTTPTransportConfig{
			BaseURL:         server.URL,
			Timeout:         5 * time.Second,
			MaxResponseSize: maxSize,
		})

		_, err := transport.Do(context.Background(), &Request{
			Method: http.MethodGet,
			Path:   "/data",
		})
		if err == nil {
			t.Fatal("expected error for oversized response, got nil")
		}
		if !errors.Is(err, CodeClientResponse) {
			t.Errorf("expected CodeClientResponse error, got %T: %v", err, err)
		}
	})
}

// TestHTTPTransportClose exercises the Close() path (http_transport.go:168-171).
func TestHTTPTransportClose(t *testing.T) {
	transport := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: "http://localhost:9999",
	})
	if err := transport.Close(); err != nil {
		t.Errorf("Close() returned unexpected error: %v", err)
	}
	// Calling Close a second time must not panic or error.
	if err := transport.Close(); err != nil {
		t.Errorf("second Close() returned unexpected error: %v", err)
	}
}

// TestHTTPTransportDefaultTimeout covers the timeout == 0 branch of
// NewHTTPTransport (http_transport.go:79-81).
func TestHTTPTransportDefaultTimeout(t *testing.T) {
	transport := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: "http://localhost:9999",
		// Timeout intentionally omitted → defaults to 30 s.
	})
	if transport.client.Timeout != 30*time.Second {
		t.Errorf("expected default timeout 30s, got %v", transport.client.Timeout)
	}
	transport.Close()
}

// TestHTTPTransportAutoMethodPost covers the method auto-detection branch
// (http_transport.go:115-116): when Body is non-nil and Method is empty,
// the transport must choose POST.
func TestHTTPTransportAutoMethodPost(t *testing.T) {
	var gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: server.URL,
		Timeout: 5 * time.Second,
	})
	defer transport.Close()

	_, err := transport.Do(context.Background(), &Request{
		// Method intentionally absent; Body is non-nil → should auto-select POST.
		Path: "/auto",
		Body: []byte(`{"auto":true}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected auto-selected method POST, got %q", gotMethod)
	}
}

// TestHTTPTransportRequestHeaders verifies that request headers supplied in
// Request.Headers are forwarded by HTTPTransport.Do (http_transport.go:127-129).
func TestHTTPTransportRequestHeaders(t *testing.T) {
	var gotAuth, gotCustom string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCustom = r.Header.Get("X-Custom-Header")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: server.URL,
		Timeout: 5 * time.Second,
	})
	defer transport.Close()

	_, err := transport.Do(context.Background(), &Request{
		Method: http.MethodGet,
		Path:   "/headers",
		Headers: http.Header{
			"Authorization":   {"Bearer secret-token"},
			"X-Custom-Header": {"custom-value"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("expected Authorization header, got %q", gotAuth)
	}
	if gotCustom != "custom-value" {
		t.Errorf("expected X-Custom-Header, got %q", gotCustom)
	}
}

// TestHTTPTransportInvalidMethod triggers the http.NewRequestWithContext error
// path (http_transport.go:123-125) by passing a method containing a control
// character, which net/http rejects.
func TestHTTPTransportInvalidMethod(t *testing.T) {
	transport := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: "http://example.com",
		Timeout: 5 * time.Second,
	})
	defer transport.Close()

	// A NUL byte in the method name causes NewRequestWithContext to return an error.
	_, err := transport.Do(context.Background(), &Request{
		Method: "GET\x00INVALID",
		Path:   "/test",
	})
	if err == nil {
		t.Fatal("expected error for invalid method, got nil")
	}
	if !errors.Is(err, CodeClientRequest) {
		t.Errorf("expected CodeClientRequest error, got %T: %v", err, err)
	}
}

// TestHTTPTransportReadBodyError covers the io.ReadAll error path
// (http_transport.go:146-148) by using a raw TCP server that sends a valid
// HTTP response header but then drops the connection while writing the body.
func TestHTTPTransportReadBodyError(t *testing.T) {
	// Start a listener that will accept one connection, write a partial HTTP
	// response, and then abruptly close the connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the request (drain it so the client doesn't get a write error).
		buf := make([]byte, 4096)
		conn.Read(buf) //nolint:errcheck
		// Write a valid status line + headers promising 100 bytes of body.
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: text/plain\r\n\r\npartial"))
		// Drop the connection before writing the rest → causes io.ReadAll to error.
	}()

	transport := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: "http://" + ln.Addr().String(),
		Timeout: 2 * time.Second,
	})
	defer transport.Close()

	_, err = transport.Do(context.Background(), &Request{
		Method: http.MethodGet,
		Path:   "/partial",
	})
	if err == nil {
		t.Fatal("expected error for truncated response body, got nil")
	}
	// The error should be a request or response error.
	if !errors.Is(err, CodeClientRequest) && !errors.Is(err, CodeClientResponse) {
		t.Errorf("expected CodeClientRequest or CodeClientResponse error, got %T: %v", err, err)
	}
}

// TestHTTPTransportBodyCloseError covers the deferred resp.Body.Close() error
// path in HTTPTransport.Do (http_transport.go:140-142). The deferred handler
// sets retErr when Close fails and no other error was returned.
func TestHTTPTransportBodyCloseError(t *testing.T) {
	transport := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: "http://example.com",
		Timeout: 5 * time.Second,
	})
	defer transport.Close()

	// Swap the underlying http.Client's transport for our fake one.
	transport.client.Transport = &fakeRoundTripper{body: `{"ok":true}`}

	// The body reads fine but Close() errors — the deferred handler should
	// capture that and return it as retErr.
	_, err := transport.Do(context.Background(), &Request{
		Method: http.MethodGet,
		Path:   "/close-err",
	})
	if err == nil {
		t.Fatal("expected error from body close failure, got nil")
	}
	if !errors.Is(err, CodeClientResponse) {
		t.Errorf("expected CodeClientResponse error, got %T: %v", err, err)
	}
}

// TestHTTPTransportResponseHeaders verifies that response headers are
// propagated back in the Response (also exercises the response-header loop).
func TestHTTPTransportResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Server-Id", "test-server")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: server.URL,
		Timeout: 5 * time.Second,
	})
	defer transport.Close()

	resp, err := transport.Do(context.Background(), &Request{
		Method: http.MethodGet,
		Path:   "/resp-headers",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Headers.Get("X-Server-Id") != "test-server" {
		t.Errorf("expected X-Server-Id header, got %q", resp.Headers.Get("X-Server-Id"))
	}
}

// TestHTTPTransportPreservesMultiValuedHeaders verifies that repeated response
// headers (e.g. multiple Set-Cookie) are preserved rather than truncated to the
// first value.
func TestHTTPTransportPreservesMultiValuedHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	transport := NewHTTPTransport(HTTPTransportConfig{BaseURL: server.URL, Timeout: 5 * time.Second})
	defer transport.Close()

	resp, err := transport.Do(context.Background(), &Request{Method: http.MethodGet, Path: "/cookies"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cookies := resp.Headers.Values("Set-Cookie")
	if len(cookies) != 2 || cookies[0] != "a=1" || cookies[1] != "b=2" {
		t.Errorf("expected both Set-Cookie values preserved, got %v", cookies)
	}
}

// countingBody reports how many bytes a consumer actually read from it, so a
// test can assert the transport BOUNDED its read rather than merely rejecting
// the result afterwards.
type countingBody struct {
	remaining int64
	read      int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > c.remaining {
		n = c.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'x'
	}
	c.remaining -= n
	c.read += n
	return int(n), nil
}

func (c *countingBody) Close() error { return nil }

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestTransportsBoundTheBytesTheyRead pins the MEMORY property the requirement
// states — "cap how much response body they read, so a hostile or
// malfunctioning provider cannot exhaust the caller's memory" — rather than only
// the verdict.
//
// The size-limit tests above assert that an oversized body is REJECTED. That
// verdict comes from the `len(respBody) > maxResponseSize` comparison, which
// still holds with the io.LimitReader removed — at which point a hostile 10 GiB
// response is fully buffered into memory before being rejected, and the stated
// property is gone with every test still green.
//
// So this test measures the read itself: a body advertising far more data than
// the cap must be read at most cap+1 bytes deep (the +1 being how the transport
// detects the overflow).
func TestTransportsBoundTheBytesTheyRead(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "bounded-responses", "the-http-transport-caps-the-response-body-it-reads")
	spectest.Proves(t, "go/typed-service-clients", "bounded-responses", "the-connect-transport-caps-the-response-body-it-reads")

	const maxSize = 64
	const hostileSize = 8 << 20 // 8 MiB advertised by the "provider"

	for _, tc := range []struct {
		name string
		// newTransport builds the transport with its private client replaced by
		// one whose RoundTripper serves the counting body.
		newTransport func(rt http.RoundTripper) Transport
	}{
		{
			name: "http",
			newTransport: func(rt http.RoundTripper) Transport {
				tr := NewHTTPTransport(HTTPTransportConfig{BaseURL: "http://provider.invalid", MaxResponseSize: maxSize})
				tr.client = &http.Client{Transport: rt}
				return tr
			},
		},
		{
			name: "connect",
			newTransport: func(rt http.RoundTripper) Transport {
				tr := NewConnectTransport(ConnectTransportConfig{BaseURL: "http://provider.invalid", MaxResponseSize: maxSize})
				tr.client = &http.Client{Transport: rt}
				return tr
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &countingBody{remaining: hostileSize}
			transport := tc.newTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       body,
					Header:     make(http.Header),
					Request:    r,
				}, nil
			}))

			_, err := transport.Do(context.Background(), &Request{Method: http.MethodGet, Path: "/data"})
			if err == nil {
				t.Fatal("expected the oversized response to be rejected")
			}

			// The transport must have stopped reading just past the cap. Without
			// the LimitReader it would have consumed all 8 MiB before deciding.
			if body.read > maxSize+1 {
				t.Errorf("transport read %d bytes of an %d-byte body; a %d-byte cap must bound the READ, not only the verdict",
					body.read, hostileSize, maxSize)
			}
		})
	}
}
