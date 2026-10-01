# ADR 0002: WebSocket admission uses a first-frame state machine

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`; the first-party WebSocket
  conversation in both providers and both clients

## Context

Browser WebSocket construction cannot attach arbitrary HTTP headers. A bearer
token, API key, resume token or trace context in the URL or subprotocol leaks
into proxy logs and routing metadata. Treating the first payload as an
application message would let a handler run before the framework has
authenticated the declared service binding.

The transport must keep the identity, security, deadline, context, typed-error,
schema and resilience semantics of REST and Connect for server, client and
bidirectional streams in Go and TypeScript. Three runtimes obey one transition
table, and none of them can import a test file.

## Decision

This ADR owns the first-party conversation. A provider-owned wire is declared
separately ([ADR 0010](0010-a-provider-owned-websocket-wire-is-declared-not-inferred.md)).

**Init.** A first-party WebSocket transport negotiates the constant subprotocol
`putnami.service.v1`. The first client frame is a bounded `init` control frame.
It carries the operation identity, consumer identity, deadline and remaining
budget, selected credential-profile values, declared ordinary headers, trace
context, an optional initial request and an optional server-stream resume
cursor. Credential and resume fields are sensitive: runtimes redact the frame
and never put them in diagnostics, spans, metrics, close reasons, URLs or
subprotocols. Deadline and budget values are exact decimal uint64 strings. `0`
disables the total stream deadline, not the handshake, idle, frame or queue
bounds.

**Admission.** The provider parses and bounds the frame, joins the operation to
the upgraded route, resolves its declared credential profiles, authenticates
and authorizes the caller, and only then creates the handler context and sends
`ready`. An application frame before `ready` is a protocol violation.

**Frames.** Data uses `message` frames. JSON values and canonical-base64
protobuf bytes have different payload shapes. Sequence numbers are canonical
decimal uint64 strings, exact in JavaScript. Client and bidirectional streams
end their input with one `half-close`. The provider ends the session with
exactly one typed `result` or `error`. `cancel`, `ping` and `pong` are control
frames. The generated resilience policy or framework defaults bound frame size,
queued message count, idle time and heartbeat cadence.

**Resume.** Automatic resume applies only to a safe server stream whose
transport declares `resume: true`. The server issues and rotates a sensitive
resume token. The next init supplies it with the last fully delivered server
sequence. Client and bidirectional replay would need acknowledgement and
deduplication, so v1 refuses it.

**The transition table is shipped code.** `NextWebSocketStateV1` answers every
(state, frame, direction, stream) triple. `WebSocketConversationV1` adds what
one frame cannot carry: per-direction sequence continuity, encoding agreement
with the selected transport, and resume agreement between init, `ready` and
the transport. The conformance corpus drives the conversation and holds no rule
of its own. A guard test fails if a frame-type switch grows beside it.

| Frame | Rule |
|---|---|
| `init` | The mandatory first client frame. |
| `cancel` | Client-only, from `await-ready` on. Before `init` there is no operation to cancel. |
| `error` | Provider-only, from `await-init` on. A refused admission is a decodable typed error, not an opaque close code. |
| `ping` / `pong` | Both directions, from `await-ready` on, so an asynchronous credential check does not trip the idle budget before `ready`. |
| `result` with a payload | Refused on a server stream, which delivers values only in `message` frames. Allowed on client and bidirectional streams. |
| `ready` with `resumed: true` | Valid only when `init` requested resume and the transport declares `resume: true`. A client cannot refuse it; a provider cannot invent it. |
| `half-close` | Client-only, from `open`, on client and bidirectional streams. |
| `message` | Refused before `ready` and in a direction the stream mode lacks. |

The cancel codes are `canceled` and `deadline_exceeded`, the spelling of
`go/framework/errors`. No reader tolerates another spelling. A drift test
asserts that the enumerations in the published schema equal the sets the parser
accepts for frame types, cancel codes and payload encodings.

**Proto encoding.** `encoding: "proto"` stays in the wire contract, so adding a
codec does not break v1. No runtime decodes it yet, so a WebSocket transport
declaring it is a strict generation error in both emitters
(`clientgen_unsupported_semantic`), never a runtime refusal: a client that
compiles and fails on its first message is the permissive fallback the
contract forbids.

## Consequences

Generated clients use the browser-compatible transport without custom
interceptors or auth code. Providers admit a stream before invoking user code,
and both languages share exact frame and transition fixtures. A provider that
cannot enforce the state machine, or cannot safely replay a resumable stream,
omits that transport or fails first-party contract generation.
