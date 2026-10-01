# ADR 0001 — The check-code taxonomy is frozen, and adding a code is additive

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/doctor` (`protocols/doctor`)

## Context

A check code is the join key of this contract: the engine writes it into a
report, a committed `doctor.waivers.json` names it, and evidence producers
target it. A committed waiver file outlives the release that wrote it, and it
fails closed: a waiver naming an unknown code is rejected and blocks the gate.
Moving a code is paid for by every repository that accepted a finding.

## Decision

1. **A code is frozen once merged.** Renaming, repurposing, or removing a
   `CheckCode`, or changing the closed `Severity` or `Profile` enums, requires a
   `ProtocolVersion` bump.
2. **Adding a code does not bump the version.** Documents written against the
   older vocabulary stay valid. An older parser meeting a newer code already
   rejects it (`doctor.invalid_check_code`).
3. **Every code carries a baked remediation.** `CheckRemediations` holds one
   actionable sentence per code, repeated in each finding.
4. **Expiry stays outside the contract.** The package checks only that
   `expires` parses; the engine compares it to an injected clock, so
   serialization stays reproducible.
5. **The rules are pinned by tests.** `TestCheckCodeTaxonomy_Frozen` (every
   required ID present, every code has a remediation, maps agree),
   `TestEnums_ValuesMatchMaps`, and `TestProtocolVersion_SchemaMatches` (for the
   report and waiver shapes in `schemas/doctor.json`).

## Rejected alternatives

- **Bump the version per added code.** Every committed waiver file would fail
  closed and block gates, for no reader benefit.
- **Free-form codes.** A typo and a newer code become indistinguishable;
  `doctor.waiver_unknown_code` exists only because the set is closed.
- **Older parsers ignore unknown codes.** A readiness gate cannot silently drop
  a finding.
- **Engine-chosen remediation text.** Two producers would word it differently.
- **Numeric per-finding severity.** Severity is a function of the profile;
  a free scale makes reports incomparable.

## Consequences

- A code name is a permanent public identifier; naming is part of review.
- The taxonomy only grows. A retired check keeps its code reserved.
- Consumers fail closed on unknown codes; forward tolerance means upgrading.
