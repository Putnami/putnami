package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"go.putnami.dev/errors"
)

// ConnectTransportConfig configures the Connect protocol transport.
type ConnectTransportConfig struct {
	// BaseURL is the base URL for all Connect RPC requests.
	BaseURL string
	// Timeout is the HTTP client timeout. Default: 30s.
	Timeout time.Duration
	// MaxResponseSize is the maximum response body size in bytes. 0 uses default (32 MiB).
	MaxResponseSize int64
}

// ConnectTransport implements Transport using the Connect protocol (HTTP/1.1 + binary proto or JSON).
// This is a simplified implementation that uses application/json content type.
// For full Connect protocol support with protobuf, use connectrpc/connect-go directly.
type ConnectTransport struct {
	baseURL         string
	client          *http.Client
	maxResponseSize int64
}

// NewConnectTransport creates a Connect protocol transport.
func NewConnectTransport(config ConnectTransportConfig) *ConnectTransport {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	maxResp := config.MaxResponseSize
	if maxResp <= 0 {
		maxResp = defaultMaxResponseSize
	}
	return &ConnectTransport{
		baseURL:         strings.TrimRight(config.BaseURL, "/"),
		client:          &http.Client{Timeout: timeout},
		maxResponseSize: maxResp,
	}
}

// Do executes a Connect protocol request.
// Connect protocol sends all requests as POST with the method name in the path.
func (t *ConnectTransport) Do(ctx context.Context, req *Request) (_ *Response, retErr error) {
	fullURL := t.baseURL + req.Path

	// safeURL is the request target with any query-parameter values
	// redacted; it is the only form that may enter error context (logs).
	safeURL := safeErrorURL(t.baseURL, req.Path, nil)

	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body)
	} else {
		body = bytes.NewReader([]byte("{}"))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, body)
	if err != nil {
		return nil, errors.Wrap(err, CodeClientRequest, errors.String("url", safeURL))
	}

	for k, vs := range req.Headers {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if httpReq.Header.Get("Connect-Protocol-Version") == "" {
		httpReq.Header.Set("Connect-Protocol-Version", "1")
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, errors.Wrap(err, CodeClientRequest, errors.String("url", safeURL))
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeClientResponse, errors.String("url", safeURL))
		}
	}()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, t.maxResponseSize+1))
	if err != nil {
		return nil, errors.Wrap(err, CodeClientResponse, errors.String("url", safeURL))
	}
	if int64(len(respBody)) > t.maxResponseSize {
		return nil, errors.New(CodeClientResponse, "response body exceeds maximum size", errors.String("url", safeURL))
	}

	return &Response{
		StatusCode: resp.StatusCode,
		// Preserve all response header values (e.g. repeated Set-Cookie) instead
		// of collapsing http.Header to its first value.
		Headers: resp.Header,
		Body:    respBody,
	}, nil
}

// Close releases idle connections.
func (t *ConnectTransport) Close() error {
	t.client.CloseIdleConnections()
	return nil
}
