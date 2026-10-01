# ADR 0001: Support entries carry status on the wire, ownership and evidence in docs

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/support` (`protocols/support`)

## Context

The support catalog (`putnami.support.json` at the workspace root) is the
reviewed product-policy authority: three statuses, three subject kinds, and two
optional independent claims (`default`, `parity`). It has no `owner` and no
`evidence` field.

A classification still needs exactly one owner, one status, and objective
evidence for that status. Without a rule, each change invents its own
identifier shape and its own place to justify a status, and the catalog becomes
a list of unattributed adjectives.

## Decision

### 1. A `protocol` entry is identified by its Go module path

The `id` of a `kind: "protocol"` entry is the module path, for example
`go.putnami.dev/protocol/capabilities`. It is the durable public identity of the
wire contract, unique in the workspace, and it resolves to
`protocols/<name>/go.mod`. The entry id therefore names exactly one owning
project, and ownership needs no wire field. The module gate resolves every
`protocol` entry to a real in-repo module.

### 2. Evidence lives in the owning module's documentation

The wire records the decision; the module records why. Each owning module's
README states its status, its owner, and the evidence behind the status, and
links the catalog. Evidence means artifacts a reviewer can open: shipping
producers and consumers, fixture corpora, determinism and cross-language parity
tests, published schemas. Intent is not evidence.

### 3. `preview` is the ceiling for a contract with only in-repo consumers

A wire contract earns `stable` by being consumed across a boundary the
repository does not control, or by being frozen with a migration promise. A
contract whose only consumers are in this workspace, or whose successor version
is mid-migration, is `preview`. `experimental` is reserved for surfaces with no
compatibility promise that are off by default; a protocol whose artifacts every
build produces by default is not experimental.

### 4. The catalog is extensible; reviewed decisions are pinned

Any change may add a classification. Every entry must be strict and canonical,
and every `protocol` entry must resolve to a module. The gate pins two sets
exactly:

- `reviewedPackageDecisions`: the statuses of `@putnami/cli`, `@putnami/go`,
  `@putnami/typescript` (`stable`) and `@putnami/python` (`experimental`).
- `productDiscoveryProtocolDecisions`: the product-discovery protocols below,
  whose statuses are not `stable` and would otherwise be unpinned.

The `stable` pin is [ADR 0002](0002-promotion-and-demotion-criteria.md).

### 5. Product-discovery protocol family

| Entry id | Status | Why not `stable` |
| --- | --- | --- |
| `go.putnami.dev/protocol/agentcontext` | `preview`, `parity: unsupported` | Go-only; no consumer outside this workspace. |
| `go.putnami.dev/protocol/capabilities` | `preview` | v1 stays readable while v2 is produced. |
| `go.putnami.dev/protocol/contracts` | `preview` | The node vocabulary still gains lowerings; no external producer. |
| `go.putnami.dev/protocol/features` | `preview` | Feature, evidence, design-graph and spec contracts still evolve. |
| `go.putnami.dev/protocol/support` | `preview`, `parity: unsupported` | Go-only; the v1 vocabulary still evolves. |

Each module's README carries the full evidence.

## Rejected alternatives

- **`owner` and `evidence` wire fields.** `owner` duplicates the entry id, and a
  free-text `evidence` string is an unverifiable claim. README links are
  reviewed with the module.
- **A parallel `putnami.support.evidence.json`.** A second file with its own
  version, gate, and drift, for prose that belongs next to the code.
- **Short protocol ids (`capabilities`).** They collide with package and
  directory names and do not resolve to an owner.
- **Classify every protocol at once.** A classification made without reading
  the module is not reviewed. An absent subject has no reviewed status.
- **`stable` for shipped contracts with only in-repo consumers.** Shipped is not
  committed-to; the promise would first be tested by breaking it.

## Consequences

- Moving or renaming a classified protocol fails the support gate until its
  entry is updated, because the entry id is the public identity.
- The support gate reads sibling modules' `go.mod` files. It is skipped when the
  module is tested standalone, like the root-catalog assertions.
- A status is a decision, not a derived fact: it does not change when the code
  changes.
- The catalog is not exhaustive. Readers treat an absent subject as
  unclassified.
