# The validation jobs

`@putnami/sdd` contributes two job commands. They are ordinary
manifest-declared jobs: the planner has no special case for them, they cache
like any other task, and they report through the standard job exit-code
contract.

| Command | Activation | Steps | Cached |
|---|---|---|---|
| `validate` | project-scoped, on `putnami.features.json` or `specs/*.json` | `features-validate` → `specs-validate` | yes: features on the Git candidate cut, specs on its documents |
| `validate-workspace` | `workspace-once` | `architecture-validate`, `specs-ratchet-validate`, `decisions-validate`, `recipes-validate`, `codeowners-sync`, `docs-links-validate`, `breaking-marker-validate`, `agent-attribution-validate`, `feature-coverage-validate`, `feature-coverage-ratchet-validate` | yes: architecture and ratchet on their documents; decisions, recipes, CODEOWNERS, docs links and feature coverage on the Git candidate cut. The breaking marker, the agent attribution and the feature-coverage ratchet are never cached |

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

One word therefore covers every step of both commands in the canonical gate:

```bash
putnami lint,test,build,validate --impacted --enforce-coverage
```

## What each step checks

- **`features-validate`** — discovers `putnami.features.json` at the workspace
  root and exact project roots, loads the separate evidence artifacts and
  Capability Manifests, coalesces identical dependency copies by semantic owner,
  and reports sorted diagnostics. Missing, stale, contradicted, and unclassified
  evidence are assessment *warnings*; malformed documents, conflicting copies,
  and broken or ambiguous contribution references *fail*. A feature may list
  the command paths that carry it under `surface`. The step
  resolves each name against the `putnami.extension.json` at the same project
  root as the declaring manifest: a command key that is not internal, the path
  of a command-group subcommand, or a verb name. It reads only
  those members, so a field a newer extension protocol adds does not fail it.
  A name that file does not serve fails as `features.unresolved_surface`. A
  project root without an extension manifest is not resolved, and no other
  extension, lock or installed state is read. `putnami features validate`
  resolves the same way; `features inspect` and `features list` report the
  surface as declared, and `features snapshot` does not resolve it.
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
  `sdd.invalid_recipe_index`. An **absent** index is adoption. A sample
  directory exists when it holds a tracked or unignored file, and the step is
  cached on the input `git:**` (see [Caching](#the-steps-cached-on-the-git-candidate-cut)).

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
  left alone. It is cached on the input `git:**`, which also holds the file it
  rewrites: the CLI never replays a run that rewrote `CODEOWNERS`.
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
  one `docs-links` diagnostic per broken link, located at the link. A link may
  name any file of the workspace, so it reads the Git candidate cut and is
  cached on the input `git:**`: a link to a file Git ignores is broken, as it
  is in a clone.
- **`breaking-marker-validate`** — the breaking-marker step. A pull request
  merges by squash: the commits of its branch are dropped, and only the squash
  commit reaches the base branch. `putnami version` reads the breaking marker
  from that commit, so a `!` or a `BREAKING CHANGE:` footer that only a branch
  commit carries is lost. The step reads the proposed squash message from the
  `PUTNAMI_PROPOSAL_MESSAGE` environment variable, which the runner sets on a
  pull request: the title, a blank line, and the body when the repository
  squashes with it. It splits the message as Git splits a commit message,
  reads the commits from the run's impacted baseline to `HEAD`, and reads every
  message with the
  [conventional-commit reader](../../../protocols/cli/doc/07-conventional-commit.md)
  the version bump uses.

  | Situation | Result |
  |---|---|
  | `PUTNAMI_PROPOSAL_MESSAGE` unset or blank | Passes, with the note "no proposal message to compare" |
  | Message set, the run resolved no impacted baseline | Fails: without a baseline there is no range to compare. This covers a run without `--impacted` and an `--impacted` run that found no base and fell back to every project |
  | Message set, the repository is a shallow clone | Fails, naming the shallow clone: the range could stop at the clone depth |
  | Message set, the baseline names no commit, or Git fails | Fails with exit `1`, an error rather than a finding |
  | No commit of the range declares a break | Passes, whatever the title says |
  | A commit declares a break, and the title carries `!` or the body a `BREAKING CHANGE:` footer | Passes |
  | A commit declares a break, and the message declares none | Fails with one `sdd.breaking_marker_dropped` diagnostic per such commit |
  | A commit declares a break, and the title is not a conventional commit | Fails: such a title declares nothing, whatever its body says |

  A failure names each commit that declares a break, by its 12-character ID
  and its subject:

  ```text
  breaking marker: the squash commit "feat(cli): rework the selection" drops the breaking change that 1 commit declares since origin/main:
    3f2a9c1d0b7e feat(cli)!: drop the --all flag
  Add "!" before the colon of the title, or a "BREAKING CHANGE:" footer to the body when the squash commit keeps the body. If the change breaks nothing, reword the commit.
  ```

  A footer in the body counts only because the runner writes the body into
  the variable when the repository squashes with it: the step cannot read that
  setting. A footer with no blank line above it is part of the subject and
  declares nothing. A merge of the base into the branch brings no commit of
  the base into the range. The step reads every commit of the range, so two
  cases fail although the squash commit breaks nothing: a branch stacked on a
  parent that has since been squash-merged still holds the parent's commits,
  and a breaking commit reverted later on the branch still declares its break.
  Rebase the branch onto the base and drop those commits. The step does not
  run when `--impacted` selects no project, because the run then plans no job. It is never cached (see
  [below](#breaking-marker-validate--never-cached)). The decision is
  [ADR 0001](adr/0001-the-squash-commit-keeps-the-breaking-marker.md).
- **`agent-attribution-validate`** — the agent-attribution step. The squash
  commit is the only commit of a pull request that reaches the base branch,
  and a coding agent, or the service that hosts it, can sign its message.
  Once the commit is on the base branch, only a history rewrite removes the
  signature. A workspace that forbids such a signature sets
  `options.sdd.agentAttribution` to `"forbid"` in `putnami.workspace.json`.
  The step then reads the proposed squash message from
  `PUTNAMI_PROPOSAL_MESSAGE`, the variable the breaking-marker step reads, and
  checks it line by line for three forms:

  | Form | Example of what fails | What passes |
  |---|---|---|
  | `generated-footer` | "Generated by" or "Generated with", then the name of an agent, whatever the markup around it | "generated by clientgen" |
  | `co-author-trailer` | a `Co-Authored-By:` trailer whose name or address is an agent's | a human co-author |
  | `session-link` | a link to an agent's session, or a `<agent>-session:` trailer | a path such as `.claude/` or `.agents/` |

  The agent names are the `agentNames` list of
  `internal/sdd/agent_attribution.go`, matched as whole words regardless of
  case.

  | Situation | Result |
  |---|---|
  | `options.sdd.agentAttribution` unset or `"allow"` | Passes, with a note that says how to opt in |
  | Any other value of the option | Fails with one `sdd.invalid_agent_attribution` diagnostic: a typo does not turn the gate off |
  | `"forbid"`, `PUTNAMI_PROPOSAL_MESSAGE` unset or blank | Passes, with the note "no proposal message to read" |
  | `"forbid"`, no line names an agent | Passes |
  | `"forbid"`, a line names an agent | Fails with one `sdd.agent_attribution` diagnostic per such line |

  A failure names each line by its number in the message and its form:

  ```text
  agent attribution: the squash commit "fix(cli): stop the leak" names an agent on 1 line(s):
    line 6 (generated-footer): _Generated by [<agent>](<url>)_
  Remove each line from the title or the body: the workspace sets options.sdd.agentAttribution to "forbid". A path such as .claude/ and a human co-author stay allowed.
  ```

  The step reads only the message: the commits of the branch never reach the
  base branch. It runs when the runner runs `validate`, so an edit of the title
  or the body is checked at the next run. A message edited in the merge dialog
  is never checked. It is never cached (see
  [below](#agent-attribution-validate--never-cached)). The decision is
  [ADR 0002](adr/0002-the-squash-commit-names-no-agent.md).

- **`feature-coverage-validate`** — the feature-coverage gate. Every project
  answers one question: which outcome does its code serve? A project answers
  with a feature in its own `putnami.features.json`, a spec under its own
  `specs/`, or a reviewed `featureAuthority` in its `putnami.json`: an `owner`
  that names an authored feature, or `none` with the reason the project has no
  feature. Once the workspace adopts the gate in `putnami.workspace.json`, the
  step judges every member of the workspace, with or without a `putnami.json`:

  ```json
  "options": {
    "sdd": {
      "featureCoverage": { "debt": ["/cloud/runtime/go"] }
    }
  }
  ```

  | Situation | Result |
  |---|---|
  | `options.sdd.featureCoverage` unset | Passes, with a note that counts the projects that give no answer |
  | A project gives no answer and `debt` does not list it | Fails with one `sdd.uncovered_project` diagnostic at the project's `putnami.json#featureAuthority` |
  | A `featureAuthority.owner` names no authored feature | Covers nothing: `sdd.uncovered_project`, naming the owner |
  | A `debt` entry whose project answers | Fails with `sdd.stale_coverage_debt`: remove the entry |
  | A `debt` entry that names no project | Fails with `sdd.unknown_coverage_debt` |
  | Another member, no `debt`, an entry that is not a project id, or a duplicate entry | Fails with `sdd.invalid_feature_coverage`, and judges no project |

  The list only shrinks: a paid debt fails until its entry is removed, and
  `feature-coverage-ratchet-validate` refuses a new entry. This step reads no
  history. A manifest or a spec covers its project when it is a tracked or
  unignored file, and the step is cached on the input `git:**` (see
  [Caching](#the-steps-cached-on-the-git-candidate-cut)). The decision is
  [ADR 0003](adr/0003-every-project-carries-a-feature-a-spec-or-a-feature-authority.md).

- **`feature-coverage-ratchet-validate`** — the shrink-only rule of the debt
  list. It reads `options.sdd.featureCoverage.debt` in `putnami.workspace.json`
  at the merge base of the run's impacted baseline and `HEAD`: the list the
  change started from. The merge base, not the baseline commit, is the
  comparison point, so a branch behind its base keeps an entry the base removed
  since without a failure.

  | Situation | Result |
  |---|---|
  | `options.sdd.featureCoverage` unset | Passes, with a note |
  | The run selected its projects without `--impacted`, or its baseline did not resolve without `--impacted-strict` | Passes, with a note: nothing names a baseline |
  | The merge base declares no `options.sdd.featureCoverage`, or no `putnami.workspace.json` | Passes, with a note: this change adopts the gate |
  | An entry the list at the merge base does not hold | Fails with one `sdd.grown_coverage_debt` diagnostic per entry, at its index in the current list |
  | An entry removed, or the list unchanged | Passes |
  | A shallow clone | Fails: its history can stop before the merge base |
  | A list at the merge base the step cannot read | Fails with `sdd.coverage_debt_baseline_unavailable` |

  A gate that compares a pull request with its base branch runs
  `putnami validate --impacted --impacted-strict --baseline origin/<base>` on a
  full history, as Contributor CI does. A run that names no baseline enforces
  nothing here. The step is never cached (see
  [below](#feature-coverage-ratchet-validate--never-cached)).

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
routinely not the project that owns the spec. Three shapes occur:

| Shape | Example in this repository |
|---|---|
| a library the owner **depends on** | a workload's requirement proven by its core library |
| a **direct dependent** that implements the owner's contract | `architecture/executable-contracts` (owned by `protocols/architecture`) proven by `@putnami/sdd`; `go/api-contracts` (owned by `go/framework/api`) proven by `openapi`, `proto`, `grpc` |
| a project whose **`featureAuthority.owner`** names a feature the owner declares | the CLI's install and smoke script tests (`@putnami/install-scripts`), which prove `cli/first-use-path` and three other CLI features |

A narrowed run (`--impacted`, `--projects`) plans none of these `test~test` jobs, so
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

Candidates are the spec-owning project's **transitive dependency closure, its
direct dependents, and the projects whose feature authority is one of its
features**, and the lookup is **lazy**: a fully verified run performs no
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

A feature-authority project can attest because its own manifest declares that its
user-facing feature is one the owner declares. It is a test-only project split out
of the owner so that only the files its tests read key it, so it shares no
dependency edge with the owner on purpose. A declaration that names another
feature admits nothing. See
[ADR 0081](../../cli/doc/adr/0081-a-feature-authority-project-attests-for-the-spec-owner.md).

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
report has no cache entry or is not cacheable, the remote cache did not answer,
or the run used `--no-cache`. The other steps of the `test` command are never
asked.

The warning states each cause in prose. The session's `spec-verification.json`
record states the same causes in each group's `unconsulted` list: one entry per
candidate project, with a `reason` from a closed set.

| `reason` | The candidate's report task | Running the same change again helps |
|----------|-----------------------------|-------------------------------------|
| `remote-unreachable` | its remote cache lookup failed | yes |
| `remote-timeout` | the remote cache did not answer in time | yes |
| `no-entry` | the caches answered and hold no entry for these inputs | no |
| `no-cache` | the run reads no cache: `--no-cache`, or no cache store | no |
| `not-cacheable` | is not cacheable | no |
| `plan-failed` | its test plan or cache keys could not be derived | no |

The list is absent when the session's own observations closed the group, when
every candidate answered, and in a record an older CLI wrote.

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

## Caching, and the steps that are not cached

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

### `features-validate` — cached on the Git candidate cut

```json
"inputs": {
  "repository": { "from": "workspace", "files": ["git:**"] },
  "manifest": { "from": "project", "files": ["putnami.features.json"] }
}
```

The extension manifest that resolves a surface is a file of the cut, so
`git:**` already holds it. A `putnami.extension.json` that git ignores is
outside the cut: the step treats its project as having no extension manifest,
while `putnami features validate` reads it from disk.

Its verdict depends on evidence **source bindings**, which is what the
`source-binding-unavailable` and `source-binding-mismatch` stale reasons
compare, so it reads every file a binding hashes, as well as the feature
manifests, evidence fragments and capability manifests at every root. The task
reads them from the workspace's candidate cut and nothing else, and `git:**`
holds that cut:

- A binding records each bound file's bytes or link text and its executable
  bit. The `git:` key holds the bit too, read where the binding reads it: from
  the file's mode, or on Windows, which stores none, from the mode the index
  records (ADR 0063 of the CLI). A `chmod +x` of a bound file moves the key.
- A binding records a submodule's checked-out commit and refuses an unmerged
  path. The `git:` key holds neither and produces no key for either, so the
  task then runs uncached instead of replaying a verdict about another commit.
- An ignored evidence fragment, an ignored artifact and ignored build output
  are neither read nor keyed, and staging changes nothing.
- Package-root evidence matches a project by name and version. Every project
  is read at the tree base version `0.0.0`, the version the CLI gives a task
  the cache can serve (ADR 0060 of the CLI) and every build stamp records, so
  no Git tag and no missing commit changes a verdict the key cannot see.
- The report names the worktree and no commit, and its selection names no
  baseline ref, so a replayed entry is true of the run that replays it.

Two runs on one tree therefore share one entry, whatever the commit, the ref
or the checkout. The cut is a workspace port, so one digest of it serves every
project's key in a run. The project port names the project's own manifest
only so that the project side of the key is not empty: the CLI keys an empty
project side on every file under the project root, ignored build output
included. Outside a Git work tree the task reads the disk, and the `git:` input
has no key, so it runs every time.

The `specs` step does not depend on this one. A step's key holds the key of
every step it depends on, so an edge to a task keyed on the whole cut would
move every project's `specs-validate` key on any edit anywhere in the
repository. `specs-validate` reads the feature manifests itself and reports one
that fails to parse, so the two steps run side by side.

`putnami features validate`, the interactive command, still reads the disk,
the version Git resolved, and the HEAD commit it reports.

### The steps cached on the Git candidate cut

```json
"inputs": {
  "repository": { "from": "project", "files": ["git:**"] }
}
```

`decisions-validate`, `recipes-validate`, `codeowners-sync`,
`docs-links-validate` and `feature-coverage-validate` read files no narrow
pattern names: the globs a repository authors in its `decisions.json`, the
sample directories an index names, every `putnami.json` above a project, any
file a link names, and the feature manifest and specs of every project. Each
reads the repository's **candidate cut** and nothing else: the tracked files
and the untracked files no ignore rule excludes, as
`git ls-files --cached --others --exclude-standard` lists them. The input
`git:**` holds exactly that cut: each candidate's path, bytes and executable
bit, and a symbolic link's target text.

- Editing, adding, deleting or renaming a candidate moves the key, so a
  deleted link target or an emptied sample directory is never served a stale
  green verdict.
- An ignored file is neither read nor keyed, and staging changes nothing.
  A directory that holds no candidate does not exist for these steps, as it
  does not exist in a clone.
- The port is project-scoped because a workspace-once task's project is rooted
  at the workspace root. A workspace port alone would leave the project side of
  the key empty, and the CLI keys an empty project side on every file under the
  root, ignored build output included.
- The key of a task cached on the cut also holds the workspace membership,
  with each project's configuration. `feature-coverage-validate` reads each
  `featureAuthority` from that configuration, so declaring one moves its key.
- Outside a Git work tree the steps read the disk, and the `git:` input has no
  key, so they run every time.

`codeowners-sync` rewrites `.github/CODEOWNERS`, which the cut holds. The CLI
replays a source writer only when rehashing its project-side key after the run
finds the bytes unchanged, so a run that rewrote the file is never replayed:
the next run is keyed on the new bytes, and its clean result is the one a later
run replays.

The `decisions-validate` walk stays bounded and deterministic: workspace-relative
paths in sorted order, never inside `node_modules`, `vendor`, or any dot
directory (`.git`, `.gen`, `.putnami`), and never through a symlink — a
generated or linked copy of a matched file would make the same registry answer
differently before and after a build.

### `breaking-marker-validate` — never cached

```json
"inputs": {
  "PUTNAMI_PROPOSAL_MESSAGE": { "from": "env" }
},
"cache": { "enabled": false }
```

The verdict reads an environment variable and the commits of the range, and
the candidate cut holds neither. A key would need those commits, and a key
describes the tree, never the commit
([ADR 0059 of the CLI](../../cli/doc/adr/0059-a-cache-key-describes-the-tree-not-the-commit.md)).
So the task runs every time, and costs one `git log` of the range. The `env`
port names the variable the step reads, and keys nothing.

### `agent-attribution-validate` — never cached

```json
"inputs": {
  "PUTNAMI_PROPOSAL_MESSAGE": { "from": "env" }
},
"cache": { "enabled": false }
```

The verdict reads an environment variable, and the candidate cut does not
hold it. So the task runs every time, and reads no file. The `env` port names
the variable the step reads, and keys nothing.

### `feature-coverage-ratchet-validate` — never cached

```json
"cache": { "enabled": false }
```

The verdict reads the debt list at a commit the run's baseline names, and the
candidate cut holds neither the baseline nor the history. So the task runs
every time, and costs a few Git reads and one blob read. It declares no
port: the current list arrives on the job context. `feature-coverage-validate`
keeps its key on the candidate cut and reads no history.

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
