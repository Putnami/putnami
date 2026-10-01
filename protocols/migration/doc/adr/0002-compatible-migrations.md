# ADR 0002: A migration may declare that the previous image can still run

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/migration` (`protocols/migration`)

## Context

Rollback moves a channel back to an earlier release set, and synchronization
switches the workload to the previous image. Migrations are forward-only, so the
database stays where the newest applied migration left it. The question a
rollback asks is never "can I undo this migration" but "can the previous image
still read and write this schema".

`Capabilities.Reversible` says a `Down` payload exists. The common safe case is
different: an expand/contract migration (add a nullable column or a table,
backfill, never drop in the same release) needs no `Down`, because the previous
image never notices it. Without a marker it is indistinguishable from a
`DROP COLUMN`.

## Decision

1. An operation may declare `capabilities.compatible`, and a normalized
   definition may declare `compatible`: the schema after the operation stays
   readable and writable by the previous application image, so an environment
   may roll back across it without running `Down`.
2. `Compatible` and `Reversible` are independent; both may be true. Validation
   adds no rule beyond "a reversible operation carries a `Down` payload".
3. Only the author claims it. `NormalizeOperation` infers `Reversible` from a
   `Down` payload, which is its own evidence. Nothing on the wire proves
   compatibility, so normalization leaves `Compatible` exactly as authored.
4. `RollbackAllowed(ops)` is the predicate a deployer asks. It is true when
   every operation is `Reversible || Compatible`, and true for an empty set. One
   operation that is neither refuses the whole set: a rollback is atomic per
   workload.
5. The field is `omitempty` on both types, so a bundle without it keeps its
   canonical form and its digest.

## Rejected alternatives

- **Infer compatibility from the DDL.** The protocol is kind-agnostic and never
  parses a payload, and the answer depends on what the previous image reads.
- **Reuse `Reversible`.** Expand/contract migrations would ship a needless
  `Down`, and the check that gives `Reversible` its meaning would go.
- **A per-bundle marker.** A release mixes both kinds, and `RollbackAllowed` must
  see the one incompatible operation.
- **Decide from the schema diff at deploy.** It puts a per-backend
  schema-comparison engine on the deploy path and still cannot know what the
  previous image reads.

## Consequences

- The marker is migration-meaningful, so a bundle that sets it digests
  differently ([ADR 0001](0001-bundle-digest-excludes-provenance.md)).
- A wrong `compatible: true` rolls back into a schema the previous image cannot
  use. The claim is a reviewed authoring act, never a default, never inferred.
- A workload whose rollback target is behind the applied migrations is refused
  and reported, never half-applied.
- `compatible` says nothing about SQL; any migration kind uses the same field.
