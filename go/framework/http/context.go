package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.putnami.dev/errors"
)

// DefaultMaxBodySize is the maximum number of bytes RawBody will read.
// Override per-request by wrapping the request body before calling RawBody.
const DefaultMaxBodySize int64 = 1 << 20 // 1 MiB — matches ServerConfig.MaxBodySize default

// Context holds per-request data including the parsed request, route
// parameters, authenticated user, and scoped DI container access.
type Context struct {
	// Request is the underlying HTTP request.
	Request *http.Request
	// Writer is the response writer.
	Writer http.ResponseWriter
	// Method is the HTTP method (GET, POST, etc.).
	Method string
	// Path is the URL path.
	Path string
	// Route is the matched route pattern (e.g., "/users/{id}").
	Route string
	// Params holds extracted path parameters.
	Params map[string]string
	// User holds authenticated user claims (set by identity middleware).
	User *Claims
	// RequestID is the correlation ID for this request, set by the RequestID
	// middleware (propagated from an inbound header or generated when absent).
	// Handlers can read it to tag application logs with the same value that
	// appears in the access log and the X-Request-ID response header.
	RequestID string
	// StatusCode can be set before writing the response.
	StatusCode int

	queryParsed bool
	queryParams url.Values
	ctx         context.Context
}

type forwardedBearerContextKey struct{}

// ContextWithForwardedBearer records an inbound bearer token in request scope
// so a generated client whose binding explicitly enables forwarded-user-token
// can propagate it without handler-level header extraction. The value is never
// global and is only observable through ForwardedBearerTokenFromContext.
func ContextWithForwardedBearer(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, forwardedBearerContextKey{}, strings.TrimSpace(token))
}

// ForwardedBearerTokenFromContext returns the request-scoped inbound bearer.
func ForwardedBearerTokenFromContext(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(forwardedBearerContextKey{}).(string)
	return token, ok && token != ""
}

// NewContext creates a request context from an HTTP request.
func NewContext(w http.ResponseWriter, r *http.Request) *Context {
	ctx := r.Context()
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if scheme, token, ok := strings.Cut(authorization, " "); ok && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(token) != "" {
		ctx = ContextWithForwardedBearer(ctx, token)
	}
	return &Context{
		Request: r,
		Writer:  w,
		Method:  r.Method,
		Path:    r.URL.Path,
		ctx:     ctx,
	}
}

// Context returns the underlying context.Context (for DI scope, cancellation, etc.).
func (c *Context) Context() context.Context {
	return c.ctx
}

// SetContext updates the internal context.Context. Use this in middleware that
// needs to propagate context changes (e.g., transactions, tenant isolation)
// to downstream handlers via ctx.Context().
func (c *Context) SetContext(ctx context.Context) {
	c.ctx = ctx
}

// WithContext returns a copy with a new context.Context.
func (c *Context) WithContext(ctx context.Context) *Context {
	c2 := *c
	c2.ctx = ctx
	return &c2
}

// QueryParams returns parsed query parameters (lazy-parsed).
func (c *Context) QueryParams() url.Values {
	if !c.queryParsed {
		c.queryParams = c.Request.URL.Query()
		c.queryParsed = true
	}
	return c.queryParams
}

// Query returns a single query parameter value.
func (c *Context) Query(key string) string {
	return c.QueryParams().Get(key)
}

// Param returns a path parameter value.
func (c *Context) Param(key string) string {
	if c.Params == nil {
		return ""
	}
	return c.Params[key]
}

// Header returns a request header value.
func (c *Context) Header(key string) string {
	return c.Request.Header.Get(key)
}

// Host returns the request host.
func (c *Context) Host() string {
	return c.Request.Host
}

// Body reads and decodes the request body as JSON into the given target.
func (c *Context) Body(target any) (retErr error) {
	defer func() {
		if cerr := c.Request.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeBody)
		}
	}()
	return json.NewDecoder(c.Request.Body).Decode(target)
}

// RawBody reads the request body up to DefaultMaxBodySize bytes.
// Returns an error if the body exceeds the limit.
func (c *Context) RawBody() (_ []byte, retErr error) {
	defer func() {
		if cerr := c.Request.Body.Close(); cerr != nil && retErr == nil {
			retErr = errors.Wrap(cerr, CodeBody)
		}
	}()
	// Read up to limit+1 to detect overflow.
	limited := io.LimitReader(c.Request.Body, DefaultMaxBodySize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > DefaultMaxBodySize {
		return nil, errors.New(CodeBody, "request body too large")
	}
	return data, nil
}

// ContentType returns the Content-Type header of the request.
func (c *Context) ContentType() string {
	ct := c.Request.Header.Get("Content-Type")
	if idx := strings.IndexByte(ct, ';'); idx != -1 {
		ct = ct[:idx]
	}
	return strings.TrimSpace(ct)
}

// Accept returns the Accept header of the request.
func (c *Context) Accept() string {
	return c.Request.Header.Get("Accept")
}

// IsSecured returns true if the user has been authenticated.
func (c *Context) IsSecured() bool {
	return c.User != nil
}

// SetHeader sets a response header.
func (c *Context) SetHeader(key, value string) {
	c.Writer.Header().Set(key, value)
}
