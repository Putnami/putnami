# ADR 0001 — The minimal spec and decision-record contract

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/features` (`protocols/features`)

## Context

Every package change ships a spec, so the spec wire is the most-copied
artifact in the repository: whatever it accepts is the shape hundreds of files
take before anyone reviews the sum.

Three authorities already own product facts, and none is the spec:

1. authored feature intent: `putnami.features.json` and native
   `Module.Feature(...)` / `module(...).feature(...)` declarations mint feature
   identity, owner, target, and stage requirements;
2. the design graph: build-time producers project routes, schemas,
   migrations, services, and config from native declarations;
3. feature evidence: framework, build, test, delivery, runtime, and human
   issuers assert what is proven, with source bindings.

A spec that restated any of them would be a hand-maintained copy that drifts.
What no producer can emit is the prose a reviewer needs: what the change is
for, what it will not do, the sentences the team agreed to, and where the
durable decisions behind them are recorded.

## Decision

### 1. A spec details exactly one already-authored feature

`Spec.Feature` is one canonical feature ID. `ValidateSpecRepository` resolves
it against an explicitly supplied authored catalog and fails with
`features.unknown_feature` when nothing mints it, and with
`features.duplicate_spec` when two documents claim one feature. The catalog is
a parameter so that a spec can never widen it: `AuthoredFeatureIDs` builds it
from manifests, and a caller may union native design-graph feature IDs.

Both findings are repository-level facts. A caller that answers about a
selected subset still supplies the whole repository, and narrows only the
findings it reports.

### 2. The field set is closed

`protocolVersion`, `feature`, `outcomes`, `nonGoals`, `requirements`, and
`decisions`, plus the optional `$schema` editor pointer. The strict decoder
refuses unknown fields, so `maturity`, `target`, `evidence`, `owner`, `nodes`
and `sourceBinding` are rejected, not ignored. `spec_test.go` pins the Go type
and the published schema to one field set, so adding a field is a reviewed act
in both places.

### 3. Durable decisions are linked, never embedded

`decisions` holds workspace-relative links to project-local `doc/adr/*.md`
records. The protocol enforces the shape (`features.invalid_decision`) and
containment (`features.invalid_path`, `features.path_escape`). It does not
read the record; link resolution belongs to `@putnami/sdd`, which reads the tree.

### 4. Canonical bytes sort identities and preserve prose

Requirements sort by ID and decision links by path: keyed identities whose
authored order means nothing. Outcomes and non-goals keep their authored
order, because sorting prose rewrites it. Empty and absent collections stay
distinct, so canonical bytes round-trip.

### 5. Location is contract; the filename is not identity

Specs are direct JSON children of `specs/` at the workspace or an exact
project root (`features.outside_discovery_root`). Identity lives in `feature`,
so renaming a file never retargets or orphans a spec.

## Rejected alternatives

- **Let a spec mint a feature, maturity, or evidence.** Two authorities for
  one identity make the catalog answer depend on the reader.
- **Copy derived facts into the spec** (maturity, owners, source bindings,
  routes, schemas, migrations). A second inventory with no producer, stale on
  the next commit.
- **Replace the markdown record with a machine decision wire** (`status`,
  `supersedes`, structured context). ADRs are reviewed and read as prose; a
  machine record needs its own tooling and drifts from the prose. A committed
  `decisions.json` registry does not replace the record: it carries a settled
  value's check and links the record for the why.
- **Filename as identity.** A rename silently retargets a document, and a slug
  cannot express a namespaced ID unambiguously.
- **Workflow and approval fields** (`status`, `reviewers`, `approvedAt`).
  Mutable process state churns a content artifact and turns it into a
  tracker.
- **Many features per spec.** "Which spec details this feature" becomes
  ambiguous.
- **Stages, evidence kinds, or acceptance status on a spec requirement.**
  `putnami.features.json` owns the evidence ladder and machine criteria
  ([ADR 0002](0002-executable-requirement-criteria.md)); a spec requirement is
  a sentence.

## Consequences

- A spec needs an authored feature first: no feature declaration, no spec.
- Tooling reuses `ParseSpec`, `ValidateSpec` and `ValidateSpecRepository`; a
  second interpretation of these bytes is a defect.
- A new public spec field needs a new ADR; a missing use case is reported, not
  accommodated.
- A moved or deleted ADR is a filesystem fact the wire cannot detect.
- A project-scoped tooling surface still reads every root's durable manifests
  and specs once, to keep identity and uniqueness exact; only evidence
  fragments, capability manifests and design graphs narrow with a selection.
