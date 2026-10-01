# ADR 0010 — The compatibility budget: a window per format, and prior-release bytes to prove it

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli`), `protocols/{cli,extension,support,workspace}`

## Context

Before v1.0.0 the stable core may break, and each break is migration-based
([First Public-Release Contract](../../../../README.md#first-public-release-contract)).
That promise alone does not tell a user whether an older `putnami.lock.json`
loads, migrates or is refused, nor a reviewer whether a change breaks someone.

Compatibility is not one policy here: some formats read a range of versions,
others accept exactly one on purpose. A test that builds its input with
today's writer proves only that a reader and a writer shipped together agree.

## Decision

### 1. Every public format states a window, and the window is a budget

A **compatibility budget** states, per format, the versions this build reads,
what migrates, what is rejected on purpose, and the command the user is told to
run. It lives once, in
[`doc/21-compatibility-and-migration.md`](../21-compatibility-and-migration.md),
and matches executable behavior exactly.

| Format | Window | Kind |
|---|---|---|
| `putnami.lock.json` | 2 – 4, writes 4 | bounded range + migration |
| Machine result envelope | emits 2, reads 1 | one-way read retention |
| `cliContract` stamp | exactly 4 | intentional exact match |
| `putnami.support.json` | exactly 1 | intentional exact match |

- A **range** fits when the newer vocabulary is additive and the version tells a
  reader which fields it may trust.
- An **exact match** is required when the version asserts behavior the reader
  cannot verify from the file, such as an extension's runtime halves or the
  meaning of an authored catalog. Reading those leniently is a silent guess.

Widening or narrowing a window edits this table and the budget document. A
change to a version constant without the budget fails
`TestCompatibilityBudgetDocumentMatchesTheCode`.

### 2. A version bump ships a migration or a rejection, never an adaptation

This generalizes [ADR 0002](0002-cli-vnext-contracts.md) §1 to every public
format:

> **A version increment ships either a MIGRATION — a command that moves a
> workspace or an artifact onto the new version — or a documented REJECTION that
> names the remedy. Never an adaptation that silently reinterprets an older
> artifact.**

Adaptation loads an artifact after deleting the part the reader disagrees
with; it is a private downgrade nobody reviewed. `putnami migrate vnext --apply`
is the lock's migration. Re-packaging is an extension's. The support catalog is
authored, so its remedy is an edit and it never gets a mechanical migration.

Two structural carve-outs from ADR 0002 §1 hold: a manifest that declares no
contract surface is outside the ladder (`extension.DeclaresContractSurface`),
and `putnami migrate vnext` is the one reader allowed below the lock floor.
`FormatVersionV1`, `dropFieldsAbove` and `ReadMigratableLockFile` stay until a
released build has carried `putnami migrate vnext`.

### 3. A migration carries integrity material through

A migration that carries integrity material is not a compatibility shim, and
must not be removed as cleanup. Per-platform SHA-256 digests (`integrities`,
`cli.integrities`) are the only check between a download and execution, so a
migration keeps every recorded platform's digest. Rebuilding the map on the
migrating machine would drop every other platform's digest.

A migration may drop the legacy scalar `integrity` only on an extension entry
that also has a per-platform map. An entry with only the scalar keeps it: it is
the sole verification datum.

### 4. Evidence is prior-release bytes, and the bytes are immutable

Compatibility fixtures are recovered, not generated: each is an artifact as
committed at the commit that shipped its format version. A `provenance.json`
beside it records the source commit, the path and the SHA-256 of the bytes.

- **A fixture is immutable.** A guard test compares each file's SHA-256 to its
  provenance record. When a version is retired, the fixture stays and its
  expected verdict moves from "loads" to "rejected with the documented remedy".
- **Gaps are recorded, not implied.** `provenance.json` carries a `gaps` array
  naming each shape the budget covers with no recoverable artifact. The test
  that covers such a shape with a synthetic golden says so at its call site.
- **Regenerating a fixture with the current writer is forbidden.** The reader
  and writer ship together, so a regenerated fixture passes by construction.

## Consequences

- A version-constant change fails the budget test naming the document to edit;
  a fixture change fails the immutability guard.
- The fixture corpus only grows.
- Gapped shapes are proven against in-package synthetic goldens until genuine
  bytes exist outside this repository.
- When lock v1 support goes, the v1 fixtures stay and expect the documented
  refusal.
