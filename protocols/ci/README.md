# `go.putnami.dev/protocol/ci`

The versioned contract for a workspace's source-controlled `putnami.ci.json`,
for the ChangePlan document `putnami change-plan` emits (see
[The change plan](#the-change-plan)), and for the ImpactPlan document
`putnami impact-plan` emits (see [The impact plan](#the-impact-plan)). The file is discovered beside
`putnami.workspace.json`. Version 3 is the only version this package reads:
earlier documents are migrated by hand.

A version 3 document declares CI intent and optional review guidance:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-ci.json",
  "version": 3,
  "commands": ["lint", "test", "build", "validate", { "name": "audit", "failOnError": false }],
  "flags": ["--enforce-coverage"],
  "runner": { "timeoutMinutes": 45 },
  "distribution": {
    "namespace": "putnami",
    "visibility": "internal",
    "registries": { "npm": { "mirror": { "to": "https://registry.npmjs.org" } } },
    "channels": { "canary": { "visibility": "internal" }, "latest": { "visibility": "public", "protected": true } },
    "versions": { "stable": "public", "prerelease": "internal" },
    "members": [{ "select": "tag:public-lib", "visibility": "public" }]
  },
  "rules": [
    { "branches": "main", "publish": ["canary"] },
    { "tags": "wip/*", "publish": false },
    { "pullRequests": true, "publish": ["pr-{number}"], "retain": "while-open" }
  ],
  "envs": { "staging": { "channel": "canary" } }
}
```

- `commands` names the canonical framework graph roots every run executes, in
  the order the CLI runs them, as one invocation
  `putnami lint,test,build,validate --impacted --enforce-coverage`. An entry is
  a bare name when it blocks, which is the default, or an object
  `{ "name", "failOnError": false }`: the command runs, its errors are reported
  as warnings, and the run passes.
- `flags` is appended verbatim to that invocation; each command takes the flags
  it declares.
- `runner` carries what the runner needs to configure itself and nothing else.
  Its content is open and owned by the runner implementation, so this package
  preserves it and only re-serializes it with sorted keys.
- `distribution` is what the repository declares to its release-set provider:
  its `namespace`, the visibility inheritance chain
  (`visibility` > `registries.<eco>.visibility` > `channels.<c>.visibility` >
  `versions.{stable,prerelease}` > the publish `--visibility` > `members[]`;
  a project's own `putnami.json` `distribution.visibility`, or its scope's,
  fills the member link too, wins over `members[]`, and must agree with a
  `members[]` rule that selects the same project),
  `channels.<c>.protected` for a channel no rule may target, and
  `registries.<eco>.mirror.to`, the external target the backend copies `public`
  members to. Validation requires an HTTPS URL or native registry destination,
  at most 2048 bytes, without credentials, query strings or fragments.
  `memberAttribution: true` opts the repository into recording each member's
  source `project` and artifact `kind` on the sets it publishes
  (`protocols/distribution` ADR 0005). It is off by default: every consumer of
  a set decodes it strictly, so the extensions the workspace pins and its
  release-set provider must know the two fields before a publisher emits them.
  `memberSourceTree: true` opts the repository into recording, on each member
  it republishes from a clean checkout, the `sourceTree` it was built from
  (`protocols/distribution` ADR 0005). It is off by default for the same
  reason.
- `rules` is an ordered first-match list. A rule selects exactly one trigger —
  `branches`, `tags`, or `pullRequests: true` — and lists in `publish` the
  channels a green run advances, in `--impacted` mode. A tag publishes by
  convention without a rule; a `tags` rule with `publish: false` opts out. A
  branch or a pull request with no rule runs the commands only. `{number}` is
  the sole channel placeholder, allowed once and only on a `pullRequests` rule.
  `retain` declares how long those channels live (see below); without it the
  rule declares no lifetime. `baseline` names one channel the publish measures
  impact against and never advances (see below); without it a first publish
  into a channel that has no head republishes every member.
- `envs` declares the environments that follow a channel: when the channel
  moves, the environment synchronizes its workloads on the named set under its
  `constraints` (`approval: manual` is the one every implementation supports),
  its `rollout`, and its ordered `workloads` selection rules. An environment
  without a `channel` only reacts to the command a person or an agent types.
- `review` is an optional closed profile with `enabled`, `fallbackEngine`
  (`codex` or `claude-code`), one to six distinct `focus` areas
  (`correctness`, `security`, `architecture`, `performance`,
  `maintainability`, `tests`), and zero to 32 ordered `instructions`.
  An instruction has 1–2,000 Unicode characters, no control characters, and no
  surrounding whitespace. All four properties are required when the profile
  is present; omission requests no review and explicit `enabled: false`
  disables that intent. The review plane must load it from the target revision.
  Credentials, image choices, commands, MCP policy and approval authority
  remain server-controlled and cannot be added to this object.

```json
"review": {
  "enabled": true,
  "fallbackEngine": "codex",
  "focus": ["correctness", "security"],
  "instructions": ["Check workspace isolation and concurrency."]
}
```

Review guidance does not change CI triggers or gate execution and does not
require a release-set provider. Its presence requests no enrollment: the
review plane still grants repository and engine access independently.

Globs and selectors are authored as one string or an array of strings; both
forms decode to the same value, and the canonical document writes the single
value back as a bare string.

The document names no job and no event envelope: the execution plane owns the
envelope, configuration, secrets, and authorization, and the DAG is one
invocation. That is what makes the native runner DAG-native without imposing a
workflow.

The package owns the JSON Schema, strict parser, field-addressable diagnostics,
normalization and SHA-256 digest, slash-aware glob matcher, command-reference
validation, and safe local explanation.

Parsing rejects documents over 256 KiB, duplicate object keys, unknown fields,
unsupported versions, invalid globs, duplicate triggers, rules that select no
trigger or more than one, a rule that targets a protected channel, channel
names outside the portable alphabet `^[a-z0-9][a-z0-9._-]{0,63}$`, unknown
visibility levels and rollout strategies, non-ascending rollout steps, an
`approval` constraint that is not `manual`, a `retain` that is neither
`while-open` on a pull-request rule nor a whole number of days in `1d`..`365d`,
a `while-open` on a channel that carries no `{number}` and that several pull
requests would therefore share, a `retain` on a rule that publishes no channel,
a `baseline` that is not a portable channel name, that carries `{number}`, that
names a channel the same rule advances, or that sits on a rule publishing no
channel, an invalid namespace, an
unrecognized selector, and out-of-bound collections (32 commands, 128 rules, 64
environments, 64 workloads per environment). Canonicalization preserves order
where it is semantic — commands, flags, rules, members, workloads, rollout
steps — and sorts every map.

`HasProviderSections` reports whether the document declares `distribution` or
`envs`; the CLI turns that into `ci.provider_required` in a workspace without a
release-set provider, because without one there is no channel, no environment,
no visibility, and no grant.

Consumers should call `Parse` for a normalized document. A validating command
uses `ParseWithDiagnostics`, then `ValidateTaskReferences` with the workspace's
discovered graph jobs.

## Producers and consumers

| Role | Who |
| --- | --- |
| Producers | a human authoring `putnami.ci.json`, and `putnami cloud ci init` / `putnami cloud ci fmt` writing the canonical form; `putnami change-plan` emitting a ChangePlan; `putnami impact-plan` emitting an ImpactPlan |
| Consumers | `putnami cloud ci` (validate, fmt, explain); `putnami publish`; `putnami deploy --env`; the generated agent guidance; a CI plane that reads the document to decide what to run; a CI plane that admits a ChangePlan; an extension that reads an ImpactPlan, or projects it onto a ChangePlan |
| Owner of this contract | this project. The `putnami cloud ci` commands validate and explain; the execution plane grants trust and resources, and neither may widen the document locally |

The plane split is part of the contract, not an implementation detail. A
`runner` object is a **request** for an execution envelope, not an allocation;
a `publish` clause is a **request**, not authorization; and a `distribution`
visibility level is a **declaration**, resolved by the backend along the
inheritance chain and never computed here. Anything that decides what a request
is actually granted lives outside this module.

## Publish after a merge, deploy after a channel move

A merge into `main` arrives as a push. The first matching rule names the
channels a green run advances:

```json
{ "branches": "main", "publish": ["canary"] }
```

Place a specific rule before a catch-all such as `**`, because rule order is
semantic and first match wins. The execution plane still resolves
authorization. Publication runs
`putnami publish --impacted --channel <channel>`: the release-set coordinator
measures impact against each channel head and releases the snapshot in one call
(`protocols/distribution`, ADR 0001).

Deploy left the rule. An environment follows a channel, and when that channel
moves the runner synchronizes the environments that follow it, honoring their
constraints; `putnami deploy --env <name>` from a laptop does the same.
`Explain` reports, for each channel a rule advances, the environments that
follow it directly or through one of their workload rules.

When a deploy must consume the exact artifact a publish produced, Delivery
authorizes it only from the single successful runtime `data.releaseSet`
outcome. It binds the deploy input to that outcome's exact
Distribution-owned `{id,digest}` pair. The authored `publish` channel remains
request intent; it is never a deploy input, and Delivery must not resolve it
again after publish. If the outcome is missing or malformed, publication was a
dry run or failure, more than one successful outcome exists, or the proposed
deploy ref differs in either field, deploy authorization fails closed.

`BindDeploymentToPublishedReleaseSet` and
`ValidateDeploymentReleaseSetBinding` encode that plane-neutral rule using
`go.putnami.dev/protocol/distribution` types directly. Neither helper performs
channel resolution, evidence persistence, authorization, or a Control handoff;
those remain provider responsibilities. Consequently, moving `canary` between
publish and deploy cannot change the ref that deploy consumes.

The immutable ref is runtime output, not source-controlled intent. The closed
`putnami.ci.json` schema deliberately rejects a `releaseSet` member anywhere in
the document; adding one would turn a completed, content-addressed fact back
into author-controlled input.

## A published channel's lifetime

A rule that publishes a channel may declare how long that channel is kept:

```json
{ "pullRequests": true, "publish": ["pr-{number}"], "retain": "while-open" }
{ "branches": "release/*", "publish": ["rc"], "retain": "30d" }
```

- `"while-open"` ties the channels to the pull request that produced them. The
  provider is expected to retract the channel head when the pull request closes
  or merges. It is refused on a `branches` or `tags` rule, where nothing closes,
  and it is refused unless every channel the rule publishes carries `{number}`:
  `while-open` names one pull request, so the channel has to belong to one.
  `{ "pullRequests": true, "publish": ["preview"], "retain": "while-open" }` is
  refused, because the first pull request to close would retract the channel
  every other open one is still publishing into. A channel several pull
  requests share takes a day count instead.
- `"<n>d"` is a whole number of days, `1d` to `365d`, counted by the provider
  from the last accepted move of the channel. After it elapses, the provider is
  expected to expire the channel head.
- Absent, the rule declares no lifetime: the channel is kept until someone
  removes it. That is the meaning every version 3 document written before this
  member had, so adding the member changed no existing digest.

Days are the only accepted duration unit, so one intent has exactly one
spelling and therefore one canonical digest; a Go duration such as `720h` is
refused with a diagnostic that says what is accepted. `retain` on a rule that
publishes no channel is refused too, because a lifetime with nothing to apply
to is an authoring mistake rather than a silent no-op. The placeholder rule is
a cross-field constraint — it reads `publish` from the same rule — so the JSON
Schema cannot express it and the Go validator owns it; the schema is
deliberately the looser of the two there.

## The head a first publish measures against

A rule that publishes into a channel nothing has published into yet may name
the channel whose head it measures impact against:

```json
{ "pullRequests": true, "publish": ["pr-{number}"], "baseline": "canary" }
```

The first run of every pull request finds `pr-{number}` empty. Without a
baseline it has nothing to compare against and republishes every member of the
workspace; with one it inherits `canary`'s records and republishes only the
members the branch actually changed, plus their dependents.

- The baseline is **read, never advanced**. It is not part of `publish`, it
  never appears in `Explanation.publish`, and nothing in this contract moves it.
  A channel that belongs to `main` therefore stays a pull request's reference
  without ever becoming a pull request's output.
- It applies **only when the first channel of `publish` has no head**. As soon
  as the pull request has published once, its own head is the baseline again,
  so the second push republishes only what changed since the first.
- It carries no `{number}`: every pull request measures against the same head.
- It must not be one of the channels the same rule advances. That channel is
  already its own baseline, so naming it twice would state two different things
  about one head.
- A **protected** channel is a valid baseline. Protection forbids advancing a
  channel (only a user moves it, with the release-set provider's channel
  command); reading its head is exactly what a first publish needs.
- A `baseline` on a rule that publishes nothing is refused, like a `retain`
  with no channel to apply to: it is an authoring mistake, not a silent no-op.

Every refusal carries the stable code `ci.invalid_baseline` on the field path
`rules[i].baseline`. `Explain` reports the baseline with the rule it matched,
and resolves nothing: which head that channel holds, and whether the
publication is authorized to read it, are remote decisions this report labels
unresolved. See [ADR 0004](doc/adr/0004-a-rule-names-its-baseline-channel.md)
and `protocols/distribution` ADR 0006.

Retraction and expiry name a **head**, never a release set: the sets a channel
pointed at are immutable and content-addressed, and other channels or
deployments may still reference them. Whether, when, and how a provider
retracts is the provider's part of the contract; this module validates the
declaration, carries it through canonicalization, and reports it in
`Explain`, which resolves no authority. See
[ADR 0003](doc/adr/0003-published-channel-lifetime.md).

## The change plan

`putnami change-plan --base <commit> --output=json` emits one immutable
admission plan for a checked-out commit range, in the structured result's
`data` member. This package owns that document: the `ChangePlan` type, its
canonical bytes, its digest and its validation. The CLI builds every plan with
it and imports nothing else to do so, and nothing in this module imports the
CLI. The command, its inputs and the meaning of each member are documented in
[`tooling/cli/doc/17-ci-change-plans.md`](../../tooling/cli/doc/17-ci-change-plans.md).

- `ChangePlanVersion` is `1`, the only version a consumer accepts.
- `ChangePlanCanonicalBytes` is Go's `encoding/json` encoding of
  `{"domain":"putnami/change-plan/v1", …}` followed by every member except
  `cache` and `digest`, in wire order. `ChangePlanDigestDomain` separates this
  digest from every other SHA-256 value Putnami computes.
- `RecomputeChangePlanDigest` is `sha256:` plus the lowercase hex SHA-256 of
  those bytes. It refuses a plan that is not canonical, so one plan has one
  digest.
- `ValidateChangePlan` is the admission check. It refuses an unknown version, a
  revision that is not a full commit ID of 40 or 64 lowercase hexadecimal
  characters, an unsorted list or one holding a duplicate, transitive
  dependents that differ from `ChangePlanImpact.DerivedTransitiveDependents`,
  an inconsistent cache summary, a repository URL carrying credentials, and a
  digest that is malformed or does not match.

`cache` is advisory: it is outside the digest, so warming or evicting a local
entry never changes a plan's identity.

Version 1 is frozen. Any change to the members, their order, their
normalization or the digest input changes the digest of every plan already
emitted, so it needs a new version and a new digest domain.
`TestChangePlanV1IdentityIsUnchanged` pins the golden fixture's digest as a
literal for that reason.

## The impact plan

`putnami impact-plan <commands> --base <commit> --output=json` emits the
impacted plan of a checked-out commit range for the commands it names, in the
structured result's `data` member. It is the document an extension reads when
it needs the changed files, the impacted projects and the planned tasks of a
change, without importing the CLI. This package owns the `ImpactPlan` type and
its validation.

An ImpactPlan holds the members of a ChangePlan that describe the change:
`generator`, `baseSHA`, `headSHA`, `changedFiles`, `impact`, `tasks` and
`cache`, with the same types and the same canonical order. It adds
`commands`, the command list it was planned for, in the requested order. It
has no `repository` and no `digest`: the document a consumer derives from it
holds both.

- `ImpactPlanVersion` is `1`, the only version a consumer accepts.
- `ValidateImpactPlan` refuses an unknown version, an incomplete generator, a
  command list that is empty, names a command twice, or holds a name that is
  empty or contains a comma or whitespace, and every defect
  `ValidateChangePlan` refuses in the shared members.
- `ChangePlanFromImpactPlan(plan, repository)` is the one projection onto a
  ChangePlan. It copies the shared members into new lists, derives the
  transitive dependents from the closure, adds the repository and stamps the
  digest. It returns only a plan `ValidateChangePlan` accepts. A ChangePlan
  does not name its commands, so the caller chooses the command list it admits
  by the ImpactPlan it passes.

`putnami change-plan` is that projection for the fixed command list
`lint,test,build,validate` and the checkout's `origin` remote: its ChangePlan
is the projection of the ImpactPlan `putnami impact-plan` emits for that list
and range. Adding the ImpactPlan changed no ChangePlan byte and no digest. See
[ADR 0005](doc/adr/0005-a-change-plan-projects-an-impact-plan.md).

## Schema and fixtures

- Schema: [`schemas/putnami-ci.json`](schemas/putnami-ci.json)
  (`$id` `https://putnami.dev/schemas/putnami-ci.json`), embedded in the package
  and returned by `Schema()`; the root is closed (`additionalProperties: false`).
- Valid corpus: [`fixtures/valid`](fixtures/valid) — `workspace.json`
  exercising CI members, `commands-only.json` (the shape a workspace without
  a release-set provider authors), `envs-only.json`, `review.json`,
  `pull-request-lifetime.json`, the declared-lifetime conformance case, and
  `pull-request-baseline.json`, the declared-baseline one.
- Invalid corpus: [`fixtures/invalid`](fixtures/invalid) — a version 2
  document, a leftover `gate`, a `deploy` clause on a rule, an unknown field, a
  rule that targets a protected channel, a non-portable channel name, a rule
  with two triggers, a rule with none, an unrecognized selector, non-ascending
  rollout steps, a `distribution` without a namespace, a `retain` spelled as a
  Go duration, a `retain` on a rule that publishes nothing, a `while-open`
  on a channel several pull requests would share, a `baseline` the same rule
  advances, a `baseline` carrying `{number}`, and a `baseline` on a rule that
  publishes nothing, each rejected by `Parse` with the stable code
  `TestInvalidFixturesReportStableCodes` pins.
- ChangePlan conformance corpus:
  [`fixtures/change-plan`](fixtures/change-plan). `expectations.json` indexes
  it: the digest of every `valid/*` plan, the exact canonical bytes of
  `golden.json`, and the refusal `ValidateChangePlan` returns for every
  `invalid/*` plan. `golden.json` exercises every member, including a path
  that Go's encoder escapes; `golden-cache-disabled.json` shows the cache is
  outside the digest; `sha256-no-tasks.json` uses 64-character object IDs and
  empty lists. Each invalid plan carries one defect — an unknown version, a
  short or uppercase SHA, unsorted changed files or tasks, a duplicate changed
  file or project, a wrong digest — and, except for the wrong digest, the
  digest of its own canonical bytes, so the defect is the only reason it is
  refused. Every fixture is the exact indented encoding of the plan it holds.
  The protocol tests and the CLI tests both run this corpus: the CLI rebuilds
  each valid plan from its planner data and must emit the same bytes.
- ImpactPlan conformance corpus:
  [`fixtures/impact-plan`](fixtures/impact-plan). `expectations.json` maps
  every `valid/*` plan to the `fixtures/change-plan/valid` plan its projection
  must equal byte for byte, and every `invalid/*` plan to the refusal
  `ValidateImpactPlan` returns. Each valid plan is its change-plan counterpart
  without `repository` and `digest`, with a command list added. Each invalid
  plan carries one defect: no command, a command named twice, a command
  holding a comma, an incomplete generator, partial transitive dependents, a
  short SHA, an unknown version, or unsorted tasks. The CLI tests rebuild each
  valid plan from its planner data and must emit the same bytes.

## Versioning and compatibility

The wire version is the required integer `version` member.

- **Versions 1 and 2** are not read. There is no migration command and no
  legacy reader: the three repositories that authored those documents migrated
  them by hand, and a document that declares an older version is rejected with
  `ci.unknown_version`.
- **Version 3** is the contract this package defines. The schema pins
  `version` to the exact constant `3`, and
  `TestSchemaVersionMatchesContractVersion` asserts that constant equals the Go
  `Version` constant, so the two cannot drift.

Within version 3, a change is **additive** only if an existing valid document
stays valid and keeps the same meaning and the same canonical digest. Adding an
optional member qualifies. Removing a member, making an optional member
required, or changing normalization or the digest input needs a new `version`.

The optional `review` profile and the optional rule members `retain` and
`baseline` are additive under that rule: a document that declares none of them
produces exactly the previous canonical bytes and digest. Consumers that use it
must pin a release that includes this member; older closed readers reject it.

Because the root is closed, an unknown member is rejected rather than ignored:
a typo in a CI document is reported, not silently dropped.

**Cross-implementation parity is claimed by nothing.** This Go package is the
only implementation; there is no TypeScript or Python parser of
`putnami.ci.json`.

## Support status

- **Subject**: `go.putnami.dev/protocol/ci`, kind `protocol`.
- **Status**: `preview`, recorded in the workspace-root
  [`putnami.support.json`](../../putnami.support.json) — the only reviewed
  authority for support status (contract:
  [`protocols/support/README.md`](../support/README.md)).
- **Owner**: this project (`protocols/ci`). It owns the schema, the parser and
  the digest; `putnami cloud ci` is a consumer of that authority.
- **Evidence for `preview` rather than `stable`**: the version token is pinned
  in both directions — the schema's `version` const against the Go `Version`
  constant by `TestSchemaVersionMatchesContractVersion` — the schema root is
  asserted closed by `TestSchemaIsEmbeddedAndClosed`, the canonical digest is
  asserted deterministic and order-sensitive by
  `TestCanonicalDigestAndFormatAreStable`, and every `fixtures/invalid/*` is
  asserted rejected by `TestInvalidFixturesFail` with a stable code
  (`TestInvalidFixturesReportStableCodes`). What is missing for `stable` is a
  second reader: **six** valid fixtures and **seventeen** invalid ones is a
  corpus, but no second implementation (the native runner reads the document
  through this module) validates the same files. That is the concrete work
  that would earn `stable`.
- **`default` and `parity`**: no claim is recorded on either axis. Omission is
  not a denial — see the support vocabulary.

## Specs and durable decisions

There is deliberately **no user-facing feature or spec for this module**. The
user-visible outcome is "my workspace's CI intent is reviewed in git and a
command tells me what it will do" — and that outcome is owned by the
`putnami cloud ci` command surface (`init`, `validate`, `fmt`, `explain`)
together with the execution plane, not by the document's byte layout. The
`tooling/cli` feature `cli/ci-document` carries the part the core CLI owns: the
generated agent guidance names the document's blocking commands as the gate.
Minting a second product feature per technical wire contract would create a
promise with no user behind it and a second authority beside the schema.

A spec for a CI change therefore belongs with the command surface that serves
the document or with the plane that executes the requests, and would link back
to this contract.

The ImpactPlan's outcome, an extension that reads the impacted plan of a
change without importing the CLI, is carried by the `tooling/cli` feature
`cli/impact-plan`.

This module records its shape in
[ADR 0001](doc/adr/0001-commands-rules-environments-distribution.md), which
replaced `gate` with `commands`, moved deploy off the branch rule, and gave
`distribution` a home, the lifetime of a published channel in
[ADR 0003](doc/adr/0003-published-channel-lifetime.md), and the ImpactPlan as
the document a ChangePlan projects in
[ADR 0005](doc/adr/0005-a-change-plan-projects-an-impact-plan.md). Anything not settled there is enforced by the schema and
the tests rather than by a separate record. When a further decision becomes
contested — the first candidate is whether the canonical digest is part of the
public contract or an implementation detail — it gets a record under
`doc/adr/` following
[`protocols/features/doc/adr/TEMPLATE.md`](../features/doc/adr/TEMPLATE.md).
