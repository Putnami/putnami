# Service-to-Service (Go)

A Go provider called by a Go consumer and by a TypeScript consumer, both through
**generated, typed clients**.

The provider declares an Items API and its first-party client contract.
`putnami clientgen` generates Go and TypeScript clients from that declaration. A
consumer registers the generated binding and calls `ItemsClient` without
hand-written URLs, JSON, authentication or resilience code.

The application declares `items/manage` once with `application.Feature(...)`.
The API surface and both generated client targets are then discovered from the
ordinary `api` and `api.Clients()` declarations:

```bash
putnami build --projects go.putnami.dev/examples/service-to-service
putnami clientgen --projects go.putnami.dev/examples/service-to-service
putnami features inspect items/manage
```

`ItemsClientDesign` embeds the spec hash and the exact operation table, with the
producer project and feature recorded on each operation — a client that spans
several features attributes each one correctly, and an operation whose module
declares no feature stays unattributed. Every generated Go request carries the
matching `FeatureTrace`, resolved from the invoked operation's canonical id; no
base-URL or package-name inference is involved.

## The documented happy path

Four steps, in this order. Nothing else is consumer-owned.

1. **Declare.** The provider registers its endpoints and its client contract in
   `service/service.go`: the service identity, the credential profiles, the query
   string, the request body, the response and the errors it may throw. Read that
   one file to know the whole contract.
2. **Generate.** `putnami clientgen --projects go.putnami.dev/examples/service-to-service`
   emits `clients/go` and `clients/ts` from the provider's own OpenAPI document,
   each with an ownership manifest.
3. **Register the binding.** The consumer calls `client.Services(...)` and the
   generated `RegisterItemsClient(module)` (Go), or `registerItemsClient(app)`
   (TypeScript). The binding carries the base URL and the values for the
   credential profiles the provider declared — nothing else.
4. **Call.** Resolve the client from DI and call the typed method. No URL string,
   no header, no interceptor, no retry policy, no JSON.

`consumer/consumer.go` and `consumer-ts/src/consumer.ts` are those four steps in
each language; their tests are the proof.

## Architecture

```
Provider (this service)            Consumer (./consumer)
  GET  /items                        itemsclient.ItemsClient
  GET  /items/{id}        ◀── HTTP ──   .GetItems(...)
  POST /items                          .CreateItems(...)
  (OpenAPI spec)                       .ListItems(...)
       │
       └── api.Clients() ──▶ clients/go  (generated, typed)
```

## Layout

| Path | Role |
|------|------|
| `service/` | Provider types, handlers, routes; `NewApp()` wires HTTP + api + openapi + `api.Clients()` |
| `main.go` | Runs the provider (`putnami serve .`) |
| `clients/go/` | **Generated** typed client module (`go.putnami.dev/examples/service-to-service/clients/go`) |
| `clients/ts/` | **Generated** typed TypeScript client project (`@example/go-items-client`) |
| `consumer/` | Go consumer: imports the generated client and calls the provider through it |
| `consumer-ts/` | TypeScript consumer (`@example/go-items-consumer`): runs the compiled provider as a subprocess and calls it through the generated TypeScript client |
| `test-scenarios.json` | Machine index of the matrix scenarios this sample proves |

The provider does not import `clients/go`: the build-time describe step compiles the
provider to generate the client, so the consumer that imports it lives in its own
package.

## How the client is generated

During `putnami build` and `putnami clientgen`:

1. Reads the OpenAPI spec from the `openapi` plugin (in-memory — no file-ordering race).
2. Writes the contract to `.gen/clientgen/config.json`.
3. Synchronizes `clients/go` and `clients/ts` with ownership manifests used by
   workspace drift guards.

The build cache captures `clients/go/` under the project-scoped `clients` resource, so a
cache hit re-materializes the generated client without re-running describe, and the
workspace `go.work` stays stable.

`clients/go/client.gen.go` is checked in (like generated protobuf) so the sample builds
out of the box; `clientsync_test.go` regenerates it and fails if it drifts. Regenerate
after an API change with:

```bash
putnami clientgen --projects go.putnami.dev/examples/service-to-service
```

## Opaque JSON

`POST /audit` echoes an `AuditRecord` whose fields the provider does not
interpret: a `map[string]any`, a `json.RawMessage` and an `any`. The contract
declares them in the closed opaque spelling (`{"type": "object",
"additionalProperties": true}` and `{"x-putnami-json": "any"}`), so the Go
client holds them as `map[string]json.RawMessage` and `json.RawMessage`, and the
TypeScript client as `Record<string, unknown>` and `unknown`. Opaque JSON has
no lossless protobuf form, so this route keeps REST and declares no Connect
transport. `consumer/consumer_opaque_test.go` and the TypeScript consumer prove
the bytes survive the round trip.

## The generated TypeScript client

`clients/ts/` is the same contract emitted for TypeScript consumers — the
`@example/go-items-client` project, built on `@putnami/client`. Both emitters read
the provider's OpenAPI document, so the two clients describe one API rather than
two hand-kept copies:

```ts
import { ItemsClient, registerItemsClient } from '@example/go-items-client';

registerItemsClient(application);
const items = application.context.get(ItemsClient);

const list = await items.getItems({ query: { search: 'et', limit: 10n } });
const item = await items.getItems_Id({ path: { id: '1' } });
```

Like the Go client, it embeds the spec hash and the per-operation producer table
(`ItemsClient.design`), so a call can be traced back to the project and feature
that produced the operation.

It is checked in for the same reason the Go client is, and it is a workspace
project, so `putnami lint,test,build --impacted` covers it whenever the provider's
contract changes.

## Run

```bash
putnami serve go.putnami.dev/examples/service-to-service

curl -s localhost:3910/items
curl -s localhost:3910/items/1
```

The provider does not call `openapiPlugin.RegisterOn(httpServer)`, so it does not
serve the document over HTTP: the client generator reads it in memory, and
`putnami build` commits it to `schema/openapi.json`. Add `RegisterOn` when
consumers should fetch the spec at runtime — and read
[the disclosure note](../../framework/openapi/README.md#http-endpoint) first,
because that route is unauthenticated.

## The cross-language matrix

Two couples of the cross-language client matrix have this project as their provider:

| Couple | Consumer | Where |
|---|---|---|
| Go→Go | `consumer/` | in-process provider on a real loopback socket |
| TS→Go | `consumer-ts/` | the compiled provider as a subprocess |

The TypeScript consumer starts the binary `putnami build` produced, reads the
bound port from the reserved `putnami.ready` log marker (the provider runs with
`PORT=0`, so no port is ever hard-coded), and kills it in `afterAll`. It never
runs `go run`, never sleeps and never scans ports. That harness is the runner:
every cross-language cell below runs against this provider as a real program,
started and reaped by the test that needs it. The command under **Verifying**
runs both couples; there is no separate script to learn.

### What the matrix covers

Nine cells — `rest-json`, `rest-binary`, `connect-unary-json`,
`connect-unary-proto`, `connect-stream`, `sse`, `ws-server`, `ws-client`,
`ws-bidi` — times two consumer languages, times five scenario families:

| Family | What it asks of a cell |
|---|---|
| `auth` | anonymous, service credentials with expiry and refresh, a forwarded user identity, an api key, a secondary credential, AND/OR without downgrade |
| `result` | success, schema, presence, bytes, exact numbers, declared retryability, unknown remote error, invalid response, redacted details |
| `policy` | budgets, per attempt, cancellation and limits, idempotency, circuit; for streams admission, idle, queues, terminal, half-close, backpressure |
| `observability` | W3C context from a real inbound request, spans and metrics without double counting, no secret, payload, query string or unbounded label, shutdown on both sides, late calls refused |
| `generation` | deterministic regeneration and contract-break detection |

`test-scenarios.json` is the machine index of all of it. Every scenario row
names the test that proves it (`<file>:<test>`), and every cell × family ×
consumer appears in `coverage` with one of three verdicts:

- `pass` — the scenarios that prove it, by id.
- `n/a` — the protocol rule that makes the combination inapplicable. It is
  never an absence of implementation: for example generation is a property of
  the contract, so it has contract-level rows rather than one per cell.
- `missing` — the reason nothing proves it yet, in words.

### The declared Connect encoding order

The bridge serves Connect JSON before protobuf, and the contract carries one
Connect transport per encoding, so a generated client dispatches whichever
comes first. `GET /quotes/{id}` keeps the bridge order. `GET /quotes/{id}/snapshot`
is the same quote with one more field in its declaration —
`ConnectEncodings: []clientcontract.Encoding{EncodingProto, EncodingJSON}` — and
every generated client, Go or TypeScript, calls it over Connect protobuf
without a consumer branch. `consumer/consumer_connect_proto_test.go` reads that
off the provider's socket.

`matrix_index_test.go` is what makes the index worth reading: it resolves every
named test in the file it names, refuses an `n/a` without a rule and a
`missing` without a reason, and requires an entry for every combination.

### The declared request policy

`POST /items` is the operation that declares one: `X-Idempotency-Key` as the
stable request identity, three attempts, 503 as the only status worth
repeating, 250 ms per attempt inside a 2 s budget, and a circuit that opens
after two consecutive failed calls and stays open for a minute. A consumer
writes none of that — `consumer/consumer_policy_test.go` asserts the generated
client applies exactly what the contract declares.

### The declared raw octet payload

`POST /blobs/echo` and `GET /blobs/{id}` declare their body and their response
with `api.Binary("application/octet-stream", 4096)`: the HTTP body *is* octets,
under a named media type, bounded. The bound is not documentation — the
provider answers 413 before it reads a body that announces more, and the
generated clients read their source one octet past it and refuse there, so an
oversized file is never drained.

`GET /blobs/{id}` shows that a binary success costs the endpoint nothing else:
the path parameter stays typed, the `catalog-key` credential is still injected
by the binding, and `not_found` still arrives as the generated typed error.
Both stored blobs are chosen to break a text pipeline — one is not valid UTF-8
and not valid JSON, the other is empty, because zero octets are a payload and
not a missing body.

### The declared server stream

`GET /items/{id}/watch` is a server stream. It is declared like any other
operation — path parameter, required `follow` query parameter, `Item` message
schema, `not_found` — and the generated clients expose a typed stream handle
rather than a one-shot call. `follow=true` keeps the stream open until the
consumer leaves, which is what makes cancellation and shutdown provable.

Its client contract declares two credential alternatives in order: the
`workload` service token first, the `catalog-key` API key second. A consumer
that can mint a service token uses it; one that can only carry a static key —
the TypeScript consumer, whose runtime refuses a static value for a service-token
profile — falls through to the second alternative instead of dispatching a
partially bound one.

Both stream cells prove the same dimensions: a refused credential reaches the
consumer as a typed error **before any message**, the credential is dropped once
and reacquired on the next open without ever replaying the stream, the terminal
error arrives as its generated type, and a followed stream ends on the consumer's
own cancellation or `Close()`.

### The cursor-continued change feed

`GET /items/changes` is the SSE server stream that survives an instance change.
Every `ItemChange` carries the provider's own opaque position after it
(`cursor`) beside the revision a reader compares, and the operation declares

```go
SSEContinuation: api.SSECursorContinuation("cursor", "cursor")
```

next to `Reconnect: true` in its stream policy: the output field the position
travels in, the query parameter that receives it on a continuation, and the
consumer half of the agreement. A generated client whose connection breaks
reopens the same operation after the last change **its caller received** — not
after the ones it had only decoded and queued — with a credential resolved at
that moment, on whichever instance answers. No consumer writes a reconnect loop
and no consumer reads the cursor. The route also speaks the negotiated SSE wire
(`X-Putnami-Stream-Wire: putnami.sse.v1`, clientcontract ADR 0013), so a
consumer reads an explicit `complete` and never mistakes a cut connection for
the end of the feed.

The feed is the catalog's durable change log: one revision per catalog item at
start, one more per `POST /items`. `cursor` is optional and `until` names the
revision after which the feed completes; a revision that does not exist yet
keeps the stream open until a creation appends it, which is what lets a test
observe a continuation. A position the log never issued is the declared
`not_found`: the feed never restarts from the beginning on a cursor it does not
recognize, and the runtime never reopens after a typed terminal.

The two consumers prove it against two provider instances behind a routing
front, the one double: `consumer/` drains one in-process instance with
`ServerPlugin.Stop` while three creations sit decoded but untaken, and reads
them once from the second instance; `consumer-ts/` drains one compiled provider
**process** with `SIGTERM` and continues on a second process that shares no
memory with it — the second places the continuation by the cursor alone, on the
revisions it seeded from the same catalog.

### The declared WebSocket conversations

`GET /items/{id}/adjust` and `GET /items/{id}/negotiate` are the other two
stream shapes, both carried by the published first-party WebSocket wire.

- **Client stream** (`adjust`): `Body(api.StreamOf[StockDelta]())` with a unary
  `Returns(api.Type[StockTotal]())`. The consumer sends deltas, ends its own
  direction with `CloseSend`, and reads the single declared result.
- **Bidirectional** (`negotiate`): both sides streamed. Every delta is answered
  with the running total, and the value the handler passes to `Result` is the
  last one the consumer reads before the stream ends.

Neither declares an SSE alternative — SSE carries one direction — so the
published transport list holds WebSocket alone and the generated clients open it
with no branch in the consuming application.

`GET /items/{id}/history` is the third stream: the same **server**-stream shape
as `watch`, declared the other way round. Its `Client(...)` block names
`Transports: {websocket, sse}` and `Resume: true`, so the published order puts
the socket first and states that the feed can be continued after a break. The
declaration is the only difference — the handler, the generated method and the
consuming application are what they would be on SSE. The handler reads
`api.StreamResumeFrom(ctx.Context.Context())` and numbers its revisions from
there, which is what makes "no gap, no duplicate" something a consumer can check
for itself.

Both declare `resilience.stream`: a 2 s handshake budget, a 5 s idle budget, a
250 ms provider heartbeat, a 16 KiB reassembled-message bound and a queue four
messages deep. The generated clients read the same numbers from the published
contract, so the two ends never disagree about when a conversation is idle. A
burst well past the queue depth slows the socket instead of dropping a message,
which `TestAdjustStockAppliesBackpressureWithoutLosingAMessage` asserts on the
total the provider returns.

Admission is decided before any frame: an unusable credential is 401 and an
unauthorized client identity is 403, both raised by `AdjustStock` itself rather
than delivered as a message. A `not_found` terminal arrives as its generated
type on both shapes.

## Verifying

```bash
putnami lint,test,build,validate --projects go.putnami.dev/examples/service-to-service,@example/go-items-client,@example/go-items-consumer
```

The sample is tagged `e2e`, so `--impacted` skips it: name the projects. The
consumer project is `@example/go-items-consumer`; `@example/go-items-client` is
the generated package it imports and carries no test of its own.

Each run publishes the checks this sample's feature declares
(`putnami.features.json`) through `spectest.Proves`, and the merged report lands
in the project's `putnami-feature-verification` artifact. Verification runs in
`report` mode: the sample is tagged `e2e`, which an enforcing gate blocks on.

## Use the typed client

```go
module.Use(client.Services())
itemsclient.RegisterItemsClient(module)

item, err := items.GetItems(ctx, itemsclient.GetItemsInput{
    Path: itemsclient.GetItemsPath{Id: "1"},
})
```

## Note on typing

All request, response and error schemas remain typed in both languages. Fields
not marked `validate:"required"` in the provider schema remain optional, so
generated scalar fields such as `Price` use presence-aware types.
