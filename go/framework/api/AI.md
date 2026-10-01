# go.putnami.dev/api

Transport-agnostic endpoint definitions and orchestrator for the Putnami Go framework.

## Surface

- `api.Endpoint(method, path)` — fluent builder, returns `*EndpointBuilder`
- `api.EndpointBuilder` — chain `.Description()`, `.Params()`, `.Query()`, `.Body()`,
  `.Returns()`, `.ReturnsStatus()`, `.Response()`, `.MayThrow()`, `.MayThrowWith()`,
  `.MayThrowDetails()`, `.Throws()` / `.ThrowsAll()`,
  `.Inject()`, `.Secure()`, `.Use()`, `.Cors()`, `.RateLimit()`, `.Cache()`, then
  `.Handle()` or `.HandleRaw()`
- `api.EndpointDefinition` — frozen output of the builder; carries metadata accessors
- `api.New(server, opts...)` — builds the orchestrator plugin
- `api.WithPrefix(prefix)` — option for path prefix
- `api.Plugin` — implements `app.Plugin` + `app.Configurer`; `.Register(def)`,
  `.DiscoveredRoutes()`, `.Prefix()`
- `api.Server` — transport contract: `Handle(method, path, handler)`. `*http.ServerPlugin`
  implements it
- `api.ResponseMeta` — `{Status, Description, Schema}` for `.Response()` / `.Throws()`
- `api.DiscoveredRoute` — flattened metadata consumed by openapi / proto / gRPC / clients;
  carries the primary `ReturnsStatus` / `ReturnsDescription`, `StreamMode` for
  streaming endpoints, `Responses.ErrorCodes` from `.MayThrow()`, and
  `Responses.ErrorDetails` from `.MayThrowDetails()`
- `.MayThrowDetails(code, api.Type[T]())` — declares a stable error code and the type
  of its envelope's `details` member (ADR 0006: the schema is `details`, never the
  envelope). It adds no retry classification; chain `.MayThrowWith()` for one. The
  status is `errors.HTTPStatusForCode(code)`, the table the provider answers with, so
  register a custom code once with `errors.RegisterHTTPStatus(code, status)`. The
  handler attaches the value with `errors.Any("details", value)` on an `errors.User`
  (or security) error; other categories never expose details. It panics on
  `errors.CodeBadRequest` and `errors.CodeInternalServer`: the framework owns the
  details of those implicit errors
- `api.CanonicalOperationID(method, path)` — the one synthesis of an operation's canonical
  identity (`GET /users/{id}` → `getUsers_Id`). The OpenAPI generator stamps it as the
  spec's `operationId`, generated clients trace with it, and the design graph joins on it
- `api.Type[T]()` — `reflect.TypeFor[T]` shorthand for chained schema declarations

Declared first-party JSON bodies are decoded into `T` and validated against the
original bytes before dispatch. Required means present on the wire, including
present zero values; explicit null, nested constraints/defaults, exact integers,
and primitive or array roots retain their JSON meaning. Query and path inputs
continue to use the low-level validated-map helpers.
Body validation runs for `POST`, `PUT`, `PATCH`, and for a `DELETE` that carries
a body. A `DELETE` with no body still reaches the handler.

`Body(api.Binary(mediaType, maxBytes))` and `Returns(api.Binary(...))` declare a
raw octet payload: the HTTP body itself is bytes, under a named media type,
bounded. The bound is mandatory for this buffered declaration. The pipeline answers 415 on an undeclared media
type and 413 on a payload past the bound, taken from `Content-Length` before
the body is read, and both refusals are published as declared errors. The
handler reads the octets with `api.BinaryBody(ctx)` and answers with
`api.BinaryResponse(status, mediaType, data)`. Base64 bytes inside a JSON
document stay `format: byte` and change nothing; `format: binary` inside a JSON
document is a generation failure (ADR 0007).

`Body(api.BinaryStream(maxBytes))` / `Returns(api.BinaryStream(maxBytes))` instead declare one
unbuffered HTTP body with a sender-chosen concrete Content-Type. The contract
publishes `*/*`, `format: binary`, `x-putnami-streamed: true`, and the positive
route-level byte bound.
Read it using `api.BinaryStreamBody(ctx)` and return
`api.BinaryStreamResponse(status, contentType, reader)`. Parameters in the media
type survive unchanged; even JSON-labelled octets remain opaque. The provider
enforces the declared bound on that route without changing the global JSON body
limit. Generated Go inputs carry `ContentType` and `Body io.Reader`; outputs
carry `Body io.ReadCloser`, which the caller must close. Uploads are not replayed
and streamed operations cannot declare a response cache (ADR 0011).

`Returns(type)` declares the compatible HTTP 200 primary response.
`ReturnsStatus(status, description, type)` declares a different 2xx primary
response on a unary endpoint, such as `201 Created`. OpenAPI publishes that exact
status and schema; generated clients retain the response type. `Response(...)`
remains for additional success responses. An operation with more than one
success status generates a Go method returning `*<Method>Result`: `Status` is
the status the provider answered, and the body is `Body` when every
body-carrying status declares the same schema (nil on a bodyless status), or
`Body<status>` per status otherwise (ADR 0008). Connect dispatch and a raw octet
response are refused beside several success statuses.

`api.GoClientOptions.OmitOperations` leaves the named operations out of the Go
target instead of failing the provider; the generated doc comment, the build
output and `omittedOperations` in `client.putnami.json` name each one, and an
unknown name fails.

A generated Go method takes its authored operationId, or else the route's REST
idiom (`GET /users/{id}` → `GetUsers`), which drops path parameters. When routes
differ only by a path parameter and would share that name, the one with fewer
path parameters keeps it and the other takes its operationId's exported form
(`GET /x/{a}/deploy/{b}` → `GetXADeployB`), the TypeScript method name with an
upper-case first letter. Registration order never changes a name (ADR 0003).

## Path syntax

| Token            | Meaning                                                                                  |
| ---------------- | ---------------------------------------------------------------------------------------- |
| `/users`         | exact match                                                                              |
| `/users/{id}`    | single-segment path parameter — rejects slashes                                          |
| `/{name...}/...` | catch-all parameter — captures one or more slash-separated segments, position-agnostic   |

Catch-all params bind the joined multi-segment value (e.g. `go.putnami.dev/protocol/diagnostic`)
to the named param. They may appear before a fixed suffix or another single-segment
param (`/{module...}/-/blobs/upload`, `/{module...}/@v/{versionfile}`). Validation of
segment shape is the surface's responsibility — the framework only routes.

OpenAPI 3.0 has no native multi-segment path-param syntax, so the spec generator
renders catch-all params as ordinary `{name}` strings with a description noting
the multi-segment semantics.

## Streaming endpoints

Streaming uses `api.Stream` / `api.StreamOf[T]()` to mark request or response
schemas, plus a typed handler adapter to wrap the user's stream function:

| Mode | Handler adapter | Wire transport |
| ---- | --------------- | -------------- |
| Server (one-way push) | `api.ServerStream(handler)` | SSE (default) or WebSocket |
| Client (one-way upload) | `api.ClientStream(handler)` | WebSocket |
| Bidirectional | `api.BidiStream(handler)` | WebSocket |

`api.ClientStream[TIn, TOut]` takes the uploaded message type and the single
response type; the handler declares that value with `ctx.Result(v)`, and a
handler that completes without one is refused. `api.BidiStream` may declare an
optional terminal value the same way.

```go
// Server stream (SSE / WS): push events to the client
api.Endpoint("GET", "/feed").
    Returns(api.StreamOf[Event]()).
    Handle(api.ServerStream(func(ctx *api.ServerStreamContext[Event]) error {
        for {
            ctx.Send(Event{...})
        }
    }))

// Bidirectional stream (WS): chat-style request/response
api.Endpoint("GET", "/chat").
    Body(api.StreamOf[ClientMsg]()).
    Returns(api.StreamOf[ServerMsg]()).
    Handle(api.BidiStream(func(ctx *api.BidiStreamContext[ClientMsg, ServerMsg]) error {
        for msg := range ctx.Messages() {
            ctx.Send(reply(msg))
        }
        return ctx.Err()
    }))
```

The stream mode is captured on `DiscoveredRoute.StreamMode` so downstream
generators (openapi, proto, gRPC bridge) emit the correct shape — proto, for
example, prefixes the appropriate Request/Reply with `stream`.

### Provider-owned WebSocket wires

Some routes speak a wire the provider owns rather than the first-party
conversation (`putnami.service.v1`). Two forms are declared, both bidirectional:

| Form | Declaration | Handler | Generated Go method returns |
| ---- | ----------- | ------- | --------------------------- |
| Byte stream | `Body(api.ByteStream())` and `Returns(api.ByteStream())`, optional `.Subprotocol(token)` | `api.ByteTunnel(func(*api.ByteStreamContext) error)` — an `io.ReadWriter` | `*client.ByteStream` (`io.ReadWriteCloser`) |
| Provider-owned subprotocol | `Body(api.StreamOf[In]())`, `Returns(api.StreamOf[Out]())` and `.Subprotocol("putnami.events.v1")` | `api.BidiStream` — one JSON value per message, no envelope | `*client.FrameStream[In, Out]` |

```go
api.Endpoint("GET", "/v1/databases/connect").
    Query(api.Type[ConnectQuery]()).
    Body(api.ByteStream()).
    Returns(api.ByteStream()).
    Secure(security.Options{Scopes: []string{"gateway:connect"}}).
    Handle(api.ByteTunnel(func(ctx *api.ByteStreamContext) error {
        return pipe(ctx, backendFor(ctx.QueryParams().Get("database")))
    }))
```

The upgrade request is the admission: the endpoint's security and validation
chain runs on it and refuses with an ordinary HTTP response, and the route
speaks exactly its declared token, or none. After that the framework owns the
socket — the declared frame and idle bounds, RFC 6455 pings at the declared
heartbeat, and close codes (`1000` when the handler returns, `1008`/`1001`/`1011`
from the handler's error status, `1003` for the wrong message kind, `1007` for a
frame that is not JSON, `1001` on shutdown). `ByteStreamContext.Read` returns
`io.EOF` when the client closes normally. The published contract carries the
wire (`wire: "provider"`, encoding `binary` or `json`) as the operation's only
transport. See `protocols/clientcontract` ADR 0010 and this module's ADR 0010.

### Declaring how an operation travels

`Client(api.ClientOperationOptions{...})` carries the client-facing policy the
route itself cannot express. Four fields decide the wire:

- `Transports` is the operation's preference order. It reorders and narrows the
  transports the bound server actually serves; it cannot add one. A protocol
  named twice, or one this provider does not serve for the route's shape, fails
  contract generation with the route named. Every generated client dispatches in
  this order, and none of them takes a transport argument.
- `ConnectEncodings` is the operation's Connect payload encoding order. Connect
  is the one transport a provider serves in more than one encoding: the contract
  carries one Connect transport entry per encoding and a generated client
  dispatches the first it can carry, so this is where a provider states which
  encoding travels. Like `Transports` it reorders and narrows what the mounted
  Connect bridge serves and never adds an encoding. Absent, the bridge order
  applies.
- `Resume` states that this server stream can be continued after a broken
  socket. It is published as `websocket.resume` and is only accepted on a
  **server** stream declared **safe**: continuing is re-reading a position the
  caller already consumed, which is only harmless there.
- `SSEContinuation` states how this operation's SSE transport continues after a
  broken connection (clientcontract ADR 0013).
  `api.SSECursorContinuation(outputField, queryParameter)` declares **cursor
  mode**: every output message carries the provider's opaque position after it
  in `outputField`, a required plain-string property, and a reopened connection
  sends the position of the last message the consumer received in
  `queryParameter`, a declared plain-string query parameter; the provider
  continues exclusively after it, on any instance.
  `api.SSEBestEffortContinuation()` declares **best-effort mode**: a reopened
  connection sends the original query and no position, and messages produced
  while no connection was open may be missing or repeated. Only a **server**
  stream declared **safe** may declare one; the projection checks both cursor
  references against the route's own schemas and refuses every other shape with
  the route named. `resilience.stream.reconnect` is the consumer half, effective
  when the operation carries `Resume` or `SSEContinuation`. A route that declares
  one also speaks the negotiated SSE wire: a consumer that sends
  `X-Putnami-Stream-Wire: putnami.sse.v1` is acknowledged with the same header
  on the response head and reads an explicit `event: complete` terminal once the
  handler returns; a consumer that sends no marker reads the legacy framing. A
  cursor is a position, not a credential: every continuation runs the endpoint's
  security chain, and the provider owns the cursor's binding, retention and the
  typed refusal of a stale or forged one.

```go
api.Endpoint("GET", "/items/{id}/history").
    Returns(api.StreamOf[Revision]()).
    Client(api.ClientOperationOptions{
        Transports: []clientcontract.TransportProtocol{
            clientcontract.TransportWebSocket, clientcontract.TransportSSE,
        },
        Resume: true,
        // The consumer half of the same agreement. The published contract
        // refuses `reconnect` without a resume-capable transport.
        Resilience: &clientcontract.ResiliencePolicy{
            Stream: &clientcontract.StreamPolicy{Reconnect: &enabled},
        },
    }).
    Handle(api.ServerStream(func(ctx *api.ServerStreamContext[Revision]) error {
        // A continuation is told where the consumer left. A fresh stream
        // continues after nothing, which is 0.
        from, _ := api.StreamResumeFrom(ctx.Context.Context())
        ...
    }))

// A change feed a broken connection continues after the last change the
// consumer received, on whichever instance answers. ItemChange.Cursor is a
// required string and ChangesQuery.Cursor a declared string query parameter.
api.Endpoint("GET", "/items/changes").
    Query(api.Type[ChangesQuery]()).
    Returns(api.StreamOf[ItemChange]()).
    Client(api.ClientOperationOptions{
        Transports:      []clientcontract.TransportProtocol{clientcontract.TransportSSE},
        SSEContinuation: api.SSECursorContinuation("cursor", "cursor"),
        Resilience: &clientcontract.ResiliencePolicy{
            Stream: &clientcontract.StreamPolicy{Reconnect: &enabled},
        },
    }).
    MayThrow(perrors.CodeNotFound).
    Handle(api.ServerStream(func(ctx *api.ServerStreamContext[ItemChange]) error {
        // Continue exclusively after the position the consumer handed back. A
        // position this provider never issued is the declared not_found: the
        // feed never restarts from the beginning.
        after, known := changes.Position(ctx.Query("cursor"))
        if !known {
            return perrors.NotFound("the position is not in the retained change log")
        }
        ...
    }))
```

`External` names the external authority that owns a route of a first-party
provider — a standard protocol served beside the Putnami routes. The route stays
served and published, with `x-putnami-external-contract` and no
`x-putnami-client`; it has no protobuf method, no Connect URL and no generated
client method, and it keeps the standard request pipeline (lenient body
decoding, the standard error body, the raw stream transport). `External` stands
alone: blank, combined with any other field, or declared on an API without
`WithClientService`, it fails `Configure` before any route is bound. See
`protocols/clientcontract/doc/adr/0011-an-external-authority-can-own-an-operation.md`.

```go
apiPlugin.Register(api.Endpoint("GET", "/v2/{name}/manifests/{reference}").
    Client(api.ClientOperationOptions{External: "OCI Distribution Specification v1.1"}).
    HandleRaw(getManifest))
```

`Resilience.Cache` declares a response cache that every generated client
honors (ADR 0007 of `protocols/clientcontract`). It is independent of
`.Cache(http.CacheOptions)`, which still only sets `Cache-Control` for HTTP
intermediaries. Only a unary operation whose idempotency is safe or idempotent
may declare it; a key field that names no declared path, query or header
parameter or body property fails publication with the route named. So does an
invalidation field that names no string, integer or boolean top-level property
of the success body (a `format: byte` or `binary` string is octets, not a string) (ADR 0007 of `protocols/clientcontract`): consumers drop every answer that carries one
value of it with `InvalidateResponsesByField`.

```go
staleMs := 300_000
api.Endpoint("GET", "/accounts/{id}").
    Params(api.Type[AccountParams]()).
    Returns(api.Type[Account]()).
    Cache(phttp.CacheOptions{MaxAge: 5}).
    Client(api.ClientOperationOptions{
        Idempotency: &clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
        Resilience: &clientcontract.ResiliencePolicy{Cache: &clientcontract.CachePolicy{
            FreshMs: 5000, StaleMs: &staleMs, KeyFields: []string{"path.id"},
            InvalidationFields: []string{"ownerId"},
        }},
    }).
    Handle(getAccount)
```

The emitted Go client pins `client.RequireRuntimeCapabilities("response-cache")`
and its `client.putnami.json` lists `runtimeCapabilities: ["response-cache"]`, so
a runtime that predates the cache fails to build the client instead of running
uncached.

`api.StreamResumeFrom(ctx)` is how a handler avoids re-sending what the consumer
already read. A handler that ignores it produces the whole stream, and the
consumer sees values twice — which is why resume is a declaration and not a
framework default.

### First-party WebSocket admission

When the API declares `api.WithClientService(...)`, a stream route negotiates the
`putnami.service.v1` subprotocol and speaks the published first-party service
protocol (`protocols/clientcontract`). A client that offers that token carries no
credential on the HTTP upgrade: identity, credentials, deadline, budget, declared
headers and propagation context travel in the protocol's mandatory `init` frame,
so a browser and a Go client admit the same way.

For a resumable stream, each admission also mints a single-use, rotating grant
and carries it in the `ready` frame. The grant is bound to the operation and the
client identity that earned it, records the highest sequence this provider put on
the wire, and is bounded by a time to live, a per-stream continuation budget and
a per-endpoint capacity. Redeeming it runs the endpoint's own security chain
again on the request rebuilt from the new `init` frame, so a credential this
session can no longer prove ends the stream with a typed terminal.

The framework reconstructs the request from that frame — each declared credential
profile back onto its injection header, ordinary declared headers replayed,
`traceparent` / `tracestate` / `baggage` / `X-Request-Id` / `X-Client-Id`
restored — then runs the endpoint's **own** security rule, middleware and
parameter validation on it, and only then answers `ready`. Nothing reaches your
handler before that. A refusal becomes a typed `error` frame carrying the same
status and code the endpoint would answer on a unary call.

`resilience.stream` on the operation (or the document default) supplies the
handshake, idle, heartbeat, frame and queue bounds. A heartbeat that is not
declared is not sent. See
[ADR 0004](doc/adr/0004-a-negotiated-websocket-admits-in-band-before-the-security-chain.md).

One wire rule is worth knowing before you shape a stream message: the published
frame vocabulary refuses a JSON `null` anywhere in a frame, application payloads
included. `ctx.Send` on a value whose nullable field is unset fails with
`client_contract.parse_error` rather than writing bytes a conforming client would
reject. Give such fields an explicit empty value, or drop them with `omitempty`.

### First-party Connect contract

The api plugin is the junction where a Connect transport becomes publishable.
Two facts have to arrive, from two plugins:

- `apiPlugin.PublishClientProtobuf(api.ClientProtobufProjection{Descriptor,
  RouteMethods})` — the proto plugin publishes the descriptor a Connect client
  decodes with, together with the `"<METHOD> <path>"` → `/package.Service/Method`
  binding. Read them back with `ClientServiceContract().Protobuf` and
  `ClientProtobufMethods()`.
- `apiPlugin.PublishClientConnectTransport(encodings)` — a mounted Connect bridge
  declares the payload encodings it actually serves. Read it back with
  `ClientConnectEncodings()`.

The OpenAPI projection advertises a Connect transport only when both are
present, so a contract never names a URL nothing answers.

`ReadOpenAPISpec` then refuses any disagreement between the three published
views — descriptor, JSON schema, IR. A descriptor field the schema does not
declare, a presence rule that differs by transport, an integer whose width
narrows on one wire, an enum member the schema does not publish, a Connect path
that is not its method identity, and an integer with no declared width are all
read failures carrying the shared corpus diagnostic code.

### Opaque JSON

A field the provider does not interpret — `json.RawMessage`, `any`,
`map[string]any` — is published as `{"x-putnami-json": "any"}` or as
`{type: object, additionalProperties: true}`, and the first-party body
validator accepts `null` for `json.RawMessage` and `any`. The emitted Go client
holds them as `json.RawMessage` and `map[string]json.RawMessage`: the provider's
bytes, never a decode into `any`. An optional opaque member is a
`json.RawMessage` with `omitempty`, so an explicit `null` and an absent member
stay two values. A route that carries opaque JSON keeps REST and declares no
Connect transport; the emitter refuses an opaque parameter and a Connect
dispatch. See `protocols/clientcontract/doc/adr/0008-opaque-json-is-a-declaration.md`.

### Optional credential

A route that answers anonymous and authenticated callers alike declares it on
its rule, then lists the credential first and the anonymous alternative (an
empty `AllOf`) last:

```go
apiPlugin.Register(api.Endpoint("GET", "/{namespace}/{package}/resolve").
    Returns(api.Type[Resolved]()).
    Secure(security.Options{Optional: true, Scopes: []string{"packages:read"}}).
    Client(api.ClientOperationOptions{Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
        {AllOf: []clientcontract.SecurityRequirement{{Profile: "user"}}},
        {AllOf: []clientcontract.SecurityRequirement{}},
    }}}).
    HandleRaw(resolve))
```

A generated client takes the first alternative its binding satisfies: it
presents the `user` credential when it holds one and calls anonymously when it
does not. The rule's scopes and roles merge into the credentialed alternatives
only. With exactly one credential profile and no `Client(...)` security, the
contract derives the same two alternatives. Publication fails, naming the route,
for an anonymous alternative on a rule that requires authentication, for an
anonymous alternative before a credentialed one, and for credential
alternatives on a route with no rule. See
`go/framework/security/doc/adr/0002-an-optional-rule-serves-a-caller-that-presents-no-credential.md`.

## Lifecycle

1. User builds endpoints via `api.Endpoint(...).Handle(...)`.
2. User calls `apiPlugin.Register(def)` — endpoint is queued.
3. `app.Start()` runs `apiPlugin.Configure()`. Each pending endpoint is built into an
   `http.Handler`, registered on the transport via `Server.Handle`, and captured as a
   `DiscoveredRoute`.
4. Downstream plugins (openapi, proto, grpc, clients) read `apiPlugin.DiscoveredRoutes()`
   in their own `Configure()` step.

## Pairing with `*http.ServerPlugin`

`*http.ServerPlugin` exposes `Handle` (satisfies `api.Server`) and
`AddPendingInjectedHandler` (so api can hand off `*http.InjectedHandler` instances built
by `.Inject()`-finalised endpoints). The api package depends on `go.putnami.dev/http` for
runtime types only; the http package does not depend on api.

## TS parity

`api.Endpoint` mirrors `endpoint()` in `@putnami/application`. `api.Plugin` mirrors the TS
`ApiPlugin`. The schema language differs: TS uses inline schema literals; Go uses
`reflect.Type` with `validate:"…"` struct tags.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the [API
contracts specification](specs/api-contracts.json), the [one-declaration
ADR](doc/adr/0001-one-declaration-many-consumers.md), the [producer-lineage
ADR](doc/adr/0002-generated-clients-carry-producer-lineage.md), and the
[raw-octet ADR](doc/adr/0007-a-raw-octet-payload-is-declared-and-bounded.md). Before v1.0,
follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor.
