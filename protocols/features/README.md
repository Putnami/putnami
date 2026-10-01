# Feature intent, evidence, and spec protocol

This module owns the durable, framework-neutral wire contracts for feature
intent, repository evidence, and durable specifications, plus the run-scoped
verification report that connects them. Derived maturity snapshots and deltas
are deliberately outside this package and remain provisional. The JSON contracts
define canonical repository interchange; they do not make JSON the preferred
place to restate technical facts already declared by code.

Each document carries its own version, so adding a field to one wire never
reopens another:

| Document | Constant | Accepted `protocolVersion` |
|---|---|---|
| `putnami.features.json` | `ManifestProtocolVersion` | `1` or `2` |
| `schema/feature-evidence/*.json` | `EvidenceProtocolVersion` | exactly `1` |
| `specs/*.json` | `SpecProtocolVersion` | exactly `1` |
| verification report | `VerificationReportProtocolVersion` | exactly `1` |

## Authoring boundary

Keep the manually maintained surface small and semantic:

- For native Go and TypeScript applications, declare the feature ID, outcome,
  and owner once with `Module.Feature(...)` or `module(...).feature(...)`.
- Framework, build, and test producers own technical facts. Native API, event,
  data, migration, DI, and client registrations populate the disposable design
  graph without a parallel `FeatureContributions` wrapper.
- When one feature spans owners the module tree does not nest — sibling modules
  of a library composer, or file routes a workload root scans — select those
  existing owners from the single declaration. TypeScript exposes this as
  `module(...).feature(definition, { modules, sources })`; Go has no parity yet.
  Do not copy source bindings, hand-author graph edges, add a metadata-only
  wrapper module, or claim a broad common ancestor instead.
- Use `putnami.features.json` only when a durable v1 consumer needs maturity
  targets, relations, or stage requirements that the provisional graph does not
  model.
- Hand-authored evidence is reserved for genuinely human claims such as
  adoption, support, availability, customer proof, or an explicit
  contradiction that cannot be derived from code.
- Snapshots, maps, source bindings, artifact digests, and other mechanical
  projections are generated outputs, never a second authored inventory.

Feature protocol v1 still discovers durable product intent from
`putnami.features.json` at the workspace or project root. That wire remains
readable for human-authority evidence, while framework technical provenance is
derived from native registrations and generated artifacts. The code-first
design graph does not require the JSON file and never copies route, config,
migration, schema, or infrastructure facts into it.

To prove a maturity requirement from a technical fact, state the association
beside the native feature declaration and let the build emit the record. The
whole authoring surface is one tuple — an authored feature, one of its authored
requirements, and one contribution the same producer publishes:

```go
app.Feature{
    ID: "telemetry-putnami-dev/cli-usage-receiver", /* … */
    Proves: []app.FeatureProof{{
        Requirement:  "expiry-runs-without-the-service",
        Contribution: app.ContributionRef{Kind: "migration", Subkind: "sql", Key: "cliagg"},
    }},
}
```

Nothing else is authored. Stage comes from the authored requirement, the issuer
is the build, and `source.binding`, provenance, and the subject identity are
computed from the build's own inventory. A contribution nobody maps stays
`unclassified`, which is a visible state rather than a defect: the alternative
is guessing from directory containment, name similarity, or the nearest feature,
and calling the guess proof. See
[`doc/adr/0003-generated-feature-evidence.md`](doc/adr/0003-generated-feature-evidence.md).

Do not manufacture maturity by repeating one broad artifact or human
attestation for every feature and stage. Distinct requirements need evidence
that independently supports their claim. If the repository has only an
inventory statement and no exact technical or human proof, leave the feature
at `modeled` rather than expanding the statement into synthetic evidence.

## Authored intent

At a workspace or exact project root, `putnami.features.json` contains:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-features.json",
  "protocolVersion": 1,
  "namespace": "billing",
  "features": []
}
```

Feature IDs are lower-case slash-separated semantic IDs whose first segment is
the manifest namespace. Types (`feature`, `journey`), relation kinds (`parent`,
`dependsOn`, `includes`), evidence kinds, and maturity stages are closed wire
vocabularies. Only journeys may use `includes`; `parent` and `dependsOn` cycles
are workspace-aggregation errors.

`modeled` is earned by a valid declaration, so requirements are allowed only
at later stages. Every stage from `coded` through the authored target needs at
least one requirement. Requirements above the target are valid: their evidence
is reported as unclaimed by assessment tooling and cannot raise maturity past
the authored target.

## Executable requirements

A textual spec requirement becomes executable when the same feature declares a
feature requirement with the same `id` and a machine criterion. The join key is
the requirement ID and nothing else: no filename convention, no prose parsing,
no heuristic. The spec wire is unchanged — a spec requirement stays a sentence.

Criteria need manifest `protocolVersion: 2`. Version 1 manifests stay readable
and byte-identical; a version 1 document that carries `verification` is refused
with `features.unknown_field` rather than ignored. Migrate a project only when
it starts declaring criteria.

```json
{
  "id": "lifecycle",
  "stage": "coded",
  "evidenceKinds": ["attestation"],
  "verification": {
    "kind": "acceptance",
    "checks": ["close-visits-every-sink", "flush-visits-every-sink"]
  }
}
```

```json
{
  "id": "delivery-slo",
  "stage": "live-verified",
  "evidenceKinds": ["attestation"],
  "verification": {
    "kind": "threshold",
    "checks": ["production-delivery-slo"],
    "metric": "logging.delivery.success",
    "aggregation": "ratio",
    "operator": "gte",
    "target": 0.999,
    "unit": "ratio",
    "window": { "kind": "rolling", "seconds": 2592000 },
    "environment": "production",
    "maxAgeSeconds": 3600
  }
}
```

- `checks` is the *expected* set: non-empty, unique, and stable. A renamed,
  skipped, or no-longer-executed check becomes missing rather than leaving an
  earlier success green.
- `acceptance` omits every measured field. `threshold` names exactly one check
  and requires a bounded semantic metric and unit, an aggregation from
  `value | count | sum | avg | min | max | p50 | p95 | p99 | ratio`, a
  non-strict operator from `eq | lte | gte`, a finite target, and a window.
- An invocation window is one run or benchmark and carries no duration,
  environment, or freshness bound. A rolling window requires a positive
  duration, an environment, a positive `maxAgeSeconds`, and the `live-verified`
  stage.
- A criterion must accept `attestation` evidence, because an adapter records
  each observed check as a source-bound automated attestation.
- A requirement without `verification` stays an ordinary maturity requirement.
  It simply cannot make a spec requirement executable.

## Verification reports

A test, build, delivery, or runtime job emits one strict, bounded
`FeatureVerificationReport` as a declared task artifact under the reserved ID
`putnami-feature-verification`. It is a run-scoped result envelope, retained
with the session — never a durable authored inventory, and never written into
the source tree.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-feature-verification.json",
  "protocolVersion": 1,
  "observations": [
    {
      "feature": "logging/structured-logging",
      "requirement": "lifecycle",
      "check": "flush-visits-every-sink",
      "status": "passed",
      "provenance": {
        "path": "logger_test.go",
        "symbol": "TestLoggerFlushVisitsEverySinkAndReturnsFirstError"
      }
    }
  ]
}
```

An observation carries either an acceptance `status`
(`passed | failed | skipped`) or a `measurement` with the `window` it covers —
never both. It cannot declare an issuer, project, source binding, threshold, or
verdict for a numeric objective: core derives those from the scheduled task, the
owning project, the current source binding, and the authored criterion, so a
producer never chooses both its target and its result. `provenance.path` is
relative to the reporting task's own project root.

`EvaluateRequirement` and `EvaluateSpecRequirements` perform the join. They are
pure — no filesystem, no clock, no CLI state; the evaluation instant is a
parameter. Declared checks classify as
`satisfied | violated | missing | stale | unclassified` and reduce to one
requirement state:

| State | Meaning |
|---|---|
| `unmapped` | no same-ID feature requirement |
| `unexecutable` | the same-ID requirement declares no criterion |
| `missing` | an expected check did not run, was skipped, was duplicated, or was reported in a shape the criterion cannot accept |
| `stale` | the only support is outside the declared rolling window, environment, or freshness bound |
| `contradicted` | an active check failed or an objective was missed |
| `verified` | every expected check passed or the current objective was met |
| `unobserved` | every unresolved check had no observation source in scope at all |

An active contradiction always wins, and one passing check never hides a check
that did not run. The pure evaluator never returns `unobserved`: with no
observations, "nothing ran" reduces to `missing`, so a reader replaying a record
cannot mistake absence for an excuse. Only a collector that knows which
producers were in scope may reclassify a `missing` requirement into
`unobserved`, and only when every one of its unresolved checks was
`check-not-observed` — a check reported skipped, contradicted, stale, or
uninterpretable rests on an observation that did arrive and stays blocking. `CheckEvaluation.EvidenceOutcome` maps a classification onto
the durable `supports` / `contradicts` vocabulary so a caller can adapt
observations into ordinary in-memory evidence records; the repository evidence
resolver stays the only reducer over committed evidence, and nothing here
derives a maturity stage. Policy, gating, exit status, and the `specs verify`
command are deliberately not part of this wire.

The reasoning, including the alternatives that were rejected, is recorded in
[`doc/adr/0002-executable-requirement-criteria.md`](doc/adr/0002-executable-requirement-criteria.md).

## The spec gate

An earlier change turns the pieces above into an enforceable loop, split by
fixed decision 9: the `@putnami/sdd` extension judges what a run is expected
to prove, and core — which holds every job's results — collects observations
and sanctions, exactly once, in its canonical session reducer.

Three additive documents carry the loop; the spec v1 and evidence v1 wires do
not change:

- **`SpecCriteriaProjection`** (reserved artifact ID `putnami-spec-criteria`)
  is the bounded, deterministic `(feature, requirement) → criterion and
  expected checks` join one spec-owning project's `specs-validate` task
  derives from durable manifests and specs alone. It is a derivation, never an
  authority, and a cache hit restores it byte-identically — a hit that lost it
  would silently disarm the gate.
- **`VerificationReport`** (reserved artifact ID
  `putnami-feature-verification`, above) is what a test run observed.
- **`SpecVerificationRecord`** (`spec-verification.json`, persisted beside a
  session's other documents) is what the gate decided: per (spec-owning
  project, feature), the effective mode with provenance, every textual
  requirement's state with per-check detail, the observation reports read with
  their SHA-256 digests, and the group's one `Blocked` decision. It is derived
  history — the audit surface `putnami specs verify --session` replays — never
  an input to current maturity.

The committed policy is one domain-keyed object under the open command-options
surface, resolved by this package. Project-scoped subjects use
`ResolveVerificationMode`: the spec-hosting project's policy, then the
workspace's, then the built-in `report`. Workspace-scoped subjects use
`ResolveWorkspaceVerificationMode`: workspace, then default, with no arbitrary
project override. The values are exactly `enforce | report | off`; the
`features` and `architecture` keys share the identical vocabulary, and
an unknown domain or value is invalid configuration detected before jobs
execute, never a silent fallback. `GroupBlocks` is the one blocking rule every
surface shares: only `enforce` blocks, every non-verified state except
`unobserved` blocks it, and report can never change an otherwise clean exit.

Automatic architecture validation is workspace-scoped. `enforce` lets a
coherent graph finding block admission, `report` evaluates the same graph and
keeps its typed findings advisory, and `off` skips only the automatic DAG task.
Unreadable policy and structural/provider-view failures remain blocking; the
interactive architecture commands are explicit requests and do not consult
this automatic-adoption mode.

Tests bind themselves to declared checks through each runtime's native
producer: in Go, `go.putnami.dev/protocol/features/spectest` —
`spectest.Proves(t, feature, requirement, check)` records the check's verdict
in `t.Cleanup`; in TypeScript, `specTest(name, binding, fn)` from
`@putnami/spectest` (re-exported as `@putnami/runtime/spectest`) wraps the
`bun:test` registration and records the verdict the runner observed; in
Python, the `putnami_proves(feature, requirement, check)` pytest marker —
inert metadata for bare pytest — is read by the adapter-injected plugin, which
records the verdict from pytest's own run report. All three write the same
process-local fragment wire, and every adapter merges through the shared trust rules in
`tooling/extension-sdk/specreport`, so the reserved report's bytes are
equivalent across runtimes by construction. Every helper is inert outside a
Putnami-provided fragment directory and can never alter a test's own verdict;
core still recomputes every requirement state against the authored criterion,
so no producer — helper included — can self-certify.

Threshold criteria have a measured producer beside the acceptance one in each
runtime, publishing one aggregate (`name`, `aggregation`, `value`, `unit`)
with the honest window the observing test actually covered — the invocation
span for an invocation objective, or the observed range and `environment` for
a rolling one:

| Runtime | Measured producer |
| --- | --- |
| Go | `spectest.ObserveMeasurement(t, feature, requirement, check, Measurement{...})`; window = call instant to cleanup instant |
| TypeScript | `observeMeasurement(name, binding, fn)`, whose body returns the `Measurement`; window = body entry to body settle |
| Python | the `putnami_observes(feature, requirement, check)` marker plus `record_property("putnami_measurement", {...})`; window = the call phase's own span |

The fragment wire refuses a fragment that carries both a status and a
measurement, and every helper publishes only from a passing test — a failed or
skipped measuring test publishes nothing at all, because a measurement
fragment has no verdict field to carry that rejection and absence resolves as
missing. So a producer can state a number but never its verdict: core
recomputes every threshold verdict from the authored target (`eq | lte | gte`,
inclusive), and judges rolling freshness — environment match, window coverage,
`maxAgeSeconds` — against the evaluation instant. `putnami specs verify`
renders the measured aggregate beside the recomputed verdict.

Rolling objectives are not measured by unit tests: their observations come
from delivery and runtime producers that state the real observed window and
environment on the report wire directly.

### The rollout ratchet

The committed `specs.baseline.json` files (`ParseSpecsBaseline` …
`CompareSpecsBaseline` here, JSON schema `putnami-specs-baseline.json`) record
the **enforced floor**: which projects committed to
`options.sdd.verification.specs = "enforce"`, and which executable
`feature#requirement` identities each covers. Each enforced project commits
its own file in its own directory (`SpecsBaselinePath`): a version 2
`ProjectSpecsBaseline` that lists only the covered identities, because the
directory names the project and the file's presence states `enforce`. The
floor is the union of those files. Version 1, one workspace-root document
naming every project, is still read at the root so a workspace can move off
it. The comparison is
pure and shrink-only — a recorded project that regressed out of `enforce`, or
lost a recorded requirement while still enforced, is a
`features.ratchet_regression` error naming the baseline edit as the reviewed
policy change; growth in either direction never produces a finding and is only
counted so a consumer can nudge (the extension does, as a
`features.ratchet_growth` warning). The `@putnami/sdd` extension runs the
comparison as the `specs-ratchet-validate` step of `validate-workspace` and
maintains the file with `putnami specs baseline --update`; both sides of the
judgment are the committed worktree, so the check stays cacheable and no git
history enters the verdict.

## Evidence

Evidence fragments live at direct children of
`schema/feature-evidence/*.json` (or `.gen/schema/feature-evidence/*.json`
before promotion):

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-feature-evidence.json",
  "protocolVersion": 1,
  "evidence": [
    {
      "id": "billing/migration-proof",
      "feature": "billing/invoice-export",
      "requirement": "implementation",
      "stage": "coded",
      "outcome": "supports",
      "issuer": { "kind": "build", "id": "go.putnami.dev/app" },
      "source": {
        "root": "project",
        "ownerProject": "go.putnami.dev/example/billing",
        "binding": "source-v1:sha256:0000000000000000000000000000000000000000000000000000000000000000"
      },
      "subject": {
        "kind": "capability",
        "contribution": {
          "ownerProject": "go.putnami.dev/example/billing",
          "kind": "migration",
          "subkind": "sql",
          "key": "billing"
        }
      },
      "provenance": {
        "root": "project",
        "path": "migrations.go",
        "symbol": "BillingMigrations"
      }
    }
  ]
}
```

Authority is explicit on every record; there is no document-level inheritance.
`subject` is a tagged union with exactly one matching `contribution`,
`artifact`, or `attestation` payload. Contribution validation preserves the
canonical `capabilities.*` diagnostic codes. An evidence provenance location
must use the same root as its source selector, so the source's exact
workspace/project/package metadata resolves both locations without fallback.
Workspace selectors omit owner/package/version metadata; project selectors
carry only `ownerProject`; package selectors carry only exact `package` and
`version` values.

Persistent validity is permitted only for human-issued attestations. The
product-promise claim codes `adoption`, `support`, `availability`, and
`customer-proof` also require human authority. `observedAt` is accepted only
for attestation subjects: capability currency is source-binding based and
artifact currency is binding-plus-digest based, regardless of issuer kind.
`environment` is a bounded semantic code rather than free-form tenant or
configuration data.

Framework-emitted evidence, `source.binding`, and artifact digests are derived
values. Regenerate and promote the owning producer output when they change; do
not patch those fields by hand or introduce a repository-wide binding-refresh
workflow as normal feature authoring.

`BuildGeneratedEvidence` is the one resolver every build and test producer uses
to turn explicit mappings into records. It is pure — no filesystem, no clock, no
discovery — so the same input yields the same bytes in every language, and the
shared `generated-evidence.golden.json` vector pins Go against TypeScript.
`GeneratedEvidenceID` derives each identity as
`<feature>/<requirement>/<12 hex of the contribution digest>`, so re-running a
producer over unchanged sources reproduces the same document byte for byte.

A mapping publishes only when its feature, requirement, and contribution all
already exist, the requirement accepts `capability` evidence, and the
contribution belongs to the producing project. Anything else returns diagnostics
and no document: a partial generated file is indistinguishable from a complete
one, so a mapping deleted by mistake would look like a feature that legitimately
lost a proof. `schema/capabilities.json` and `schema/feature-evidence/` are
excluded from the source binding, so committing generated output never
invalidates the record that produced it; any other source change does, and the
resolver reports the committed record stale rather than rebinding it.

## Specs and durable decisions

A spec is the prose half of spec-driven development: what a change is for, what
it deliberately will not do, the sentences the team agreed to, and where the
durable decisions behind them live. It is the only artifact here that no
producer emits, and the only one that carries no derived facts at all.

**Canonical home.** Specs are direct JSON children of `specs/` at the workspace
root or at an exact project root — `specs/*.json`, never a nested directory and
never a file that discovery merely happens to reach. The filename is free text:
identity is the `feature` field, exactly as with evidence fragments, so a rename
can never retarget or orphan a spec.

**Authoring rules.**

- One spec details exactly one already-authored feature. If the feature does not
  exist yet, declare it first — a spec never mints a feature, a maturity stage,
  an evidence record, or a design-graph node.
- Exactly one spec per feature across the whole workspace.
- Never copy derived facts. Current maturity, owners, source bindings, routes,
  schemas, and migrations belong to the feature declaration, the design graph,
  and evidence; a spec that restates them is a second inventory that goes stale.
- `outcomes` and `nonGoals` are ordered prose and keep their authored order.
  `requirements` are stable sentences with spec-local IDs so a review or a
  decision can name the exact statement that changed; they are not the evidence
  ladder, which `putnami.features.json` requirements already own.
- `decisions` links to durable decision records under the existing project-local
  `doc/adr/*.md` convention, by workspace-relative path. Copy
  [`doc/adr/TEMPLATE.md`](doc/adr/TEMPLATE.md) to
  `<project>/doc/adr/NNNN-kebab-title.md` and link the result. There is no
  second decision wire protocol, and decision content is never embedded.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-spec.json",
  "protocolVersion": 1,
  "feature": "billing/invoice-export",
  "outcomes": [
    "A customer exports an issued invoice without leaving the billing workspace",
    "An exported invoice stays readable after the issuing revision is superseded"
  ],
  "nonGoals": [
    "Exporting a draft invoice",
    "Exporting invoices owned by another tenant"
  ],
  "requirements": [
    {
      "id": "export-format",
      "text": "An export states its format in the response content type and never guesses it from the file name."
    },
    {
      "id": "retention",
      "text": "An exported invoice stays retrievable for the full invoice retention period."
    }
  ],
  "decisions": [
    "protocols/features/doc/adr/0001-minimal-spec-contract.md"
  ]
}
```

**Relationship to features and evidence.** The feature declaration owns
identity, owner, target, and the requirements that earn maturity. Evidence
proves those requirements. A spec sits beside both and adds only intent: it
resolves against the authored catalog and cannot extend it. `ValidateSpec`
checks one document's shape; `ValidateSpecRepository` checks discovery location,
feature resolution against an explicitly supplied authored catalog
(`AuthoredFeatureIDs`, unioned with native design-graph feature nodes by callers
that have them), and one-spec-per-feature uniqueness. Whether a linked decision
record exists on disk is a filesystem fact for tooling, not part of the wire
contract.

The reasoning, including the alternatives that were rejected, is recorded in
[`doc/adr/0001-minimal-spec-contract.md`](doc/adr/0001-minimal-spec-contract.md).

### The decision registry

A spec's `decisions` array links the prose that explains a choice. A
committed `decisions.json` (`ParseDecisionRegistry` … `EvaluateDecisionCheck`
here, JSON schema `putnami-decisions.json`) is the other half: the
**enforceable** statement, which `validate` can prove.

A registry's scope is its directory. The one at the workspace root holds the
decisions every project follows; the one in a project directory holds that
project's own. Every path a registry names — its check globs and its `adr`
link — is relative to its directory and cannot leave it. Ids are unique across
all registries of a workspace. `DecisionRegistryPath` and
`DecisionScopeRelative` spell those two rules.

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-decisions.json",
  "protocolVersion": 1,
  "decisions": [
    {
      "id": "D-001",
      "statement": "serverless workloads scale to zero when idle",
      "settled": "2026-09-03",
      "settledBy": "fdumay",
      "adr": "protocols/infra/doc/adr/0003-deployers-own-runtime-cost-policy.md",
      "check": {
        "kind": "json-value",
        "files": ["**/infra/requirements.json"],
        "pointer": "/scaling/minInstances",
        "rule": "equals",
        "value": 0,
        "whenMissing": "satisfied"
      }
    },
    {
      "id": "D-002",
      "statement": "every pull request names what it deletes",
      "settled": "2026-09-03",
      "settledBy": "fdumay",
      "reviewOnly": true
    }
  ]
}
```

Every entry carries a stable id, the one-line statement, the date it was
settled and who settled it, plus **exactly one** of:

- a `check` — v1 has one kind, `json-value`. Every file in the registry's
  directory matching any glob in `files` is read; `pointer` is resolved per RFC 6901 (`~0` and `~1`
  included); `equals` is violated when the resolved value differs and
  `notEquals` when it matches. Numbers compare by value, so `0`, `0.0` and `-0`
  agree, and a resolved object or array never equals a scalar. Zero matching
  files is satisfied — a repository with no such file cannot violate the
  decision — while a matched file that is not valid JSON is a **violation**
  naming the parse error, never a skip. `whenMissing` decides a file whose
  pointer resolves to nothing and defaults to `satisfied`.
- `"reviewOnly": true` — the mark of a statement no check can prove. It is
  injected into the generated agent guidance and never fails a build.

`adr` is optional and is **justification only**: the record explains why the
decision was taken and never restates the statement, so there is no second copy
of a rule to drift.

The registry validates itself and fails closed: duplicate ids, a missing field,
a `settled` date that is not `YYYY-MM-DD`, an unknown `kind`, `rule` or
`whenMissing`, a check with no glob, or an `adr` outside the `doc/adr`
convention all produce `features.invalid_decision_registry`. An **absent**
`decisions.json` is adoption, never a failure. The `@putnami/sdd` extension
proves every registry as the `decisions-validate` step of `validate-workspace`.
`putnami context generate` renders every root entry into the workspace's
`AGENTS.md` guidance block and names each project registry there; the
`putnami.context` MCP tool returns a project's own decisions with that
project.

The reasoning is recorded in
[`tooling/cli/doc/adr/0030-settled-decisions-are-a-registry-validate-reads.md`](../../tooling/cli/doc/adr/0030-settled-decisions-are-a-registry-validate-reads.md).

## Agent and review consumption

Agents and reviewers should query the narrowest projection that answers the
task:

- `putnami features validate` for structural validity and counts;
- `putnami features inspect <feature-id>` for one feature, its requirements,
  evidence, and diagnostics;
- `putnami features diff <base> <head>` for revision changes.

Those commands and the MCP tools below are provided by the `@putnami/sdd`
extension, not by the CLI itself; a workspace that does not declare it has
neither.

Through the local Putnami MCP server, use `sdd.list_features` (optionally with
query terms) to discover the small semantic catalog and `sdd.feature_context` to
retrieve one compact authored feature. These tools merge only explicit authority from
native design feature nodes and durable `putnami.features.json` files at the
workspace or exact project roots. They preserve every sorted declaration source
and durable manifest field; technical categories and critical paths appear only
when a native graph declares them. A spec, nested file, directory name, or
heuristic never mints a feature. `agent_context` also exposes the native feature
summaries for its selected project while keeping the durable agent-context
document unchanged.

Do not inject an entire manifest/evidence corpus or a full snapshot into an
agent prompt when a scoped inspection is sufficient. Whole-corpus JSON is a
generator, conformance, and protocol-debugging surface, not the default context
surface for implementation work.

## Strictness and canonical bytes

Readers accept only the exact integer version tokens listed in the table above,
reject every unknown field, validate semantic
duplicates/references/stages/paths, and return stable structured diagnostics.
`CanonicalManifest`, `CanonicalEvidenceDocument`, `CanonicalSpec`, and
`CanonicalVerificationReport` copy before sorting; they never reorder caller
slices or share a caller-owned criterion pointer. Canonical bytes use declared
field order, two-space indentation, and one trailing newline. Canonical ordering
sorts keyed identities only — expected checks, evidence records, spec
requirements, observations — while authored prose such as spec outcomes and
non-goals keeps its order, because reordering it would rewrite the statement
rather than normalize it.

Use `ValidateManifest`/`ValidateEvidenceDocument`/`ValidateSpec`/
`ValidateVerificationReport` for local shape validation. Use path-aware
`ValidateRepository` for workspace-wide feature/evidence IDs, cross-document
relations, cycles, and feature/requirement/stage references, and
`ValidateSpecRepository` for spec discovery locations, feature resolution, and
one-spec-per-feature uniqueness. The reusable byte-conformance runners and the
Go/TypeScript/Python-authored and hand-authored vectors live under
`conformance/` and `fixtures/equivalence/`.

## Ownership and support status

- **Owner**: `go.putnami.dev/protocol/features` (`protocols/features`).
- **Status**: `preview` — recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json), with no `default` or
  `parity` claim.
- **Evidence**: the `@putnami/sdd` extension consumes these contracts for feature
  and spec discovery, validation, inspection, initialization, and diffing, and
  runs the first two as DAG jobs. The same builders expose agent-facing MCP
  `sdd.list_features`, `sdd.feature_context`, `sdd.list_specs`, and
  `sdd.spec_context` surfaces. Strict parsers, repository validators, canonical
  marshallers, the pure verification evaluator, valid/invalid fixture corpora,
  reusable conformance runners, and Go/TypeScript/Python-authored equivalence
  vectors exercise the wire contracts.
- **Boundary**: the feature-intent, evidence, provisional design-graph, spec,
  and verification-report contracts are still evolving, and no consumer outside
  this repository is guaranteed. No producer emits a verification report yet,
  and no gate consumes one. Those constraints keep the reviewed status at
  `preview`.
