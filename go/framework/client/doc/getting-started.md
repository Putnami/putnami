# Generated service clients

The first-party path starts at the provider declaration. Add
`api.WithClientService`, declare endpoint schemas and policies, then run
`putnami clientgen`. The generated package exposes a typed client and a
`Register<Client>` binding function.

On the consumer, register the framework config and generated binding:

```go
module.Use(client.Services())
usersclient.RegisterUsersClient(module)
```

Then call the typed method:

```go
user, err := users.GetUsersId(ctx, usersclient.GetUsersIdInput{
    Path: usersclient.GetUsersIdPath{Id: "123"},
})
```

The `clients` config block supplies the service URL, client identity and named
credential sources. The runtime acquires and refreshes credentials, propagates
request context and traces, and applies the provider's resilience and typed
error contract. Callers do not add auth or retry interceptors.

## Before the application container exists

A config Source can construct a generated client from an explicit binding
while `config.Load` is still running:

```go
users, err := usersclient.NewUsersClientBinding(client.ServiceBinding{
    URL: endpoint,
    ClientID: "config-source",
    Credentials: map[string]client.CredentialBinding{
        "service": {Source: client.CredentialSourceGCPIDToken, Audience: audience},
    },
})
if err != nil {
    return err
}
```

Use the credential profile name declared by the provider (`service` above).
The binding supplies deployment values; the generated descriptor still supplies
authentication requirements, resilience, schemas and typed errors. Construction
returns binding errors directly and needs no module, DI container or `clients`
config block. Each client owns a separate registry and credential cache, with no
`Close` API exposing registry shutdown: application shutdown does not automatically
close its stream sessions or cancel in-flight credential refreshes. Use explicit
construction for one-time unary config calls and DI registration for long-lived
clients.
Consumer-specific snapshot fallback and audience migration remain caller-owned.

## Caller-supplied endpoints

Use `client.WithEndpoint` on a generated call to address a fleet member or a
workspace endpoint resolved at runtime. The client remains bound to its declared
service and keeps the same security, schemas and resilience policy:

```go
targetCtx, cancel := context.WithTimeout(client.WithEndpoint(ctx, "users", targetURL), 5*time.Second)
defer cancel()
user, err := users.GetUsersId(targetCtx, input)
```

For fan-out, run one call per trusted endpoint with its own context and record
each result or error independently. The runtime supplies no fan-out scheduler
or aggregate failure rule. Omitting `WithEndpoint` uses the configured URL; an
empty override fails with `client.config` before credential acquisition.

The service ID scopes the selection when several generated clients share the
same context. The selection is consumed by the generated call; credential
providers, interceptors and other callbacks cannot accidentally redirect a
nested client call.

Only use trusted deployment URLs: the service's credentials are sent to the
selected target. The binding's HTTPS and `allowInsecure` rules still apply,
redirects remain disabled, and low-level clients without a binding refuse an
override. A `gcp-id-token` credential defaults its audience to the selected URL;
an explicit credential audience still wins. Circuits, cached responses and
coalesced calls are isolated by endpoint. Cache invalidation still covers the
whole service, including every endpoint. Stream overrides use the same registry
and end when the application stops.

## Verbatim success bodies

When a caller verifies an answer over its exact bytes — a canonical body whose
digest the provider derived from the byte sequence it sent — pass a
`client.SuccessBody` with the call. The ordinary generated method runs
unchanged and, on success, the sink holds the declared JSON body exactly as
the provider sent it, after the same status, media type, schema and decode
checks the value went through:

```go
// items is the generated Items client of the service-to-service sample.
var raw client.SuccessBody
item, err := items.GetItems(client.WithSuccessBody(ctx, &raw), GetItemsInput{Path: GetItemsPath{Id: "1"}})
if err != nil {
	return err
}
if err := verifyDigest(raw.Bytes()); err != nil { // the caller's own canonical-form check
	return err
}
```

A cached answer delivers the stored bytes, as a copy. A failed call leaves
`Bytes()` nil. The option is refused with `client.config` before anything is
sent on Connect with the `proto` encoding, on a void or raw octet operation,
and on a stream; a sink serves one call at a time. See
[ADR 0008](adr/0008-a-call-delivers-its-success-body-through-a-caller-owned-sink.md).

### Static non-secret headers

Add `headers` to a service binding when every call needs a constant advisory
value beside its credentials:

```yaml
clients:
  clientId: mcp-consumer
  services:
    intelligence:
      url: https://intelligence.example
      headers:
        X-Putnami-Observed-Revision: revision-one
      credentials:
        user:
          source: forwarded-user
```

Each generated call supplies its own bearer through
`client.WithForwardedUserToken(ctx, token)`. A programmatic registration override
uses `ServiceBinding.Headers map[string]string`; the client snapshots that map.
Operation headers take precedence over these defaults, and cache keys see the
effective values. Credentials belong in `Credentials`, since `Headers` is
ordinary, non-secret configuration. Invalid or duplicate names, control
characters, credential headers and framework identity, tracing or transport
headers fail with `client.config`; an operation's idempotency key cannot be
static either. Stream calls carry these headers too: first-party WebSocket
streams use the init frame's ordinary headers and keep credentials separate.

Forwarding and browser-origin metadata are also reserved:
`X-Forwarded-For`, `Forwarded`, `X-Real-IP`, `X-Cloud-Trace-Context` and `Origin`,
as are the request-context headers `X-Trace-Id`, `X-Region` and `X-Experiments`.
Header names are checked case-insensitively. A value is visible ASCII, with
spaces or tabs inside it but not at either end.

TypeScript clients apply the same rules to the same configuration, so a Go and a
TypeScript consumer of one deployment send the same headers.

## Low-level external client

For tests and external contracts, the fluent `Builder` API remains available.
Its defaults are independent of first-party provider policies.

```go
c, err := client.NewBuilder().BaseURL("https://external.example.com").Build()
```

The low-level builder applies these defaults:

| Setting | Default |
|---------|---------|
| Timeout | 30 seconds |
| Max retries | 3 |
| Retry base delay | 200ms |
| Retry max delay | 5s |
| Transport | `HTTPTransport` |

### Client ID

Set a `ClientID` to automatically attach an `X-Client-Id` header to every outgoing request. This is useful for tracing and service-to-service identification.

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    ClientID("billing-service").
    Build()
```

## Making Requests

Call `client.Do` with a `Request` struct. The `Method` field defaults to `GET` when there is no body, or `POST` when a body is present.

```go
resp, err := c.Do(ctx, &client.Request{
    Method: "GET",
    Path:   "/api/users/123",
})
if err != nil {
    // handle error
}
if resp.IsSuccess() {
    // use resp.Body ([]byte)
}
```

### Sending a Body

```go
body, _ := json.Marshal(map[string]string{"name": "Alice"})

resp, err := c.Do(ctx, &client.Request{
    Method: "POST",
    Path:   "/api/users",
    Body:   body,
})
```

When a body is present and no `Content-Type` header is set, the HTTP transport defaults to `application/json`.

### Query Parameters

```go
resp, err := c.Do(ctx, &client.Request{
    Path:  "/api/search",
    Query: map[string]string{"q": "putnami", "limit": "10"},
})
```

### Custom Headers

```go
resp, err := c.Do(ctx, &client.Request{
    Path: "/api/protected",
    Headers: http.Header{
        "Authorization": {"Bearer " + token},
    },
})

// Or use the SetHeader helper, which lazily allocates the header map:
req := &client.Request{Path: "/api/protected"}
req.SetHeader("Authorization", "Bearer "+token)
```

### Response Helpers

The `Response` struct provides convenience methods for status classification:

```go
resp.IsSuccess()   // true for 2xx status codes
resp.IsRetryable() // true for 408, 429, 500, 502, 503, 504
```

## Retries

Retries are enabled by default with exponential backoff and jitter. The retry interceptor automatically retries on network errors and retryable HTTP status codes (408, 429, 500, 502, 503, 504). Non-retryable errors such as 400 or 404 are returned immediately.

### Configuring Retries

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    Retry(client.RetryConfig{
        MaxRetries: 5,
        BaseDelay:  100 * time.Millisecond,
        MaxDelay:   10 * time.Second,
    }).
    Build()
```

### Disabling Retries

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    Retry(client.RetryConfig{MaxRetries: 0}).
    Build()
```

### Custom Retry Logic

Provide a `RetryableFunc` to override the default retry decision:

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    Retry(client.RetryConfig{
        MaxRetries: 3,
        BaseDelay:  200 * time.Millisecond,
        MaxDelay:   5 * time.Second,
        RetryableFunc: func(resp *client.Response, err error) bool {
            if err != nil {
                return true
            }
            // Only retry on 503
            return resp != nil && resp.StatusCode == 503
        },
    }).
    Build()
```

### Observing Retries

Retries are silent by default. Wire `OnRetry` to make retry storms observable — it fires before each retry's backoff sleep with the upcoming attempt number (1-based), the planned delay, and the response/error that triggered the retry:

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    Retry(client.RetryConfig{
        MaxRetries: 3,
        OnRetry: func(attempt int, delay time.Duration, resp *client.Response, err error) {
            log.Warn("retrying request",
                slog.Int("attempt", attempt),
                slog.Duration("delay", delay))
        },
    }).
    Build()
```

`OnRetry` must be non-blocking.

### Backoff Strategy

The retry delay follows exponential backoff with 0-25% jitter:

```
delay = min(baseDelay * 2^(attempt-1), maxDelay) + jitter
```

For the default configuration (200ms base, 5s max), the delays are approximately:

| Attempt | Delay |
|---------|-------|
| 1 | 200-250ms |
| 2 | 400-500ms |
| 3 | 800ms-1s |

Retries respect context cancellation. If the context is cancelled during a backoff wait, the client returns `ctx.Err()` immediately.

## Circuit Breaker

The circuit breaker prevents cascading failures by short-circuiting requests to a failing downstream service. It follows three states:

- **Closed** -- Normal operation. Requests flow through. Consecutive failures are counted.
- **Open** -- All requests are rejected immediately with a `client.CodeCircuitOpen` error. No traffic reaches the downstream service.
- **Half-Open** -- After a reset timeout, a limited number of requests are allowed through to test recovery. Successes transition back to Closed; a single failure re-opens the circuit.

### Enabling the Circuit Breaker

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    CircuitBreaker(client.CircuitBreakerConfig{
        FailureThreshold:      5,                // Open after 5 consecutive failures
        ResetTimeout:          30 * time.Second, // Try half-open after 30s
        SuccessThreshold:      2,                // Close after 2 successes in half-open
        HalfOpenMaxConcurrent: 2,                // At most 2 probes in flight while half-open
    }).
    Build()
```

In the half-open state at most `HalfOpenMaxConcurrent` probe requests are admitted concurrently; additional callers are rejected with `client.CodeCircuitOpen` until a probe completes, so a burst arriving right after the reset timeout sends only a trickle to the recovering downstream rather than a flood.

Default values are applied for any zero-valued field:

| Field | Default |
|-------|---------|
| `FailureThreshold` | 5 |
| `ResetTimeout` | 30 seconds |
| `SuccessThreshold` | 2 |
| `HalfOpenMaxConcurrent` | `SuccessThreshold` |
| `FailureStatuses` | 500, 502, 503, 504 |

### Observing State Changes

Breaker transitions are silent by default. Wire `OnStateChange` so an open breaker is visible to operators. It fires on every transition (closed→open→half-open→closed) with the previous and new state, after the breaker's internal lock is released (so the callback may safely call back into the breaker):

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    CircuitBreaker(client.CircuitBreakerConfig{
        FailureThreshold: 5,
        OnStateChange: func(from, to client.CircuitState) {
            log.Warn("circuit breaker state change",
                slog.String("from", from.String()),
                slog.String("to", to.String()))
        },
    }).
    Build()
```

`OnStateChange` must be non-blocking.

### Custom Failure Statuses

By default, HTTP status codes 500, 502, 503, and 504 are considered failures. Override this with `FailureStatuses`:

```go
client.CircuitBreakerConfig{
    FailureThreshold: 3,
    ResetTimeout:     15 * time.Second,
    SuccessThreshold: 1,
    FailureStatuses:  []int{500, 502, 503},
}
```

### Handling Circuit Open Errors

When the circuit is open, `client.Do` returns a structured error with code `client.CodeCircuitOpen`. The error is marked as transient and retryable.

```go
resp, err := c.Do(ctx, req)
if err != nil {
    if errors.Is(err, client.CodeCircuitOpen) {
        // Circuit is open -- back off or use a fallback
    }
}
```

### Using the Circuit Breaker Standalone

The `CircuitBreaker` type can be used independently of the client builder:

```go
cb := client.NewCircuitBreaker(client.CircuitBreakerConfig{
    FailureThreshold: 3,
    ResetTimeout:     10 * time.Second,
    SuccessThreshold: 1,
})

if err := cb.AllowRequest(); err != nil {
    // circuit is open
}

// After a successful call:
cb.OnSuccess()

// After a failed call:
cb.OnFailure()

// Check current state:
state := cb.State() // CircuitClosed, CircuitOpen, or CircuitHalfOpen
```

## Interceptors

Interceptors provide a composable middleware pattern for cross-cutting concerns such as logging, authentication, metrics, and tracing. They wrap the request/response cycle and execute in registration order.

### Writing an Interceptor

An interceptor receives the request, a `next` function to call the next step in the chain, and returns the response.

```go
func loggingInterceptor(ctx context.Context, req *client.Request, next client.InterceptorFunc) (*client.Response, error) {
    start := time.Now()
    log.Printf("-> %s %s", req.Method, req.Path)

    resp, err := next(ctx, req)

    log.Printf("<- %d (%s)", resp.StatusCode, time.Since(start))
    return resp, err
}
```

### Registering Interceptors

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    Interceptors(loggingInterceptor, authInterceptor).
    Build()
```

Interceptors execute in registration order. For the example above, the execution order is:

```
loggingInterceptor -> authInterceptor -> transport
```

Each interceptor can modify the request before calling `next`, and modify or inspect the response afterward.

### Built-in Interceptors

The builder automatically adds these interceptors in order:

1. **Client ID** (if `ClientID` is set) -- Adds `X-Client-Id` header.
2. **User-provided interceptors** -- In registration order.
3. **Circuit breaker** (if configured) -- Rejects requests when the circuit is open.
4. **Retry** (if `MaxRetries > 0`) -- Retries failed requests with exponential backoff.

## Transports

A transport defines how requests are physically sent over the network. The package provides two built-in transports.

### HTTP Transport

The default transport. Uses `net/http` and supports all standard HTTP methods.

```go
transport := client.NewHTTPTransport(client.HTTPTransportConfig{
    BaseURL: "https://api.example.com",
    Timeout: 10 * time.Second,
})
```

When no `Method` is set on a request, the transport defaults to `GET` (no body) or `POST` (with body).

### Connect Transport

A transport for services using the [Connect protocol](https://connectrpc.com/). All requests are sent as `POST` with `application/json` content type and the `Connect-Protocol-Version: 1` header.

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    Transport(client.NewConnectTransport(client.ConnectTransportConfig{
        BaseURL: "https://api.example.com",
        Timeout: 10 * time.Second,
    })).
    Build()
```

### Custom Transports

Implement the `Transport` interface to create a custom transport:

```go
type Transport interface {
    Do(ctx context.Context, req *Request) (*Response, error)
}
```

Optionally implement `Close() error` to release resources when the client is closed.

## Timeouts

The `Timeout` setting on the builder configures the per-request timeout on the underlying HTTP client. Each individual attempt (including retries) is subject to this timeout.

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    Timeout(5 * time.Second).
    Build()
```

For overall deadline control across all retry attempts, use a context with a deadline:

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

resp, err := c.Do(ctx, &client.Request{Path: "/api/slow"})
```

## Best Practices

- **Always call `Close`** on the client when done to release idle HTTP connections.
- **Set a `ClientID`** for every service client to improve observability and tracing across services.
- **Tune retry parameters** for your use case. High-throughput services may need lower `MaxRetries` and shorter delays to avoid request pile-up.
- **Enable the circuit breaker** for calls to external or unreliable services. This prevents your service from exhausting resources waiting on a failing dependency.
- **Use context deadlines** to set an overall timeout across all retry attempts. The per-request `Timeout` applies to each individual attempt.
- **Keep interceptors lightweight.** Interceptors run on every request, so avoid expensive operations like synchronous I/O in the request path.
- **Order interceptors intentionally.** Logging and tracing interceptors should be registered first so they capture the full lifecycle, including retries and circuit breaker rejections.

## Contract and compatibility

See the [typed service clients
specification](../specs/typed-service-clients.json), the [resilience-chain
ADR](adr/0001-resilience-is-an-ordered-chain.md), the [producer-lineage
ADR](../../api/doc/adr/0002-generated-clients-carry-producer-lineage.md), and
[support evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.

## Bind caller-resolved endpoints

Generated clients also export `Bind<Client>(client.ServiceBinding)` and
`Close<Client>`. Supply the owner URL, credential audience and optional
`OperationPaths` keyed by a declared operation ID. This binds one generated
protocol client to an owner at call time without registering a service in DI.
Reuse the binding for its page sequence and defer `Close<Client>` afterward.
Only fixed unary REST JSON operations accept an unescaped same-authority path
mapping; schemas, security and resilience stay declared. Closing releases the
private registry and refuses later calls, including anonymous ones.
