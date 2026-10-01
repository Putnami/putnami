# ADR 0002: One typed request, one provider RPC, one bundle

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/runner` execution request, provider RPC
  and session bundle

## Context

An execution provider must carry a run without inventing flag semantics,
rerunning impact analysis, or replacing a baseline. A lost acknowledgement, a
reconnect, or a cancel must never create a second attempt or an ambiguous
outcome.

## Decision

### 1. The request freezes what the submitting engine resolved

Parameters carry an explicit type, because their Go type is cache-significant.
Flags keep their explicit-versus-default distinction. The selection carries the
commit its baseline ref named. The expected plan carries identities, edges,
contracts, deadlines and declared resources, so the executing engine refuses a
divergent graph before scheduling. Cache presence is an observation and stays
outside the plan. Placement is never a request member.

The source block carries `source.bound`: the sorted, unique, canonical paths of
bound entries ([ADR 0001](0001-source-content-identity.md)), absent when empty;
an explicit empty list is non-canonical. The executing engine verifies it
against the plan it re-derives.

The execution-input digest covers source (including `source.bound`),
invocation, selection and environment under its own domain string. It excludes
the derived plan, the per-submission control block, the negotiated
capabilities, and the selection diagnostics, and it is never a task cache key.
Repository identity binding is not part of the request.

### 2. The provider RPC is its own contract

It reuses the cache provider's transport shape (JSONL with correlation ids,
bytes through a content-addressed exchange directory) under its own ops and
version, because a runner is not a cache. Envelopes have a twelve-level nesting
budget above the eight-level document budget, because a submit payload wraps a
request whose declared resources already sit at document depth. Acceptance and
completion are distinct states. The client requires an explicit protocol version
and echoed capabilities, and treats every other answer, including "not ready",
as a configuration error, never as a local fallback.

### 3. Lifecycle ops

- `lookup` takes the request's idempotency key and answers the attempt the
  provider accepted under it, its state and the execution-input digest of that
  request, or nothing. A client asks before every submit, and a provider answers
  a repeated submit with the attempt the key already names. One key is one
  attempt; an intentional retry is a new key.
- `cancel` takes an attempt reference and answers the state observed once the
  request was applied: a terminal state already reached, or the non-terminal
  state while the provider terminates the process tree. The answer is an
  observation, never a promise; the client follows the attempt to a terminal
  state within its own bound. A cancel racing a completion converges on the one
  outcome the provider recorded.
- `follow` cursors are strictly increasing. A reconnecting client passes the
  last cursor it persisted and drops any record at or below it. An answer
  carries at most `MaxFollowRecords` records, so a long stream drains in bounded
  steps.

### 4. The session bundle

The bundle returns the executing engine's session directories by digest, gate
session first, with parent links preserved. Importers validate against
`protocols/cli`, refuse documents that do not name their session, and never
overwrite a different record under an existing id. The submitter's durable
attempt record and the executing engine's session provenance live in their
owning contracts (`tooling/cli`, `protocols/cli`).

### 5. The bound-request channel

`PUTNAMI_RUNNER_REQUEST` is the protocol-owned channel a provider uses to hand
the bound request to the pinned entrypoint. The executing engine removes it from
its environment before any task runs, so a nested CLI can never be hijacked into
a second bound execution.

## Consequences

- A provider implements the whole lifecycle (`submit`, `lookup`, `cancel`,
  `follow`) in protocol version 1.
- A provider that is not ready or not negotiated fails the run; the CLI never
  falls back to local execution silently.
