# `go.putnami.dev/protocol/doccov`

The repository's **documentation-coverage guard** over the wire contracts in the
sibling `protocols/*` packages.

A protocol package's exported struct fields ARE its contract. Automation is a
first-class consumer here, so a field carrying a JSON value must be explainable
from the types and the schemas alone — not from a commit message, a chat, or the
person who wrote it. This package makes that checkable.

## What it does

`ScanPackage(dir)` walks a protocol package's non-test Go source **as text**
(`go/parser`, never importing the package) and reports, for every exported field
of every JSON-marshaled struct, whether it is documented in either of the two
places a reader looks:

- a field-level Go comment (a doc comment above it or a line comment beside it),
  or
- a `description` on the field's JSON name anywhere in the package's
  `schemas/*.json` (or the contract-twin `schema/` subdirectory).

A field with neither is undocumented. `PackageReport` aggregates the per-field
results into a total, an undocumented list, and a ratio.

It scans source as text on purpose: importing every protocol package would put
this guard in a module that depends on all of them, which is exactly the
dependency edge a guard must not have. The cost is that it sees syntax, not
types — embedded fields are skipped because their members are documented on the
embedded type, and `json:"-"` fields are skipped because they are not on the
wire.

Scanning is deliberately shallow: the package root plus the contract-twin
`schema/` subdirectory, one level. Conformance helpers and testdata are not a
package's wire surface, so they are not scanned.

## The guard and its ratchet

`doccov_test.go` runs the scan over every sibling package that has a `go.mod`
and enforces three rules:

1. **The bar.** A package must keep at most `maxUndocumentedRatio` (10%) of its
   exported wire fields undocumented. Adding an undocumented field to a
   documented package fails here.
2. **The allowlist, which only shrinks.** `pendingBackfill` names packages
   TOLERATED above the bar because their documentation is not written yet. It is
   an admission of debt, not a permission. A **ratchet** fails an allowlisted
   package that has already reached the bar, with "remove it from the
   allowlist" — so a backfill is not finished until its entry is deleted and the
   package becomes guarded. Adding a NEW entry moves a package from guarded to
   tolerated and is a deliberate, reviewable act.
3. **Non-vacuity.** A guarded package that scans **zero** wire fields fails. A
   scanner regression would otherwise report ratio 0, sail under the bar, and
   silently pass for every package at once — the worst failure a guard can have.
   A package that genuinely exposes no wire structs (its contract is constants
   plus validation functions) is named in `wirelessContracts` instead.

`documentedPackages` pins the packages whose docs were backfilled after the
guard already existed: they must not be on the allowlist AND must genuinely pass
the bar, so a relapse fails as a named regression instead of being absorbed by
re-allowlisting.

## Producers and consumers

| Role | Who |
| --- | --- |
| Producers | none — this package emits no document and defines no wire shape |
| Consumers | this repository's own test gate, through `TestWireFieldDocCoverage` and `TestDocumentedPackagesAreGuarded` |
| Owner | this project |

It reads its sibling packages' sources and schemas; nothing reads it. That is
recorded in its `putnami.json`, which declares
`options.test.filePatterns` over `../*/*.go` and `../*/schemas/*.json` so the
guard re-runs when a sibling's wire surface changes — a test whose inputs are
outside its own project would otherwise be cached against a stale answer.

## Versioning and compatibility

There is **no wire version**, because there is no wire. The compatibility
surface is the Go API (`ScanPackage`, `PackageReport`, `FieldReport`) plus the
policy constants in the test file. The bar, the allowlist and the pinned lists
are repository policy: they may be tightened at any time, and loosening one is a
review decision, not a compatible change.

## Support status

**This module is deliberately absent from the public support catalog**
([`putnami.support.json`](../../putnami.support.json)).

The catalog records **public support commitments**: what a user outside this
repository may depend on and what promise comes with it. This module is not a
public surface by any of the three tests that matter:

- its `putnami.json` declares **no `publish` channel**, so it is never
  published — there is no artifact a user could depend on;
- it defines **no wire contract** — no schema, no fixture corpus, no document
  anyone exchanges;
- its only consumer is this repository's own test gate.

Recording a status for it would be a claim about a public commitment that does
not exist, and would make the catalog a directory of Go packages rather than the
reviewed statement of what Putnami supports. Absence from the catalog already
has a defined meaning — "no reviewed public support status" — which is exactly
the true answer here. An entry added "for completeness" would be worse than
nothing: it would be the first entry whose status no user could act on.

If this package is ever published (its `putnami.json` gains a `publish`
channel), that changes the answer and it should be classified then, on the
evidence available at that point.

## Specs and durable decisions

There is **no user-facing feature or spec for this module**, and there should
not be one. It is a repository quality gate: a user obtains no outcome from it,
and the outcome maintainers obtain — "a protocol field cannot ship
unexplained" — is an internal engineering standard, not a product promise. The
user-facing benefit is indirect and already owned by the protocol modules whose
documentation this guard defends.

This module records **no durable decisions of its own**. The two rules worth
stating — scan as text rather than import, and let the allowlist only shrink —
are documented above and are enforced by the code and the ratchet rather than by
a separate decision record. When a decision here becomes contested it gets a
record under `doc/adr/` following
[`protocols/features/doc/adr/TEMPLATE.md`](../features/doc/adr/TEMPLATE.md).
