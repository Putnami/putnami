# ADR 0004 — A negotiated WebSocket admits in band, and the security chain runs after the first frame

- **Status**: accepted
- **Scope**: `go.putnami.dev/api` (`go/framework/api`), `go.putnami.dev/http` (`go/framework/http`)

## Context

`api.Plugin` binds `api.ServerStream`, `api.ClientStream` and `api.BidiStream`
endpoints onto `http.ServerPlugin.HandleStream`. A first-party client puts its
identity, credentials, deadline, headers and propagation context in the `init`
frame, not on the upgrade request, because a browser `WebSocket` cannot set
headers
([clientcontract ADR 0002](../../../../../protocols/clientcontract/doc/adr/0002-websocket-admission-uses-a-first-frame-state-machine.md)
owns the frame vocabulary and the transition table). Running the endpoint's
security chain on the bare upgrade would refuse every conforming client.

## Decision

**A stream route declares the one subprotocol it speaks. When the client
offers exactly that token, the framework negotiates it, skips `Before` on the
upgrade, and hands the framed socket to the protocol that owns it.**

- `http.StreamHandler` has `Subprotocol` and
  `Serve func(*Context, *WebSocketConn) error`. `go.putnami.dev/http` owns RFC
  6455 only: masking, continuation reassembly under `DefaultMaxBodySize`,
  control frames, close codes and the message bound. It knows nothing of
  `init`, `ready` or sequences.
- A client that offers a token the route cannot speak gets `400`. A client
  that offers no token gets the raw transport stream, and `Before` runs on the
  upgrade.
- `go.putnami.dev/api` fills `Serve`. It waits for `init`, maps each declared
  credential profile back to its header (`Authorization` for a service or
  forwarded-user token, the declared header for an API key or named header,
  the inverse of what the generated client applied), replays the declared
  headers and bounded propagation values, and runs the endpoint's own `Before`
  chain on that rebuilt request. Only then does it answer `ready`. The
  security chain stays the authority; only the request it reads is rebuilt.
- Between the end of that chain and `ready`, the state is re-read. A client
  cancel, an elapsed deadline or a server drain ends the conversation with a
  typed `error` frame instead of `ready`.
- Every frame both ways passes through one
  `clientcontract.WebSocketConversationV1`. This package holds no phase rule.
  It re-parses each frame it composes with the strict parser before writing,
  so a malformed provider frame is a local fault with the contract's code.

## Consequences

- A first-party stream endpoint's security comes from one chain for SSE and
  WebSocket; a route anonymous over SSE is anonymous over WebSocket.
- `api.ClientStream` takes `TIn` and `TOut`. The handler declares the single
  result with `ClientStreamContext.Result`; completing without one is a
  contract violation.
- The declared `resilience.stream` policy supplies the handshake, idle,
  heartbeat, frame and queue bounds of an admitted socket; the framework floor
  applies only where the contract is silent. An undeclared heartbeat is not
  sent, because it would hide an idle socket.
- The wire refuses JSON `null` anywhere in a frame, payloads included. A
  provider value with an unset nullable field is refused at `Send` with
  `client_contract.parse_error`. Widening this is a change to
  `protocols/clientcontract`.

## Rejected alternatives

- **Keep `Before` on the upgrade and read handshake headers.** A browser
  cannot set them, so Go and TypeScript clients would admit differently.
- **Let `go.putnami.dev/http` speak the frame vocabulary.** The HTTP package
  would own a contract it does not publish.
- **Restate the transition table in the provider.** `NextWebSocketStateV1` is
  published so every runtime shares one authority.
