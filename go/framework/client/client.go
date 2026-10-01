// Package client provides a service client builder with resilience patterns.
//
// It includes HTTP and Connect protocol transports, retry with exponential backoff,
// circuit breaker, and an interceptor chain for cross-cutting concerns.
package client

import (
	"context"
	"time"

	"go.putnami.dev/errors"
)

// CodeClientConfig is the error code for client configuration errors.
const CodeClientConfig errors.Code = "client.config"

// Config holds the configuration for a service client.
type Config struct {
	// BaseURL is the base URL for all requests (e.g. "https://api.example.com").
	BaseURL string
	// ClientID identifies this client in logs and tracing. Optional.
	ClientID string
	// Timeout is the per-attempt timeout: it bounds a single HTTP attempt, not
	// the whole call. With retries enabled, total wall-clock can reach roughly
	// (MaxRetries+1) × Timeout plus backoffs. Default: 30s. Use TotalTimeout to
	// bound the entire call.
	Timeout time.Duration
	// TotalTimeout, when > 0, bounds the whole request — every retry attempt and
	// backoff — with a single deadline, so retries cannot run several times the
	// per-attempt Timeout. Zero (default) leaves the call bounded only by
	// Timeout per attempt and the caller's context deadline.
	TotalTimeout time.Duration
	// Retry configures automatic retry with exponential backoff.
	Retry RetryConfig
	// Interceptors is the middleware chain applied to each request.
	Interceptors []Interceptor
	// CircuitBreaker configures the circuit breaker. Nil disables it.
	CircuitBreaker *CircuitBreakerConfig
}

// defaultConfig returns a client config with sensible defaults.
func defaultConfig() Config {
	return Config{
		Timeout: 30 * time.Second,
		Retry:   defaultRetryConfig(),
	}
}

// Builder constructs service clients with resilience patterns.
type Builder struct {
	config    Config
	transport Transport
}

// NewBuilder creates a new client builder.
func NewBuilder() *Builder {
	return &Builder{
		config: defaultConfig(),
	}
}

// BaseURL sets the service endpoint.
func (b *Builder) BaseURL(url string) *Builder {
	b.config.BaseURL = url
	return b
}

// ClientID sets the client identifier (sent as X-Client-Id header).
func (b *Builder) ClientID(id string) *Builder {
	b.config.ClientID = id
	return b
}

// Timeout sets the per-attempt timeout (bounds a single HTTP attempt).
func (b *Builder) Timeout(d time.Duration) *Builder {
	b.config.Timeout = d
	return b
}

// TotalTimeout sets a deadline for the whole request across all retry attempts
// and backoffs. Zero disables the total bound.
func (b *Builder) TotalTimeout(d time.Duration) *Builder {
	b.config.TotalTimeout = d
	return b
}

// Retry configures retry behavior.
func (b *Builder) Retry(config RetryConfig) *Builder {
	b.config.Retry = config
	return b
}

// CircuitBreaker enables the circuit breaker.
func (b *Builder) CircuitBreaker(config CircuitBreakerConfig) *Builder {
	b.config.CircuitBreaker = &config
	return b
}

// Interceptors adds interceptors to the chain.
func (b *Builder) Interceptors(interceptors ...Interceptor) *Builder {
	b.config.Interceptors = append(b.config.Interceptors, interceptors...)
	return b
}

// Transport sets a custom transport implementation.
func (b *Builder) Transport(t Transport) *Builder {
	b.transport = t
	return b
}

// Build creates a configured Client ready for use.
func (b *Builder) Build() (*Client, error) {
	if b.config.BaseURL == "" {
		return nil, errors.New(CodeClientConfig, "baseURL is required")
	}

	transport := b.transport
	if transport == nil {
		transport = NewHTTPTransport(HTTPTransportConfig{
			BaseURL: b.config.BaseURL,
			Timeout: b.config.Timeout,
		})
	}

	// Build the interceptor chain.
	var interceptors []Interceptor

	// Add client ID interceptor if configured.
	if b.config.ClientID != "" {
		interceptors = append(interceptors, clientIDInterceptor(b.config.ClientID))
	}

	// Add user-provided interceptors.
	interceptors = append(interceptors, b.config.Interceptors...)

	// Bound the whole request — every retry attempt and backoff — when a total
	// timeout is set. Placed outside the circuit breaker and retry interceptors
	// so its deadline caps the entire loop; each attempt's context then carries
	// the remaining total budget on top of the per-attempt Timeout.
	if b.config.TotalTimeout > 0 {
		interceptors = append(interceptors, totalTimeoutInterceptor(b.config.TotalTimeout))
	}

	// Add circuit breaker if configured.
	if b.config.CircuitBreaker != nil {
		cb := NewCircuitBreaker(*b.config.CircuitBreaker)
		interceptors = append(interceptors, cb.Interceptor())
	}

	// Add retry interceptor.
	if b.config.Retry.MaxRetries > 0 {
		interceptors = append(interceptors, retryInterceptor(b.config.Retry))
	}

	c := &Client{
		transport: transport,
		config:    b.config,
	}
	// Compose the interceptor chain once: the interceptor slice and transport
	// are fixed for the client's lifetime, so there is no need to re-allocate
	// one closure per interceptor on every request.
	c.chain = buildChain(interceptors, func(ctx context.Context, req *Request) (*Response, error) {
		return c.transport.Do(ctx, req)
	})
	return c, nil
}

// Client is a configured service client that applies interceptors around transport calls.
type Client struct {
	transport Transport
	// chain is the composed interceptor chain, built once in Build and reused
	// by every Do call.
	chain        InterceptorFunc
	config       Config
	service      *serviceRuntime
	descriptor   *ServiceDescriptor
	ownsRegistry bool
}

// Do executes a request through the cached interceptor chain and transport.
func (c *Client) Do(ctx context.Context, req *Request) (*Response, error) {
	return c.chain(ctx, req)
}

// Close releases any resources held by the client.
func (c *Client) Close() error {
	var failures []error
	if c.ownsRegistry && c.service != nil && c.service.registry != nil {
		if err := c.service.registry.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	if closer, ok := c.transport.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return errors.NewAggregate("close service client", failures)
	}
	return nil
}

// clientIDInterceptor adds the X-Client-Id header.
func clientIDInterceptor(clientID string) Interceptor {
	return func(ctx context.Context, req *Request, next InterceptorFunc) (*Response, error) {
		req.SetHeader("X-Client-Id", clientID)
		return next(ctx, req)
	}
}
