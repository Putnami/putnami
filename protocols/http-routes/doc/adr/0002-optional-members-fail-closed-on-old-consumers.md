# ADR 0002 — An optional member fails closed on a consumer that predates it

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/http-routes` (`protocols/http-routes`)

## Context

A page route gains `proves`, the feature requirements the page renders proof
of. The member is test metadata: it changes no path, method or visibility.
Every validating decoder of `putnami.http-routes.v1` is strict and rejects an
unknown member as `http_routes.unknown_field`, and ADR 0001 forbids applying
part of an invalid inventory. A consumer built before `proves` therefore refuses an
inventory that carries it, and every route the inventory declares stays
unreachable through that consumer.

## Decision

1. **`proves` is an optional member of `putnami.http-routes.v1`, not a new
   major.** It changes no matching, canonicalization or digest rule for a
   manifest without it, and such a manifest keeps its bytes and digest.
2. **Validating decoders stay strict.** A consumer that predates the member
   refuses the inventory rather than drop a member it cannot read.
3. **Consumers upgrade first.** A producer writes `proves` only when its
   author declares it on a page, so an inventory carries the member only after
   that opt-in. The protocol README and the web documentation state the order:
   upgrade every consumer of the inventory, then declare `proves`.

## Rejected alternatives

- **A new major identifier.** Every consumer would need two decoders for a
  member that changes no reachability.
- **Lenient decoding of unknown members.** A consumer would accept a future
  member that does change reachability.
- **A separate proves artifact.** A tool would join two files on route
  identity, and the inventory would no longer be the one table that lists a
  page's routes.

## Consequences

- An inventory with `proves` fails closed on an old consumer: the deploy is
  refused, nothing becomes reachable by accident.
- A producer built before `proves` drops the member it does not know when it
  aggregates fragments, so the inventory it writes lacks `proves` without an
  error. Upgrade the producer with the consumers.
- The next optional member follows the same rule and the same order.
