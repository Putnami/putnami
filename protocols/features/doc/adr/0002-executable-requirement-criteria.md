# ADR 0002 — Executable requirement criteria live in the feature manifest

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/features` (`protocols/features`)

## Context

`specs validate` proves specs are well formed, but nothing relates a
requirement sentence to the test that protects it. Deleting, renaming, or
skipping that test changes no reported state. Making a requirement executable
needs a machine criterion an author can review, a way for a run to say what
it observed, and a rule that stops a producer from choosing both its target
and its result. [ADR 0001](0001-minimal-spec-contract.md) keeps acceptance
status off the spec requirement, so the criterion must live elsewhere.

## Decision

### 1. The spec wire does not change

`SpecRequirement` keeps exactly `id` and `text`, and `SpecProtocolVersion`
stays `1`.

### 2. The join key is the requirement ID

A spec requirement is executable when the same feature declares a feature
requirement with the same `id` and a verification criterion. Nothing else
joins them: no filename convention, no prose parsing. A feature requirement
without a matching sentence stays valid.

### 3. `putnami.features.json` owns machine criteria, on manifest v2

Each document has its own protocol version: `ManifestProtocolVersion = 2`,
`EvidenceProtocolVersion`, `SpecProtocolVersion` and
`VerificationReportProtocolVersion` all `1`. Manifest readers accept 1 and 2.
A version 1 manifest carrying `verification` is refused with
`features.unknown_field`, so a criterion is never silently dropped.

### 4. The criterion is closed, and a threshold is complete or invalid

`acceptance` names a non-empty, unique set of stable check IDs and carries no
measured fields. `threshold` names exactly one check and requires a bounded
metric and unit, an aggregation, a non-strict operator, a finite target, and
a window. An invocation window carries no duration; a rolling window requires
a positive duration, an environment, a positive freshness bound, and the
`live-verified` stage. `eq` is exact, with no tolerance. `checks` is the
expected set, so a renamed or skipped check becomes missing instead of leaving
an old success green. A criterion must accept `attestation` evidence, because
each observed check is recorded as a source-bound automated attestation.

### 5. A run report is transport, never authority

`FeatureVerificationReport` is a strict, bounded envelope. An observation
states its `(feature, requirement, check)`, an acceptance status or a
measurement with its window, and a project-relative provenance location. It
cannot state an issuer, project, source binding, threshold, or a verdict for a
numeric objective: core derives those from the scheduled task, the owning
project, the current source binding and the authored criterion. The report is
a declared task artifact with the reserved ID `putnami-feature-verification`,
retained with the session; no generated evidence enters the source tree.

### 6. Evaluation is a pure join, not a second reducer

`EvaluateRequirement` and `EvaluateSpecRequirements` read no filesystem and no
clock; the instant is a parameter. They classify checks as `satisfied`,
`violated`, `missing`, `stale` or `unclassified`, and reduce a requirement to
`unmapped`, `unexecutable`, `missing`, `stale`, `contradicted` or `verified`.
An active contradiction always wins, one passing check never hides one that
did not run, and a duplicated observation resolves `missing`. Only a
collector that knows no producer in scope could report a check may reclassify
`missing` as `unobserved`. `CheckEvaluation.EvidenceOutcome` maps results onto
the evidence vocabulary; the repository evidence resolver stays the only
reducer over committed evidence, and nothing here derives a maturity stage.

## Rejected alternatives

- **Status, test paths, or thresholds on a spec requirement.** A second
  authority, and it churns prose for machine reasons.
- **`SpecProtocolVersion = 2` carrying criteria.** It reopens the most-copied
  wire and rewrites every committed spec.
- **A producer-reported verdict for a numeric objective.** A producer that
  reports target and result can make any KPI green; a status on a threshold
  check only says the benchmark did not run.
- **Infer the protecting test from a name.** A rename would silently retarget
  or orphan the proof.
- **One shared protocol version.** Every wire would move whenever the manifest
  gains a field.
- **A committed `verification/*.json` inventory.** A hand-written authority
  with no producer that invites a hand-authored passing record.
- **A tolerance on threshold equality.** A producer could pass by choosing
  its own epsilon.
- **Prefer one report for a duplicated observation.** Duplication is when two
  producers can disagree.

## Consequences

- A project migrates its manifest to version 2 only to declare criteria;
  until then its spec requirements report `unexecutable`.
- Authoring a criterion is a reviewed manifest change, on a path CODEOWNERS
  can protect.
- Widening the criterion vocabulary needs a new ADR.
- A cached or batched run must restore or split the per-project report; a
  cache hit that loses it is a defect, never a verified requirement.
