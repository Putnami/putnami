# Service Clients

`go.putnami.dev/client` runs the clients Putnami generates from a provider
declaration. The provider owns the schemas, the typed errors, the security
profiles, the transports and the resilience policy; the consumer registers one
binding and calls typed methods.

The happy path has four steps, and none of them is transport code:

```
declare the provider  →  generate  →  register the binding  →  call
   api.Endpoint(...)     putnami       client.Services()       items.GetItems(ctx, in)
   api.Clients(...)      clientgen     RegisterItemsClient
```

`go/samples/service-to-service` runs all four end to end.

## 1. Declare the provider

Endpoints are declared once, on the api plugin. `MayThrow` names the framework
error codes the operation can return, so they arrive at the consumer as typed
errors instead of a formatted string:

```go
apiPlugin.Register(api.Endpoint("GET", "/items/{id}").
    Params(api.Type[GetItemParams]()).
    Returns(api.Type[Item]()).
    MayThrow(perrors.CodeNotFound).
    Handle(getItem))
```

When an error carries structured data, `MayThrowDetails` declares the type of
the envelope's `details` member for that code. The status is the one
`perrors.HTTPStatusForCode` returns, which is also what the provider answers,
so a code of your own is registered once. The handler attaches the value with
`perrors.Any("details", value)` on a `perrors.User` error; an internal error
never exposes details. `MayThrowDetails` refuses `perrors.CodeBadRequest` and
`perrors.CodeInternalServer`: every endpoint answers those implicit codes, and
the framework writes their details:

```go
perrors.RegisterHTTPStatus("deploy_rejected", http.StatusBadRequest)

apiPlugin.Register(api.Endpoint("POST", "/deploys").
    Body(api.Type[DeployRequest]()).
    Returns(api.Type[Deploy]()).
    MayThrowDetails("deploy_rejected", api.Type[DeployRejected]()).
    Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
        return phttp.ErrorResponse(perrors.User("deploy_rejected", "deploy rejected",
            perrors.Any("details", DeployRejected{Retryable: false})))
    }))
```

`api.WithClientService` declares the service identity and the credential
profiles the generated clients must satisfy. Profiles are names and header
placements; credential *values* stay in consumer configuration and never enter
generated source:

```go
apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
    Service: clientcontract.Service{ID: "items", Audience: "urn:putnami:items"},
    Credentials: map[string]clientcontract.CredentialProfile{
        "workload":    {Kind: clientcontract.CredentialServiceToken},
        "catalog-key": {Kind: clientcontract.CredentialAPIKey, Header: CatalogKeyHeader},
    },
}))
```

`Client(api.ClientOperationOptions{...})` carries the client-facing policy the
route cannot express: which credential alternatives satisfy the operation, the
resilience policy, the transport preference order, and whether a server stream
may be continued.

```go
apiPlugin.Register(api.Endpoint("GET", "/items/{id}/history").
    Params(api.Type[GetItemParams]()).
    Returns(api.StreamOf[ItemRevision]()).
    Client(api.ClientOperationOptions{
        Transports: []clientcontract.TransportProtocol{
            clientcontract.TransportWebSocket, clientcontract.TransportSSE,
        },
        Resume: true,
    }).
    MayThrow(perrors.CodeNotFound).
    Handle(api.ServerStream(itemHistory)))
```

An SSE server stream continues across a broken connection, on another instance
included, when the provider declares how. Cursor mode names the output field
that carries the provider's opaque position and the query parameter that
receives it on a reopening:

```go
apiPlugin.Register(api.Endpoint("GET", "/items/changes").
    Query(api.Type[ChangesQuery]()).
    Returns(api.StreamOf[ItemChange]()).
    Client(api.ClientOperationOptions{
        Transports:      []clientcontract.TransportProtocol{clientcontract.TransportSSE},
        SSEContinuation: api.SSECursorContinuation("cursor", "cursor"),
        Resilience: &clientcontract.ResiliencePolicy{
            Stream: &clientcontract.StreamPolicy{Reconnect: &reconnect},
        },
    }).
    MayThrow(perrors.CodeNotFound).
    Handle(api.ServerStream(itemChanges)))
```

`ItemChange.Cursor` is a required string, `ChangesQuery.Cursor` a declared
string query parameter, and `itemChanges` continues exclusively after the
position it is given and answers one it never issued with `not_found`.
`go/samples/service-to-service` serves this route, and its generated consumers
prove the continuation across an instance change.
`api.SSEBestEffortContinuation()` is the other mode: the reopening sends the
original query and no position, and the stream may miss or repeat messages.

| Declaration | What it produces |
|---|---|
| `Returns(api.Type[T]())` | A typed unary result |
| `Returns(api.StreamOf[T]())` | A server stream |
| `Body(api.StreamOf[T]())` + `Returns(api.Type[R]())` | A client stream with one declared result |
| `Body(api.StreamOf[T]())` + `Returns(api.StreamOf[R]())` | A bidirectional stream |
| `Body(api.Binary(mediaType, maxBytes))` | A raw octet request body, bounded |
| `Returns(api.Binary(mediaType, maxBytes))` | A raw octet response, bounded |
| `MayThrow(code)` | A typed client error carrying that stable code |
| `MayThrowDetails(code, api.Type[T]())` | The same typed error, with its `details` body decoded as `T` |
| `Transports: [...]` | The dispatch order every generated client follows |
| `ConnectEncodings: [...]` | The Connect payload encoding order a generated client dispatches |
| `Resume: true` | A server stream a broken socket may continue |
| `SSEContinuation: api.SSECursorContinuation("cursor", "cursor")` | An SSE server stream a broken connection continues after the last delivered cursor, on any instance; `api.SSEBestEffortContinuation()` reopens with the original query and may miss or repeat messages |
| `External: "OCI Distribution Specification v1.1"` | Nothing: the route stays served and published, and an external authority owns it |

The bound on `api.Binary` is mandatory. An unbounded octet body is the one shape
generation refuses outright, because no consumer could size a buffer for it.

A provider that serves a standard protocol beside its own routes — an OCI
registry, a Go module proxy, an npm registry — marks each standard leg with
`External`. The leg stays in the OpenAPI document with
`x-putnami-external-contract` instead of `x-putnami-client`, and no generated
client, protobuf descriptor or Connect bridge includes it; its callers use the
standard's own adapters. `External` stands alone: blank, combined with another
client option, or declared without `api.WithClientService`, it fails the
provider at start.

Finally, declare the targets to emit:

```go
clients := api.Clients(api.ClientsOptions{
    Targets: []string{"go", "ts"},
    Go:      api.GoClientOptions{PackageName: "itemsclient", ClientName: "ItemsClient"},
    TS:      api.TSClientOptions{Output: "clients/ts", PackageName: "@example/items-client"},
}).From(apiPlugin)
```

## 2. Generate

```bash
putnami clientgen --projects items-provider
```

Generation is strict for a provider-owned contract: a semantic the shared client
IR cannot carry fails with the operation named and a remedy, rather than
emitting a client that silently loses it. The workspace commands, the drift
guard and the full diagnostic list are in
[Generated clients](/docs/tooling-&-workspace/generated-clients).

## 3. Register the binding

```go
module := app.NewModule("items-consumer")
module.Use(client.Services())
itemsclient.RegisterItemsClient(module)
```

Deployment values arrive through the typed `clients` config block, so the
generated package holds no URL, token or key:

```yaml
clients:
  clientId: orders.api
  services:
    items:
      url: https://items.internal
      credentials:
        service:
          source: gcp-id-token
```

A `gcp-id-token` credential requests the ID token for the binding URL, which is
what Cloud Run verifies it against, so most consumers configure nothing else.
Set `audience` on the credential to request a different one; for the OAuth
sources it overrides the provider's profile and contract audience.

An operation that declares a credential is never sent anonymously. When no
declared alternative is satisfiable, or the client has a descriptor but no
registered binding, the call fails with `client.credential` before any byte
reaches the network. The exception is an optional credential the provider
declares: a route whose rule is `security.Options{Optional: true}` may end its
alternatives with an anonymous one (an empty `AllOf`). The client then presents
the credential when the binding holds it and calls anonymously when it does not.

`client.Services()` implements `app.Stopper`: the application's stop phase
closes the registry, cancels credential refreshes in flight and closes every
tracked stream. Two applications in the same process share nothing.

## 4. Call

```go
item, err := items.GetItems(ctx, itemsclient.GetItemsInput{
    Path: itemsclient.GetItemsPath{Id: "123"},
})
```

A declared error arrives as a `*client.RemoteError` carrying the stable code,
the status, the retry hint and the redacted declared details — never the raw
response bytes, the request headers or the URL:

```go
var remote *client.RemoteError
if errors.As(err, &remote) {
    log.Warn("dependency failed",
        "service", remote.Service(), "operation", remote.Operation(),
        "code", remote.Code(), "retryable", remote.Retryable())
}
```

An error declared with `MayThrowDetails` also has a generated type named after
the method and the code, whose `Payload` is the decoded `details` body:

```go
var rejected *deploysclient.CreateDeploysDeployRejectedError
if errors.As(err, &rejected) && rejected.Payload != nil {
    for _, rejection := range rejected.Payload.Rejections {
        fmt.Println(rejection.Project, rejection.Error)
    }
}
```

### The shapes a generated client exposes

| Declared shape | Generated entry point |
|---|---|
| Unary | `items.GetItems(ctx, in)` |
| Raw octets | the same method, returning status, content type and the bytes unchanged |
| Server stream | `client.OpenOperationServerStream[T]` — SSE, WebSocket or Connect, in declared order |
| Client stream | `client.OpenClientStream[TIn, TOut]` — WebSocket |
| Bidirectional stream | `client.OpenBidiStream[TIn, TOut]` — WebSocket |
| Connect unary | the same method; the transport is chosen from the declared order |

The caller never names a transport. Credentials, deadline, retry, circuit
breaker, trace propagation and metrics are the same chain on every wire.
`go/framework/client/README.md` and `AI.md` hold the per-transport rules:
admission, the five session phases, the four stream budgets, declared fallback,
declared resume and declared SSE continuation.

### Rules, not gaps

These are contract decisions, and they do not change with a later release:

- A WebSocket transport declaring `proto` encoding is refused at generation. The
  wire keeps the encoding in the contract; the Go client carries JSON messages.
- A unary call never falls back to another transport. A server stream falls back
  once, before admission, and only when the provider *answered* that this wire
  is not served here.
- A stream never continues unless the provider declared `Resume` or
  `SSEContinuation` on a `safe` server stream and the operation's effective
  `resilience.stream.reconnect` is on. A continuation follows a transport break
  and nothing else: a typed error, an explicit `complete`, a contract error, a
  cancellation or an expired budget ends the stream. Five continuations per
  session is the framework cap.
- A delivered message is never replayed by `Resume` or by a cursor
  continuation: the position advances only when the caller took the value, and
  the provider continues exclusively after it. A best-effort continuation is
  the one declared exception — it reopens the original query, and messages may
  be missing or repeated — and it is never substituted for a cursor
  continuation that failed.
- A cursor is a position, not a credential. Every reopening resolves the
  consumer's credentials again and runs the provider's full security chain,
  and the provider answers a stale, forged or out-of-scope cursor with a typed
  error, never by restarting from the beginning.
- A declared SSE continuation speaks a negotiated wire: the request carries
  `X-Putnami-Stream-Wire: putnami.sse.v1`, the provider acknowledges it on the
  response head and ends a successful stream with `event: complete`. A provider
  that does not acknowledge fails the call before any message; a consumer that
  does not ask reads the legacy framing. Upgrade the provider first, then its
  consumers.
- A nullable member over `connect+proto` is refused: proto3 has one absence, so
  null and absent would become one value.

## Low-level client

`NewBuilder`, transports and interceptors stay supported for tests and for
contracts Putnami does not own. They are not part of the first-party path above,
and a handwritten first-party transport fails the workspace guard.

```go
c, err := client.NewBuilder().
    BaseURL("https://external.example.com").
    ClientID("my-service").
    Timeout(10 * time.Second).
    Build()
if err != nil {
    log.Fatal(err)
}
defer c.Close()
```

### Builder methods

| Method | Description |
|--------|-------------|
| `BaseURL(url)` | Service base URL |
| `ClientID(id)` | Client identity (sent as `X-Client-Id` header) |
| `Timeout(d)` | Per-request timeout |
| `TotalTimeout(d)` | Deadline for the whole call, across retries and backoff |
| `Retry(config)` | Enable retry with exponential backoff |
| `CircuitBreaker(config)` | Enable circuit breaker |
| `Interceptors(...)` | Add custom interceptors |
| `Transport(t)` | Use a custom transport |
| `Build()` | Build the client |

### Making requests

```go
resp, err := c.Do(ctx, &client.Request{
    Method:  "GET",
    Path:    "/api/users/123",
    Headers: map[string]string{"Accept": "application/json"},
    Query:   map[string]string{"include": "orders"},
})
if err != nil {
    return err
}

if resp.IsSuccess() {
    var user User
    json.Unmarshal(resp.Body, &user)
}
```

```go
type Request struct {
    Method  string
    Path    string
    Headers map[string]string
    Query   map[string]string
    Body    []byte
}

type Response struct {
    StatusCode int
    Headers    map[string]string
    Body       []byte
}

resp.IsSuccess()   // true for 2xx
resp.IsRetryable() // true for 408, 429, 500, 502, 503, 504
```
### Retry

Enable automatic retry with exponential backoff for transient failures:

```go
c, _ := client.NewBuilder().
    BaseURL("http://users-api:3000").
    Retry(client.RetryConfig{
        MaxRetries: 3,
        BaseDelay:  100 * time.Millisecond,
        MaxDelay:   5 * time.Second,
    }).
    Build()
```

#### Retry configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `MaxRetries` | `int` | — | Maximum retry attempts |
| `BaseDelay` | `time.Duration` | — | Initial delay between retries |
| `MaxDelay` | `time.Duration` | — | Maximum delay cap |
| `RetryableFunc` | `func(*Response, error) bool` | — | Custom retry logic |

By default, the following status codes trigger retries: `408`, `429`, `500`, `502`, `503`, `504`.

#### Custom retry logic

```go
client.RetryConfig{
    MaxRetries: 3,
    BaseDelay:  time.Second,
    RetryableFunc: func(resp *client.Response, err error) bool {
        if err != nil {
            return true // retry on network errors
        }
        return resp.StatusCode == 503 // only retry 503
    },
}
```

### Circuit breaker

Protect against cascading failures by opening the circuit after repeated errors:

```go
c, _ := client.NewBuilder().
    BaseURL("http://users-api:3000").
    CircuitBreaker(client.CircuitBreakerConfig{
        FailureThreshold: 5,
        ResetTimeout:     30 * time.Second,
        SuccessThreshold: 2,
    }).
    Build()
```

#### Circuit breaker configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `FailureThreshold` | `int` | — | Consecutive failures before opening |
| `ResetTimeout` | `time.Duration` | — | Time before transitioning to half-open |
| `SuccessThreshold` | `int` | — | Successes in half-open to close |
| `FailureStatuses` | `[]int` | — | Status codes counted as failures |

#### Circuit breaker states

```
Closed  → (failures reach threshold)  → Open
Open    → (reset timeout expires)     → Half-Open
Half-Open → (successes reach threshold) → Closed
Half-Open → (any failure)              → Open
```

- **Closed** — requests flow normally, failures are counted
- **Open** — requests fail immediately without calling the downstream service
- **Half-Open** — limited requests are allowed through to test recovery

### Low-level interceptors

Add custom logic to the request/response pipeline:

```go
type Interceptor func(req *Request, next func(*Request) (*Response, error)) (*Response, error)
```

```go
// Logging interceptor
loggingInterceptor := func(req *client.Request, next func(*client.Request) (*client.Response, error)) (*client.Response, error) {
    start := time.Now()
    resp, err := next(req)
    duration := time.Since(start)
    fmt.Printf("%s %s → %d (%dms)\n", req.Method, req.Path, resp.StatusCode, duration.Milliseconds())
    return resp, err
}

c, _ := client.NewBuilder().
    BaseURL("http://users-api:3000").
    Interceptors(loggingInterceptor).
    Build()
```

The interceptor chain handles client ID headers, retry with exponential backoff, and circuit breaker state management.

### Low-level transports

#### HTTP transport (default)

```go
client.NewBuilder().
    Transport(client.NewHTTPTransport(client.HTTPTransportConfig{})).
    Build()
```

#### Connect transport

For calling gRPC services via the Connect protocol:

```go
client.NewBuilder().
    Transport(client.NewConnectTransport(client.ConnectTransportConfig{})).
    Build()
```

#### Custom transport

Implement the `Transport` interface for custom protocols:

```go
type Transport interface {
    RoundTrip(ctx context.Context, req *Request) (*Response, error)
    Close() error
}
```

### Low-level full example

```go
c, err := client.NewBuilder().
    BaseURL("http://users-api:3000").
    ClientID("order-service").
    Timeout(10 * time.Second).
    Retry(client.RetryConfig{
        MaxRetries: 3,
        BaseDelay:  200 * time.Millisecond,
        MaxDelay:   5 * time.Second,
    }).
    CircuitBreaker(client.CircuitBreakerConfig{
        FailureThreshold: 5,
        ResetTimeout:     30 * time.Second,
        SuccessThreshold: 2,
    }).
    Build()
if err != nil {
    log.Fatal(err)
}
defer c.Close()

resp, err := c.Do(ctx, &client.Request{
    Method: "GET",
    Path:   "/api/users/123",
})
if err != nil {
    // Network error or circuit breaker open
    return err
}
if !resp.IsSuccess() {
    return fmt.Errorf("unexpected status: %d", resp.StatusCode)
}
```

### Bounding the whole call

`Timeout` bounds a single attempt. With retries enabled, total wall-clock can
reach roughly `(MaxRetries+1) × Timeout` plus backoffs, so use `TotalTimeout`
when the call as a whole needs a deadline:

```go
c, err := client.NewBuilder().
    BaseURL("http://users-api:3000").
    Timeout(2 * time.Second).       // per attempt
    TotalTimeout(5 * time.Second).  // whole call, including retries and backoff
    Build()
```

The interceptor chain is fixed and composed once:

```
ClientID → your interceptors → TotalTimeout → CircuitBreaker → Retry → Transport
```

Retries sit closest to the transport, so an attempt is a real network attempt.
The circuit breaker sits outside them, so one logical call is one breaker
observation and an open breaker rejects before any retry is planned.

### Observing retries

Retries are otherwise silent. Wire `OnRetry` to a logger or a counter:

```go
client.RetryConfig{
    MaxRetries: 3,
    OnRetry: func(attempt int, delay time.Duration, resp *client.Response, err error) {
        log.Warn("retrying", slog.Int("attempt", attempt), slog.Duration("delay", delay))
    },
}
```

It is called after the triggering attempt failed and before the backoff sleep,
with the upcoming attempt number and the result that triggered it. It must not
block.

## Producer lineage

Every generated operation records the project and feature that produced it, so a
call can be traced back to the capability on the other end:

```go
trace := itemsclient.ItemsClientDesign.Trace("getItems")
// trace.ProducerProject, trace.ProducerFeature — empty when the producing
// module declares no feature, which is a first-class state rather than a guess.
```

## Related guides

- [Generated clients](/docs/tooling-&-workspace/generated-clients) — the workspace generate/sync/check commands, the drift guard, and migrating a handwritten client
- [Smart Client Library](/docs/frameworks/typescript/smart-client) — the same four steps in TypeScript
- [HTTP & Middleware](/docs/frameworks/go/http) — server-side HTTP
- [gRPC & Connect](/docs/frameworks/go/grpc) — gRPC services
- [Telemetry](/docs/frameworks/go/telemetry) — client tracing

## Support and contract

`go.putnami.dev/client` is `stable` in the workspace support catalog. Its
behavior, together with the client generator in `go.putnami.dev/api`, is defined
by the typed service clients specification and its accepted decision records
(`go/framework/client/specs/`, `go/framework/client/doc/adr/`, and
`go/framework/api/doc/adr/`). Before v1.0.0 a minor `0.x` release may still
contain a documented breaking change; strict compatibility between every pre-1.0
minor is not promised.
