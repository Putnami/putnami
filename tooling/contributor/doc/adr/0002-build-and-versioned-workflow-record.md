# ADR 0002 — One build loop with a versioned local workflow record

- **Status**: accepted for implementation
- **Scope**: `@putnami/contributor` content and its consumer CLI validator

## Context

The contributor extension already has one execute loop, a v2 candidate
dossier, native feature/spec/decision contracts and provider-backed task and
proposal references. Its older entries grew separate planning names, while a
coordinated request can contain several useful units and later requirements.
A passing first unit must not silently replace the requested outcome.
Original material objections must survive revision and host replacement.

## Decision

`build` is the canonical outcome owner. It reuses the single physical
`execute/references/loop.md` and its records, policy and collaboration paths
for compatibility. `spec`, `design`, `prototype`, `review`, `adr` and `check`
are focused interventions. `plan`, `execute`, `fix`, `epic`, `code-review`
and four `putnami-*` aliases remain compatible entries with their former
arguments and authority. `fix-loop` and `content-bump` remain explicit-only.
Alias removal is outside this pre-1.0 line and requires a separately
announced breaking change. Existing helper paths remain stable.

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
track work. A bounded first unit retains later requirements and any
outcome-changing exclusion needs actual mandate authority. Material artifact
opinions bind to the exact source and relevant context bytes in an original
reviewer-authored report; changing an index digest does not renew them. The
whole-tree candidate binding remains stricter. Handoffs name available
versioned inputs and explicitly identify local assets that cannot travel.

The existing finalizer accepts `--partial-delivery` for an intermediate unit.
It keeps a non-closing parent reference, rejects caller closing directives in
title/body/branch commits, and skips the parent task transition. When a
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
  receipts. The mandate still controls every external action.

## Alternatives rejected

- A second controller or general workflow datastore would duplicate native
  projects, provider state and producer records.
- Adding planning fields to closed feature/spec or hosted review contracts
  would force a broader migration without a needed capability.
- A manually maintained checklist of expected checks would drift from the
  feature declaration and could mark a partial passing report complete.
- Relying on the caller to remember to omit the closing line for an
  intermediate unit would leave an avoidable parent-task closure path.
