package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/errors"
)

// safeErrorURL builds a log-safe representation of a request target for
// error context. The base URL and path are retained (neither is sensitive),
// but query-parameter values are dropped — only the parameter names are
// kept — because query strings routinely carry secrets (API keys, bearer
// tokens, signed-URL signatures) and errors.Error captures a stack and is
// designed to be logged. Any query string already embedded in path is
// redacted the same way.
func safeErrorURL(baseURL, path string, query map[string]string) string {
	pathOnly := path
	keySet := make(map[string]struct{}, len(query))
	if i := strings.IndexByte(path, '?'); i >= 0 {
		pathOnly = path[:i]
		if inline, err := url.ParseQuery(path[i+1:]); err == nil {
			for k := range inline {
				keySet[k] = struct{}{}
			}
		}
	}
	for k := range query {
		keySet[k] = struct{}{}
	}
	safe := baseURL + pathOnly
	if len(keySet) > 0 {
		keys := make([]string, 0, len(keySet))
		for k := range keySet {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		safe += "?" + strings.Join(keys, "&")
	}
	return safe
}

// Error codes for HTTP transport operations.
const (
	CodeClientRequest  errors.Code = "client.request"
	CodeClientResponse errors.Code = "client.response"
)

// defaultMaxResponseSize is the default maximum response body size (32 MiB).
const defaultMaxResponseSize int64 = 32 << 20

// HTTPTransportConfig configures the HTTP transport.
type HTTPTransportConfig struct {
	// BaseURL is the base URL for all requests.
	BaseURL string
	// Timeout is the HTTP client timeout. Default: 30s.
	Timeout time.Duration
	// MaxResponseSize is the maximum response body size in bytes. 0 uses default (32 MiB).
	MaxResponseSize int64
}

// HTTPTransport implements Transport using net/http.
type HTTPTransport struct {
	baseURL         string
	client          *http.Client
	maxResponseSize int64
}

// NewHTTPTransport creates an HTTP transport.
func NewHTTPTransport(config HTTPTransportConfig) *HTTPTransport {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	maxResp := config.MaxResponseSize
	if maxResp <= 0 {
		maxResp = defaultMaxResponseSize
	}
	return &HTTPTransport{
		baseURL:         strings.TrimRight(config.BaseURL, "/"),
		client:          &http.Client{Timeout: timeout},
		maxResponseSize: maxResp,
	}
}

// Do executes an HTTP request.
func (t *HTTPTransport) Do(ctx context.Context, req *Request) (_ *Response, retErr error) {
	fullURL := t.baseURL + req.Path

	if len(req.Query) > 0 || len(req.QueryValues) > 0 {
		params := cloneURLValues(req.QueryValues)
		if params == nil {
			params = make(url.Values, len(req.Query))
		}
		for k, v := range req.Query {
			params.Set(k, v)
		}
		fullURL += "?" + params.Encode()
	}

	// safeURL is the URL with query-parameter values redacted; it is the
	// only form that may enter error context (and therefore logs).
	safeURL := safeErrorURL(t.baseURL, req.Path, req.Query)

	var body io.Reader
	if req.BodyStream != nil {
		if req.Body != nil {
			return nil, errors.New(CodeClientRequest, "request cannot combine buffered and streamed bodies")
		}
		stream := req.BodyStream
		if req.MaxRequestBytes > 0 {
			stream = &boundedWireReader{
				Reader: stream, remaining: req.MaxRequestBytes, code: CodeClientRequest,
				message: "service request body exceeds the declared bound",
			}
		}
		body = singleUseRequestBody{Reader: stream}
	} else if req.Body != nil {
		body = bytes.NewReader(req.Body)
	}

	method := req.Method
	if method == "" {
		if body != nil {
			method = http.MethodPost
		} else {
			method = http.MethodGet
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, errors.Wrap(err, CodeClientRequest, errors.String("url", safeURL), errors.String("method", method))
	}
	if req.BodyStream != nil {
		// Even seekable input is a single-use stream: do not let net/http infer
		// a replay callback from a bytes.Reader or strings.Reader.
		httpReq.GetBody = nil
	}

	for k, vs := range req.Headers {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}

	if body != nil && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	httpClient := t.client
	if req.BodyStream != nil {
		copy := *httpClient
		copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		httpClient = &copy
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, errors.Wrap(err, CodeClientRequest, errors.String("url", safeURL), errors.String("method", method))
	}
	if req.StreamResponse && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body := resp.Body
		if req.MaxPayloadBytes > 0 {
			body = &boundedWireReader{
				Reader: resp.Body, remaining: req.MaxPayloadBytes, code: CodeClientResponse,
				message: "service response exceeds the declared bound",
			}
		}
		return &Response{StatusCode: resp.StatusCode, Headers: resp.Header, BodyStream: body}, nil
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeClientResponse, errors.String("url", safeURL))
		}
	}()

	maxResponseSize := t.maxResponseSize
	if req.MaxResponseBytes > 0 {
		maxResponseSize = req.MaxResponseBytes
	}
	if req.MaxPayloadBytes > 0 && req.MaxPayloadBytes < maxResponseSize &&
		resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// The declared payload bound stops the read itself: an oversized success
		// is refused one byte past the bound rather than buffered and checked.
		maxResponseSize = req.MaxPayloadBytes
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, errors.Wrap(err, CodeClientResponse, errors.String("url", safeURL))
	}
	if int64(len(respBody)) > maxResponseSize {
		return nil, errors.New(CodeClientResponse, "response body exceeds maximum size", errors.String("url", safeURL))
	}

	return &Response{
		StatusCode: resp.StatusCode,
		// Pass through the full http.Header so multi-valued headers (e.g.
		// Set-Cookie) are preserved rather than truncated to the first value.
		Headers: resp.Header,
		Body:    respBody,
	}, nil
}

// Close releases idle HTTP connections.
func (t *HTTPTransport) Close() error {
	t.client.CloseIdleConnections()
	return nil
}

// Hide concrete reader types from net/http so even an empty bytes.Reader keeps
// a non-replayable body, while retaining ownership of an io.Closer source.
type singleUseRequestBody struct{ io.Reader }

func (body singleUseRequestBody) Close() error {
	if closer, ok := body.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

type boundedWireReader struct {
	io.Reader
	remaining int64
	code      errors.Code
	message   string
	exceeded  bool
}

func (reader *boundedWireReader) Read(value []byte) (int, error) {
	if reader.exceeded {
		return 0, errors.New(reader.code, reader.message)
	}
	if int64(len(value)) > reader.remaining {
		value = value[:reader.remaining+1]
	}
	n, err := reader.Reader.Read(value)
	if int64(n) > reader.remaining {
		allowed := int(reader.remaining)
		reader.remaining = 0
		reader.exceeded = true
		return allowed, errors.New(reader.code, reader.message)
	}
	reader.remaining -= int64(n)
	return n, err
}

func (reader *boundedWireReader) Close() error {
	if closer, ok := reader.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
