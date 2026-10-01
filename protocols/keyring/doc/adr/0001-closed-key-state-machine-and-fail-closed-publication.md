# ADR 0001: A closed key-state machine with fail-closed publication

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/keyring` (`protocols/keyring`)

## Context

A signing keyring answers two questions on every rotation: which key may sign,
and which keys a verifier may still trust. Implicit lifecycles (an "active"
flag, an expiry timestamp, a JWKS built from whatever is in storage) fail by
publishing too much: a compromised key that is merely "not active" stays in the
JWKS, and every verifier keeps accepting it.

## Decision

Key lifecycle is a closed state machine, and publication derives from state:

1. Four states: `active` (signs and verifies), `retiring` (verifies during
   rollover, signs nothing), `revoked` (withdrawn, typically compromised),
   `expired` (validity window elapsed).
2. Legal edges: `active → retiring | expired | revoked`,
   `retiring → expired | revoked`, `expired → revoked`. `revoked` is the only
   terminal state and is reachable from every other state. A same-state edge is
   rejected. `Transition` / `CanTransition` enforce the table;
   `KeyState.AllowedTransitions()` returns legal targets sorted.
3. `PublicJWKS` publishes only `active` and `retiring` keys. It drops `revoked`
   and `expired` keys, and symmetric `oct` keys, which have no public form.
4. Private material never reaches the public projection. The public `JWK` type
   has no `d, p, q, dp, dq, qi, k` fields, and `ParseJWKS` strict-parses, so a
   document carrying any of them is rejected as an unknown field.

## Rejected alternatives

- **An `active` flag plus an expiry.** It cannot tell natural expiry from
  compromise, and only compromise must never be published again.
- **Each consumer decides what to publish.** Publication is the security
  boundary; a consumer rule ends up as "publish everything in storage".
- **`revoked → active`.** A key ever published as revoked is distrusted
  everywhere; recovery is a new key.
- **`expired → active`.** It makes "expired" advisory.
- **Enforce the invariant in tests only.** A type that cannot carry private
  material and a parser that refuses it are enforcement; a test is a reminder.

## Consequences

- Adding a state or edge is a wire change: the enum, the table, the schemas and
  the fixtures move together.
- A "suspended, may return" state must be modeled outside the keyring.
- Adding a field to `JWK` is a security review, because that is how private
  material would leak.
- Rotation policy (when to retire, how long to overlap) stays a consumer
  concern; the table says which moves are legal, never when.
