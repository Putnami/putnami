# Doctor Protocol

> **This package is the contract, not the engine.** It pins the v1 wire
> contract, the check-code taxonomy, and strict validation for `putnami doctor`.
> The checks themselves, profile resolution, and waiver-clock evaluation live in
> the CLI (`tooling/cli/internal/commands/doctor*.go`), which consumes this
> package read-only.

## Why

Promoting a workload to production has a long tail of easy-to-miss readiness
gaps: an auth signing key generated in-process, a datasource left on an
in-memory store, a plaintext endpoint, an in-process rate limiter that does not
hold across replicas, a stale committed schema. `putnami doctor` surfaces these
as a single, reviewable report evaluated under a deployment **profile**, and
lets a team consciously **waive** a finding in a committed file that is reviewed
like any other source.

This package fixes the two wire shapes that report travels on — the **Report**
and the committed **WaiverFile** (`doctor.waivers.json`) — plus the frozen
**check-code taxonomy** every finding draws from. It is the contract, not the
engine.

## What

### Vocabulary

- **Severity** (closed enum, ascending urgency): `info`, `warning`, `high`,
  `critical`.
- **Profile** (closed enum, ascending strictness): `dev`, `test`, `production`.
  Callers outside this package select and grade by these exact value strings, so
  they are frozen.
- **Check codes** (`ValidCheckCodes`, a frozen taxonomy) — each with a
  baked, actionable remediation in `CheckRemediations`:

  | Check code | Meaning | Remediation (baked) |
  | --- | --- | --- |
  | `doctor.incomplete_capability` | A capability is missing a required provider. | Declare the missing provider, then re-run `putnami doctor --profile production`. |
  | `doctor.missing_required_config` | A profile-required config key is unset. | Set the key (or supply a binding) and re-run `putnami doctor`. |
  | `doctor.volatile_persistence` | A datasource is bound to an ephemeral store. | Bind it to a durable, provisioned store before promoting. |
  | `doctor.ephemeral_signing_key` | An auth signing key is generated in-process. | Configure a persistent, externally managed signing key. |
  | `doctor.insecure_transport` | An endpoint is served over plaintext. | Require TLS/HTTPS for the endpoint. |
  | `doctor.local_rate_limit` | An in-process limiter does not hold across replicas. | Replace it with a shared/distributed limiter. |
  | `doctor.invalid_generated_schema` | A committed generated schema is stale. | Regenerate with `putnami build` and commit the artifact. |
  | `doctor.config_shadowing` | A config key is owned by more than one source. | Remove the shadowing source or rename the key. |
  | `doctor.waiver_expired` | A waiver's expiry has passed. | Renew `expires` in `doctor.waivers.json` or fix the finding. |
  | `doctor.waiver_unknown_code` | A waiver references an unknown check code. | Correct the waiver's `code` or remove it. |
  | `doctor.committed_manifest_stability` | A committed generated artifact carries workspace-derived content (workspace version, dependency-closure enumeration, source binding). | Regenerate and commit; committed content must depend only on the owning project's declared inputs. |
  | `doctor.undeclared_schema_commit` | A project commits generated schemas without declaring `options.generate.schema`. | Declare the regime explicitly (`false` keeps them in `.gen/`, the recommended default for applications). |
  | `doctor.crlf_checkout` | A tracked text file has CRLF endings in the working tree while the index has LF, or `core.autocrlf=true` with no LF policy in `.gitattributes`. | Add `* text=auto eol=lf` to the workspace-root `.gitattributes`, commit it, and check the files out again. |
  | `doctor.long_paths_disabled` | On Windows, the `LongPathsEnabled` registry value is not 1. | Set `HKLM\SYSTEM\CurrentControlSet\Control\FileSystem\LongPathsEnabled` to 1 from an elevated PowerShell. |
  | `doctor.git_long_paths_disabled` | On Windows, Git does not set `core.longpaths`. | Run `git config --global core.longpaths true`. |
  | `doctor.vc_runtime_missing` | On Windows, the Microsoft Visual C++ runtime (`vcruntime140.dll`) is not in the system directory, so Biome cannot start. | Install the Visual C++ Redistributable for x64 with `winget install --id Microsoft.VCRedist.2015+.x64` or its installer. |
  | `doctor.missing_readme` | A project has no `README.md` at its root. | Add one with a one-line purpose, the project's commands, and where its docs live. |

  The IDs are frozen once merged: config and capability evidence is produced
  targeted at these exact codes (notably `doctor.ephemeral_signing_key`), and a
  committed `doctor.waivers.json` names them. Renaming, repurposing, or removing
  a code — or changing any closed enum — requires a `ProtocolVersion` bump.
  Adding a code is additive and does not: documents written against an earlier
  vocabulary stay valid, and an older parser meeting a newer code already fails
  closed. The reasoning is recorded in
  [`doc/adr/0001-frozen-check-code-taxonomy.md`](doc/adr/0001-frozen-check-code-taxonomy.md).

### Wire shapes

- **Report** — `{ $schema?, protocolVersion, profile, findings[], summary }`,
  emitted per profile evaluation.
  - **Finding** — `{ code, severity, profile, project, message, evidence?, remediation, waivedBy? }`.
  - **Evidence** — `{ path?, field? }`. Deliberately has **no slot for a
    resolved value**: a report is committed and reviewable and must never carry
    cleartext configuration values, which may be secrets. Evidence names *where
    to look*, not *what was found*.
  - **WaiverProvenance** (`waivedBy`) — `{ owner, reason, expires }`, set by the
    engine when a committed waiver matched the finding.
  - **Summary** — `{ findings, info, warning, high, critical, waived }`; the
    per-severity counts sum to `findings` and `waived` counts findings that
    carry `waivedBy`. Validation recomputes and rejects a summary that lies.
- **WaiverFile** (`doctor.waivers.json`, workspace root) —
  `{ $schema?, protocolVersion, waivers[] }`.
  - **Waiver** — `{ code, project?, owner, reason, expires }`. An empty
    `project` makes the waiver apply workspace-wide; a set `project` scopes it.

### Waiver semantics and the expiry boundary

A waiver matches a finding by `code` and, when `project` is set, by project. The
committed file is the reviewed record of accepted findings.

**Expiry is data, never evaluated here.** A `Waiver.expires` and a
`Finding.waivedBy.expires` are validated only for *parsability* (RFC 3339 date
`YYYY-MM-DD` or full timestamp). This package never compares them to a clock:
expiry evaluation lives in the CLI engine with an injected clock so the decision
is deterministic and testable, and a passed expiry is surfaced as a
`doctor.waiver_expired` finding rather than computed here. Keeping time out of
the package is what makes serialization and validation pure and reproducible.

## How

Tooling consumes each shape through parse + validate entry points:

- `ParseReport` / `ParseWaiverFile` — strict parse (unknown fields rejected).
- `ValidateReport` / `ValidateWaiverFile` — structural and semantic validation.
- `ParseAndValidateReport` / `ParseAndValidateWaiverFile` — parse then validate.

Diagnostics use the shared `go.putnami.dev/protocol/diagnostic` shape with a
stable taxonomy (`ValidDiagnosticCodes`), distinct from the findings vocabulary:

`doctor.parse_error`, `doctor.unknown_field`, `doctor.invalid_protocol_version`,
`doctor.invalid_severity`, `doctor.invalid_profile`, `doctor.invalid_check_code`,
`doctor.missing_project`, `doctor.missing_message`, `doctor.missing_remediation`,
`doctor.invalid_evidence`, `doctor.invalid_summary`, `doctor.missing_owner`,
`doctor.missing_reason`, `doctor.invalid_expires`, `doctor.duplicate_waiver`.

### Determinism

Field order in every struct is deliberate; the canonical serialization is
`json.MarshalIndent(v, "", "  ")+"\n"`. `fixtures/valid/full.json` and
`fixtures/waivers/valid/full.json` are the exact canonical forms;
`determinism_test.go` pins them and asserts serialization is stable across 100
marshals and idempotent under round-trip.

### Fixtures

The two shapes have separate corpora so each parser is exercised in isolation:

- `fixtures/valid/` and `fixtures/invalid/` — Reports.
- `fixtures/waivers/valid/` and `fixtures/waivers/invalid/` — WaiverFiles.

`conformance_test.go` runs every valid fixture clean and asserts every invalid
fixture produces at least one coded diagnostic; there is one invalid fixture per
diagnostic code across the two corpora.

### Versioning and compatibility

`ProtocolVersion` is `1`, and `version_test.go` asserts
[`schemas/doctor.json`](schemas/doctor.json) pins exactly that value for both
the `report` and `waiverFile` shapes.

| Change | Version impact |
| --- | --- |
| Adding a `CheckCode` (with its baked remediation) | **additive** — no bump |
| Adding an optional member to a shape | **additive** — no bump |
| Adding a fixture, valid or invalid | **additive** — no bump |
| Renaming, repurposing or removing a `CheckCode` | **breaking** — bump |
| Changing the `Severity` or `Profile` enums | **breaking** — bump |
| Removing a member, or making an optional one required | **breaking** — bump |
| Changing field order (and therefore the canonical bytes) | **breaking** — bump |

A bump means a new `ProtocolVersion` constant, the schema updated to pin it, and
new canonical fixtures. Adding a code deliberately does NOT bump, because a bump
would invalidate every already-committed `doctor.waivers.json` — which fails
closed and blocks the gate — in exchange for nothing a reader can act on. The
other direction is already safe: an older parser meeting a newer code rejects it
as out-of-taxonomy rather than accepting it silently.

## Producers and consumers

| Role | Who |
| --- | --- |
| Report producer | `putnami doctor` (`tooling/cli/internal/commands/doctor*.go`), the only first-party evaluator |
| WaiverFile producer | a human, committing `doctor.waivers.json` at the workspace root and reviewing it like source |
| Consumers | the CLI's renderers and gate, CI systems reading the structured report, and any reviewer reading the committed waiver file |
| Owner of this contract | this project. The CLI evaluates; it does not own the vocabulary or the shapes |

## Schema and corpus at a glance

- Schema: [`schemas/doctor.json`](schemas/doctor.json) — both the `report` and
  `waiverFile` shapes, pinned to `ProtocolVersion` by `version_test.go`.
- Report corpus: [`fixtures/valid`](fixtures/valid) (2) and
  [`fixtures/invalid`](fixtures/invalid) (11).
- WaiverFile corpus: [`fixtures/waivers/valid`](fixtures/waivers/valid) (3) and
  [`fixtures/waivers/invalid`](fixtures/waivers/invalid) (7).

See [Fixtures](#fixtures) above for why the two corpora are separate.

## Support status

- **Subject**: `go.putnami.dev/protocol/doctor`, kind `protocol`.
- **Status**: `stable`, recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) — the only reviewed
  authority for support status (contract:
  [`protocols/support/README.md`](../support/README.md)).
- **Owner**: this project (`protocols/doctor`). It owns the taxonomy, the enums
  and the two shapes; the CLI is a read-only consumer of that authority.
- **Evidence**: `TestProtocolVersion_SchemaMatches` pins the schema to
  `ProtocolVersion` for both shapes; `TestCheckCodeTaxonomy_Frozen` pins every
  required check code, its baked remediation, and the agreement of the two maps;
  `TestEnums_ValuesMatchMaps` pins the closed `Severity`/`Profile` enums against
  their lookup maps; `TestDiagnosticTaxonomy_Membership` pins the diagnostic-code
  set. Canonical bytes are pinned by `TestSerialization_CanonicalByteForm`,
  stability across 100 marshals by `TestSerialization_Stable100`, and round-trip
  idempotence by `TestRoundTrip_Idempotent`. The corpus is 5 valid and 18 invalid
  fixtures covering the 15 diagnostic codes, and the conformance tests assert
  every valid fixture parses clean and every invalid one produces at least one
  code from the closed diagnostic taxonomy.
- **Cross-implementation parity is claimed by nothing.** There is one
  implementation (this Go package) and one evaluator (the CLI). `stable` is a
  commitment about how this wire evolves, not a statement that a second
  implementation exists.
- **`default` and `parity`**: no claim is recorded on either axis. Omission is
  not a denial — see the support vocabulary.

## Specs and durable decisions

There is deliberately **no user-facing feature or spec for this module**. What a
user wants is to be told, before promoting a workload, what would break — and
that outcome is owned by the `putnami doctor` command, not by the JSON its
report travels in. A product feature per technical wire contract would be a
promise with no user behind it and a second authority beside the schema.

The user-facing surface that owns the outcome is **`putnami doctor`** together
with the committed `doctor.waivers.json` workflow. A spec for a doctor change
belongs with that command.

Durable decisions recorded here:

- [`doc/adr/0001-frozen-check-code-taxonomy.md`](doc/adr/0001-frozen-check-code-taxonomy.md)
  — a check code is frozen from the moment it merges, adding a code is additive
  and does not bump the version, and every code carries a baked remediation.
