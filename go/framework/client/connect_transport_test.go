package client

import (
	"go.putnami.dev/protocol/features/spectest"

	"bytes"
	"context"
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

// connectErrOnCloseReader wraps a reader but returns an error on Close,
// used to exercise the deferred resp.Body.Close() error in ConnectTransport.Do.
type connectErrOnCloseReader struct {
	*bytes.Reader
}

func (c *connectErrOnCloseReader) Close() error {
	return fmt.Errorf("simulated connect body close error")
}

// connectFakeRoundTripper is an http.RoundTripper that returns a response
// whose body succeeds on read but errors on Close.
type connectFakeRoundTripper struct {
	body string
}

func (f *connectFakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       &connectErrOnCloseReader{bytes.NewReader([]byte(f.body))},
	}, nil
}

// TestConnectTransportDo verifies the full happy-path of ConnectTransport.Do
// against a real httptest.Server.
func TestConnectTransportDo(t *testing.T) {
	var gotMethod, gotConnectHeader, gotContentType, gotCustomHeader string
	var gotContentTypes []string
	var gotBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotConnectHeader = r.Header.Get("Connect-Protocol-Version")
		gotContentType = r.Header.Get("Content-Type")
		gotContentTypes = r.Header.Values("Content-Type")
		gotCustomHeader = r.Header.Get("X-Custom")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", 500)
			return
		}
		w.Header().Set("X-Response", "yes")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	transport := NewConnectTransport(ConnectTransportConfig{
		BaseURL: server.URL,
		Timeout: 5 * time.Second,
	})
	defer transport.Close()

	t.Run("POST method and Connect-Protocol-Version header", func(t *testing.T) {
		resp, err := transport.Do(context.Background(), &Request{
			Path:    "/mypackage.v1.MyService/Method",
			Body:    []byte(`{"field":"value"}`),
			Headers: http.Header{"X-Custom": {"hello"}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Errorf("expected POST, got %q", gotMethod)
		}
		if gotConnectHeader != "1" {
			t.Errorf("expected Connect-Protocol-Version=1, got %q", gotConnectHeader)
		}
		if !strings.HasPrefix(gotContentType, "application/json") {
			t.Errorf("expected Content-Type application/json, got %q", gotContentType)
		}
		if len(gotContentTypes) != 1 {
			t.Errorf("expected exactly one Content-Type value, got %v", gotContentTypes)
		}
		if gotCustomHeader != "hello" {
			t.Errorf("expected X-Custom=hello, got %q", gotCustomHeader)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}
		// Response headers propagated.
		if resp.Headers.Get("X-Response") != "yes" {
			t.Errorf("expected response header X-Response=yes, got %q", resp.Headers.Get("X-Response"))
		}
	})

	t.Run("nil body defaults to empty JSON object", func(t *testing.T) {
		_, err := transport.Do(context.Background(), &Request{
			Path: "/mypackage.v1.MyService/OtherMethod",
			// Body intentionally nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(gotBody) != "{}" {
			t.Errorf("expected body to be {}, got %q", string(gotBody))
		}
	})

	t.Run("request content type overrides default without duplication", func(t *testing.T) {
		_, err := transport.Do(context.Background(), &Request{
			Path: "/mypackage.v1.MyService/CustomEncoding",
			Body: []byte(`{}`),
			Headers: http.Header{
				"Content-Type": {"application/connect+json"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotContentType != "application/connect+json" {
			t.Errorf("expected caller content type, got %q", gotContentType)
		}
		if len(gotContentTypes) != 1 {
			t.Errorf("expected exactly one Content-Type value, got %v", gotContentTypes)
		}
	})
}

// TestConnectTransportResponseSizeLimit verifies that responses exceeding
// MaxResponseSize are rejected with CodeClientResponse, and that a response
// of exactly MaxResponseSize is accepted.
func TestConnectTransportResponseSizeLimit(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "bounded-responses", "the-connect-transport-caps-the-response-body-it-reads")
	const maxSize = 64

	t.Run("body exactly at limit is accepted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(strings.Repeat("x", maxSize)))
		}))
		defer server.Close()

		transport := NewConnectTransport(ConnectTransportConfig{
			BaseURL:         server.URL,
			Timeout:         5 * time.Second,
			MaxResponseSize: maxSize,
		})
		defer transport.Close()

		resp, err := transport.Do(context.Background(), &Request{Path: "/svc/Method"})
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
			w.Write([]byte(strings.Repeat("x", maxSize+1)))
		}))
		defer server.Close()

		transport := NewConnectTransport(ConnectTransportConfig{
			BaseURL:         server.URL,
			Timeout:         5 * time.Second,
			MaxResponseSize: maxSize,
		})
		defer transport.Close()

		_, err := transport.Do(context.Background(), &Request{Path: "/svc/Method"})
		if err == nil {
			t.Fatal("expected error for oversized response, got nil")
		}
		if !errors.Is(err, CodeClientResponse) {
			t.Errorf("expected CodeClientResponse error, got %T: %v", err, err)
		}
	})
}

// TestConnectTransportClose ensures that Close() returns no error and can be
// called multiple times safely (covers the Close path).
func TestConnectTransportClose(t *testing.T) {
	transport := NewConnectTransport(ConnectTransportConfig{
		BaseURL: "http://localhost:9999",
	})
	if err := transport.Close(); err != nil {
		t.Errorf("Close() returned unexpected error: %v", err)
	}
	// Second call must also be safe.
	if err := transport.Close(); err != nil {
		t.Errorf("second Close() returned unexpected error: %v", err)
	}
}

// TestConnectTransportTimeout is the default-timeout branch of
// NewConnectTransport (Timeout == 0 → use default 30 s).
func TestConnectTransportDefaultTimeout(t *testing.T) {
	transport := NewConnectTransport(ConnectTransportConfig{
		BaseURL: "http://localhost:9999",
		// Timeout intentionally omitted → defaults to 30 s.
	})
	if transport.client.Timeout != 30*time.Second {
		t.Errorf("expected default timeout 30s, got %v", transport.client.Timeout)
	}
	transport.Close()
}

// TestConnectTransportInvalidURL triggers the http.NewRequestWithContext error
// path (connect_transport.go:67-69) by constructing a transport whose baseURL
// contains a NUL byte, which net/http rejects when building the request.
func TestConnectTransportInvalidURL(t *testing.T) {
	// A NUL byte in the URL causes http.NewRequestWithContext to fail.
	transport := NewConnectTransport(ConnectTransportConfig{
		BaseURL: "http://example.com\x00bad",
		Timeout: 5 * time.Second,
	})
	defer transport.Close()

	_, err := transport.Do(context.Background(), &Request{
		Path: "/svc/Method",
	})
	if err == nil {
		t.Fatal("expected error for invalid URL, got nil")
	}
	if !errors.Is(err, CodeClientRequest) {
		t.Errorf("expected CodeClientRequest error, got %T: %v", err, err)
	}
}

// TestConnectTransportReadBodyError covers the io.ReadAll error path
// (connect_transport.go:88-91) by using a raw TCP server that returns a valid
// HTTP header but drops the connection mid-body, causing io.ReadAll to fail.
func TestConnectTransportReadBodyError(t *testing.T) {
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
		// Drain the request so the client write succeeds.
		buf := make([]byte, 4096)
		conn.Read(buf) //nolint:errcheck
		// Send a response header that promises 100 bytes but close early.
		conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\npartial"))
		// Abrupt close → io.ReadAll in Do will fail.
	}()

	transport := NewConnectTransport(ConnectTransportConfig{
		BaseURL: "http://" + ln.Addr().String(),
		Timeout: 2 * time.Second,
	})
	defer transport.Close()

	_, err = transport.Do(context.Background(), &Request{
		Path: "/svc/Method",
		Body: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected error for truncated response body, got nil")
	}
	// Accept either CodeClientRequest (if the conn drop looks like a dial error)
	// or CodeClientResponse (if the header was received but body read failed).
	if !errors.Is(err, CodeClientRequest) && !errors.Is(err, CodeClientResponse) {
		t.Errorf("expected transport error code, got %T: %v", err, err)
	}
}

// TestConnectTransportDoResponseHeaders verifies that the Connect transport
// propagates response headers back in the Response struct.
func TestConnectTransportDoResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Trace-Id", "trace-123")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":"ok"}`))
	}))
	defer server.Close()

	transport := NewConnectTransport(ConnectTransportConfig{
		BaseURL: server.URL,
		Timeout: 5 * time.Second,
	})
	defer transport.Close()

	resp, err := transport.Do(context.Background(), &Request{
		Path: "/svc/Method",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Headers.Get("X-Trace-Id") != "trace-123" {
		t.Errorf("expected X-Trace-Id header, got %q", resp.Headers.Get("X-Trace-Id"))
	}
}

// TestConnectTransportBodyCloseError covers the deferred resp.Body.Close()
// error path in ConnectTransport.Do (connect_transport.go:83-85). The deferred
// handler sets retErr when Close fails and no other error was returned.
func TestConnectTransportBodyCloseError(t *testing.T) {
	transport := NewConnectTransport(ConnectTransportConfig{
		BaseURL: "http://example.com",
		Timeout: 5 * time.Second,
	})
	defer transport.Close()

	// Swap the underlying http.Client's transport so the body errors on Close.
	transport.client.Transport = &connectFakeRoundTripper{body: `{"ok":true}`}

	_, err := transport.Do(context.Background(), &Request{
		Path: "/svc/BodyCloseErr",
	})
	if err == nil {
		t.Fatal("expected error from body close failure, got nil")
	}
	if !errors.Is(err, CodeClientResponse) {
		t.Errorf("expected CodeClientResponse error, got %T: %v", err, err)
	}
}
