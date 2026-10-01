# Service Clients

The `client` package runs generated first-party service clients. A provider owns
the operation schemas and policies; generated registration resolves deployment
bindings and applies authentication, retries, deadlines, response limits,
circuit breaking, typed errors, context propagation and telemetry.

Explicit streamed binary operations carry raw HTTP request and response bodies
without buffering. Uploads accept an `io.Reader` and a concrete Content-Type;
downloads return `*client.StreamedBinaryPayload` with the status, full media type
(including parameters) and a caller-owned `io.ReadCloser`. Always close the
download body, even if you stop reading early. Bytes are never encoded as JSON
or base64, and a JSON media type is still opaque content in this mode.

Authentication is checked before an upload is read. Uploads are single-use and
are never automatically replayed; streamed operations cannot use the response
cache. The operation's attempt and total deadlines remain active while a
download is consumed. EOF, cancellation, Close and application stop release it.
Bounded binary operations retain their existing byte limits and buffered result.
See [the ownership decision](doc/adr/0007-unframed-binary-bodies-transfer-ownership.md).

## Response validation

A generated call checks every response against the provider's published schema
before it returns a typed value. A property the schema does not declare is
dropped, not refused, so a provider can add an optional response property and
roll out before its consumers; every declared property is still validated. The
same rule applies to stream messages and declared error details. A request body
or client message with an undeclared property is still refused before dispatch,
and the `WithSuccessBody` sink still receives the provider's bytes unchanged.
See [ADR 0014](../../../protocols/clientcontract/doc/adr/0014-a-client-drops-a-response-property-it-does-not-declare.md).

## Generated client binding

Register the generated client and the framework service registry:

```go
module.Use(client.Services())
usersclient.RegisterUsersClient(module)
```

The typed `clients` config supplies deployment values without putting credentials
in generated source:

```yaml
clients:
  clientId: checkout-service
  services:
    users:
      url: https://users.internal
      credentials:
        service:
          source: gcp-id-token
```

Call the generated method directly:

```go
user, err := users.GetUsersId(ctx, usersclient.GetUsersIdInput{
    Path: usersclient.GetUsersIdPath{Id: "123"},
})
```

The generated contract selects credential alternatives in provider order and
never embeds credential values. Service tokens are cached and refreshed by
service, client identity, audience and scopes. Forwarded caller identity must be
enabled explicitly in the binding and remains scoped to the inbound request.

A `gcp-id-token` credential requests the ID token for the binding URL, which is
what Cloud Run verifies it against; the contract's audience is not used. Set
`audience` on the credential to request a different one. For the OAuth sources
`audience` overrides the provider's profile and contract audience:

```yaml
      credentials:
        service:
          source: gcp-id-token
          audience: https://users-abc.a.run.app
```

An operation that declares a credential is never sent anonymously. When no
declared alternative is satisfiable, or when the client has a generated
descriptor but no registered binding, the call fails with `client.credential`
before any byte reaches the network. The provider declares an anonymous
operation explicitly, as an alternative with no requirement.

## Static headers

Static non-secret headers belong in `ServiceBinding.Headers` (or `headers` in
the `clients.services` config), beside credentials:

```go
binding := client.ServiceBinding{
    URL: "https://intelligence.example", ClientID: "mcp-consumer",
    Headers: map[string]string{"X-Putnami-Observed-Revision": revision},
    Credentials: map[string]client.CredentialBinding{
        "user": {Source: client.CredentialSourceForwardedUser},
    },
}
// Pass binding to the generated registration override. Each call supplies its
// own user token through client.WithForwardedUserToken(ctx, token).
```

The client snapshots the map. Every outgoing attempt carries its defaults;
explicit operation headers win. Invalid headers, duplicate case aliases,
authentication, identity, tracing and transport headers, and provider-declared
credential headers are rejected with `client.config`. A static operation
idempotency key is also refused. Keep secret values in `Credentials`: static
headers are ordinary configuration. On first-party WebSocket streams they
travel in the init frame, alongside its separate credentials, and all existing
transport bounds still apply.

A value is visible ASCII, with spaces or tabs inside it but not at either end.
TypeScript clients apply the same rules to `clients.services.<service>.headers`,
so one shared configuration sends the same headers from both runtimes.

## Registry lifetime

The registry is scoped to the application that registered it. Every module of one
application resolves the same `*client.ServiceBindings` and shares one credential
cache; two applications in the same process share none, and the package keeps no
ambient cache.

`client.Services()` implements `app.Stopper`, so the application's stop phase
closes the registry. `Close` is idempotent and ends everything the registry owns:

- the credential refreshes still in flight are canceled, and no credential is kept;
- the stream sessions tracked on it are closed, each emitting its single call
  measurement with the `client.canceled` terminal;
- every later binding, acquisition or session fails with `client.closed`, which
  is distinct from the caller's own `client.canceled` and `client.deadline`.

A built-in credential source ends with the registry. A `CredentialBinding.Provider`
you supply stays yours: the registry stops calling it but never closes it.

A stream transport tracks its session so shutdown can close it:

```go
release, err := bindings.TrackStream(session)
if err != nil {
    return nil, err // the application already stopped
}
defer release()
```

## Typed remote errors

A non-success response becomes a `*RemoteError`:

```go
user, err := users.GetUsersId(ctx, input)
var remote *client.RemoteError
if errors.As(err, &remote) {
    log.Warn("dependency failed",
        "service", remote.Service(),      // "users"
        "operation", remote.Operation(),  // "GetUsersId"
        "code", remote.Code(),            // provider-declared stable code
        "status", remote.StatusCode,
        "retryable", remote.Retryable())
}
```

The service and operation come from the generated contract, not from the
response, so they are present even when the body is empty or undeclared. The
error never carries the raw response bytes, the request headers or the URL.

`RemoteError.Payload` holds the provider-declared error details, and only after
redaction:

- a credential value is removed whether it arrives as a string, a JSON number, a
  boolean, or base64 bytes — the decoded bytes are compared, not just the text;
- a provider-declared property keeps its value whatever its name resembles, so
  `tokenCount` and `authorizationLevel` survive; only a property whose whole name
  is a credential (`token`, `clientSecret`, `password`, …) is dropped;
- an undeclared free-form key is dropped when its name looks like a credential;
- a value that cannot be redacted without breaking the declared schema drops the
  whole payload: `Payload` is nil rather than lossy.

## Retry-After and backoff

A 429 or 503 answer carrying `Retry-After` replaces the computed backoff with
the delay the provider asked for. Both RFC 9110 forms are read: delta-seconds
and an HTTP date. A malformed or negative value is ignored, and the runtime
falls back to its own backoff instead of retrying immediately.

Every wait is bounded by the budget left on the call. When the advisory — or the
computed backoff — does not fit, the runtime stops retrying and returns the
typed `*RemoteError` instead of spending the rest of the deadline asleep and
reporting `client.deadline`.

Backoff base and ceiling come from the declared resilience policy rather than
fixed constants: an operation declaring `timeoutMs: 500` never plans a 5s wait
between two of its attempts.

## Server streams

A provider-declared server stream opens with `OpenServerStream[T]`, which
returns a typed `Stream[T]`: `Messages()` is the bounded channel of validated
provider messages, `Done()` closes at the terminal, `Err()` holds the single
sanitized terminal error, and `Close()` is idempotent.

```go
stream, err := client.OpenServerStream[Item](ctx, bound, &client.Request{}, operation)
if err != nil {
    return err
}
defer stream.Close() //nolint:errcheck // the terminal error is read from Err
for item := range stream.Messages() {
    handle(item)
}
return stream.Err()
```

The lifecycle is owned by `StreamSession`, which every first-party stream
transport drives so the phase rules are stated once:

| Concern | Rule |
| --- | --- |
| Phases | `connecting` → `admitted` → `active` → `terminal` → `closed`, in order, never backwards. A session always reaches `closed`. |
| Admission | The provider's protocol acceptance: for SSE, a 2xx response carrying the declared content type. Not the first message. |
| Retry | Only before admission, only for `safe` or `idempotent` operations, only for a declared retryable failure class. A delivered message is never replayed. |
| Circuit breaker | Admission records one success. Nothing is recorded after admission, for a rejected request, for a failure that never reached the provider, or for a caller cancellation. |
| Budgets | `handshake`, `idle`, frame size and queue depth bound the session independently of `resilience.timeoutMs`, which bounds the whole session only when it is declared. |
| Credentials | Re-checked at the send point, so an acquisition that outlived its own credential never emits the expired value. A 401 or 403 invalidates the credential exactly once and never replays. |
| Registry | Every open stream is tracked on the application registry, so the application's stop phase closes it with the `client.canceled` terminal. A registry that already stopped refuses to open one at all. |
| Terminal | Exactly one of complete, error or cancel reaches the caller; the channel closes once; the telemetry call measurement is emitted once. |

The handshake budget is declared today through `resilience.attemptTimeoutMs`.
Exceeding it fails the stream with `client.deadline` before admission; exceeding
the idle budget fails it with `client.deadline` after admission. The two are
independent: once the stream is admitted the handshake budget no longer bounds
it, and any byte the provider sends — including an SSE comment used as a
heartbeat — resets the idle budget.

## Transport dispatch and Connect

A generated method describes the call once and hands it to the runtime, which
decides which wire carries it. A REST request and a Connect request are two
projections of one declaration: REST puts a path parameter in the URL, Connect
puts it in a request message.

```go
call := &client.OperationCall{Request: request, PathParams: params}
item, err := client.CallOperation[Item](ctx, bound, call, getItemOperation)
stream, err := client.OpenOperationServerStream[Item](ctx, bound, call, watchOperation)
```

An operation that declares more than one success status — `200` or `201`,
`200` or `202`, `200` or `204` — goes through `client.CallOperationResponse`
instead. It runs the same dispatch, response cache and resilience chain, and
returns the response with the status the provider answered. The generated
method decodes the body with `client.DecodeResponse[T]`, which selects the
schema of that status. An undeclared status, a body on a status that declares
none, and a Connect transport are refused.

| Concern | Rule |
| --- | --- |
| Selection | The first transport in `operation.transports` this runtime drives. The provider's declared order is the dispatch order. |
| Fallback | None here. A transport that fails does not silently become another one; preference and resumption are a separate declared mechanism. |
| Resilience | The Connect path reuses the credential, deadline, retry, circuit and telemetry chain of the REST path — not a second copy of it. |
| Proto codec | Driven by the descriptor at `contract.protobuf`. No stub is generated; the JSON side is the published schema's JSON, so one value is identical on every transport. |
| Errors | The Connect error document becomes the same typed `*RemoteError` a REST error produces, with the stable code, the exact status and the declared details read from a `putnami.client.v1.FrameworkError` detail — the message both first-party providers publish. |
| Server stream | Drives the shared `StreamSession`. Admission is the accepted response headers; the stream ends on exactly one `EndStreamResponse`. |

Four declarations are refused rather than carried lossily: a `proto` transport
with no published descriptor, a repeated query parameter over Connect, a
non-object request body over Connect, and — refused at generation — a nullable
member over `connect+proto`, because proto3 has one absence and a caller reading
`Optional.IsNull()` can tell null from absent. The decision is recorded in [ADR
0004](doc/adr/0004-the-connect-transport-is-a-projection-of-one-declaration.md).

## WebSocket streams

A provider-declared `websocket` transport opens with one of three typed entry
points. All three drive the same `StreamSession` as SSE and the same published
`WebSocketConversationV1` as the provider, so the phase rules and the wire rules
are each stated once, in one place.

| Entry point | Handle | Caller surface |
| --- | --- | --- |
| `OpenServerStreamWS[T]` | `*Stream[T]` | `Messages()`, `Done()`, `Err()`, `Close()` |
| `OpenClientStream[TIn, TOut]` | `*ClientStream[TIn, TOut]` | `Send()`, `CloseSend()`, `Result()`, `Done()`, `Err()`, `Close()` |
| `OpenBidiStream[TIn, TOut]` | `*BidiStream[TIn, TOut]` | `Send()`, `CloseSend()`, `Recv()`, `Done()`, `Err()`, `Close()` |

```go
stream, err := client.OpenBidiStream[Command, Event](ctx, bound, &client.Request{}, operation)
if err != nil {
    return err
}
defer stream.Close() //nolint:errcheck // the terminal error is read from Err
if err := stream.Send(ctx, Command{Name: "start"}); err != nil {
    return err
}
if err := stream.CloseSend(); err != nil {
    return err
}
for {
    event, err := stream.Recv(ctx)
    if errors.Is(err, io.EOF) {
        return nil
    }
    if err != nil {
        return err
    }
    handle(event)
}
```

**`CloseSend` does not report the outcome.** A provider may end a client or
bidirectional stream before it reads one message — a refusal of an unknown
resource does — and close the socket, so the caller's half-close can land after
the conversation ended. `CloseSend` then returns `nil`, and the typed terminal
reaches the caller from `Result`, `Recv` or `Err`.

**Admission is in band.** The RFC 6455 opening handshake offers only the
negotiation headers and the `putnami.service.v1` subprotocol — no credential, no
client identity, no tracing header — and the provider must echo the subprotocol.
The operation identity, client identity, deadline, budget, declared ordinary
request headers, propagation context and credentials all travel in the first
application frame, and the provider answers `ready`. That is the admission the
session records, and it is what lets a browser client negotiate the same way.

**The client's own first frame is checked before any network byte.** It is
marshalled, re-parsed by the strict contract parser and accepted by a throwaway
conversation, so a frame the wire would refuse is reported locally with the
contract's own diagnostic code as a `*WebSocketContractError`.

**JSON carries the messages.** Wire v1 keeps `proto` in the contract, and a
declared transport whose encoding is `proto` is refused at open, naming the
operation and the encoding, before a socket is opened. Refusing an undeliverable transport belongs to strict generation; this
is the runtime belt that keeps a stale descriptor from failing mid-stream.

**Bounds and heartbeats.** `resilience.stream.maxFrameBytes` bounds a whole
reassembled message, including one spread over continuation frames;
`maxBufferedMessages` bounds the delivery queue; `handshakeTimeoutMs` bounds
connect-to-admission on its own budget; `idleTimeoutMs` releases a socket that
went quiet after admission; `heartbeatMs` sets the client ping cadence, which
starts with the conversation rather than with admission so a slow admission
never looks idle. Absent, no heartbeat is sent.

**Cancellation.** `Close()` emits exactly one `cancel` frame and then releases
the socket. The reader keeps ownership of the terminal, so the single telemetry
call measurement carries `client.canceled` rather than an empty code.

## Provider-owned WebSocket wires

A provider may declare a WebSocket wire it owns instead of the first-party
conversation (`wire: "provider"` in the contract). Two entry points open one:

| Entry point | Handle | Caller surface |
| --- | --- | --- |
| `OpenByteStream` | `*ByteStream` | `io.ReadWriteCloser`, plus `Done()`, `Err()` |
| `OpenFrameStream[TIn, TOut]` | `*FrameStream[TIn, TOut]` | `Send()`, `Recv()`, `Close()`, `Done()`, `Err()` |

```go
tunnel, err := gateway.ListV1DatabasesConnect(ctx, in) // *client.ByteStream
if err != nil {
    return err // a refused upgrade is the operation's declared typed error
}
defer tunnel.Close() //nolint:errcheck // Err reports the terminal
_, err = io.Copy(tunnel, local)
```

**Admission is the upgrade request.** There is no `init` frame: the upgrade
carries what a unary call carries — the declared credential in its own header,
`X-Client-Id`, `X-Request-ID` and trace context — and offers exactly the
declared subprotocol, or none. A provider that answers with an ordinary HTTP
status is decoded into the operation's declared errors; a provider that does not
echo the declared token is refused.

**The framework owns the socket, the provider owns the vocabulary.** A byte
stream carries binary messages and splits a write under
`resilience.stream.maxFrameBytes`; a frame stream carries one text message per
value and validates each against `messages.input` or `messages.output`. A
message of the wrong kind closes with `1003`, a frame outside its schema with
`1007`. The declared handshake, idle and operation budgets bound the stream, the
declared heartbeat is an RFC 6455 ping, and the call is measured once. A normal
provider close reads `io.EOF`; any other close is a `client.response` error
naming its code. `Close` ends the stream normally with `1000`.

## Low-level client builder

`NewBuilder`, transports and interceptors remain supported for tests and external
contracts. They are not required for a first-party generated client.

```go
c, err := client.NewBuilder().
    BaseURL("https://external.example.com").
    Build()
```

## Low-level transports

### HTTP Transport

Standard HTTP transport using `net/http`:

```go
transport := client.NewHTTPTransport(client.HTTPTransportConfig{
    BaseURL: "http://users-api:3000",
    Timeout: 30 * time.Second,
})
```

### Connect Transport

[Connect protocol](https://connectrpc.com/) transport for gRPC-compatible services:

```go
transport := client.NewConnectTransport(client.ConnectTransportConfig{
    BaseURL: "http://users-api:3000",
    Timeout: 30 * time.Second,
})
```

### Custom Transport

Implement the `Transport` interface for custom protocols:

```go
type Transport interface {
    Do(ctx context.Context, req *Request) (*Response, error)
}
```

## Low-level retry

Automatic retry with exponential backoff and jitter:

```go
client.NewBuilder().
    Retry(client.RetryConfig{
        MaxRetries: 3,                      // Retry up to 3 times
        BaseDelay:  200 * time.Millisecond, // Initial delay
        MaxDelay:   5 * time.Second,        // Max delay cap
    })
```

Retryable conditions (by default):
- Network errors (connection refused, timeout)
- HTTP status codes: 408, 429, 500, 502, 503, 504

Custom retry logic:

```go
client.RetryConfig{
    MaxRetries: 3,
    RetryableFunc: func(resp *client.Response, err error) bool {
        return err != nil || (resp != nil && resp.StatusCode == 503)
    },
}
```

## Low-level circuit breaker

Prevent cascading failures with the circuit breaker pattern:

```go
client.NewBuilder().
    CircuitBreaker(client.CircuitBreakerConfig{
        FailureThreshold: 5,           // Open after 5 consecutive failures
        ResetTimeout:     30 * time.Second, // Wait before trying again
        SuccessThreshold: 2,           // Close after 2 successes in half-open
        FailureStatuses:  []int{500, 502, 503, 504},
    })
```

### State Machine

```
Closed (normal) → Open (fail-fast) → Half-Open (trial)
       ↑                                    │
       └────────────── success ─────────────┘
```

- **Closed**: requests flow through, consecutive failures tracked
- **Open**: all requests rejected with `CircuitOpenError`
- **Half-Open**: limited requests allowed to test recovery

## Low-level interceptors

Add cross-cutting concerns with interceptors:

```go
type Interceptor func(ctx context.Context, req *Request, next InterceptorFunc) (*Response, error)
```

### Custom Interceptor

```go
loggingInterceptor := func(ctx context.Context, req *client.Request, next client.InterceptorFunc) (*client.Response, error) {
    start := time.Now()
    resp, err := next(ctx, req)
    log.Printf("%s %s → %d (%v)", req.Method, req.Path, resp.StatusCode, time.Since(start))
    return resp, err
}

client.NewBuilder().
    Interceptors(loggingInterceptor)
```

### Chain Order

Interceptors execute in registration order:

```
ClientID → User Interceptors → Circuit Breaker → Retry → Transport
```

## Response Helpers

```go
resp.IsSuccess()   // true for 2xx status codes
resp.IsRetryable() // true for 408, 429, 500, 502, 503, 504
```

## Configuration Reference

| Builder Method | Default | Description |
|---|---|---|
| `BaseURL(url)` | required | Service base URL |
| `ClientID(id)` | `""` | Client identity (X-Client-Id header) |
| `Timeout(d)` | `30s` | Per-request timeout |
| `Retry(config)` | 3 retries | Retry configuration |
| `CircuitBreaker(config)` | disabled | Circuit breaker configuration |
| `Interceptors(...)` | none | Custom interceptors |
| `Transport(t)` | HTTP | Custom transport |
| `TotalTimeout(d)` | disabled | Deadline for the whole call, across every retry attempt and backoff |

`Timeout` bounds a single attempt. With retries enabled, total wall-clock can
reach roughly `(MaxRetries+1) × Timeout` plus backoffs, so use `TotalTimeout` when
the call as a whole needs a deadline.

`RetryConfig.OnRetry` is called before each retry, after the triggering attempt
failed and before the backoff sleep, with the upcoming attempt number, the planned
delay, and the response or error that triggered it. Wire it to a logger or a
counter so retry storms are observable instead of silent; it must not block.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/client` is public, documented, maintained,
and classified `stable` in the workspace [support
catalog](../../../putnami.support.json). Before v1.0.0, a minor `0.x` release may
still contain a breaking change; the [release policy](../../../RELEASE.md) requires
release notes and migration documentation rather than strict compatibility between
every pre-1.0 minor.

The [typed service clients specification](specs/typed-service-clients.json) covers
both halves of the outcome: the generator in [`go.putnami.dev/api`](../api) and this
runtime. Durable decisions: [resilience is an ordered
chain](doc/adr/0001-resilience-is-an-ordered-chain.md), [stream sessions have five
phases and four
budgets](doc/adr/0002-stream-sessions-have-five-phases-and-four-budgets.md), [the
service registry is scoped to the
application](doc/adr/0005-the-service-registry-is-scoped-to-the-application.md),
and [generated clients carry producer
lineage](../api/doc/adr/0002-generated-clients-carry-producer-lineage.md).

Regression evidence covers [the builder, retries, circuit breaker, and interceptor
chain](client_test.go), [the HTTP transport and its error
redaction](http_transport_test.go), [the Connect transport](connect_transport_test.go),
[transport dispatch and the Connect protocol](connect_dispatch_test.go), [the wire
vectors the proto codec is pinned to](../../../protocols/clientcontract/connect/oracle_test.go), [the shared Connect
conformance corpus](../../../protocols/clientcontract/connect/corpus_test.go),
[circuit-breaker concurrency](circuit_breaker_concurrency_test.go), [transport
helpers](transport_test.go), [the stream session lifecycle](stream_session_test.go),
[server streams over SSE](server_stream_test.go), [the application-scoped registry
and its lifetime](service_registry_test.go), [credential lifetime, renewal and
placement](credential_test.go), and [generated-client feature
traces](design_test.go).

## Caller-owned bindings

Generated `Bind<Client>` functions use `NewServiceClientBinding` to bind a
caller-resolved URL, audience and optional `OperationPaths`. The map is
programmatic configuration, keyed by declared operation ID. Only fixed unary
REST JSON paths can be mapped; the declared method, schemas, security and
resilience stay intact. URL prefixes are retained and redirects remain disabled.

Reuse the bound client for one owner's page sequence. Generated
`Close<Client>` releases its private registry and refuses later calls.
Closing an ordinary client resolved from application DI leaves the shared
registry alive. No credential is acquired while constructing a binding.
