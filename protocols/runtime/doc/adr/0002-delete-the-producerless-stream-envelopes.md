# ADR 0002: Delete the producer-less stream envelopes instead of freezing them

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/runtime` (`protocols/runtime`)

## Context

A `--output=jsonl` line has two layers: the outer session-stream record (which
task started, which event it produced, how it and the session ended) and the
inner subprocess event (one log line, metric, diagnostic, or result). The outer
record is `SessionStreamRecord` in `protocols/cli`. A shape declared in two
packages drifts by construction.

## Decision

**This package owns the inner subprocess event and nothing else.** The inner
event is what a `task:event` record carries in its `event` member. Session
framing (record types, verdicts, exit codes) belongs to `protocols/cli`, and
this package's documentation links there instead of restating it.

**A protocol with no producer and no consumer is deleted, not frozen.** This
package declares no outer envelope, schema, or fixture.
`TestOuterSessionStreamIsNotOwnedHere` guards the absence.

## Rejected alternatives

- **Freeze an unused envelope as a compatibility surface.** It still costs
  review, still makes "which envelope is current?" ambiguous, and invites the
  next author to emit it.
- **Declare the outer record here as well.** "What does a session record look
  like" would depend on which package you asked, and a bump would be agreed
  twice.
- **Move the session record into this package.** Session framing is
  orchestration; a subprocess does not know it is part of a session.

## Consequences

- Reintroducing an outer envelope here reverses this decision and needs a
  producer and a consumer that exist before the shape does.
