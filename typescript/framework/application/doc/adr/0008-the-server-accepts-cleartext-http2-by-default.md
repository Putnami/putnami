# ADR 0008 — The server accepts cleartext HTTP/2 by default

- **Status**: proposed, for the owner to review on its proposal
- **Scope**: `@putnami/application` (`http/http.plugin.ts`, `http/http-version.ts`,
  `grpc/grpc-handlers.ts`)

## Context

A Connect client or bidirectional stream needs a request body the server reads
while it answers. HTTP/1.1 frames a request body as one unit, so over it the
Connect route answered those RPCs `unimplemented` and the generated clients
carried them over WebSocket.

Bun 1.4.5 serves HTTP/2 in cleartext when `Bun.serve` is given `http2: true`
and the client opens with the HTTP/2 preface (h2c, prior knowledge). HTTP/1.1
requests and WebSocket upgrades keep working on the same port, and an HTTP/2
request body reaches the handler as it arrives.

`Bun.serve` states the protocol of neither kind of request, so the server can
tell them apart only from their headers.

## Decision

1. **Every HTTP server accepts cleartext HTTP/2 by default.** `http()` passes
   `http2: true` to `Bun.serve`; `http({ http2: false })` serves HTTP/1.1 only.
   A caller that does not open with the HTTP/2 preface sees no change.
2. **A request is read as HTTP/2 only when nothing says HTTP/1.1.** A request
   with no body, or one that carries `Content-Length`, `Transfer-Encoding` or a
   connection-specific header HTTP/2 forbids, is HTTP/1.1. An HTTP/2 caller that
   states a `Content-Length` is therefore refused a duplex stream.
3. **Only a Connect streaming request over HTTP/2 opens a duplex stream.** Over
   HTTP/1.1, or with another media type, a client or bidirectional RPC keeps
   answering `unimplemented`.
4. **A duplex stream runs under the endpoint's declared stream budgets**, by
   the same resolution as a first-party WebSocket stream; a route without a
   contract gets the framework floors. A caller that resets the stream is read
   as a cancellation as far as Bun lets the server tell: see Consequences.
5. **Deployment is a separate decision.** The TypeScript extension states
   `runtime.protocols.http2: false` by default, so the platform keeps speaking
   HTTP/1.1 to the container; a workload's own `infra/runtime.json` may state
   `true` (`@putnami/typescript` ADR 0013).

## Consequences

- Any TypeScript server now answers the HTTP/2 preface on its listening port.
  That is a wider network surface than before, bounded by the same middleware,
  admission and request limits as HTTP/1.1.
- The protocol is inferred, not stated. A Bun release that states it per
  request would replace the header rule in `http-version.ts`.
- Bun 1.4.5 ends the body of a reset HTTP/2 request exactly as it ends a
  completed one, and aborts the request signal in a later event-loop turn,
  observed no later than the turn after the one that ended the body. The
  server therefore reads the end of a body as the caller's end only after
  three event-loop turns, counted with `setImmediate`, gave the signal its
  chance to abort: the two turns observed, and one of margin. The wait counts
  turns, not time: an earlier 10 ms grace let a reset read as an end whenever
  the turn that handled a burst of resets ran longer than 10 ms under load.
  The guarantee rests on that observed Bun scheduling, which a Bun upgrade
  must keep; the duplex stream tests check it, margin included, and the
  limitation is worth reporting to Bun.
- A stream stopped by its idle budget or its deadline ends `ctx.messages()`
  normally with `ctx.signal` aborted, as on WebSocket: a handler that commits
  must check `ctx.signal` after its loop.
