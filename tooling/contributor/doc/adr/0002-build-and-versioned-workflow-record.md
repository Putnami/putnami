# ADR 0002 — One build loop with a versioned local workflow record

- **Status**: accepted for implementation, superseded in part by [ADR 0003](0003-build-publishes-its-proposal-by-default.md)
- **Scope**: `@putnami/contributor` content and its consumer CLI validator

## Context

The contributor extension already has one execute loop, a v2 candidate
dossier, native feature/spec/decision contracts and provider-backed task and
proposal references. Its older entries grew separate planning names, while a
coordinated request can contain several useful units and later requirements.
A passing first unit must not silently replace the requested outcome.
Original material objections must survive revision and host replacement.

## Decision

`build` is the canonical outcome owner. The single physical loop and its
records, policy and collaboration references live under `build/references/`.
`spec`, `design`, `prototype`, `review`, `adr` and `check` are focused
interventions. The former `plan`, `execute`, `fix`, `epic`, `code-review`,
`putnami-plan`, `putnami-change`, `putnami-check`, `putnami-review` and
`fix-loop` entries are removed by the explicit owner-authorized breaking
change in this pull request. `content-bump` remains an active explicit-only
repository integration. Build neither selects backlog work nor inherits
external action authority from a removed entry. A named task claim requires
explicit authorization.
Publication and language/load helpers move to native CLI commands:
`proposals finalize`, `contributor language` and `contributor machine-load`.
The direct `tree fingerprint` command remains the tree producer. Native
finalization consumes an exact current dossier or explicit hosted check mode;
it never runs or retries a local gate. The finalization, load and fingerprint
shell helpers and optional session-cap hook no longer install. The active
audit integration keeps only a thin `check/scripts/english-only.sh` relay to
the native language command.

An independently versioned v1 local workflow JSON references native
feature/spec sources, partitions their full requirement inventory and maps
observable units, stage contracts, material artifact challenge, experiments,
handoffs and actual delivery receipts. It authors no expected check inventory
or passing producer verdict: feature declarations and actual native session
reports supply those. `tree verify --workflow` validates progress and never
grants ready. The v2 dossier may reference the workflow by exact path and
digest; the current CLI then joins complete current requirements to producer
evidence while retaining the existing dossier's review, gate, qualification
and acceptance checks. A caller first confirms `--workflow` is supported,
since an older dossier reader can ignore an unknown member.

Units describe delivered behavior and required receipts; milestones and tasks
track work. Stage entries require a nonempty `entryCondition` and are linked
to a unit; each unit has the full
canonical stage inventory. A current unit cannot be ready with blocked stages
or unresolved decisions; later units keep their pending stages visible. Native
next-cycle task refs retain opaque provider `{source,id}` identity, with a
revision and actual `tasks get` receipt together when exact inputs matter, or
an exact local task record. A bounded first unit retains later requirements and any
outcome-changing exclusion needs actual mandate authority. Material artifact
opinions bind to the exact source and relevant context bytes in an original
reviewer-authored report; immutable examined bytes and later independent
`challenge.revisions` retain the chain after correction, with exact snapshots
for same-path changes. Changing an index digest does not renew them. The
whole-tree candidate binding remains stricter. Handoffs name available
versioned inputs and explicitly identify local assets that cannot travel.

Native `putnami proposals finalize` accepts `--partial-delivery` for an intermediate unit.
It keeps a non-closing parent reference, rejects caller closing directives in
title, the fully normalized proposal body and branch commits before a write,
and skips the parent task transition. When a
workflow dossier reports `outcomeComplete: false`, normal parent-task
finalization is refused. Normal policy close and delivered transition remain
for a complete mandated outcome. All existing gate, proof, English, provider
capability, proposal read-back and comment controls continue in either mode.

## Consequences

- Source skills and refs are packaged once for both hosts; generated root
  copies come only from the coordinator's context generation.
- A settled small correction may reuse native intent/design, while an
  uncertain material choice can be challenged through a bounded prototype.
- Local workflow validation supports progress and resume, but local writer
  identity and semantic scope completeness still require independent review.
- Ready, merged, published, deployed and adopted remain distinct actual
  receipts. The mandate still controls every external action except the
  run's own proposal, which ADR 0003 publishes by default.

## Alternatives rejected

- A second controller or general workflow datastore would duplicate native
  projects, provider state and producer records.
- Adding planning fields to closed feature/spec or hosted review contracts
  would force a broader migration without a needed capability.
- A manually maintained checklist of expected checks would drift from the
  feature declaration and could mark a partial passing report complete.
- Relying on the caller to remember to omit the closing line for an
  intermediate unit would leave an avoidable parent-task closure path.

Hosted ready is an additional native producer path. A parent task's hosted
publication requires an immutable workflow, completion eligibility before
writes, and exact required provider checks on the current full HEAD and tree.
Only eligible current units advance; later incomplete obligations remain open.
A partial delivery whose changed projects all declare `featureAuthority.none`
needs no workflow: no unit could hold a native requirement, so the exact
required provider checks on its head are its evidence and it emits no receipt.
The native result retains a typed hosted receipt referencing the original
workflow and raw provider status. A new workflow revision can replay that
receipt; rewriting the original makes it stale. Standalone planning validation
cannot mint this receipt or promote a unit.
