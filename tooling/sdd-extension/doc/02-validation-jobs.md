# The validation jobs

`@putnami/sdd` contributes two job commands. They are ordinary
manifest-declared jobs: the planner has no special case for them, they cache
like any other task, and they report through the standard job exit-code
contract.

| Command | Activation | Steps | Cached |
|---|---|---|---|
| `validate` | project-scoped, on `putnami.features.json` or `specs/*.json` | `features-validate` → `specs-validate` | features: **no**; specs: yes |
| `validate-workspace` | `workspace-once` | `architecture-validate`, `specs-ratchet-validate`, `decisions-validate`, `recipes-validate`, `codeowners-sync`, `docs-links-validate` | architecture and ratchet: yes; decisions, recipes, CODEOWNERS and docs links: **no** |

Two commands rather than one, because one command carries one activation and
these two have different subjects. A project's features and specs are a
per-project question; the architecture graph is a property of the whole
workspace and answering it 82 times would be 82 identical answers.

That split is scheduling reality, not a user-facing concept — nobody thinks
"validate my work, per activation model". So `validate` declares
`"alsoRuns": ["validate-workspace"]` in the manifest: requesting `validate`
also plans `validate-workspace`, with its own workspace-once activation, even
when no project's features or specs are touched (a change to only
`putnami.architecture.json` still gets its verdict). Requesting both names
explicitly stays one plan of each, and `validate-workspace` remains directly
requestable.

One word therefore covers all five steps in the canonical gate:

```bash
putnami lint,test,build,validate --impacted --enforce-coverage
```

## What each step checks

- **`features-validate`** — discovers `putnami.features.json` at the workspace
  root and exact project roots, loads the separate evidence artifacts and
  Capability Manifests, coalesces identical dependency copies by semantic owner,
  and reports sorted diagnostics. Missing, stale, contradicted, and unclassified
  evidence are assessment *warnings*; malformed documents, conflicting copies,
  and broken or ambiguous contribution references *fail*.
- **`specs-validate`** — runs the spec protocol's own parser and repository
  validator. It fails on an invalid document, an unresolvable or duplicated
  feature reference, an unsafe decision path, or two specs for one feature. It
  warns, without changing the exit code, on the authoring gaps:
  `specs.missing_spec`, `specs.missing_support_entry`,
  `specs.missing_feature_link`, `specs.unresolved_decision`. It
  also derives and emits the spec-criteria projection (reserved artifact ID
  `putnami-spec-criteria`, `spec-criteria.json`): the bounded, deterministic
  `(feature, requirement) → criterion and expected checks` join core's
  spec-verification gate consumes. The projection is computed from durable
  manifests and spec documents alone — the same inputs the cache key already
  names — so a cache hit restores it byte-identically, and a hit that lost it
  would silently disarm the gate.
- **`architecture-validate`** — validates the ARC/DARC declarations and compares
  exact cross-domain project dependencies against declared bindings, using the
  resolved dependency edges the job context carries. It also reads
  each mapped project's **committed capability manifest** and joins the
  framework implementations recorded there to the declarations — see
  [Framework evidence](#framework-evidence) below.

Automatic architecture admission resolves only the committed workspace
`options.sdd.verification.architecture` policy:

| Mode | Automatic evaluation | Admission |
|---|---|---|
| `enforce` | runs | coherent blocking findings fail the task |
| `report` | runs | the same typed findings are retained and emitted as warnings, but do not change an otherwise successful exit |
| `off` | skipped | succeeds with mode, provenance, and `automaticEvaluation: false`; no evaluation fields are invented |

Policy decoding, malformed declarations, and incomplete provider/workspace
views fail in every mode. `report` is advisory only for findings from a
structurally coherent evaluation. This repository commits `enforce` to preserve
its existing admission gate; the shared absent-policy default remains `report`.
- **`specs-ratchet-validate`** — the rollout ratchet of the executable-spec
  gate. It derives the current enforced floor — every project
  whose effective `options.sdd.verification.specs` is `enforce`, with the
  `feature#requirement` identities its criteria make executable — and compares
  it against the `specs.baseline.json` each enforced project commits in its
  own directory, with the protocol's shrink-only rule. A recorded project that regressed to
  `report`/`off`, or lost recorded requirement coverage, **fails** with
  `features.ratchet_regression`; the reviewed policy change is an edit to the
  project's committed baseline in the same diff, written with
  `putnami specs baseline --update`. Growth passes and draws one
  `features.ratchet_growth` warning nudge to raise the floor; an absent
  baseline is initial adoption, never a failure. A project's file lists only
  its covered requirements (`protocolVersion: 2`); its directory names the
  project. A workspace-wide document in a project directory, or a project
  recorded twice, fails closed. The older workspace-root file
  (`protocolVersion: 1`, naming every project) is still enforced, and draws a
  `features.misplaced_baseline` warning until
  `putnami specs baseline --update` moves each entry into its project.

- **`decisions-validate`** — the decision gate. It reads every
  committed `decisions.json`: the workspace-root one and the one in each
  project directory. It proves every entry that carries a `json-value` check
  against the files the check's own globs name inside the registry's
  directory, and fails with `features.decision_violated` naming the decision —
  id, statement, settled date and registry — so the reader can tell a bug from
  a deliberate reversal:

  ```
  decision D-001 "serverless workloads scale to zero when idle" is violated by sites/putnami.dev/infra/requirements.json: scaling.minInstances = 1
  Settled 2026-09-03 by fdumay. To change it, change the decision in decisions.json, not the code.
  ```

  One diagnostic per violating file, anchored on that file. An entry marked
  `"reviewOnly": true` is carried, counted, injected into the generated agent
  guidance by `putnami context generate`, and never changes an exit code. An
  **absent** registry is adoption, never a failure. An unusable one — duplicate
  ids, within a registry or across two, a missing field, a `settled` date that is not `YYYY-MM-DD`, an unknown
  `kind`/`rule`/`whenMissing`, a check with no glob, or an `adr` that does not
  resolve to a file in this worktree — fails closed with
  `features.invalid_decision_registry` rather than enforcing the entries that
  happened to parse. There is **no verification-mode knob**: `architecture` has
  one because a workspace adopts a graph gradually, while a decision a
  repository wrote down and dated is either held or re-decided.

- **`recipes-validate`** — the recipe gate. It reads every committed
  `<lang>/samples/recipes.json` (`typescript`, `go`, `python`). A recipe whose
  `sample` is not a directory in this worktree fails with
  `sdd.recipe_sample_missing`, naming the sample. A second recipe for one
  intention, an unknown member, or an empty or multi-line field fails with
  `sdd.invalid_recipe_index`. An **absent** index is adoption. It is
  uncacheable: the verdict depends on whether each sample directory exists, and
  no file pattern expresses that.

- **`codeowners-sync`** — the CODEOWNERS step. It writes
  `.github/CODEOWNERS` from `options.sdd.owners`: `putnami.workspace.json`
  gives the catch-all, and a scope or project `putnami.json` gives a rule for
  its directory, placed after the rules above it. When the workspace commits
  a `specs.baseline.json`, the spec-governance files close the file with the
  workspace owners. It rewrites the committed file only when it differs, and CI
  reports a stale or missing one through the rule that fails a run whose gate
  changed the tree. An invalid owner, owners in a root `putnami.json`, or an
  `options.sdd` that is not an object (`sdd.invalid_owners`), a directory
  declaration without a catch-all (`sdd.codeowners_no_default`), or a
  `putnami.json` the step cannot read (`sdd.codeowners_read_failed`) fails
  without writing. A workspace that declares no owners and has no such file is
  left alone. It is uncacheable: it
  rewrites a file that is also its input.
- **`docs-links-validate`** — the documentation link gate. Each
  language extension's `lint` checks the `README.md` files and `doc/` trees
  inside its project, which is the feedback you get while working on it. This
  step checks every `README.md` file and `doc/` tree of the workspace, once
  per run whatever the selection. It covers what no `lint` task does: the
  documents no project owns, the projects without a language extension, and a
  link from one project into another that a change to the other breaks. A link
  broken inside a selected project is reported by both. It walks the
  workspace once, with one `git` process, in about 4 s on this repository.
  Inside a project it reads what that project's `lint` reads, and it skips a
  project that sets `docs-links: false` under its `lint` options or under
  one of its own extensions' options. A directory inside a project that is not itself a project,
  such as a nested Go module, belongs to that project. It applies the same
  rule, from the extension SDK's
  [`docslinks`](../../extension-sdk/docslinks/README.md) package, and fails with
  one `docs-links` diagnostic per broken link, located at the link. It is
  uncacheable: a link may name any file of the workspace.

Selection narrows what a run **owns**, never what it **reads**: durable
manifests and canonical spec documents are read at every root under any
selection, because "one spec per feature" is a workspace-wide guarantee that a
subset cannot prove. See [03-commands.md](03-commands.md#selected-projections).

## What the gate collects, and from where

The projection above says what a run is expected to prove. Core's post-session
gate says what it observed, and it reads from two places.

**The session.** Every `test` job planned in the run contributes the
`putnami-feature-verification` report it declared. This is the ordinary path and
it is unchanged.

**An attester's cache entry, only when the session left something unresolved.**
A `spectest.Proves` call lives where the code it protects lives, which is
routinely not the project that owns the spec. Two shapes occur:

| Shape | Example in this repository |
|---|---|
| a library the owner **depends on** | a workload's requirement proven by its core library |
| a **direct dependent** that implements the owner's contract | `architecture/executable-contracts` (owned by `protocols/architecture`) proven by `@putnami/sdd`; `go/api-contracts` (owned by `go/framework/api`) proven by `openapi`, `proto`, `grpc` |

A narrowed run (`--impacted`, `--projects`) plans neither shape's `test~test`, so
the requirement used to read `check-not-observed` on a run that had
regressed nothing.

So a group the in-session join could not close is retried against the captured
report inside each unplanned attester's own `test~test` cache entry, read in
place at the key the current inputs derive. Nothing is materialized into
`.putnami/out`, and provenance containment is not relaxed — a restored
observation must still name a regular file of the project that published the
entry. The recovered reference is recorded with its content digest and marked
`restored`, so an audit can tell evidence a run produced from evidence a run
recovered.

The invariant closes on the cache key, not on trust: an attester that changed is
already selected by `--impacted` and runs, and one that did not change keeps its
key, so the entry it published is the entry for exactly these inputs.

Candidates are the spec-owning project's **transitive dependency closure plus its
direct dependents**, and the lookup is **lazy**: a fully verified run performs no
extra planning, no extra keying, and never touches the store. A project leaves
the set only when the run planned the job that writes its report. A
`test~generate` planned as a prerequisite does not count.

The two radii differ because the two directions attest for different reasons. A
dependency can attest because the owner's tests *execute* it, so depth there is
unbounded. A direct dependent can attest because it *produces* the artifact the
contract describes — and that relationship lives exactly one hop up. Beyond one
hop a consumer exercises the contract only through the layer between, which is
already a candidate, so transitive dependents stay out of scope. The direct set
is bounded by fan-in: 31 projects for the widest spec owner in this workspace of
147. See [ADR 0026](../../cli/doc/adr/0026-spec-gate-evidence-scope.md) for the
measurements.

## When nothing could be consulted

A check can still be unresolved because no observation source existed at all:
no planned test for any candidate, and no cache entry (an empty local store, a
cold remote, or `--no-cache`).

That is not a regression, and it does not sanction the run. The requirement
resolves to **`unobserved`** — a state the shared `GroupBlocks` rule does not
block on — and the run prints a warning naming the projects whose tests would
have attested the check and why they were not consulted.

Silence is an answer. A dependency whose cache entry exists and records the
report output **empty** ran for these inputs and observed nothing, so it counts
as consulted; so does a project with no task that declares the report. Only a
candidate that could not be reached is unconsulted: the test task that writes its
report has no cache entry or is not cacheable, or the run used `--no-cache`. The
other steps of the `test` command are never asked.

The exemption cannot launder a real defect. It applies only when every one of a
requirement's checks is either satisfied or missing with reason
`check-not-observed`. A check reported **skipped, failed, stale, duplicated, or
in a shape the criterion cannot read** rests on an observation that did arrive
and keeps blocking under `enforce`. Equally, once every candidate *was*
consulted — every warm run, and every run that is not narrowed — the unconsulted
set is empty and a still-missing check blocks exactly as it did before.

Under `--no-cache` with a narrowed selection no stored byte may be served, so
every unresolved check falls to this path and warns. That is the accepted
outcome; the reasoning is in
[ADR 0026](../../cli/doc/adr/0026-spec-gate-evidence-scope.md).

**An exhausted scope blocks, and says so.** When every project that could attest
a check was already in the run and none reported it, the requirement stays
`missing` and the group stays blocked — the check has no test, which is exactly
what the gate exists to catch. The group still carries a
`features.unobserved_requirement` warning, this one stating that the evidence
scope was exhausted and that the fix is a `spectest.Proves` call rather than a
wider selection, so a bare `check-not-observed` never reaches a reader on its
own.

## Caching, and the one step that is not cached

A task may be cacheable only when its declared inputs cover what it reads.

### `specs-validate` — cached, with a workspace-wide identity port

```json
"inputs": {
  "manifest": { "from": "project",   "files": ["putnami.features.json"] },
  "specs":    { "from": "project",   "files": ["specs/*.json"] },
  "identity": { "from": "workspace", "files": ["**/putnami.features.json", "**/specs/*.json"] }
}
```

The task reports on one project but resolves against the workspace's authored
identities, so the key follows the **read set**, not the ownership. A key that
stopped at the project boundary would let a sibling's manifest change this
verdict silently. Verified by measurement: touching a spec in another project
produces a miss.

### `architecture-validate` — cached, and it never reads git

```json
"inputs": {
  "policy": {
    "from": "workspace",
    "files": ["putnami.workspace.json"]
  },
  "declarations": {
    "from": "workspace",
    "files": ["**/putnami.architecture.json", "**/architecture.baseline.json", "**/architecture.waivers.json"]
  },
  "evidence": {
    "from": "workspace",
    "files": ["**/schema/capabilities.json"]
  }
}
```

The policy port is load-bearing: changing `enforce`, `report`, or `off` changes
the task verdict even when declarations and graph evidence are unchanged, so
`putnami.workspace.json` must change the cache key.

The job evaluates the **current worktree** and does not compare against the
frozen adoption baseline. The comparison would resolve a commit — which ref
`origin/HEAD` names, where the branch forked — and none of that is in the key,
so a restored verdict would rest on state the key does not name. Dropping it
makes the job strictly **stricter**: nothing is excused as known debt.
Shrink-only ratcheting stays with the interactive
`putnami architecture validate --baseline <ref>`.

Interactive `architecture validate`, `snapshot`, and `inspect` remain explicit
requests and are unchanged when automatic evaluation is `off`.

`TestWorktreeValidationReadsNoGitHistory` pins it.

The `evidence` port is what keeps the framework-evidence half honest under the
same rule. A capability manifest is a committed file at an exact project-relative
path, so it can be named as a pattern; asking a build to run instead would put
unkeyed state behind a restored verdict.

#### Framework evidence

A `domainAccess` row in a committed capability manifest says a running component
was configured with one declared import (`go.putnami.dev/app/darc`). The
job joins those rows to the declarations and reports two findings, deliberately
asymmetric:

| Finding | When |
|---|---|
| `architecture.evidence_without_declaration` | A record no declaration matches — an unknown import, or the same import declared with another mode. **Always** fails: a record proves a component exists, so the code is enforcing a contract nobody reviewed. |
| `architecture.declared_without_evidence` | An **active** declaration nothing implements, inside a domain that already implements something else. |

The second is scoped on purpose. A domain whose workloads use no framework
primitive emits no evidence, and demanding an implementation from it would report
every honest declaration in the repository as a violation. Once a domain
implements one import, partial adoption inside it is exactly what this catches. A
`planned` import is never expected to be implemented — it is a target.

Neither finding can create a permission: evidence is derived fact, like an
observed edge, and only a reviewed declaration authorizes anything
([ADR 0001](../../../protocols/architecture/doc/adr/0001-declarations-are-authority-observations-are-evidence.md)).

The coverage report says exactly what happened. `coverage.domainAccess` moves
from `not-detected` to **`framework-evidence`**, and a transport category moves
with it only when a record declares that carrier. It is never called
"observed": nothing watched a request, a query, or a delivered event — a
producer recorded what a component was configured with.

### `specs-ratchet-validate` — cached, worktree-only on both sides

```json
"inputs": {
  "floor": {
    "from": "workspace",
    "files": ["**/putnami.features.json", "**/specs/*.json", "**/putnami.json", "putnami.workspace.json", "**/specs.baseline.json"]
  }
}
```

Both sides of the ratchet comparison are committed files the input patterns
name — the manifests, specs, and options the floor derives from, and the
committed baseline it compares against — and the job reads no git history, for
the same reason `architecture-validate` reads none. The reviewed policy change
the rule demands is an edit to a `specs.baseline.json`, which changes an input
and re-keys the task.

### `features-validate` — uncacheable in v1

Its verdict depends on evidence **source bindings**: arbitrary bound files plus
their git blob state, which is what the `source-binding-unavailable` and
`source-binding-mismatch` stale reasons compare. No file-pattern key can express
that read set, and an under-declared key does not merely miss a change — it
serves a stale verdict while reporting that the check ran.

Caching it needs a **binding-aware** key: the resolved binding set plus each
bound blob's content digest, folded in after the bindings resolve. That is a
follow-up, not a smaller version of this one.

### `decisions-validate` — uncacheable, for the same reason

A check's read set is whatever its own `files` globs name, and those globs are
authored inside each repository's `decisions.json` files — content this manifest
cannot see, let alone name in a static input pattern. Declaring a broad pattern
to make the task cacheable would key it on a superset it does not read and a
subset it does, which is the under-declared key again in a friendlier shape.

The walk it does instead is bounded and deterministic: workspace-relative paths
in sorted order, never descending into `node_modules`, `vendor`, or any dot
directory (`.git`, `.gen`, `.putnami`), and never following a symlink — a
generated or linked copy of a matched file would make the same registry answer
differently before and after a build.

### `docs-links-validate` — uncacheable, for the same reason

A link may name any file of the workspace, so no static input pattern covers
what the step reads. A key that missed a deleted link target would replay a
green verdict for a link that no longer resolves.

## Exit codes

The steps use the standard job contract. A seeded invalid spec produces task
exit `2` and session exit `1`. `contracts check`'s own exit-`2` drift cycle is
**not** part of these jobs — see
[03-commands.md](03-commands.md#contracts) for why contracts stayed interactive.

## Reading the wire

Everything a step knows about the workspace arrives on the job-context
document. The extension has no loader and may not exec `putnami`. If a step
needs a fact the context does not carry, the fix is an additive member in
`protocols/job` — not a private read.

One fact is still missing on the job wire, documented in
`internal/wsview`'s package doc and in
[ADR 0013](../../cli/doc/adr/0013-sdd-as-a-standalone-extension.md#3-the-extension-gets-workspace-knowledge-from-the-wire-never-from-a-loader):

| Missing | Consequence today |
|---|---|
| Loader warning codes | The view raises `workspace.provider_view_unavailable` itself when a workspace-scoped job receives no membership, so `architecture-validate` refuses instead of reporting a subset as the whole. |

It fails closed. That is the condition for leaving it open.

## Inspecting what a job received

```bash
putnami sdd-selfcheck
```

`sdd-selfcheck` is an internal command. It reports the extension identity, the
job-context protocol version, and the resolved `selection` block — the member
every SDD verdict depends on, because a validator told it ran over three
projects cannot otherwise tell a deliberate `--projects` narrowing from a
whole-workspace run of a three-project tree.
