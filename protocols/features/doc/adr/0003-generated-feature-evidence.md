# ADR 0003 — Generated feature evidence comes from explicit build-producer mappings

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/features` (`protocols/features`)

## Context

A feature earns `coded` only through evidence. Capability manifests already
commit the technical facts (routes, migrations, events) a feature requirement
could be proven by, but nothing says which requirement a contribution proves,
so each one stays `features.unclassified_contribution`. The authoring surface
is `Module.Feature(...)` plus the native design graph; hand-written evidence
seams let a plugin assert any stage for any feature.

## Decision

### 1. The association is an explicit three-part tuple, never an inference

One generated record requires an authored feature ID, an authored requirement
ID on that feature, and one canonical contribution identity the same producer
publishes in the same run. `BuildGeneratedEvidence` resolves exactly that
tuple. Directory containment, ownership, name similarity, the nearest feature,
contribution kind, an ancestor module, and discovery order are not inputs. An
unmapped contribution stays `unclassified`: visible and honest.

### 2. The producer derives every field it could otherwise choose

The author supplies the tuple, declared with `Proves` / `proves` beside the
feature. Stage comes from the authored requirement; outcome is `supports`;
issuer is the producer; source root, owner project and `source.binding` come
from the build's scheduler-stamped inventory; the subject is the exact
published identity. `GeneratedEvidenceMapping` carries no owner project: the
emitting producer owns one project, so a workload cannot claim a contribution
its dependency owns.

### 3. The producer is the describe-time capability emitter

`go.putnami.dev/app` and `@putnami/application` emit generated evidence to
`.gen/schema/feature-evidence/<language>-framework.json` during describe,
after the capability manifest is canonical and validated, atomically beside
it, and delete the file when they emit nothing. Every record resolves against
the manifest that ships with it. The describe producer issues records as
`build`; `BuildGeneratedEvidence` accepts only a `build` or `test` issuer, so
a caller cannot widen the claim.

### 4. Identity is derived, so re-running changes no bytes

`GeneratedEvidenceID` is `<feature>/<requirement>/<12 hex of the contribution
digest>`, a pure function of the association. Contribution keys are free
text (`GET /tasks/{id}`, `sql:cliagg`) and the ID grammar is lower-case kebab,
so the identity is digested, not slugged; `subject.contribution` keeps it
readable. Byte-identical duplicate mappings coalesce; two that agree on
identity and differ otherwise fail publication.

### 5. Failure refuses the whole document, and never the application

An unknown feature or requirement, a requirement whose stage cannot carry
evidence or that does not accept capability evidence, an unpublished or
dependency-owned contribution, provenance outside the source root, and a
contradictory duplicate each return diagnostics and no document. A partial
file would look complete while claiming less. The mapping is inert data read
only during describe: it cannot change configuration, startup, injection,
routing, migrations, authorization, or shutdown.

### 6. Freshness is source-binding based, and the binding excludes the output

`source.binding` is the `source-v1` digest of the owning project root.
`schema/capabilities.json` and `schema/feature-evidence/` are excluded, so
committing the output does not invalidate it. Any other source change makes
the committed record stale; the resolver never rebinds it.

### 7. Both languages run the same algorithm against one vector

Go calls `BuildGeneratedEvidence`; TypeScript ports it. The shared
`generated-evidence.golden.json` vector pins both, including what must not be
emitted (an unmapped contribution and a dependency-owned one). Neither
language may gain a broader association rule or weaker validation. The
equivalence covers the resolver, not the manifest readers: Go parses
`putnami.features.json` strictly, TypeScript refuses only what it cannot
interpret, and `putnami features validate` owns strict validation for both.

## Rejected alternatives

- **A free-form evidence contributor seam** (`FeatureEvidenceContributor`).
  An author writes IDs, stages and provenance by hand: self-certification.
- **Derive evidence from the design graph** (everything under a feature's
  module proves it). It answers "which feature is this near?" and calls that
  proof.
- **Put the mapping in `putnami.features.json`.** Contribution identities are
  design-graph facts; the manifest would become a second inventory.
- **A separate evidence job outside describe.** It re-derives what describe
  computed, and any drift publishes evidence against a manifest that never
  shipped.
- **Spell the contribution into the ID.** Every slug that fits the grammar
  makes distinct contributions collide.
- **Publish the resolved records and report the rest.** A mapping deleted by
  mistake would look like a lost proof.

## Consequences

- Mapping needs an authored `putnami.features.json` requirement first.
- Contributions a dependency owns stay `unclassified` in the workload; the
  owning package maps them.
- A root that authors no feature may answer `featureAuthority` in its project
  document: `owner` resolves to a durable authored feature, or `none` carries
  the reason. Without an answer every contribution warning stays.
- Renaming a contribution key changes the evidence ID; review
  `subject.contribution`, not the ID churn.
- A new failure mode is added in both languages and pinned in the vector.
- The frameworks publish no hand-authored evidence API (`FeatureEvidence`,
  `FeatureContribution.Evidence`); declare `Proves` / `proves` instead.
