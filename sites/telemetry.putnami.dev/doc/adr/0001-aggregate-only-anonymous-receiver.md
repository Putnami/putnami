# ADR 0001 — The receiver is anonymous on ingest and aggregate-only on read

- **Status**: accepted
- **Scope**: `telemetry.putnami.dev` (`sites/telemetry.putnami.dev`)

## Context

Putnami needs to know which CLI commands people run, and its documentation
tells users that nothing identifying leaves their machine. Both hold only if the
*service* cannot answer an identifying question. A receiver that stores raw
events and promises not to look relies on policy, which a reader cannot check
and which does not survive an operator mistake.

Ingest is reached by a CLI, and a CLI must never get slower, fail, or leak a
stack trace because a telemetry endpoint had a bad day.

## Decision

Ingest is anonymous and unauthenticatable. `/v1/logs` accepts no session, API
key or cookie, and the identity chain is route-scoped, so a bearer token
presented there is ignored and cannot drive a key fetch.

The receiver reduces every record to a compiled field allowlist and closed
vocabularies before storing anything, and drops the caller's network address
after applying request limits. An unknown command name is dropped.

An accepted ingest is not an oracle. A `202` is byte-identical whether the
payload had content problems or storage was unavailable, and no answer is a
redirect; the production sender refuses to follow one.

Read access is aggregate-only by shape. One authenticated route returns counts
for fixed windows, counts distinct rotating identifiers without returning one,
and withholds any group below the distinct-contributor threshold. No export,
raw-event access or per-device query is implemented. A dimension that hits its
storage ceiling answers "unavailable", never a partial count.

The rotating-identifier membership projection exists only to make suppression
correct across a window. It holds no raw events, is never returned by the read
API, and expires with the aggregates.

Retention is scheduled in the database, so it runs while the receiver is scaled
to zero.

The served surface is declared and committed: the route artifact is
byte-identical to what the running server describes and independent of
deploy-time configuration.

## Rejected alternatives

- **Authenticate ingest to reduce spam.** Every CLI would carry a credential,
  the identifying material this design avoids, and an outage would look like an
  auth failure.
- **Store raw events and restrict access by policy.** Only an unimplementable
  identifying query survives an operator mistake.
- **Exact counts for every group.** Small groups identify people.
- **Partial counts at a storage ceiling.** A plausible wrong number is
  indistinguishable from a right one downstream.
- **Retention in the application.** A scale-to-zero workload would keep data as
  long as it happened to be idle.
- **Let the CLI wait for or react to the answer.** Telemetry health would
  become a reliability risk for every command.

## Consequences

- Per-device or per-workspace questions are unanswerable by construction; the
  answer is to change the aggregate shape under review, not to add a raw read.
- Rare command combinations may never appear, staying below the threshold.
- Abuse is handled only by volume limits and allowlisted vocabularies.
- The public documentation page and this workload change together: a change to
  the collected fields, retention windows or served surface regenerates the
  committed route artifact and updates the page in the same change.
- The distinct-contributor projection is retained data justified only by the
  suppression rule; if that rule changes, the projection is removed, not
  repurposed.
