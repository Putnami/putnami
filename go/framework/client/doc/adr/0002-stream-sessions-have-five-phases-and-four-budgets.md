# ADR 0002 — A stream session has five phases and four budgets

- **Status**: accepted
- **Scope**: `go.putnami.dev/client` (`go/framework/client`)

## Context

[clientcontract ADR 0005](../../../../../protocols/clientcontract/doc/adr/0005-stream-sessions-have-five-phases-and-four-budgets.md)
owns the language-neutral session contract: the five phases, admission per
transport, the four budgets, the credential re-check, and the single terminal
and measurement. This record states how the Go runtime implements it and the
breaker details both runtimes share.

## Decision

**One `StreamSession` owns the lifecycle.** SSE, Connect, first-party
WebSocket and provider-owned WebSocket drive it; none restates a phase rule or
writes to the breaker itself (`TestStreamTransportsDoNotWriteToTheBreakerThemselves`).

**No automatic opening retry.** A stream opens once per declared transport.
Reopening happens only through a declared fallback or continuation
([ADR 0006](0006-a-declared-fallback-happens-before-admission-and-a-continuation-never-repeats-a-value.md)).

**The breaker is written at most once per session, never after admission.**

- A circuit rejection records nothing and releases nothing.
- A failure before `StreamSession.Dispatch` (local configuration, credential
  acquisition) or a caller cancellation releases the probe without a verdict:
  nothing about the provider was observed.
- A provider answer that the declared circuit policy does not count as a
  failure records a success.
- Any other pre-admission failure records a failure; admission records the one
  success.

**Budgets.** `StreamBudgets` carries `Handshake`, `Idle`, `MaxFrameBytes`,
`MaxBufferedMessages` and `Session`. The handshake budget comes from
`resilience.stream.handshakeTimeoutMs`, defaulting to `attemptTimeoutMs`.
`Session` is the declared `resilience.timeoutMs`; zero leaves the session
unbounded in time.

## Rejected alternatives

- **Phase rules in each transport.** The state machine would exist once per
  transport and language, and the copies diverge.
- **The first message as admission.** An accepted, silent stream is admitted.
- **Breaker writes after admission.** A mid-stream break is a session fact;
  counting it makes a provider with long-lived streams look unavailable.
- **Credential expiry tracked in the session.** The credential manager owns
  freshness; two clocks could disagree.
