# ADR 0013 — An SSE stream declares how it continues, and a negotiated stream ends explicitly

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`, both provider projections
  (`go.putnami.dev/api`, `go.putnami.dev/http`, `go.putnami.dev/openapi`,
  `@putnami/application`), both readers, both emitters, both client runtimes

## Context

Some server streams must survive a broken connection: a log tail whose frames
carry a provider-owned cursor the provider accepts on any instance, and a live
selector that is reopened after a disconnect and may lose lines from the gap.
WebSocket resume cannot serve them. Its grants are single-use and live in the
memory of the issuing instance, on purpose, since a restarted provider cannot
prove a position is gap-free. These providers scale to zero behind a request
ceiling, so a continuation must land on another instance.

Plain SSE cannot tell "the stream is over" from "the connection dropped": an
end of body at an event boundary looks like success, and no provider writes a
successful terminal. A reconnect loop built on that reading either reopens a
finished stream or drops a live one.

## Decision

### 1. The provider declares the continuation on its SSE transport

`sse.continuation` is the provider half. The consumer half is
`resilience.stream.reconnect`, resolved operation first, then document default;
the default reaches server streams only.

```json
{"protocol":"sse","path":"/runs/{run}/logs/tail","encoding":"json",
 "sse":{"continuation":{"mode":"cursor","cursor":{"outputField":"cursor","queryParameter":"cursor"}}}}

{"protocol":"sse","path":"/logs/tail","encoding":"json",
 "sse":{"continuation":{"mode":"best-effort"}}}
```

An SSE transport without a continuation omits `sse`. Both readers refuse
everything else:

| Rule | Code |
|---|---|
| `sse` appears only on an `sse` transport | `client_contract.invalid_transport` |
| `sse` carries `continuation`; `continuation` carries `mode` | `client_contract.required` |
| `mode` is `cursor` or `best-effort` | `client_contract.invalid_enum` |
| `cursor` mode carries `cursor` with non-blank `outputField` and `queryParameter` | `client_contract.required` |
| `best-effort` mode carries no `cursor` | `client_contract.invalid_resilience` |
| The operation is a server stream declared `safe` | `client_contract.invalid_resilience` |
| `outputField` is a required plain-string property of the output message | `client_contract.invalid_resilience` |
| `queryParameter` is a declared plain-string query parameter | `client_contract.invalid_resilience` |
| An effective reconnect needs a resumable first-party WebSocket transport or a continuable SSE transport | `client_contract.invalid_resilience` |
| No credential or idempotency header is named `X-Putnami-Stream-Wire` | `client_contract.invalid_credential`, `client_contract.invalid_idempotency` |
| Any other member | `client_contract.unknown_field` |

A plain string is `type: string` with no `format`, `enum` or `oneOf`, not
nullable and not `x-putnami-json`, after one local component reference: a
position is opaque text copied verbatim. The reference checks need the whole
document, so readers run `ValidateSSEContinuationReferences`. The wire
vocabulary lives in [`sse.go`](../../sse.go). The contract stays at version 1;
an older strict reader refuses the unknown `sse` member and fails closed.

A continuation without an effective reconnect is valid: the stream still uses
the negotiated wire and ends explicitly, and a break ends it with an error.

### 2. Two modes

**Cursor.** Each output message carries the provider's position after it in
`outputField`. A reopening sends the position of the last delivered message in
`queryParameter`. The provider continues exclusively after it, on any
instance. The runtime never parses, orders, increments, compares or synthesizes
the value. This mapping is the only position authority: a provider writes no
`id:` on the negotiated wire, and a runtime neither sends `Last-Event-ID` nor
reads `id:` or `retry:`.

**Best-effort.** A reopening sends the original query unchanged. Messages from
the gap may be missing and some may repeat. The contract and docs say so. It is
never called lossless, and a runtime never substitutes it for a failed cursor
continuation.

**A cursor is a position, not a credential.** Every opening and continuation
runs the endpoint's full authentication and authorization chain. The provider
binds the cursor to its operation, workspace and selector, checks its
authenticity and retention, continues exclusively after it, and answers a
stale, forged or out-of-scope cursor with a typed declared error, never by
restarting. The framework keeps no grant store for SSE and claims neither
exactly-once processing nor recovery across a consumer restart; those belong
to the consumer.

### 3. The position advances on delivery

The delivered position is the cursor of the last complete, validated message
handed to the consumer. Parsing, decoding or queueing is not delivery.
Heartbeats and terminals never advance it. Go uses a bounded delivery bridge
with an observable handoff, since a buffered channel send proves nothing.
TypeScript counts the observer delivery, not the pre-subscription queue. On a
break the runtime keeps order and settles queued, undelivered values before it
reopens: it delivers them and advances, or discards them and asks again from
the last delivered position, never both. Before any delivery, a reopening sends
the original query, including a caller-supplied initial position.
`SSEReopenQuery` computes the reopened query.

### 4. A negotiated stream ends explicitly

**Negotiation.** The runtime sends `X-Putnami-Stream-Wire: putnami.sse.v1` on
every opening and reopening of an operation whose SSE transport declares a
continuation, and on no other. The provider echoes the same header and value
on the admitted 2xx response head, before the first body byte, only when the
route declares a continuation, the request carries the marker, and the
operation is admitted. A refused admission is an ordinary HTTP status with no
acknowledgment. `NegotiatesSSEWire` joins the field lines as RFC 9110 does,
trims optional whitespace and compares to `putnami.sse.v1` byte for byte; a
list, a repeated line, another case or another version does not negotiate. The
header is reserved: no credential profile or idempotency key may use it.

**Closed vocabulary.**

| Event block | Meaning |
|---|---|
| no `event` field, or `event: message` | an application message of the declared output type |
| `event: error` | the typed terminal error, envelope `{status, code, error, message, details?}` |
| `event: complete` with data exactly `{}` | the successful terminal; it delivers no value |
| a comment (`: heartbeat`) | liveness only |
| anything else, including `complete` with other or no data | a terminal contract error |

The successful terminal is exactly `event: complete\ndata: {}\n\n` (26 bytes),
pinned by `SSECompleteFrame` and
[`fixtures/sse/wire.json`](../../fixtures/sse/wire.json).

**Provider obligations.** A provider writes `complete` only when the handler
returned without error and the consumer did not cancel; after a handler failure
it writes the typed error, after a cancellation nothing. Either terminal uses
the queue slot reserved for the terminal.

**A draining provider never reports success.** When its server begins draining
(a graceful stop, a scale to zero, an instance replacement), the provider
cancels the handler of every negotiated stream and ends the response with no
terminal, even if the handler returns without error. The consumer reads an
interruption and continues elsewhere. A legacy stream keeps its framing. The
drain signal belongs to the HTTP server plugin serving the stream (Go
`ServerPlugin.Stop`; the TypeScript HTTP plugin's signal, aborted before it
waits on in-flight requests and stamped on every request), never to a route: a
scanned route folder is served by every application instance that scans it.

**Reader classification.**

- A terminal ends the connection and the session. Bytes after it are never
  delivered, and it never leads to a reopening.
- An end of body or socket loss before a terminal is an interruption. An event
  truncated by the break is discarded. An interruption is the only outcome a
  continuation may follow.
- A complete malformed event, an invalid cursor field, an oversize frame or a
  schema violation is a terminal contract error.

**Mixed versions.**

| Consumer | Provider | Result |
|---|---|---|
| old | new | No marker, so no acknowledgment and no `complete`: legacy framing, byte for byte. |
| new | old | No acknowledgment: admission fails with a contract error before any message, with no fallback, retry or continuation. |
| new | new | The negotiated wire. |
| any | operation without a continuation | Legacy framing. |

A reopening that lands on an older instance during a rolling deploy follows the
"new consumer, old provider" row. The terminal is never sent unasked because
legacy readers treat every event type other than `error` as data.
[`fixtures/sse/scenes.json`](../../fixtures/sse/scenes.json) pins every row as
reader outcomes.

### 5. One session across every connection

This extends [ADR 0005](0005-stream-sessions-have-five-phases-and-four-budgets.md):

- A reopened connection belongs to the same `StreamSession`, which stays
  `active`. A reopening is not a second admission: one public stream, one
  terminal, one breaker verdict, one call measurement. The breaker stays silent
  after admission, including for a failed reopening.
- A reopening happens only after an interruption of the negotiated wire, while
  the session is live and the effective reconnect is on. Never after a typed
  error or `complete`, a contract error, a caller cancellation, an expired
  session or idle budget, or the provider's typed refusal of a cursor.
- Continuations share the cap of five per session that WebSocket resume uses.
  The sixth break ends the session with the break as its error.
- A reopening is one handshake. `resilience.retry` never applies to it, and a
  failed reopening ends the session with that failure.
- Credentials are resolved again before every reopening. A 401 at a reopening's
  admission uses the one-shot credential invalidation and refresh. No second
  retry policy, no refresh on 403, no reopening after an in-stream typed
  authorization error.
- Each connection gets its own handshake budget. The session duration and the
  idle budget run across connections. Frame and queue bounds are unchanged.
- Reopenings stay on the admitted SSE transport: no fallback to another
  transport, no downgrade from cursor to best-effort.
- Application stop, `Close` or unsubscribe, and context cancellation close
  every active or reconnecting connection, reader and timer.

A provider may declare both WebSocket resume and an SSE continuation on one
operation. Each connection continues by the mechanism of the transport it was
admitted on.

### 6. Runtime capability

A generated target whose contract declares a continuation requires the
`sse-continuation` capability (`RuntimeCapabilitySSEContinuation`). A manifest
is validated against the runtime of its own language
(`RuntimeCapabilitiesImplementedBy`), so one runtime can ship a capability
before the other. `ImplementedRuntimeCapabilities`, which the manifest schema
enumerates, lists what every runtime implements, including
`sse-continuation`. The Go generator validates the manifest it writes, and
workspace inspection validates TypeScript manifests with the same code. A
continuation is never generated into a client that would ignore it.

## Rejected alternatives

- **`Last-Event-ID`.** Two position authorities for one operation, and a header
  cannot name which output field carries the value.
- **`complete` on every SSE stream.** Legacy readers deliver an unknown event
  type as data: a phantom message or a schema failure.
- **Completion inferred from the end of body.** A truncated and a finished
  stream look identical.
- **WebSocket grants stretched to SSE.** A grant lives on one instance. A
  signed stateless grant would be a credential that opens a stream on its own;
  a cursor is not, because every continuation runs the full security chain.
- **A media-type parameter.** Intermediaries normalize `Content-Type` and lose
  parameters silently.
- **The continuation on the operation.** It is a property of one wire; a
  reopening never switches wires.
- **One shared capability list.** A finished runtime would stay unusable until
  the other caught up.

## Consequences

- A provider that declares a continuation writes the acknowledgment and the
  terminal on the negotiated wire. Old consumers see no change.
- A consumer generated against a continuation needs a runtime with the
  capability. Upgrade providers first, then consumers. A new consumer that
  reaches an old provider fails explicitly instead of reading a truncated
  stream as complete.
