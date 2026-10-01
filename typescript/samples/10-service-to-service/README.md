# Service-to-Service

A TypeScript provider called by a TypeScript consumer and by a Go consumer,
both through **generated, typed clients**.

## Features

- Provider service exposing an items API with `openapi()` and a first-party
  client contract
- Typed `ItemsClient` generated from that contract, resolved from DI
- Cross-language Go client (`clients/go`) generated from the same document
- Proxy endpoint demonstrating consumer-side usage
- One native `items/manage` feature declaration with an automatically derived
  API and generated-client graph

## The documented happy path

Four steps, in this order. Nothing else is consumer-owned.

1. **Declare.** `src/main.ts` declares the client contract on `api({ client: … })`;
   the endpoints under `src/api/` declare their query string, request headers,
   body, response and the errors they may throw.
2. **Generate.** `putnami clientgen --projects @example/10-service-to-service`
   emits `clients/ts` and `clients/go` from the provider's own OpenAPI document,
   each with an ownership manifest. Both trees are committed.
3. **Register the binding.** `registerItemsClient(instance)` in `src/main.ts`
   (TypeScript) or `RegisterItemsClient(module)` (Go). The `clients` block of the
   configuration carries the base URL and the credential values — nothing else.
4. **Call.** Resolve the client from DI and call the typed method:
   `src/api/proxy/get.ts` does it with a top-level import and `.inject()`, with no
   base URL, no interceptor and no deferred import.

## Architecture

```
Provider (this service)    Consumer (proxy endpoint)
  /items                     /proxy
  /items/[id]                  ↓
  /openapi.json           ItemsClient → /items
```

## Routes

| Method | Path | Description |
|--------|------|-------------|
| GET | `/items` | List items, filtered by the declared `search`/`limit` query and the declared `x-catalog-tenant` header (provider) |
| POST | `/items` | Create an item (provider) |
| GET | `/items/[id]` | Get a single item; declares `not_found` with `.mayThrow('NotFound')` (provider) |
| GET | `/items/[id]/watch` | Server stream of one item; requires the `catalog-key` credential and the `catalog.watch` scope (provider) |
| GET | `/items/[id]/history` | Server stream of item revisions, declared WebSocket-first and resumable (provider) |
| GET | `/items/changes` | SSE server stream of the catalog change feed, declared with a cursor continuation; `cursor` optional, `until` names the last revision (provider) |
| GET | `/items/[id]/adjust` | Client stream of stock deltas over WebSocket, answered with one declared total (provider) |
| GET | `/items/[id]/negotiate` | Bidirectional stock conversation over WebSocket (provider) |
| POST | `/blobs/echo` | Echo raw octets unchanged; body and response declared with `Binary({ maxBytes })` (provider) |
| GET | `/blobs/[id]` | Read one stored blob verbatim; requires the `catalog-key` credential and declares `not_found` (provider) |
| GET | `/proxy` | Fetch items via typed client (consumer) |
| GET | `/openapi.json` | OpenAPI specification |
| GET | `/healthz` | Aggregate health check |

## Run

```bash
putnami serve .
```

## Try It

Once the server is running, open a new terminal:

**1. Call the provider directly:**

```bash
curl 'http://localhost:3910/items?search=&limit=10' -H 'x-catalog-tenant: sample-tenant'
```

Expected response — the tenant the provider read back from the declared header,
and the matching items:

```json
{
  "tenant": "sample-tenant",
  "items": [
    { "id": "...", "name": "...", "price": 9.99, "stock": 100 },
    "..."
  ]
}
```

**2. Create an item:**

```bash
curl -X POST http://localhost:3910/items \
  -H 'Content-Type: application/json' \
  -d '{"name":"Bolt","price":1.5,"stock":25}'
```

Expected response — an `item` object with generated `id`, `name`, `price`, and `stock`.

**3. Get a single item:**

```bash
curl http://localhost:3910/items/<id>
```

Expected response — an `item` object with `id`, `name`, `price`, and `stock`.

**4. Call the consumer proxy (typed client with retry interceptor):**

```bash
curl http://localhost:3910/proxy
```

Expected response:

```json
{
  "message": "Fetched items through the generated service binding",
  "tenant": "sample-tenant",
  "items": [
    { "id": "...", "name": "...", "price": 9.99, "stock": 100 },
    "..."
  ]
}
```

The proxy endpoint injects `ItemsClient` and calls it. It writes no base URL and
no interceptor: the binding and the provider's declared policy supply both.

The generated class also embeds the spec hash and the exact operation table,
with the producer project and feature recorded on each operation — a client that
spans several features attributes each one correctly, and an operation whose
module declares no feature stays unattributed. Each generated method passes its
canonical operation id, so `BaseClient` resolves the matching feature trace from
the invoked operation; interceptors and telemetry consume it without URL
guessing.

Inspect the functional and technical path with:

```bash
./putnamiw build --projects @example/10-service-to-service
./putnamiw clientgen --projects @example/10-service-to-service
./putnamiw features inspect items/manage
```

**5. Browse the OpenAPI spec:**

```bash
curl http://localhost:3910/openapi.json
```

Expected response — full OpenAPI specification describing the provider endpoints.

## Test

```bash
putnami test .
```

## What this sample proves

One project is both the provider and the consumer: the API under `src/api/items/`
generates the OpenAPI document, and the client generated from that document is
what `src/api/proxy/get.ts` calls. The test drives the generated typed client
rather than raw `fetch`, so a provider change that was not regenerated fails the
suite instead of failing at runtime.

`clients/go/` is the committed cross-language client for the same contract. It
is generated, not written, and it is committed the way a `.pb.go` is — so
reviewing an API change means reviewing what it did to every consumer. Its
`client.gen.go` carries the operation set the document declares; regenerating it
after a provider change is part of changing the provider.

## The cross-language matrix

Two couples of the cross-language client matrix have this project as their provider:

| Couple | Consumer | Where |
|---|---|---|
| TS→TS | `test/` | the provider started in-process on a real socket |
| Go→TS | `clients/go/` | this provider run as a subprocess |

The Go test starts the provider the way `putnami serve` does (`bun src/serve.ts`,
the `./serve` export), waits for the reserved `putnami.ready` log marker rather
than sleeping, and kills it in its cleanup. That harness is the runner: every
cross-language cell runs against this provider as a real program, started and
reaped by the test that needs it. The command under **Verifying** runs both
couples; there is no separate script to learn.

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

The gRPC plugin serves Connect protobuf before JSON, and the contract carries
one Connect transport per encoding, so a generated client dispatches whichever
comes first. `GET /quotes/[id]` keeps the plugin order. `GET /quotes/[id]/snapshot`
is the same quote with one more line in its declaration —
`connectEncodings: ['json', 'proto']` — and every generated client, Go or
TypeScript, calls it over Connect JSON without a consumer branch.
`clients/go/connect_json_foreign_provider_test.go` reads that off the wire of
this provider run as a subprocess.

`test/matrix-index.test.ts` is what makes the index worth reading: it resolves
every named test in the file it names, refuses an `n/a` without a rule and a
`missing` without a reason, and requires an entry for every combination.

### The declared request policy

`POST /items` is the operation that declares one: `X-Idempotency-Key` as the
stable request identity, three attempts, 503 as the only status worth
repeating, 250 ms per attempt inside a 2 s budget, and a circuit that opens
after two consecutive failed calls and stays open for a minute. A consumer
writes none of that — `test/policy.test.ts` asserts the generated client applies
exactly what the contract declares.

### The declared raw octet payload

`POST /blobs/echo` and `GET /blobs/[id]` declare their body and their response
with `Binary({ maxBytes })`: the HTTP body *is* octets, under a named media
type, bounded. The bound is not documentation — the provider answers 413 before
it reads a body that announces more, and the generated clients read their
source one octet past it and refuse there, so an oversized file is never
drained.

`GET /blobs/[id]` shows that a binary success costs the endpoint nothing else:
the path parameter stays typed, the `catalog-key` credential is still injected
by the binding, and `not_found` still arrives as the generated typed error.
Both stored blobs are chosen to break a text pipeline — one is not valid UTF-8
and not valid JSON, the other is empty, because zero octets are a payload and
not a missing body.

### The declared server stream

`GET /items/[id]/watch` returns `Stream(itemSchema)`, so the generated clients
expose a typed stream handle rather than a one-shot call. `follow=true` keeps it
open until the consumer leaves, which is what makes cancellation provable.

Admission is decided before the response head. `src/workload-identity.ts`
resolves the caller from the `X-Catalog-Key` header once, before every route, and
the endpoint declares only what it requires (`.secure({ scopes })`). A missing
key is 401 and a key without `catalog.watch` is 403 — both an HTTP status, never
a 200 followed by a terminal event. That split is also what keeps the endpoint
representable in the first-party client contract, which refuses a custom
verifier or guard.

### The cursor-continued change feed

`GET /items/changes` is the SSE server stream that survives an instance change.
Every change carries the provider's own opaque position after it (`cursor`)
beside the revision a reader compares, and the route declares

```ts
sseContinuation: { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: 'cursor' } },
resilience: { stream: { reconnect: true, ... } },
```

the output field the position travels in, the query parameter that receives it
on a continuation, and the consumer half of the agreement. A generated client
whose connection breaks reopens the same operation after the last change **its
caller received** — not after the ones it had only decoded and queued — with a
credential resolved at that moment, on whichever instance answers. No consumer
writes a reconnect loop and no consumer reads the cursor. The route also speaks
the negotiated SSE wire (`X-Putnami-Stream-Wire: putnami.sse.v1`, clientcontract
ADR 0013), so a consumer reads an explicit `complete` and never mistakes a cut
connection for the end of the feed.

The feed is the catalog's durable change log (`src/change-log.ts`): one revision
per catalog item at start, one more per `POST /items`. `cursor` is optional and
`until` names the revision after which the feed completes; a revision that does
not exist yet keeps the stream open until a creation appends it, which is what
lets a test observe a continuation. A position the log never issued is the
declared `not_found`: the feed never restarts from the beginning on a cursor it
does not recognize, and the runtime never reopens after a typed terminal.

The two consumers prove it against two provider instances behind a routing
front, the one double: `test/` stops one in-process instance with `app.stop()`
after three creations reached the consumer and continues on the second, which
shares the process-wide log; `clients/go/` drains one provider **process** with
`SIGTERM` and continues on a second process that shares no memory with it — the
second places the continuation by the cursor alone, on the revisions it seeded
from the same catalog. A stopping instance drains its negotiated streams at once
on the HTTP plugin's drain signal, whether the route was registered by hand or
loaded from the scanned `src/api` folder.

### The declared WebSocket conversations

`GET /items/[id]/adjust` declares `.body(Stream(stockDeltaSchema))` with a unary
`.returns(stockTotalSchema)`: a client stream. The consumer sends deltas, calls
`end()`, and reads the single declared result as the last message before
completion. `GET /items/[id]/negotiate` streams both directions; the value its
handler returns is the terminal one.

Neither declares an SSE alternative — SSE carries one direction — so the
published transport list holds WebSocket alone and the generated clients open it
with no branch in the consuming application.

`GET /items/[id]/history` is the third stream: the same **server**-stream shape
as `watch`, declared the other way round. Its `.client({ ... })` block names
`transports: ['websocket', 'sse']` and `resume: true`, so the published order
puts the socket first and states that the feed can be continued after a break.
The declaration is the only difference — the handler, the generated method and
the consuming application are what they would be on SSE. The handler reads
`ctx.resumeFrom` and numbers its revisions from there, which is what makes "no
gap, no duplicate" something a consumer can check for itself.

Both declare `resilience.stream` with the same numbers as the Go sample, so the
two providers are comparable cell by cell: a 2 s handshake budget, a 5 s idle
budget, a 250 ms heartbeat, a 16 KiB reassembled-message bound and a queue four
messages deep.

Two dimensions are recorded there as unavailable rather than skipped quietly: a
TypeScript provider cannot declare an integer width or a nullable property yet,
so no integer beyond JS precision and no explicit `null` crosses the network
from this provider. Both are proven on the cells whose provider is Go.

## Verifying

```bash
putnami lint,test,build,validate --projects @example/10-service-to-service,go.putnami.dev/examples/ts-items-client
```

The sample is tagged `e2e`, so `--impacted` skips it: name the projects.

Each run publishes the checks this sample's feature declares
(`putnami.features.json`) through `specTest`, and the merged report lands in the
project's `putnami-feature-verification` artifact. Verification runs in `report`
mode: the sample is tagged `e2e`, which an enforcing gate blocks on.

Contract: [`@putnami/client`](../../framework/client/README.md) —
[service-clients specification](../../framework/client/specs/service-clients.json).
