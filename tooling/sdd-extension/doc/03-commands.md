# The interactive commands

Four command groups, eighteen subcommands. All of them are `interactive: true`,
the deliberate scheduler bypass: the CLI spawns the process with the
terminal's own stdout and parses nothing, so what the binary writes *is* the
command's output.

These sections were part of the CLI's own command reference. They
describe the same surface, now served by `@putnami/sdd`.

| Group | Subcommands | Positionals | Selection flags |
|---|---|---|---|
| `features` | `list` `validate` `snapshot` `inspect` `diff` | `[query]`, none, none, `<feature-id>`, `<base> <head>` | honored, honored, honored, **refused**, **refused** |
| `specs` | `list` `validate` `inspect` `init` | none, none, `<feature-id>`, `<feature-id>` | honored, honored, **refused**, **refused** |
| `architecture` | `validate` `snapshot` `inspect` `init` `sync` | none, none, `<domain-id>`, `<domain-id>`, none | global `--baseline`; `init` **honors** selection, `sync` **refuses** it |
| `contracts` | `generate` `check` | none (`--project <selector>`) | `--projects` |

## `architecture`

The architecture namespace is **experimental**. `go.putnami.dev/protocol/architecture`
is classified `experimental` in `putnami.support.json`, so the manifest format,
finding IDs, and detector coverage may change without a migration path.

Adopting it is an explicit opt-in, and automatic admission is a second,
separate decision under workspace `options.sdd.verification.architecture`.
`enforce` blocks on coherent findings, `report` keeps the same typed findings
advisory, and `off` skips the automatic DAG evaluation. Structural and policy
errors always fail closed. This repository commits `enforce`, so
`architecture-validate` remains a blocking step of `validate-workspace`, which
the required `quality` job runs.

The namespace makes Architecture Rules as Code (ARC) and Domain
Access & Replication Contracts (DARC) executable from any Putnami workspace:

```bash
putnami architecture validate
putnami architecture snapshot --output=json
putnami architecture inspect <domain-id>
putnami architecture validate --baseline origin/main
```

Domains author exact `putnami.architecture.json` files. The producer declares
authoritative exported facts and compatibility; the consumer declares its
minimized import, semantic access mode, lifecycle, transport availability,
consistency and local-model guarantees. The global graph is derived from those
distributed declarations, never authored as a second central matrix.

`validate` performs one contained discovery, validates both halves through
`go.putnami.dev/protocol/architecture`, and compares exact cross-domain project
dependencies in the resolved Putnami graph with declared bindings. A `planned`
import cannot carry a binding, so a target design cannot hide current drift.
The v1 snapshot explicitly reports database, HTTP and event detectors as
`not-detected`; a declared API or event is not treated as observed evidence.

Existing adoption debt may be listed in the optional workspace-root
`architecture.baseline.json`. Once committed, that file is shrink-only against
the resolved Git baseline. Later temporary exceptions belong in
`architecture.waivers.json`; expired and stale entries fail. The commands never
create, grow, or rewrite either file. `--baseline <git-ref>` overrides the Git
comparison point using the ordinary global baseline flag.

The **cached `architecture-validate` job** deliberately runs without a baseline
and reads no git history at all; baseline ratcheting is an interactive and
review concern. See [02-validation-jobs.md](02-validation-jobs.md).

That automatic mode never changes these interactive commands: even at `off`,
an explicit `putnami architecture validate`, `snapshot`, or `inspect` request
runs normally.

`snapshot` emits the complete canonical declared/observed graph, coverage,
stable findings and ratchet dispositions. `inspect` accepts only an exact
manifest-minted domain ID and returns that domain plus its inbound, outbound,
observed and violating relationships. Human and ResultV2 output are projections
of the same evaluation. Relationship direction follows fact flow
(`producer → consumer`): inbound means the selected domain consumes the facts;
outbound means it produces facts consumed by another domain. This is not the
dependency-graph arrow convention. See the
[protocol README](../../../protocols/architecture/README.md) and
[ADR 0008](../../cli/doc/adr/0008-executable-architecture-command-boundary.md).

### `init` and `sync` — the authoring half

```bash
putnami architecture init <domain-id> [--owner <owner>] [--at <dir>] [--dry-run]
putnami architecture sync [--apply]
```

Both write into the source tree, and both stop exactly where a human decision
starts.

`init` creates one canonical `putnami.architecture.json` for a domain that does
not exist yet. It states only what you stated: the domain id, its owner
(defaulting to the domain id), and the projects your **selection** resolved to —
this is the one subcommand of the four groups whose project selection is its
*content* rather than its scope, so `--projects` and `--scope` are honored. Two
modes are refused instead: `--impacted` resolves to what changed, which is not a
membership, and `--all` would claim every project in the workspace for one
domain. An unscoped run writes `"projects": []`, which is the protocol's own way
of saying the domain maps nothing yet.

The manifest lands beside the first project the domain maps, in canonical order;
`--at <dir>` puts it anywhere else in the workspace. A domain another manifest
already declares is refused by identity, an existing file at the target path is
refused by the exclusive create itself, and `--dry-run` prints the target path
and the exact canonical bytes without writing. Exports, imports, and bindings
stay for you to author, for the same reason `specs init` writes no requirement:
each is an agreement between two domains that no generator can derive.

`sync` reconciles the **mechanical half** of every existing manifest against the
resolved graph:

| Change | Why it needs no decision |
|---|---|
| Drop a project entry naming a project the workspace no longer contains | Already a hard `architecture.unknown_project` error; there is nothing left to decide about |
| Drop a binding whose project edge the graph no longer observes | Already a hard `architecture.declared_binding_unobserved` error; removing a permission is never the dangerous direction |
| Add a binding for an observed edge the consumer domain **already** declares an import for | The relation was negotiated; the binding only names which projects implement it |

Everything else is refused and named. Sync never writes an **import** — minimized
facts, an access mode, consistency, a justification are a contract between two
domains — so an observed dependency with no declared relation stays a failing
`architecture validate` finding until somebody declares it. Two narrower cases
refuse for the same reason: more than one candidate import (sync will not choose
which contract authorizes an edge) and a planned-only candidate (a planned target
cannot carry a binding, so the decision is whether to promote the import).
Adding a **project** to a domain is refused by omission: membership is authority,
not observation.

Without `--apply` nothing is written. With it, a changed manifest is rewritten in
canonical form through the same builder an authoring program uses; an unchanged
one is left exactly as its author committed it. **The git diff is the
authorization moment** for every binding sync proposes — the write is not.

Sync succeeds even when it refuses: it is an authoring aid, and the verdict
belongs to `architecture validate`. Like `contracts generate`, neither
subcommand is a step of `validate`
([ADR 0013, decision 5](../../cli/doc/adr/0013-sdd-as-a-standalone-extension.md#5-contracts-stays-interactive-only)):
a gate does not write into the source tree.

Both are additions made after the extraction, so neither has a recorded parity oracle — see
[05-parity.md](05-parity.md).

## `features`

The feature-evidence namespace provides local, read-only commands for validating
authored product intent against exact build/framework evidence:

```bash
putnami features list [query]
putnami features validate
putnami features snapshot
putnami features inspect <feature-id>
putnami features diff <base-revision> <head-revision>
```

`list`, `validate`, and `snapshot` take the normal project-selection
vocabulary — `--projects`, `--impacted`, `--all`, `--tag`, `--exclude-tag`,
`--exclude`, `--baseline` — with the same identities, aliases, groups, scope
expressions, baseline resolution, and deterministic ordering a job command uses.
`inspect` and `diff` already name an exact target, so they **reject** those
flags instead of accepting one that would change nothing.

`list` is the compact catalog: every explicitly authored feature — native design
declarations plus durable `putnami.features.json` entries — with its outcome,
owner, implementing projects, and exact declaration sources. The
`sdd.list_features` MCP tool answers from the same builder, as one bounded page
of short entries ([04-mcp-tools.md](04-mcp-tools.md)). An optional `query` filters
ids, names, outcomes, and owners **after** project selection.

`validate` discovers `putnami.features.json` only at the workspace root and
exact project roots, loads their separate evidence artifacts and Capability
Manifests, coalesces identical dependency copies by semantic owner, and reports
strict, sorted diagnostics. Missing, stale, contradicted, and unclassified
evidence stays visible as assessment warnings; malformed documents, conflicting
copies, and broken or ambiguous contribution references fail.

### Selected projections

Selected projects are **seeds, not a filesystem wall**. A scoped run:

- reads durable `putnami.features.json` and canonical `specs/*.json` at **every**
  root. Those are the only artifacts that mint an identity, so reading them
  workspace-wide is what keeps global duplicate detection exact under any
  selection. Each is one bounded document per root.
- reads evidence fragments, capability manifests, and generated design graphs
  only for the selected projects **plus the exact roots a selected feature
  reaches**: the project that produced its evidence, and the project that owns a
  technical contribution that evidence names. Followed records keep their exact
  source project and path.
- never opens an unselected project's design graph. That is the read whose cost
  grows with the repository.
- fails on an external error that invalidates a selected feature — a duplicate
  declaration of a selected id is still an error, still named by its own path —
  and never on an error wholly outside the selection.

Human output opens with the resolved scope, and `--output=json` carries a
`selection` object with the mode, resolved baseline and its resolution tier, the
selected project ids, the selected feature/spec counts, and every external
record followed. A `--impacted` run that finds no change is a successful,
explicit no-op; a `--projects` selector matching nothing is a not-found error.

The selection is resolved by the **CLI**, not here: it arrives on the wire as a
`selection` block naming the mode, the resolved baseline and its resolution
tier, and the project ids. Nothing in this extension re-resolves it, because a
second impact resolver is a second definition of what changed.

`inspect` shows one feature's authored intent and target, evidence-derived
current maturity, each requirement state, exact semantic contribution
identities/owners, transport containers, and content-bound declaration/artifact
provenance. Feature metadata is inspection-only and never affects runtime
activation or policy.

`snapshot` emits the deterministic current-workspace projection. `diff`
resolves both arguments to exact commits, rebuilds each commit's own workspace
and project membership, and evaluates its manifests, evidence, Capability
artifacts, and source bindings directly from immutable Git objects. It reports
added, removed, promoted, regressed, newly stale, newly contradicted, and newly
unclassified facts without checking out either revision, moving `HEAD`, or
consulting current-worktree artifacts. Unsafe, unresolved, or ambiguous
revision arguments fail with typed diagnostics.

Snapshot and diff `data` are deliberately **provisional** until a separate
compatibility decision freezes them. `--output=json` uses the stable CLI
ResultV2 envelope while data carries `compatibility: "provisional"`; human
output also labels these views provisional and is not a parsing surface. See the
[feature evidence and Capability Manifest v2 RFC](../../../protocols/features/design.md)
for discovery, maturity, diagnostics, determinism, containment, redaction, and
migration rules.

## `specs`

The spec namespace reads the durable specifications defined by the
[spec contract](../../../protocols/features/doc/adr/0001-minimal-spec-contract.md):
direct JSON children of `specs/` at the workspace root or at an exact project
root, each detailing exactly one already-authored feature.

```bash
putnami specs list
putnami specs inspect <feature-id>
putnami specs validate
putnami specs verify [--session <id>]
putnami specs baseline [--update]
putnami specs init <feature-id> [--dry-run]
```

`list` and `validate` take the same project-selection vocabulary `features list`
does, and scope the same way (see [Selected projections](#selected-projections)).
`inspect` and `init` name an exact feature, and `baseline` derives a
whole-workspace floor; all three reject those flags rather than silently
ignoring them. There is no filename filter on any of them: identity is the
`feature` field.

`list` is the compact catalog: for every document, the feature it details, its
exact path and owning project, its first intended outcomes, and its non-goal,
requirement, and decision counts. A filename never mints identity — the
`feature` field does — so a path is provenance only. Discovery degrades rather
than fails: a document that cannot be parsed is reported as a diagnostic and a
document that does not resolve is marked `valid: false`, instead of hiding every
healthy spec.

`inspect` answers for one exact feature id: its durable declaration from
`putnami.features.json`, the spec's outcomes, non-goals and agreed requirement
sentences, every linked `doc/adr/*.md` record with whether that record exists in
this worktree, the source path and owning project, and the diagnostics scoped to
that document. It loads no design graph, evidence document, or capability
manifest.

`validate` reuses the protocol's own parser and repository validator — a second
interpretation of these bytes would be a defect — and **fails** on an invalid
document, an unresolvable or duplicated feature reference, an unsafe decision
path, or two specs for one feature. It also **reports**, as warnings that never
change the exit code, the authoring gaps around the contract: authored features
with no spec (`specs.missing_spec`), publishable projects with no entry in
`putnami.support.json` (`specs.missing_support_entry`), publishable projects with
no owning feature link (`specs.missing_feature_link`), and linked decision
records that are not in the tree (`specs.unresolved_decision`). Human output
prints every error and a bounded number of warnings; `--output=json` always
carries the complete list. Under a selection the completeness counts narrow to
the **selected** publishable projects and authored features, while duplicate
detection stays workspace-wide.

`verify` is the audit surface of the executable-spec gate (decision 9, fixed).
Core's post-session finalizer — attached to every real engine
session containing `test` — joins the criteria projection `specs-validate`
emits with the observation reports the test jobs declared, decides each
textual requirement (`verified`, `unmapped`, `unexecutable`, `missing`,
`stale`, or `contradicted`) through the protocol's one pure evaluator, and
persists the verdict record beside the session. `specs verify` **reproduces**
that decision: per spec-owning project, the effective
`enforce | report | off` mode with its provenance (project override,
workspace policy, or the built-in `report`), every requirement's state with
per-check test provenance, the observation reports read with their SHA-256
digests, and the recorded blocking decision — reproduced, never recomputed.
`--session <id>` names an exact recorded session; the default follows the
latest one, and a spec no session covered is evaluated against zero
observations through the same shared rule, so nothing unproven reads as
green. Under an effective `off` policy the group reports
`automaticEvaluation: false` and succeeds; structural spec errors keep
failing exactly as `specs validate` fails. The committed policy lives in
`options.sdd.verification` (workspace or spec-hosting project), and an
unknown domain or value there fails every run before jobs execute.

`baseline` maintains the rollout ratchet of that gate. It
derives the **enforced floor** — every project whose effective specs
verification mode is `enforce`, with the `feature#requirement` identities its
criteria make executable — and compares it byte-for-byte with the committed
`specs.baseline.json` files. Each enforced project commits its own file in its
own directory, so two changes to two projects never edit the same file.
`--update` rewrites those files to the canonical derived floor, deletes the
file of a project that left `enforce`, and moves the entries of an older
workspace-root file into each project's directory. It is the one place the
baseline is raised or deliberately lowered; the `specs-ratchet-validate` step of
`validate-workspace` only ever compares, and fails when an enforced project
regressed to `report`/`off` or lost recorded requirement coverage without an
edit to the project's committed baseline in the same change. Growth never requires an
edit — the check passes and nudges you to raise the floor. The floor is
whole-workspace by construction, so project selection flags are rejected.

`init` creates one minimal canonical skeleton next to the declaration of an
already-authored feature. It refuses an unknown feature, refuses a feature that
already has a spec, and never overwrites an existing file (the document is
created exclusively, so the guarantee is a property of the write). The skeleton
states only what the declaration states — the feature's own outcome, an empty
requirement list, no decisions — because requirements and decisions are
agreements no generator can derive. Its default path uses the final feature-id
segment: `billing/invoice-export` creates `specs/invoice-export.json`, while the
document and report keep the complete `billing/invoice-export` identity.
Existing specs are never renamed. If another feature already occupies the same
leaf path, init refuses that path instead of inventing a different filename.
`--dry-run` prints the same target path and exact canonical bytes without
writing. There is no batch or `--all` mode; see
[ADR 0007](../../cli/doc/adr/0007-read-mostly-spec-surface.md) for why this
surface stays read-mostly and non-destructive.

## `contracts`

Generate and check a project's committed contract artifacts.

```bash
putnami contracts generate --project /tooling/cli   # Regenerate committed artifacts
putnami contracts check --project /tooling/cli      # Fail on drift or a breaking change
```

`generate` validates the authored contract manifest and atomically rewrites the
committed canonical IR, the Go type twin, and the JSON Schema under `schema/`.
`check` regenerates in memory and exits `2` on artifact drift *or* on a
renamed/removed enum value, scope, claim, or grant compared with the
git-committed prior contract, emitting a machine-readable compatibility report.
Both accept `--output=jsonl`.

**Neither is part of the `validate` job**, deliberately. `check`'s exit `2` means
"artifacts drifted", which is a different verdict from the job contract's
generic failure, and flattening the two would lose the distinction exactly where
it matters. `generate` writes into the source tree, which a gate should not do.
Both take a per-project positional target rather than the selection vocabulary.
Revisit after the validation jobs have adoption
([ADR 0013, decision 5](../../cli/doc/adr/0013-sdd-as-a-standalone-extension.md#5-contracts-stays-interactive-only)).

## Where the dispatcher answers instead of the extension

Four differences from the pre-extraction built-ins are the CLI's, not this
extension's, and cannot be reproduced from inside a subprocess the CLI never
reaches:

- `putnami features nonexistent` is answered by the CLI's extension dispatcher
  before the binary runs, so the message is the dispatcher's ("unknown
  subcommand", subcommands listed alphabetically) and the exit code is `1`
  rather than the built-in's `2`.
- `putnami features` with no subcommand prints the group's subcommand listing
  instead of the built-in's "unknown subcommand" error.
- An undeclared flag is a deprecation **warning** for an extension group and a
  hard rejection for a built-in.
- A selection-rejecting subcommand given an **invalid** selector
  (`putnami features inspect X --projects nope`) reports the selector's own
  not-found, because selection resolves before dispatch. A valid selector still
  gets the verbatim refusal.

Everything else is byte-identical to the built-in it replaced, proven by the
recorded parity fixtures — see
[05-parity.md](05-parity.md).
