# Public support-status protocol

This module owns the strict v1 contract for Putnami's public support catalog.
The only reviewed authority is `putnami.support.json` at the workspace root.
Project-local catalogs are not discovered and generated views must point back
to that root file instead of becoming another inventory.
The broader human-readable scope, platform, version, and license promise lives
in the [First Public-Release Contract](../../README.md#first-public-release-contract).
Where this catalog sits in the per-format compatibility budget — exact-match
only, no read window, no migration, because the file is authored rather than
generated — is stated in
[Compatibility and Migration](../../tooling/cli/doc/21-compatibility-and-migration.md).

## Vocabulary

Support status is a public product commitment. It is deliberately separate
from the seven-stage feature `maturity` evidence ladder in
[`protocols/features`](../features/README.md): support entries have a `status`,
never a `maturity` field, and the two vocabularies are neither aliases nor
ordered against one another.

V1 has exactly three support statuses:

- `stable` — carries the normal public support commitment;
- `preview` — public and usable, but may change before becoming stable;
- `experimental` — no compatibility or parity promise and never default-on.

`beta` and `evolving` are not wire values. Existing prose that uses those terms
can be normalized to `preview` by the later classification slices.

Entries classify a `protocol`, `package`, or `feature`. The optional `default`
boolean is an independent statement about the default experience. The optional
`parity` field is also independent; v1 accepts only `unsupported`, which records
an explicit absence of a cross-implementation parity commitment. Omitting an
optional field makes no claim about that axis.

## Root authority

The single reviewed catalog is [`putnami.support.json`](../../putnami.support.json)
at the workspace root. Read it for the current classification of any subject:
this document describes the contract, never the inventory. Restating entries
here would create a second authority that goes stale — which is exactly what
happened while this section reproduced a nine-entry snapshot of a catalog that
had grown to eighty.

The shape of one entry, and the two optional independent claims:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-support.json",
  "protocolVersion": 1,
  "entries": [
    {
      "id": "@putnami/cli",
      "kind": "package",
      "status": "stable"
    },
    {
      "id": "@putnami/python",
      "kind": "package",
      "status": "experimental",
      "default": false,
      "parity": "unsupported"
    }
  ]
}
```

Protocol modules are classified with the same three statuses:

```json
{
  "id": "go.putnami.dev/protocol/database",
  "kind": "protocol",
  "status": "stable"
}
```

The catalog also classifies the product-discovery protocol family —
`go.putnami.dev/protocol/{agentcontext,capabilities,contracts,features,support}`,
each `preview`. Each module's README carries its evidence, as
[`doc/adr/0001-support-entry-ownership-and-evidence.md`](doc/adr/0001-support-entry-ownership-and-evidence.md)
requires.

Entries not present in the catalog have no reviewed public support status. The
catalog does not infer or exhaustively classify other protocols, packages, or
features.

## Classifying a subject

A classification needs three things: exactly one owner, one status, and
objective evidence. Only the status is on the wire, and that is deliberate —
[`doc/adr/0001-support-entry-ownership-and-evidence.md`](doc/adr/0001-support-entry-ownership-and-evidence.md)
records the decision and the reasoning.

- **Owner.** A `protocol` entry's `id` is the owning module's Go module path
  (`go.putnami.dev/protocol/<name>`), so exactly one in-repo project owns each
  classification and the gate resolves it to `protocols/<name>/go.mod`. Owner is
  derived from identity rather than declared a second time.
- **Status.** `stable` is a commitment to consumers outside this workspace.
  A contract whose only consumers are in-repo, or whose successor version is
  mid-migration, is `preview`. `experimental` stays reserved for surfaces with
  no compatibility promise that are not part of the default experience.
- **Evidence.** The owning module's README states the status, the owner, and the
  artifacts that back it — producers, consumers, fixture corpora, determinism
  and parity tests, published schemas. Evidence is never a wire field: it is
  reviewed with the module it justifies.

A feature usually needs no entry: it inherits the status of the package or
protocol that ships it. A `feature` entry states a status that *differs* from
the shipping subject's — an experimental capability inside a stable package —
which is the only reason the wire keeps the kind.

## Promotion and demotion

What earns each status, and what forces it back down, is
[`doc/adr/0002-promotion-and-demotion-criteria.md`](doc/adr/0002-promotion-and-demotion-criteria.md).
Who approves the move is
[GOVERNANCE.md](../../GOVERNANCE.md#support-status-promotion-and-demotion), and
the decision's record is the entry in the root catalog. Two rules matter before
you edit the catalog:

- **Every subject, every status.** The entry resolves to exactly one owning
  project, and that project's documentation carries a support section stating
  this exact status and linking the catalog. `criteria.go` reads that section —
  the first heading naming support, and the first status it writes as inline
  code, with fenced examples stripped first — so a module and the catalog cannot
  drift apart.
- **`stable` is pinned.** `reviewedStableSubjects` in `conformance_test.go` maps
  every subject the repository promises to support to the `default` and `parity`
  claims reviewed with that promise. Promotion, demotion, and a changed claim all
  fail the gate until the same pull request edits that map, so a public promise
  cannot change as a side effect of an unrelated edit. `preview` and
  `experimental` stay unpinned: a subject that is still moving must be able to
  move.

Before v1.0.0 the promises read through [`RELEASE.md`](../../RELEASE.md): a
`stable` entry means publicly supported, documented, and maintained, and a
pre-1.0 minor may still carry a documented migration.

### What each status requires

| Status | The subject must | The project commits to |
|---|---|---|
| `experimental` | Exist, and be off by default — `default: false`, which `ValidateCatalog` enforces rather than trusting the author to remember | Nothing. No compatibility promise, no parity promise, no support |
| `preview` | Be documented, run under the repository gate, and carry a written statement of what may still change | Keeping it usable, and saying what changed when it changes |
| `stable` | Do all of the above on every platform in the release contract's matrix, and — when it has a wire format a user can hold on disk — carry an entry in the [compatibility budget](../../tooling/cli/doc/21-compatibility-and-migration.md) | The normal public support commitment |

### Promotion

Promotion moves one step at a time: `experimental` → `preview` → `stable`.
Skipping `preview` is the same claim made with less evidence, and the step that
would be skipped is the one where users first depend on the subject.

A promotion pull request carries three things: the changed catalog entry, the
evidence its target row names, and the rationale. Prose is not evidence — a
subject that cannot point at documentation, gate coverage, and (where the row
requires it) a budget entry is not ready to be promoted.

### Demotion

Demotion may skip steps and takes effect immediately. It corrects a promise the
project can no longer keep, and a notice period only extends the time users
spend relying on the wrong one. Demoting to `experimental` also sets
`default: false`; the validator refuses the combination that would leave an
experimental subject on by default.

Removing an entry is not a demotion. An absent subject has no reviewed status at
all, which says strictly less than `experimental` does.

### Python

`@putnami/python` is `experimental`, `default: false`, `parity: unsupported` for
the first public release, and the root catalog is where that decision lives. It
is held there on purpose: the Python surface is present and usable but carries
no parity commitment with the Go and TypeScript implementations. Promoting it
takes exactly the evidence the table above requires of anything else.

## Producers, consumers, and versioning

- **Producer**: humans. The workspace-root catalog is authored and reviewed; no
  build step emits it, and no generator may write it.
- **Consumers**: this module's own canonical-authority gate and the CLI's
  `specs validate` completeness check. There is no site consumer — a generated
  view must link to the root file instead of copying its values.
- **Versioning**: `ProtocolVersion` is the exact integer `1`. Readers accept no
  other token, and `schemas/putnami-support.json` pins the same constant, so the
  editor validation and the parser cannot disagree.
- **Compatibility**: the status, subject-kind, and parity vocabularies are
  closed. Adding a value, a field, or a discovery location is a wire change that
  needs a version bump, not an additive edit. Absence stays meaningful in both
  directions: an omitted optional field makes no claim, and an absent subject has
  no reviewed status.

## Ownership and support status

- **Owner**: `go.putnami.dev/protocol/support` (`protocols/support`).
- **Status**: `preview`, `parity: unsupported` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json).
- **Evidence**: strict parser, canonical marshaller, valid/invalid fixture
  corpus, and the gate below that keeps the root catalog strict, canonical, and
  faithful to the reviewed decisions, plus the CLI completeness consumer. Go is
  the only implementation, so no cross-implementation parity is promised. The
  v1 vocabulary is still evolving and has no consumer outside this repository,
  so `stable` would over-promise.
- **User-facing feature**: none, deliberately. A support status is product
  policy about other subjects, not a feature a user enables. The durable
  decisions behind this module live in [`doc/adr/`](doc/adr/) and are linked
  from specs the way
  [`protocols/features`](../features/README.md#specs-and-durable-decisions)
  defines.

### Owner and evidence live with the subject

An `Entry` carries identity, status, and the two optional independent claims —
nothing else. There is deliberately no `owner` or `evidence` field: adding one
would be a wire-contract change, and it would turn the catalog into a second
inventory of facts the subject's own documentation already states. A classified
subject records its single owner and the objective evidence behind its status in
its own README (see the `Support` section of any
[`protocols/*`](../README.md) module), and links back to this catalog for the
reviewed value.

A status is a claim, so it needs evidence: shipping consumers, conformance or
drift suites, and cross-language parity proofs. Promotion without that evidence
is the failure mode this authority exists to prevent.

## Strictness and canonical bytes

`ParseCatalog` rejects malformed JSON, duplicate or unknown fields, explicit
nulls, trailing values, and any `protocolVersion` token other than the exact
integer `1`. `ValidateCatalog` enforces the closed vocabularies, canonical
subject IDs, unique `(kind, id)` identities, and the experimental/non-default
rule. The JSON Schema lives at
[`schemas/putnami-support.json`](schemas/putnami-support.json); shared valid and
invalid examples live under [`fixtures/`](fixtures) — one invalid fixture per
diagnostic code, each valid fixture also asserted to be in canonical form.

`MarshalCatalog` sorts a copy by kind and ID and emits two-space-indented JSON
with one trailing newline. The project gate (`conformance_test.go`) validates
that the workspace-root catalog is strict and canonical, that every `protocol`
entry resolves to a module in `protocols/`, that the four reviewed package
decisions keep their exact status and their `default`/`parity` claims, and that
the product-discovery protocol statuses still match
[ADR 0001](doc/adr/0001-support-entry-ownership-and-evidence.md).

It then applies the two checks
[ADR 0002](doc/adr/0002-promotion-and-demotion-criteria.md) adds:

- the set of `stable` subjects matches `reviewedStableSubjects` exactly, in both
  directions, and each promise's `default` and `parity` claims match what was
  reviewed with it — appearing, disappearing, and changing all fail;
- every classified protocol module's README declares the status the catalog
  records and links to the catalog.

`preview` and `experimental` entries are deliberately outside both checks, claims
included: a subject that is still moving must be able to move.
