# Local workflow record

This is a versioned, workspace-local projection of an accepted outcome. Its
consumer is the consumer workspace's `putnami change verify --workflow FILE`.
First check that `"$PUTNAMI_CLI" change verify --help` advertises `--workflow`;
an older CLI may accept a v2 dossier while ignoring its optional workflow
member. An absent capability is a missing validator, never progress proof.
Save records under the ignored `.putnami/<issue>/` directory. Give a v2
dossier an optional `workflow: {path, sha256}` exact reference. Keep the
original v2 snapshot, candidate review, gate, qualification and acceptance
checks: a workflow reference adds completeness constraints and cannot waive
them. The standalone workflow verdict is `workflow valid` at stage `progress`,
never `Putnami verified` at stage `ready`.

The v1 record has `version: 1`, a stable `outcome`, an exact `mandate` reference,
and `sources: [{feature, manifest: ref, spec: ref}]`. `sources` points to
native feature declarations and specs by exact bytes. Load their entire
requirement inventory; identifiers are qualified as `feature#requirement`
because requirement IDs are local to a spec. The manifest and spec paths
must be Git-tracked in the consumer workspace;
an ignored local spec cannot redefine the authoritative inventory.
Workflow references refuse symlinks and bind the exact regular-file bytes.
The record's `requirements` partitions every selected native requirement
into `current`, `later` or
`excluded`. Current and later rows name a `unit`; an exclusion instead names
`reason` and actual `authority` references. Do not drop a requested large
outcome when a first unit has a smaller limit. An unsupported exclusion blocks
completion. The native feature declaration, not this record, owns expected
verification check IDs.

`units` contain a stable `id`, observable `outcome`, native `projects` as
`[{id, manifest: ref}]` for each exact project `putnami.json`, exact
`requirements`, `dependsOn` unit IDs, a `boundary` ref, `exclusions` refs,
native `nextCycleTasks` references, unresolved `decisions` refs,
`requiredStage`, current `stage`, `nextResult`, and a `receipt` when the stage
has one. A merged claim also names the unit's opaque `proposal` reference,
which the CLI checks against a native proposal-status receipt. Required stage is one of
`candidate`, `ready`, `merged`, `published`, `deployed`, `adopted`; the current stage can
also be `planned` or `building`. A unit's requirement list must equal its
assigned requirement rows. Dependencies explain delivery order, while native
project dependencies control builds. A milestone or task count is not a
receipt. Unresolved `decisions` block that unit's ready claim. Each next-cycle
task is either `{kind: "provider", ref: {source, id}, revision?, receipt?}`
with the provider identity kept opaque, or `{kind: "local", record: ref}`.
`revision` and a native `tasks get` envelope `receipt` travel together when
exact provider input is needed; never reconstruct a task reference by parsing
a display string. The local task record is strict JSON
`{version: 1, id, inputs: [ref], nextResult}`. Ready source, merge, package publication, deployment and consumer
adoption use distinct actual producer or provider references. Never infer one
from another or store a manually synchronized completion percentage.

Use `artifacts` for a proportionate native spec, design note, prototype, ADR,
review or other source. Each carries `id`, `kind`, `contractVersion`, exact
`source` and `context` refs, `author`, `units` and native `scopes` links,
`lifecycle` (`proposed`, `challenged`, `accepted`, `superseded`) and `material`.
Artifact and handoff scope IDs must match declared unit projects.
A material artifact also carries a distinct
`challenge: {original: ref, revisions?: [ref], snapshots?: [{original: ref,
copy: ref}], resolutions: [...]}`. The original immutable
reviewer-authored JSON v1 has `version`, `reviewer`, `author`, exact `artifact`
and `context` refs, a report ref and stable findings, each with `id`,
`scenario`, `consequence` and `correction`. The challenge resolution list
names the finding `id`, `state` (`fixed`, `refuted`, `deferred`), `note` ref
and actual `authority` ref for deferral. Preserve the original report and
examined artifact and context bytes at immutable versioned paths. If a
historical examined artifact or context path changes in place and its original
digest is no longer there, a snapshot pairs its old exact ref with a copy at
a distinct path whose digest matches those historical bytes. Each revision is
a later independent reviewer-authored v1 challenge, with finding IDs unique
across the chain. The final review must bind the current source and context;
a fixed finding needs a later independent review whose artifact digest differs
from the version it corrects. Preserve every
original finding and append correction/re-review, counterevidence or authorized
deferral. A new source or relevant context digest reopens its opinion; an
unrelated edit does not. A
whole-tree candidate review and gate remain bound to their exact tree even
when an artifact opinion can be reused.

An `experiments` entry has `id`, `question`, `effortCeiling`, exact `inputs`,
inspectable `asset`, `instructions`, observed result ref, `simulated` behaviors,
`assumptions`, `conclusion` and remaining `uncertainty`. Only run one when it
can resolve a material question. A simulation remains a simulation and cannot
establish production acceptance. A reused prototype implementation enters
ordinary build review and proof.

`stages` contains exactly the canonical `intent`, `spec`, `design`,
`prototype`, `review`, `adr`, `candidate` and `handoff` entries **for each
unit**. Each has a `unit` ID, `name`, `state` (`pending`, `blocked`, `passed`,
`not-applicable`), nonempty `entryCondition` describing the observable
prerequisite for starting that stage, exact `inputs`, `expectedOutput`,
`exitCriteria`, and a real `receipt` for passed or a `reason` ref for
non-applicable. The entry condition is separate from the referenced inputs
and the supported result. A settled change may mark a new
prototype or ADR not applicable with a reason; it does not omit the stage.
Every current unit's stages must have passed or be not applicable, and its
candidate stage must be passed, for that unit to reach ready. Pending or
blocked stages of later units remain visible without falsely failing a
current ready unit. `handoff` names the same `mandate` ref as the
top level, a `unit`, native `scopes`, the receiving `responsibility` and
`receiver`, exact `inputs`, `expectedOutput`, `acceptance`, `nextAction` and
`unresolvedFindings`. Use repository or provider
versioned paths for portable inputs; an ignored local asset that was not
transferred must be named as unavailable and blocks only the stage consuming
it. For the handoff unit, include every next-cycle provider `receipt` ref and
local task `record` ref in `handoff.inputs`; a provider entry without a receipt
is a planning identity, not an exact task-body handoff. Reconcile provider
revisions, open findings and current tree on resume.

Optional `proof: {session: ref, specVerification: ref}` points to the native
producer session and its sibling `spec-verification.json` on the same current
tree. Never write these receipts by hand. The verifier joins native expected
checks to producer observations: missing, duplicate, unknown, skipped, failed
or stale results cannot be counted as passed. A restored producer result needs
its native cache and session corroboration; a handwritten restored flag is
not proof. A well-formed planning record
can have no proof and still report progress. A v2 ready dossier that references
the workflow requires all current included native requirements proven from
the actual producer, in addition to its existing review/gate/qualification and
acceptance checks. An unexecutable native criterion stays visible and needs
actual reviewed acceptance evidence, not an invented native verdict.

`outcomeComplete` also requires each unit's exact `requiredStage` with its
actual receipt. Current native checks and a candidate-ready dossier cannot
prove a required published, deployed or adopted result. When the CLI cannot
verify a later provider receipt, it reports incomplete conservatively.

The JSON contract and diagnostic spelling are defined by the installed CLI.
If its `--workflow` is missing, unknown-version, malformed, unreadable or
reports unavailable inputs, stop the affected transition and report that
exact failure. Do not reconstruct a producer receipt or recalculate an old
opinion's digest to conceal changed inputs.
