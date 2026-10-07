# Architecture Rules and Domain Access contracts

`go.putnami.dev/protocol/architecture` is **experimental**: explicit opt-in,
never part of the default experience, and free to change its wire format,
finding IDs, and detector coverage without a migration path.

This repository DOES gate on it. `architecture validate` runs inside the fixed
maintainer gate (`lint,test,build,validate`, see
[Maintainer CI](../../CONTRIBUTING.md#maintainer-ci)), reached through
`validate`, and that is what keeps its ten declared domains honest.
What the experimental status costs is stated rather than avoided: a finding ID,
a wire member, or a detector's coverage may change between releases with no
migration, so a workspace that gates on this signs up to re-read its own
findings when the protocol moves. That is a maintenance cost to take on
deliberately — not a reason to run the check in advisory mode.

`go.putnami.dev/protocol/architecture` owns the framework-neutral v1 wire
contracts for Architecture Rules as Code (ARC) and Domain Access & Replication
Contracts (DARC). It turns domain boundaries, cross-domain access, local copies,
and known architecture debt into strict data that the Putnami CLI can validate
without network or cloud access.

The central rule is:

> Data may be copied. Authority may not be copied.

A domain may keep a stable reference, query an owner, attach an immutable
snapshot, maintain a rebuildable projection, or send a command. The access mode
is declared for its meaning; the protocol deliberately does not rank events,
APIs, imports, or databases as one universal transport preference.

## Authored artifacts

### Domain manifest

Each domain owns one exact `putnami.architecture.json`. Tooling discovers that
filename below the workspace while excluding generated, dependency, VCS, and
tool-state directories. Identity comes from the `domain`, export, and import
fields—not from the directory name.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-architecture.json",
  "protocolVersion": 1,
  "domain": "observability",
  "owner": "observability",
  "projects": ["/observability/workloads/telemetry-api"],
  "owns": [],
  "exports": [],
  "imports": []
}
```

The producer owns an `export`: facts, authoritative domain for every fact,
allowed access modes, lifecycle, and compatibility. `classification` and
`personalData` are required together for every fact when the export allows any
mode other than `reference`. A pure code surface whose sole mode is `reference`
may omit both: the contract boundary and authority still matter, but there is no
data crossing to classify. A mixed-mode export does not get that exemption, and
older reference-only declarations that state both fields remain valid. The
consumer owns an `import`: producer export reference, minimized facts, semantic
mode, lifecycle, transport availability, consistency guarantees, local model,
deletion, and justification. Repository validation joins these halves; neither
side copies the other's whole manifest.

`bindings` are ARC permissions for exact current project dependencies. They are
allowed only on non-`planned` imports. A planned target therefore cannot hide a
legacy dependency merely because both connect the same pair of domains.

### Baseline and waivers

The optional workspace-root `architecture.baseline.json` records only debt that
already existed when ARC was adopted. Every entry includes an exact stable
finding ID, owner, reason, scope, and a removal condition or expiry. Validation
against a Git baseline makes the file shrink-only after its first commit.

Later exceptions belong in `architecture.waivers.json`, with the same metadata.
They are temporary and cannot be promoted into the architecture graph. An
expired or stale waiver fails validation.

The distinction is intentional:

- a manifest says an architecture relation is valid;
- a baseline says an invalid relation was already known at adoption;
- a waiver says a later violation is temporarily tolerated;
- observed edges remain derived repository facts.

## Projection invariants

A `projection` import must declare:

- a bootstrap and an update transport, each with independent availability;
- bounded maximum staleness and explicit missing/stale behavior;
- source-version or sequence ordering, idempotency, and late-event behavior;
- source identity, provenance, observation time, and freshness fields;
- the exact projected fields, separate from locally authoritative fields;
- one projector/ingester writer;
- rebuild/replay semantics; and
- deletion or tombstone behavior.

The imported fact list must exactly equal the projected field list. A generic
shared `Workspace` model is neither required nor implied. The Cloud pilot in
[`fixtures/valid/cloud-pilot`](fixtures/valid/cloud-pilot) calls its local model
`observability.workspace-context`, keeps Observability retention policy local,
and marks the not-yet-existing Runtime API and event as `planned`.

## Derived graph and findings

`BuildGraph` aggregates valid manifests deterministically. `CompareObserved`
compares exact detector output with declared bindings and emits semantic stable
finding IDs. `ApplyRatchet` classifies those findings as new, known debt, or
waived; rejects baseline growth; and requires stale entries to be removed.

`BuildSnapshot` produces the machine view used by the CLI. It always states its
real detector coverage. V1 enforces project dependency edges only when both
projects are mapped to declared domains. Database, HTTP, and event detection are
reported as `not-detected` unless framework evidence names that carrier; no
consumer should infer enforcement from a DARC transport declaration.

## Framework evidence

An `EvidenceRecord` is one implementation of a declared import, observed from a
committed capability manifest — what a build recorded a framework component was
configured with. It is derived fact, exactly like an observed edge: tooling
supplies it, `CompareEvidence` compares it, and neither can authorize anything.

The two findings are asymmetric, because the two directions carry different
certainty:

- `architecture.evidence_without_declaration` **always** fails. A record proves a
  component exists; if no declaration matches its import *and* mode, the code is
  enforcing a contract nobody reviewed. The mode is part of the match on purpose:
  a contract copied into code and later changed in the manifest is exactly the
  drift this catches.
- `architecture.declared_without_evidence` fails only inside a domain that
  already emits evidence. A domain whose workloads use no framework primitive
  emits nothing, and demanding an implementation from it would report every
  honest declaration as a violation. Once a domain implements one import, every
  ACTIVE import it declares is expected to be implemented too. A `planned` import
  is never expected — it is a target.

Coverage says exactly what happened. `coverage.domainAccess` reports
`framework-evidence` once any implementation is recorded, and a transport
category (`http`, `events`) moves with it only when a record declares that
carrier. The tier is deliberately not called "observed": nothing watched a
request, a query, or a delivered event.

Both runtimes implement the consumer half. In Go the components live HERE, in
[`darc/`](darc/) — framework-free, so any Go program can enforce a declared
contract — and [`go.putnami.dev/app/darc`](../../go/framework/app/darc)
re-exports them for applications and adds the describe carrier. In TypeScript
they are the `darc` module of
[`@putnami/application`](../../typescript/framework/application/src/darc); there
is no TypeScript protocol package to host them, and the shared conformance
corpus, not the location, is what holds the two implementations together. Each
takes a declared import as its configuration and enforces it at run time;
describe emits the row this comparison reads. A row therefore still requires an
application to describe itself, which is the boundary
[ADR 0002](doc/adr/0002-runtime-evidence-needs-an-application-carrier.md)
records.

## Putnami CLI

The bundled tooling is local, read-only, and network-free:

```bash
putnami architecture validate
putnami architecture snapshot --output=json
putnami architecture inspect observability
```

`validate` is the CI verdict; `snapshot` is the complete canonical machine
projection; `inspect` is the bounded view of one exact domain identity. When a
workspace-root baseline exists, all three resolve the prior baseline directly
from immutable Git objects. Pass the global `--baseline <git-ref>` only to
override Putnami's normal comparison ref. No command creates declarations,
baselines, or waivers.

The commands live in the SDD extension,
[`tooling/sdd-extension`](../../tooling/sdd-extension/README.md). The rationale is
recorded in [ADR 0008](../../tooling/cli/doc/adr/0008-executable-architecture-command-boundary.md).

## Strictness and determinism

Readers accept only the exact integer token `protocolVersion: 1`, reject unknown
fields and explicit nulls, validate local and repository-wide semantics, and
return stable `go.putnami.dev/protocol/diagnostic` records. Canonical writers
deep-copy before sorting and emit two-space-indented JSON with one trailing
newline.

Published schemas:

- `schemas/putnami-architecture.json`
- `schemas/putnami-architecture-baseline.json`
- `schemas/putnami-architecture-waivers.json`
- `schemas/putnami-architecture-snapshot.json`

## Current repository adoption

The Putnami repository maps **94 of its 132 projects to 10 domains** and
excludes the other 38 with a reason and an owner each. There is no workspace-root
baseline and no waiver file: adoption started clean and every wave landed with
its declarations complete, so there has never been debt to record.

`putnami architecture validate` reports:

| Measure | Value |
|---|---|
| Domains | 10 |
| Exports | 13 |
| Imports | 21 |
| Observed cross-domain edges | 526, every one covered by an exact binding |
| Findings | 0 |
| Evidence rows | 8 |
| `coverage.projectDependencies` | `enforced-for-mapped-projects` |
| `coverage.domainAccess` | `framework-evidence` |

| Domain | Owner | Projects | What it decides |
|---|---|---|---|
| `protocols` | protocols | 40 | The strict wire contracts every other domain speaks |
| `go-framework` | framework | 24 | The Go application runtime API and its documentation |
| `typescript-framework` | framework | 14 | The TypeScript application runtime API and its documentation |
| `extension-providers` | extension-providers | 8 | Probe answers, verification observations, provider results, the Python surface |
| `cli` | cli | 3 | The workspace engine and the tooling documentation |
| `sdd` | sdd | 1 | The spec-driven method and its documentation |
| `extension-sdk` | tooling | 1 | The extension runtime an extension binary is built on |
| `agent-workflows` | tooling | 1 | The agent content of the `@putnami/contributor` extension |
| `observability` | observability | 1 | Anonymous CLI-usage aggregates |
| `public-docs` | public-docs | 1 | What the public documentation site publishes |

The excluded 38 are the 28 samples, 7 templates, 2 template proofs, and the
standalone agent-readiness repository collector. Samples and templates prove or
seed the public API without owning a fact another repository project consumes.
The collector produces an optional anonymous payload for an external report;
no repository project consumes it, and no existing domain owns the assessment
semantics. Mapping it now would imply an authority that domain does not hold.
The full inventory, the reason for every exclusion, and the completion rule live
in [`doc/repository-adoption-matrix.md`](doc/repository-adoption-matrix.md), and
`TestEveryWorkspaceProjectIsClassified` in
[`tooling/sdd-extension/repository_adoption_test.go`](../../tooling/sdd-extension/repository_adoption_test.go)
fails on a project that is neither mapped nor excluded.

### Permissions and flows are not the same thing

Two different things are declared here and it matters which is which:

- **ARC project permissions.** Nineteen of the twenty imports authorize exact
  project dependencies. `go-framework.protocol-contracts.v1` is 222 reviewed
  `(consumer, producer)` pairs, and nothing observes them at run time — the gate
  compares them with the resolved graph and that is the whole contract.
- **DARC runtime-enforced flows.** Five imports are `snapshot` contracts owned by
  `public-docs`, one per repository-owned documentation source. They are
  configuration for a running component, not a description of one.

A package edge is never a semantic data-access contract, and a `bindings` list is
never evidence that anything runs.

### The documentation snapshot, as a worked example

The public site publishes documentation five other domains write. That flow used
to exist only as `generate.assets` copies in `sites/putnami.dev/putnami.json`,
where a deleted source produced a skipped copy, a warning nobody reads, and a
section that silently disappeared.

It is now declared at both halves. `cli`, `sdd`, `go-framework`,
`typescript-framework` and `extension-providers` each export the documentation
fact they are authoritative for; `public-docs` imports each one as a `snapshot`
carrying a deterministic tree digest as its source version, `fail-closed` on a
missing source and `use-stale` past its freshness bound. The site's runtime takes
those declarations as its configuration through the `darc` module of
[`@putnami/application`](../../typescript/framework/application/src/darc), so a
missing source now fails the build instead of shrinking the site.

### Evidence is atomic per domain

`architecture.declared_without_evidence` fires only inside a domain that already
emits evidence, and then for **every** active import that domain declares. So a
domain adopts the runtime half all at once or not at all:

- `observability` implements both of its imports and emits two rows from
  `/sites/telemetry.putnami.dev`.
- `public-docs` implements all six and emits six rows from `/sites/putnami.dev`.
- The other eight domains emit nothing, and pass.

That silence is a decision, not an omission. The CLI, the SDD extension and the
language extensions enforce their imports at precise, real code paths, but none
of them is a Putnami application: a capability manifest is written by
`Application.Describe`, and a CLI binary started under `PUTNAMI_DESCRIBE`
dispatches a subcommand and exits. Taking `go.putnami.dev/app/darc` to obtain a
carrier would also link a dependency-injection container, a config loader and a
migration engine into the tooling that builds framework applications. The verdict,
the dependency path, and the smallest change that would unblock it are recorded in
[ADR 0002](doc/adr/0002-runtime-evidence-needs-an-application-carrier.md).

The Cloud pilot documents under [`fixtures/valid/cloud-pilot`](fixtures/valid/cloud-pilot)
remain conformance fixtures with non-authority filenames. They are not part of
that graph, and they carry the two shapes the repository's own manifests do not —
a planned import and a projection with a complete consistency block — which is
why the SDK's builder is held to them
(`tooling/sdd-extension/architecture_cloud_pilot_pin_test.go`).

### Adopting in another repository

The order matters, because each step is only checkable once the previous one
exists:

1. **Declare the domains.** `putnami architecture init <domain> --projects …`
   scaffolds one canonical manifest per domain: its id, its owner, and the
   projects it maps. Nothing else — an export or an import is an agreement
   between two domains and no generator can derive one.
2. **Author the halves.** The producer declares its `exports`: the facts, the
   domain authoritative for each, the allowed access modes, and the compatibility
   policy. Facts on a reference-only code surface may omit `classification` and
   `personalData`; every other export declares both. The consumer declares its
   `imports`: the producer export, the minimized fact list, the access mode, and
   — for a projection — bootstrap, updates, consistency, deletion, and the local
   model.
3. **Run the gate and read the findings.** Every observed cross-domain project
   dependency with no exact binding is a finding. Add a binding to the import
   that authorizes it; that edit is the review. `putnami architecture sync`
   proposes the bindings whose contract is already declared and REFUSES the rest,
   so an undeclared relation cannot be granted by a tool. It also refuses an edge
   whose consumer domain declares **two** imports from the same producer
   (`ambiguous-import`): choosing which contract authorizes a dependency is the
   reviewer's decision, not the tool's. This repository hits that once —
   `public-docs` consumes `typescript-framework` as a package reference and as a
   documentation snapshot, and only the reference can be implemented by a package
   dependency — so those five bindings are hand-authored after reading the
   refusal.
4. **Baseline what you cannot fix now.** Record the remaining findings in a
   workspace-root `architecture.baseline.json`, with an owner, a reason and a
   removal condition each. The file is shrink-only from its first commit, and a
   baselined edge warns — it never becomes a declared right.
5. **Implement the projections.** A workload takes its declared import as
   configuration — [`go.putnami.dev/app/darc`](../../go/framework/app/darc) in
   Go, the `darc` module of
   [`@putnami/application`](../../typescript/framework/application/src/darc) in
   TypeScript — and registers the describe carrier. The gate then reports
   `coverage.domainAccess: framework-evidence` and holds the domain's other
   active imports to the same standard.
6. **Optionally, move the authoring into code.** A `Pin`-backed builder test
   (`go.putnami.dev/sdk/extension/architecture`) makes a Go program the author of
   a manifest and the committed JSON its projection. It is a later step, not a
   prerequisite: the manifests are the contract either way.

Steps 1–5 are language-neutral. Step 6 is Go-only, because extensions are Go.

The two runtimes are held to each other by
[`fixtures/conformance/`](fixtures/conformance/README.md): one corpus of
contracts and ordered operations that both execute. A behavior that differs
between them fails on one side rather than shipping as two runtimes that
describe the same manifest differently.

## Ownership and support

- **Owner:** `go.putnami.dev/protocol/architecture`
- **Status:** `experimental`, explicit opt-in and never part of the default
  experience, recorded as
  `{"id": "go.putnami.dev/protocol/architecture", "kind": "protocol", "status": "experimental", "default": false}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)). The wire
  format, finding IDs, and detector coverage may change without a migration.
- **Runtime boundary:** the package is pure and hermetic; filesystem discovery,
  Git baseline reads, and Putnami project-graph detection belong to the CLI
- **Cloud boundary:** Putnami Cloud authors domain manifests and debt records; it
  does not fork the parser, graph, finding IDs, or ratchet

The main format decision and rejected alternatives are recorded in
[`doc/adr/0001-declarations-are-authority-observations-are-evidence.md`](doc/adr/0001-declarations-are-authority-observations-are-evidence.md).
