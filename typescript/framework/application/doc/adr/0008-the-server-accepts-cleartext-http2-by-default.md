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
4. **A duplex stream runs under the endpoint's declared stream budgets**, the
   ones its WebSocket transport enforces, and a caller that resets the stream
   is read as a cancellation, never as the end of its messages.
5. **Deployment is a separate decision.** The TypeScript extension still states
   `runtime.protocols.http2: false` for every workload, so the platform keeps
   speaking HTTP/1.1 to the container.

## Consequences

- Any TypeScript server now answers the HTTP/2 preface on its listening port.
  That is a wider network surface than before, bounded by the same middleware,
  admission and request limits as HTTP/1.1.
- The protocol is inferred, not stated. A Bun release that states it per
  request would replace the header rule in `http-version.ts`.
- A cancellation is confirmed one macrotask after the request body ends,
  because Bun aborts the request signal only then.
