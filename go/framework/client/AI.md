# go.putnami.dev/client

Generated first-party service clients with typed bindings, authentication,
resilience, errors, context propagation, and telemetry. The low-level builder
remains available for tests and external integrations.

## Quick Start

```go
module.Use(client.Services())
usersclient.RegisterUsersClient(module)

user, err := users.GetUsersId(ctx, usersclient.GetUsersIdInput{
    Path: usersclient.GetUsersIdPath{Id: "123"},
})
```

The provider declares the service contract with `api.WithClientService`, runs
`putnami clientgen`, and the generated registration resolves URL, client
identity and credential sources from framework config. Generated calls apply
provider-owned retries, deadlines, response caps, circuit policy, typed errors
and trace propagation automatically.

Before DI exists, such as inside a config Source, use
`usersclient.NewUsersClientBinding(client.ServiceBinding{URL: endpoint,
ClientID: "config-source", Credentials: credentials})`. It returns
`(*UsersClient, error)` and preserves the provider's authentication, resilience,
schemas and typed errors through the safe binding runtime. Each explicit client
owns its registry and credential cache, with no `Close` API exposing registry
shutdown: application shutdown does not automatically close its stream sessions or
cancel in-flight credential refreshes. Use explicit construction for one-time unary
config calls and DI registration for long-lived clients. The descriptor stays
private. Initial binding discovery, snapshot fallback and legacy audience retries
remain consumer responsibilities.

`ServiceBinding.Headers map[string]string` supplies static non-secret request
defaults (`headers` in config), such as `X-Putnami-Observed-Revision`. The binding
snapshots the map; explicit operation headers win case-insensitively. Credential,
identity, tracing and transport headers are reserved, and a static operation
idempotency key fails before dispatch. Keep secrets in `Credentials` and pass
per-call user tokens with `WithForwardedUserToken`. Every transport carries the
defaults, with first-party WebSocket ordinary headers in the init frame.

TypeScript clients apply the same rules to `ServiceBinding.headers` and to
`clients.services.<service>.headers`, so shared config sends the same headers
from both runtimes. Values are visible ASCII without surrounding whitespace.

## Response validation

A response, stream message or declared error `details` carrying a property the
schema does not declare is accepted and the property is dropped from the typed
value; declared properties are fully validated. Adding an optional response
property is therefore safe for deployed clients. Request bodies and client
messages stay strict (protocols/clientcontract ADR 0014).

## Registry lifetime

`client.Services()` publishes one `*client.ServiceBindings` per application and
implements `app.Stopper`. One application, one registry, one credential cache:
modules share it, two applications never do, and the package holds no ambient
cache.

```go
release, err := bindings.TrackStream(session) // stream transports only
defer release()

err = bindings.Close() // idempotent; the application stop phase calls it
```

`Close` cancels the refreshes in flight, drops every cached credential and every
cached response, and closes the tracked stream sessions. After it, `For`, `NewServiceClient`, credential
acquisition and `TrackStream` all return `client.closed` — never the caller's
`client.canceled` or `client.deadline`. A `CredentialBinding.Provider` you supply
stays caller-owned and is never closed by the registry.

## Response cache

A provider declares a cache on one safe or idempotent unary operation with
`ClientOperationOptions.Resilience.Cache` (`freshMs`, optional `staleMs`,
`maxEntries`, `keyFields`, `invalidationFields`). The generated call honors it
with no consumer code:

- Younger than `freshMs`, the stored answer is returned without a call.
- Otherwise one call per key and forwarded identity goes upstream; concurrent
  callers wait for it.
- A transport error, the operation's own timeout, an open breaker or a
  retryable answer left after the retries returns the stored answer while it is
  younger than `staleMs`, counts `rpc.client.cache.stale_served` and logs one
  warning. Anything else, and everything past `staleMs`, reaches the caller.

A caller whose own context deadline passes while the provider hangs also
takes the stored answer inside `staleMs`; the shared call keeps running for the
others. An explicit cancellation always returns `client.canceled`.

The cache lives on the registry: `Close` drops it and cancels the shared calls
in flight. Entries never cross a forwarded user identity or a service binding,
and that is the whole partition: a tenant carried another way (a header set
from the request context, a field left out of `keyFields`) must be in the
operation's key — the provider owns that. Revoke answers at once:

```go
bindings.InvalidateResponses("identity", "effectiveAccess?")           // one operation
bindings.InvalidateResponses("identity", `effectiveAccess?body.principal=%22p1%22`)
```

The key is `<operationId>?<field>=<value>&…` (ADR 0007 of the client contract),
the same bytes the TypeScript runtime renders.

A revocation often names a value the request never carried: an access answer
names its `principalId` while the request named an issuer and a subject. When
the provider lists that property in `invalidationFields`, drop every answer
that carries it, across the service's operations and for every identity:

```go
dropped, err := bindings.InvalidateResponsesByField("identity", "principalId", event.PrincipalID)
dropped, err = identityClient.InvalidateResponsesByField("principalId", event.PrincipalID)
```

The value is a string, a boolean, an integer of any width (or a named type of
one), or a `json.Number` holding an integer. It is compared in the canonical
form both runtimes render, so `"42"` and `42` are different values. Any other
value returns `client.config` instead of matching nothing. A call in flight
for an operation that declares the field still answers its callers, but its
answer is never stored.

A route that must act on the provider's current answer — a mutation checking
access before it writes — bypasses the cache for its calls:

```go
access, err := identity.EffectiveAccess(client.WithoutResponseCache(ctx), input)
```

A bypassed call neither reads nor stores an entry and never waits on a shared
call in flight. No stored answer masks its failure. See ADR 0007 of the client
contract.

## Typed remote errors

A non-success response is a `*RemoteError`. `Service()` and `Operation()` name
the failing call from the generated contract; `Code()` is the provider-declared
stable code; `Payload` holds declared details after credential redaction, and is
nil when redaction cannot preserve the declared schema. Raw response bytes,
request headers and the URL never reach the error.

Redaction removes credential material in every encoding it can take — string,
JSON number, boolean, or base64 bytes compared decoded — and keeps
provider-declared business fields such as `tokenCount` whatever their name
resembles.

`Message` is the provider's free-text envelope `message`, and it is empty unless
the binding asked for it:

```go
binding := client.ServiceBinding{URL: endpoint, CarryRemoteMessage: true}
```

Set it on a consumer that shows the failure to the human who made the request —
a CLI, for one — and that consumer then owns what it logs. The carried text is
redacted of this call's own credential material, on a declared and an undeclared
code alike, and on an HTTP answer and a WebSocket error frame alike. The
envelope's `error` member is never carried: it is always the status text. See
ADR 0006 of the client contract.

## Retries and Retry-After

`Retry-After` on 429 or 503 replaces the computed backoff, in delta-seconds or
as an HTTP date. Any wait that does not fit the call's remaining budget stops
the retries and surfaces the typed remote error instead of `client.deadline`.
Backoff base and ceiling come from the declared resilience policy.

An operation that declares a credential is never sent anonymously: without a
satisfiable alternative, or without a registered binding, the call fails with
`client.credential` before dispatch. The one exception is the provider's own
declaration: an operation whose last alternative is anonymous (an empty
`AllOf`, published only for a route whose rule serves anonymous callers) presents
the credential when the binding satisfies an earlier alternative and is sent
anonymously otherwise.

## Server streams

`OpenOperationServerStream[T]` is the single entrypoint a generated server-stream
method calls, whatever the declared order is: it walks the provider's declared
transports, opens the first this runtime can carry — Connect, SSE or the
first-party WebSocket wire — and the caller never branches on which.
`OpenServerStream[T]` is the SSE opener behind it, and `OpenServerStreamWS[T]`
the WebSocket one; both return the same typed `Stream[T]`. The lifecycle belongs
to `StreamSession`, which every first-party stream transport drives:

- **Five phases**, in order and without going back: connecting, admitted,
  active, terminal, closed. A session always reaches closed.
- **Admission is the handshake** — a 2xx response carrying the declared content
  type — not the first message. Nothing reaches the caller before it.
- **An attempt is replayable only before admission**, only when the declared
  idempotency is `safe` or `idempotent`, and only for a declared retryable
  failure class. A delivered message is never replayed.
- **The circuit breaker records admission once and nothing after it.** A
  mid-stream break is a session fact, not an availability fact.
- **Four budgets** bound the session independently of the declared operation
  duration: handshake, idle, frame size and queue depth. `resilience.timeoutMs`
  bounds the whole session only when it is declared.
- **Credentials are re-checked at the send point**, so an acquisition that
  outlived its own credential never emits the expired value. A 401 or 403 at
  admission invalidates it exactly once, without replaying the stream.
- **The session is tracked on the application registry**: stopping the
  application closes an open stream with the `client.canceled` terminal, and a
  stopped registry refuses to open one (`client.closed`).
- **An SSE comment is liveness, not a message**: it resets the idle budget and
  never reaches the caller.
- **One terminal, one closure, one measurement**: `Stream.Err()` holds the
  single sanitized terminal error, `Messages()` closes exactly once, and the
  telemetry call measurement is emitted exactly once.

## Transport dispatch and Connect

A generated method describes one call and lets the runtime choose the wire:

```go
call := &client.OperationCall{Request: request, PathParams: params}
item, err := client.CallOperation[Item](ctx, c.transport, call, getItemOperation)
stream, err := client.OpenOperationServerStream[Item](ctx, c.transport, call, watchOperation)
```

An operation that declares more than one success status calls
`client.CallOperationResponse`, which keeps the status the provider answered,
and decodes the body with `client.DecodeResponse[T]` under that status's own
schema. The runtime refuses an undeclared status, a body on a bodyless status,
and a Connect transport, whose response carries one status.

- **The declared order is the dispatch order.** The runtime takes the first
  transport in `operation.transports` it can drive: `rest-json` or `connect`
  (json or proto) for a unary call; `sse`, `websocket` or `connect` for a server
  stream. A unary call has no fallback — a transport that fails does not become
  another one. A **server stream** has one, and only one: see "Declared
  preference and fallback" below.
- **The Connect path rides the shared chain.** Credentials, the deadline, retry,
  the circuit breaker and trace propagation are the ones the REST path uses. The
  transport translates at the wire boundary and hands back the first-party
  shape: the declared success status with the schema's JSON, or the first-party
  error envelope rebuilt from the Connect error document — whose stable code,
  exact status and declared details come from the
  `putnami.client.v1.FrameworkError` detail both first-party providers publish.
- **The proto codec comes from `contract.protobuf`**, the descriptor the
  provider publishes. No stub is generated. The JSON side is the published
  schema's JSON — a 64-bit integer is a number, an enum is its published member,
  a Duration is int64 nanoseconds — so one value is identical on all three
  transports.
- **A Connect server stream drives the same `StreamSession`.** Admission is the
  accepted response headers; the stream ends on exactly one `EndStreamResponse`,
  and a stream that ends without one, or carries bytes after one, is a terminal
  error rather than a clean end.
- **Refused, never degraded**: a `proto` transport with no published descriptor,
  a repeated query parameter or a non-object body over Connect, and — at
  generation — a nullable member over `connect+proto`, because proto3 has one
  absence and a caller reading `Optional.IsNull()` can tell null from absent.

See [ADR 0004](doc/adr/0004-the-connect-transport-is-a-projection-of-one-declaration.md).

### Declared preference and fallback

The provider declares the order (`api.ClientOperationOptions.Transports`), and
`OpenOperationServerStream` follows it. It opens the **next** declared transport
only when the provider *answered* that this wire is not served at this path: HTTP 404, 405 or
426, gRPC status 12 `UNIMPLEMENTED`, or a completed handshake that did not select
`putnami.service.v1`. Every other answer — a refused credential, a server fault,
a rate limit, a budget, a contract violation, a caller withdrawal — is a fact
about the call and is surfaced as it is.

Two further rules bound it: a fallback is only legal for an operation the
provider declared `safe` or `idempotent`, and there is none after admission,
because the next wire would deliver what was already delivered. A client or
bidirectional stream never falls back at all.

`OpenServerStreamWS[T]` remains the explicit WebSocket-only entrypoint for a
caller that has a reason to name the wire.

### Declared resume

A server stream continues over a new socket after a transport break when all
four halves agree: the shape is a server stream, the transport declares
`websocket.resume`, the operation declares `resilience.stream.reconnect`, and the
operation is declared `safe`. The published contract refuses `reconnect` unless
one declared transport can honor it: `websocket.resume` here, or an SSE
continuation (see [Declared SSE continuation](#declared-sse-continuation)). A
resume token is redeemed only where it was minted; an SSE cursor is a position,
not a grant, and is redeemed on any instance.

- The continuation presents the token the previous `ready` issued and the
  sequence of the **last message the caller completely received**.
- Credentials are re-resolved before it dials; a failure ends the session before
  admission.
- A provider that answers a continuation with a fresh stream is refused rather
  than consumed — it would deliver read values a second time.
- Continuations are bounded by a framework limit of five, and every socket
  belongs to one session: one breaker verdict, one call measurement.
- Nothing continues unless the declaration says so, and a break on a stream that
  was not declared resumable is a typed terminal.

### Declared SSE continuation

An SSE transport that declares `sse.continuation` (clientcontract ADR 0013)
speaks the negotiated wire on every connection: the request carries
`X-Putnami-Stream-Wire: putnami.sse.v1`, and a response without that
acknowledgment is a contract error before any message, with no fallback. The
stream ends only at `event: complete` or a typed error; an end of body before
either is an interruption. With `resilience.stream.reconnect`, an interruption
reopens the same operation in the same session:

- **Cursor mode** reopens with the declared query parameter set to the cursor of
  the **last message the caller received**. `Messages()` is unbuffered behind a
  bounded queue, so a received value is one the caller took; values queued but
  not taken are dropped and sent again by the provider.
- **Best-effort mode** reopens with the original query and keeps its queue.
  Messages produced while no connection was open may be missing or repeated.
- Credentials are resolved again for every reopening. A reopening refused with
  401 re-mints a forwarded user credential once through the binding's `Refresh`;
  the first opening never re-mints.
- Reopenings share the framework limit of five with WebSocket resume. A typed
  error, a contract error, cancellation or an expired budget never reopens.
- `Done()` and `Err()` answer at the terminal, and the call measurement is
  emitted then. Values queued before the terminal stay readable after it, even
  when the declared duration or the caller's context ends first; `Messages()`
  closes once the caller has taken them or closed the stream.

## Raw octet payloads

A provider that declares a bounded raw octet payload (`api.Binary`) gets a
generated method that carries the bytes unchanged:

```go
stored, err := c.PutBlob(ctx, PutBlobInput{Path: PutBlobPath{Id: "1"}, Body: file})
blob, err := c.GetBlob(ctx, GetBlobInput{Path: GetBlobPath{Id: "1"}})
// blob.Status, blob.ContentType, blob.Body
```

- **The request payload is an `io.Reader`**, read by `client.ReadBoundedBody`
  under the declared bound and refused one octet past it, so an oversized
  source is never drained and no socket is opened.
- **The success payload carries the octets, the status and the content type.**
  A caller that had to re-read the media type from a header would be writing
  transport code the contract already declares.
- **The response read is capped by the declaration**, on 2xx only: an error
  envelope is not the payload the contract bounded, and capping it there would
  make a provider's own refusal unreadable.
- **Nothing is base64-encoded and nothing is wrapped in JSON.** A raw octet
  response reached through the JSON decoder is refused rather than guessed.
- **Only `rest-json` carries octets.** A contract that dispatches them on
  Connect is refused, at generation and at dispatch.

See [ADR 0005](../api/doc/adr/0007-a-raw-octet-payload-is-declared-and-bounded.md).

For an explicitly streamed binary declaration, the generated upload accepts an
`io.Reader` and caller-supplied concrete Content-Type without reading it first.
The generated download returns `*client.StreamedBinaryPayload`: `Status`, the
full `ContentType` including parameters, and a caller-owned `io.ReadCloser`
`Body`. Always close that body. Bytes remain opaque even for `application/json`.
Unary deadlines continue through consumption; EOF, cancellation, Close and
application stop release the body. Uploads are never replayed, including on a
credential rejection, and streamed operations cannot use the response cache.
These are raw HTTP bodies, without stream framing. Bounded `api.Binary` methods
keep their existing behavior. See [ADR 0007](doc/adr/0007-unframed-binary-bodies-transfer-ownership.md).

## Verbatim success bodies

Some answers are proven over their exact bytes rather than trusted as a decoded
value — a body whose identifier or digest the provider derived from the byte
sequence it sent. Marshalling the generated value again produces other bytes,
so a caller that owns such a check asks the ordinary generated call for the
body it accepted:

```go
// items is the generated Items client of the service-to-service sample.
var raw client.SuccessBody
item, err := items.GetItems(client.WithSuccessBody(ctx, &raw), GetItemsInput{Path: GetItemsPath{Id: "1"}})
if err != nil {
    return err
}
// raw.Bytes() is the provider's body, verbatim: verify a digest over it, or
// derive a canonical identifier from it — the decoded item cannot give it back.
```

- **The bytes are the provider's, after the call's own checks.** Declared
  security and resilience, the declared status and media type, the schema
  projection and the typed decode all ran; `Bytes()` is the body they accepted.
- **A cached answer delivers the stored bytes.** The sink and the cache each
  hold their own copy, so nothing the caller does with the bytes reaches the
  cache.
- **A failed call leaves the sink empty.** `Bytes()` is `nil` after a refused
  status, media type or schema, a provider error, or a transport failure —
  never an earlier call's body.
- **Only a transport that carries the document unchanged delivers it**:
  `rest-json` and Connect with the `json` encoding. Connect with the `proto`
  encoding is refused with `client.config` before dispatch, because the runtime
  rebuilds the JSON from the protobuf reply. A void operation, a raw octet
  payload and a stream refuse the sink the same way; octets have
  `CallOperationBinary`.
- **One sink, one call at a time**, and a callback's nested generated call
  neither fills the outer sink nor is refused because of it.

See [ADR 0008](doc/adr/0008-a-call-delivers-its-success-body-through-a-caller-owned-sink.md).

## WebSocket streams

`OpenServerStreamWS[T]`, `OpenClientStream[TIn, TOut]` and
`OpenBidiStream[TIn, TOut]` open a provider-declared `websocket` transport. They
drive the same `StreamSession` as SSE, so every rule above applies unchanged, and
they drive the published `WebSocketConversationV1` for the wire, so the runtime
states no transition of its own.

- **Admission is in band.** The opening handshake carries only RFC 6455
  negotiation headers and the `putnami.service.v1` subprotocol; identity,
  deadline, budget, declared ordinary headers, propagation context and
  credentials travel in the first application frame. The provider's `ready`
  frame is the admission.
- **The first frame is validated locally first.** A frame the contract would
  refuse is reported before any network byte as a `*WebSocketContractError`
  carrying the contract's diagnostic code.
- **JSON carries the messages.** Wire v1 keeps `proto` in the contract; a
  `proto` transport is refused at open, naming the operation and the encoding,
  never mid-stream.
- **`ClientStream.Result` returns the declared terminal value**; a
  bidirectional terminal value is returned as the last `Recv` before `io.EOF`.
- **`Close()` emits one `cancel` frame** and lets the reader keep the terminal,
  so exactly one call measurement is emitted with the right code.
- **Resumption follows the declaration, never a caller argument.** See "Declared
  resume" above: the four halves of the agreement are what continue a stream.

## Provider-owned WebSocket wires

`OpenByteStream` returns a `*ByteStream`, an `io.ReadWriteCloser` over binary
messages; `OpenFrameStream[TIn, TOut]` returns a `*FrameStream` with `Send`,
`Recv` (`io.EOF` on a normal provider close), `Close`, `Done` and `Err`. They
open an operation whose websocket transport declares `wire: "provider"`:

- the upgrade request is the admission and carries the declared credential,
  `X-Client-Id`, `X-Request-ID` and trace context; a refused upgrade is the
  declared typed error;
- exactly the declared subprotocol is offered, or none, and must be echoed;
- writes are split (bytes) or refused (frames) under the declared frame budget;
  frames are validated against the declared schemas;
- the declared idle, heartbeat (RFC 6455 ping) and operation budgets apply;
- a message of the wrong kind closes with `1003`, a frame outside its schema
  with `1007`; any provider close other than `1000` is a typed error naming the
  code.

The wrong entry point for the declared encoding, or a first-party operation, is
refused with `client.config` before anything is dialed.

## Low-level builder

```go
c, err := client.NewBuilder().
    BaseURL("https://api.example.com").
    ClientID("my-service").
    Timeout(10 * time.Second).
    Retry(client.RetryConfig{
        MaxRetries: 3,
        BaseDelay:  200 * time.Millisecond,
        MaxDelay:   5 * time.Second,
    }).
    CircuitBreaker(client.CircuitBreakerConfig{
        FailureThreshold: 5,
        ResetTimeout:     30 * time.Second,
    }).
    Build()
```

Setters use unprefixed names (`Retry`, `CircuitBreaker`, `Interceptors`,
`Transport`).

## Low-level interceptors

```go
c, _ := client.NewBuilder().
    BaseURL("https://api.example.com").
    Interceptors(func(ctx context.Context, req *client.Request, next client.InterceptorFunc) (*client.Response, error) {
        req.SetHeader("X-Request-ID", "req-123")
        return next(ctx, req)
    }).
    Build()
```

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [typed
service clients specification](specs/typed-service-clients.json), the
[resilience-chain ADR](doc/adr/0001-resilience-is-an-ordered-chain.md), the
[stream-lifecycle ADR](doc/adr/0002-stream-sessions-have-five-phases-and-four-budgets.md),
and the
[producer-lineage ADR](../api/doc/adr/0002-generated-clients-carry-producer-lineage.md).
Before v1.0, follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor.
