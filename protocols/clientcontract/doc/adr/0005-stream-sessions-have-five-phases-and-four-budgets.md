# ADR 0005 — Stream sessions have five phases and four budgets

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/clientcontract`; the Go and TypeScript
  stream runtimes

## Context

Four transports in two languages carry streams. Without one shared lifecycle,
each restates when a stream is admitted, what the breaker records, which budget
bounds the handshake and when a replay is legal, and the copies diverge. Three
questions need one answer everywhere: is a provider that accepts a stream and
then delivers nothing admitted; does a mid-stream break count against the
provider's availability; and does a `timeoutMs` written for a unary call bound
a subscription.

## Decision

Each runtime has one `StreamSession` type that owns these rules. Every stream
transport drives it instead of restating them.

**Phases.** A session passes through at most five phases, in order and without
return: `connecting` → `admitted` → `active` → `terminal` → `closed`. A session
always reaches `closed`.

**Admission** is the provider's protocol acceptance, the same act in every
transport: for SSE, a 2xx response with the declared `Content-Type`; for
WebSocket, the `ready` frame; for Connect, accepted response headers; for unary
REST, the response. Admission is not the first application message. Nothing
reaches the caller before admission.

**Replay.** An opening may be replayed only before admission, only when the
declared idempotency is `safe` or `idempotent`, and only for a failure class
declared retryable. No runtime replays a stream opening today. After admission
the only way back is a continuation the provider declares (WebSocket resume,
[SSE continuation](0013-an-sse-stream-declares-how-it-continues-and-a-negotiated-stream-ends-explicitly.md)).

**Credentials.** Credentials are resolved before the handshake. When an
asynchronous acquisition returns a value that has expired by the send point,
the runtime re-resolves before emitting the first frame or request; the
credential manager, which owns freshness, returns the value it holds or
acquires a new one. A failed re-resolution ends the session before admission.
A 401 or 403 invalidates the credential exactly once and replays nothing.

**Circuit breaker, by phase.** The session writes the breaker at most once:

- A request the open circuit rejected records nothing.
- A failure before dispatch (local configuration, credential acquisition) or a
  caller cancellation or deadline releases the probe without a verdict.
- A provider answer the declared circuit policy does not count as a failure
  records a success.
- Every other pre-admission failure records a failure.
- Admission records the single success.
- After admission the session writes nothing: a mid-stream break is a fact
  about that session, not about the provider's availability.

A transport never writes the breaker itself.

**Four budgets, all distinct from the declared duration.**

- `handshake`: from connection open to admission. Declared by
  `resilience.stream.handshakeTimeoutMs` (positive integer); absent, it
  defaults to `attemptTimeoutMs`.
- `idle`: the maximum interval between two provider frames after admission.
- `frame`: the maximum size of one frame or event.
- `queue`: the depth of the unconsumed message queue. Exceeding it is a
  terminal backpressure error, never a silent drop.

`resilience.timeoutMs` bounds the whole session only when declared. Absent or
zero, the session is unbounded in time and the other budgets still apply.

**Termination.** Exactly one terminal among `complete`, `error` and `cancel`
reaches the caller. The message channel closes exactly once. Closing is
idempotent; closing before a terminal records `cancel` first. The call
measurement is emitted exactly once, on entry to `closed`.

## Rejected alternatives

- **Phase rules in each transport.** The state machine would exist five times,
  with no single place to decide whether a fallback is still legal.
- **The first message as admission.** It conflates accepting the operation with
  having something to say. An accepted, silent stream is admitted.
- **The declared operation duration as the stream bound.** A value authored for
  a unary call would cap a subscription, and "open fast, stay open" could not be
  declared.
- **Breaker writes after admission.** A healthy provider with long-lived streams
  would look unavailable to unary callers of the same service.
- **Credential expiry tracked in the session.** It duplicates what the
  credential manager owns and gives two clocks a chance to disagree.

## Consequences

- A provider that accepts a stream and then delivers nothing is recorded as
  admitted.
- A long-lived stream that ends badly does not open the circuit for unary
  callers of the same service.
- A pre-admission failure must say whether the attempt reached the provider;
  the session marks the dispatch instant.
- A new stream transport drives the session and adds no phase rule. In Go,
  `TestStreamTransportsDoNotWriteToTheBreakerThemselves` fails if a transport
  writes the breaker itself.
