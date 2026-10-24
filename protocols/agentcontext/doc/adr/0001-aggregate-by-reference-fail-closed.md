# ADR 0001 — Aggregate by reference, and fail closed before the document leaves the workspace

- **Status**: accepted
- **Amended**: 2026-10-24, decisions 2 and 5, with protocol version 2: no
  producer writes the document to a file, and the tests section carries no
  test policy.
- **Scope**: `go.putnami.dev/protocol/agentcontext` (`protocols/agentcontext`)

## Context

To answer "what is this project", an agent otherwise walks the files: roots,
capability and contract artifacts, representative sources, tests. Every tool
repeats that walk and reaches a slightly different answer.

One document per project fixes that, but a document that embeds file content
redistributes everything in the repository, including gitignored files,
`sensitive` config values, and keyring material, to any hosted index that
consumes it.

## Decision

1. **Every fact is a reference; the types have no content slot.** An artifact
   is `path` + `sha256:<64hex>` (+ optional `kind`, `sensitive`). A
   representative source is `path` + 1-based line range + reason + token
   estimate. Redaction is a property of the schema, not of producer care.
2. **The document is built in memory and never written.** `putnami context
   pack` prints it and the MCP `agent_context` tool returns it, both from the
   same builder. It embeds the workspace revision and digests, so committing it
   would churn on every commit, and a stored copy is one more thing to go
   stale. Tests prove determinism at a fixed tree.
3. **Publish safety is a fail-closed gate.** `ValidatePublishSafety(doc, opts)`
   runs before a document leaves the workspace. It errors on a reference to a
   caller-flagged sensitive path not marked `sensitive`, a string long enough to
   look like embedded content, a non-relative or `..`-escaping path, and a
   duplicate reference. A `nil` document fails. The caller supplies the
   sensitive-path set, because only it knows what is gitignored, sensitive, or
   keyring material.
4. **Author overrides are narrow.** `<project>/schema/agent-context.overrides.json`
   may add or remove representative-source and doc entries and force-flag a path
   as sensitive. Nothing else: identity, capabilities, contracts, and
   provenance are framework-owned facts.
5. **Every enum is closed and the version is exact.** `RootKind`,
   `SourceReason`, `DocRelationship`, `AbsenceReason`, `TokenMethod`, and
   `AggregationMethod` change only with a `ProtocolVersion` bump. A tests
   section with no packs must carry an `absenceReason`. The tests section names
   packs and fixtures only: how test infrastructure is provisioned is a setting
   of the test jobs, so version 2 deletes `tests.policy`.

## Rejected alternatives

- **Embed source content.** It is the whole leak in one field and makes the
  document unbounded; a path plus line range lets an agent fetch the same bytes
  under its own permissions.
- **Commit the document.** It churns on every commit, and a stale committed
  copy is worse than none. Every read re-derives instead.
- **Write an ephemeral copy under `.gen/`.** No reader needs it: the CLI prints
  and the MCP serves the document from the same builder, and a file left on
  disk needs its own freshness check.
- **Carry the test policy in the tests section.** It restated one job setting
  that the job reads from its own configuration, so the two could disagree.
- **Redact by scanning strings at publication.** A scanner is a guess;
  structural absence plus a fail-closed reference check is checkable.
- **Overrides that restate identity or capabilities.** That is a hand-written
  context pack that can contradict the build.
- **Warn on an unmarked sensitive reference.** A warning on a publication path
  is an error nobody sees.
- **Derive the sensitive set inside the protocol.** It needs filesystem, VCS,
  and config knowledge, and degrades silently to "nothing is sensitive".

## Consequences

- A consumer that wants file content fetches it with its own authorization.
- The gate is only as good as the caller's `SensitivePaths`; a producer that
  passes an empty set must defend that claim.
- Every read rebuilds the document from committed facts, so no stored copy
  can be stale.
- Adding an enum member is a version bump.
- The contract is Go-only (one producer and one MCP consumer, both in the CLI),
  so no cross-language byte parity is promised. A second implementation
  reopens this decision.
