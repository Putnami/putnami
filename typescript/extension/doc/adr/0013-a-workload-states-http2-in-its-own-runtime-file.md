# ADR 0013 — A workload states HTTP/2 in its own runtime file

- **Status**: proposed, for the owner to review on its proposal
- **Scope**: `@putnami/typescript` (`cmd/putnami-ts/build_infra.go`)

## Context

The infra phase and the `deployment` package step resolve a workload's runtime
block from its authored `infra/runtime.json` or from the framework defaults,
then pass it to the extension's compatibility hook. Until now that hook set
`protocols.http2` to `false` whatever the block said, because Bun did not serve
cleartext HTTP/2 and the deploy target may default a service to HTTP/2.

Since `@putnami/application` ADR 0008, a TypeScript server accepts cleartext
HTTP/2 with prior knowledge, and a Connect client or bidirectional stream works
only over HTTP/2. That ADR left deployment as a separate decision.

Bun does not carry WebSockets over HTTP/2. If the deploy target's ingress
sends every request to the container over HTTP/2 once `http2` is on, a
workload can lose its WebSocket transport. That has not been tested on a real
deployment yet, so the default cannot move.

## Decision

1. **The default stays off.** A runtime block that states no
   `protocols.http2`, the synthesized defaults included, gets
   `protocols.http2: false`, stated explicitly so the deploy target never
   applies its own default.
2. **An authored value wins.** When the workload's own `infra/runtime.json`
   states `protocols.http2`, `true` or `false`, the hook keeps it. The infra
   manifest and the deployment declaration carry that value.
3. **The default moves only on a recorded deployment test.** Deploying a
   workload with HTTP/2 on, and checking a unary RPC, a bidirectional Connect
   stream and a WebSocket stream through the ingress, decides whether the
   default can turn on (`doc/02-build.md`, "Turning HTTP/2 on").

## Consequences

- A project that opts in gets Connect client and bidirectional streams over
  HTTP/2, and takes the risk of losing its WebSocket transport on an ingress
  that speaks HTTP/2 for every request. The docs say so where the opt-in is
  described.
- The hook is no longer a constraint the authored file cannot lift. The SDK
  still applies it to both sources; the TypeScript hook tells them apart only by
  whether the block states the value.
- The opt-in is per workload. Nothing turns HTTP/2 on for every TypeScript
  workload until the deployment test is recorded.
