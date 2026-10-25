# Verify local evidence

`putnami change verify`, which the `@putnami/contributor` executable serves,
checks the local evidence of an execute run against the tree digest that
`putnami tree fingerprint` prints, and prints one JSON verdict. It takes
exactly one mode:

```bash
putnami change verify --snapshot --base origin/main       # Dossier skeleton for the current tree
putnami change verify --ref .context/review.md            # The file's path as given and its SHA-256
putnami change verify --workflow .context/workflow.json   # Validate local v1 planning and progress
putnami change verify --record .context/dossier.json      # Verify a dossier
putnami change verify --gate .putnami/sessions/<id>/session.json --report .putnami/reports/<id>.json
```

`--record` checks the dossier's tree binding, changed files, policy files,
consulted scopes, independent review, gate, qualification verdicts and
acceptance evidence, and fails if the dossier changes while it reads it.
An optional `workflow: {"path":".context/workflow.json","sha256":"..."}`
in a v2 dossier also checks the separate v1 workflow record. Existing v2
dossiers without this member keep their earlier meaning. Check `change verify
--help` for `--workflow` before relying on the optional member: an older
verifier ignores an extra dossier member.

The v1 workflow record uses `version: 1`, a stable `outcome`, a `mandate`
reference, `sources`, `requirements`, `units`, `artifacts`, `experiments`,
`stages`, `handoff`, and optional `proof`. A reference is the exact `{path,
sha256}` from `change verify --ref`; paths resolve from the consumer's Git root.
Each source is `{feature, manifest: ref, spec: ref}`. The manifest must be a
native `putnami.features.json`, and the spec a direct child of `specs/` for
that feature; both paths must be tracked by Git. Workflow evidence refs are
bounded workspace-relative files and refuse symlink components before reading.
Every requirement in those specs appears exactly once as
`{feature, id, disposition, unit}` with `disposition` `current` or `later`,
or as `{feature, id, disposition: "excluded", reason: ref, authority: ref}`.
`later` retains requested outcome scope and keeps `outcomeComplete` false;
`excluded` requires a bound explanation and actual scope authority. Native
feature verification criteria supply expected check IDs. The workflow cannot
author a smaller expected-check list.

A unit records `id`, `outcome`, `boundary: ref`, `exclusions: [ref]`, qualified
`requirements` (`feature#requirement`), `dependsOn` unit IDs,
`nextCycleTasks` native task references, unresolved `decisions: [ref]`, `nextResult`,
`requiredStage`, and `stage`. Each `projects` entry is `{id, manifest: ref}`;
its canonical ID is derived from the referenced native `putnami.json` path.
The feature/spec source's project must belong to the unit. `requiredStage` is
the mandate's delivery target, among `candidate`, `ready`, `merged`,
`published`, `deployed`, and `adopted`. `stage` also allows `planned` and
`building`. A unit with `later` or unverified requirements cannot claim
`candidate`. Unit dependencies express delivery ordering, not a scheduler; a
current unit cannot become ready while a dependency remains incomplete. A
verified achieved stage at or beyond `requiredStage` satisfies the delivery
target: native `ready` covers `candidate`, and native `merged` covers `ready`.
The achieved stage needs its own valid receipt. `published`, `deployed`, and
`adopted` stay incomplete until native receipt adapters exist. Each
next-cycle provider task is `{kind:"provider",ref:{source,id}}`; an exact task
handoff pairs `revision` with a `receipt: ref` to the native `tasks get`
envelope. A local task is `{kind:"local",record:ref}` to a strict v1 local
task JSON with `id`, `inputs:[ref]`, and `nextResult`. Provider identity is
the opaque source/ID pair, never a concatenated display string. A task receipt
or local task record for the handoff unit must also appear in its exact
`handoff.inputs`.
`--record` can establish `ready` for a current unit at `candidate` without a
digest cycle. Hosted `change finalize --gate ci --workflow-file` can also
establish it after the policy-named checks pass on the exact full HEAD. The
hosted checks never stand for native proof: a unit at `building` stays there,
and a partial delivery that publishes it names no ready unit. To
resume a unit already at `ready`, its `receipt: ref` points to the unchanged
JSON output of either a prior successful `change verify --record` call or that
hosted finalizer. The local receipt requires the native ready verdict,
assurance, exact current tree fingerprint and full HEAD, and the same dossier
base when verifying a new dossier. The hosted receipt carries the original
workflow path and digest, base, HEAD, fingerprint, proposal reference, named
checks, and native proposal status. The verifier replays the hosted check
against the unchanged original workflow; write a later workflow revision to a
different path to preserve that original. Overwriting it invalidates the
receipt. A standalone `--workflow` call checks either receipt but still only
projects progress; it cannot issue a ready verdict.
Current native requirements, stages, decisions, and dependencies are checked
again, so an earlier generic ready receipt cannot cover missing unit proof.
A `merged` claim needs a matching native `proposals status`
receipt for the unit's `proposal: {source,id}`, with state `merged` and the
exact current HEAD. Other external receipt kinds remain visible but cannot
establish `outcomeComplete` until a native adapter validates them.

Material artifacts carry `id`, `kind`, `contractVersion`, `lifecycle`,
`units`, native project `scopes`, `source: ref`, `context: [ref]`, `author`,
and `material: true`. Their `challenge` has `original: ref` and
`resolutions`, with optional `revisions:[ref]` for later independent v1
reviews and `snapshots:[{original:ref,copy:ref}]` for historical bytes when a
source or context path is repaired in place. Each copy has a distinct path
and the original digest. The original reviewer-authored JSON v1 record contains
`reviewer`, `author`, `artifact: ref`, `context: [ref]`, `report: ref`, and
`findings` with stable `id`, `scenario`, `consequence`, and `correction`.
An appended resolution names the finding `id`, `state` (`fixed`, `refuted`,
or `deferred`), and `note: ref`; a deferral also needs `authority: ref`.
Replacing relevant artifact or context bytes requires a later independent
review of the corrected version. Earlier findings remain in the chain and a
`fixed` resolution needs that later review; rehashing the index cannot renew
an old opinion. An unrelated tree edit
leaves that challenge intact, while it still invalidates a whole-tree
candidate dossier. An
experiment records `question`, `effortCeiling`, exact `inputs`, inspectable
`asset`, `instructions`, `observed` refs, explicit `simulated` behavior,
`assumptions`, `conclusion`, and `uncertainty`. Stage entries cover exactly
`intent`, `spec`, `design`, `prototype`, `review`, `adr`, `candidate`, and
`handoff` for each unit. Each records `unit`, `state`, `entryCondition`, `inputs`,
`expectedOutput`, `exitCriteria`, and a receipt or reason for passed, blocked,
or nonapplicable results. `entryCondition` states what must be true before the
stage can start; `inputs` binds the concrete material. `handoff` binds
the same `mandate`, one declared `unit`, native project `scopes`, `receiver`,
`responsibility`, `inputs`, `expectedOutput`, `acceptance`, `nextAction`,
and `unresolvedFindings`.

`proof` is `{session: ref, specVerification: ref}`. The latter is the native
`spec-verification.json` beside the exact v2 session file, with a matching
session ID and current tree fingerprint. Its native per-requirement and
per-check verdicts must match the complete authored spec and manifest
inventories. The verifier recomputes each verdict from the producer reports.
A requirement recorded `unobserved`, because the gate could not consult a
project outside its selection that might attest it, matches when the
recomputed verdict is `missing` only because no observation reported a check,
when its group reclassifies every such requirement, and when the group carries
the native `features.unobserved_requirement` warning. Missing, duplicate,
failed, skipped, stale, or unknown checks cannot pass a current unit, and
neither can an `unobserved` requirement.
Unmapped and unexecutable requirements remain visible as incomplete. Native collection errors block proof; advisory native
warnings remain visible with the affected incomplete unit, so a later
unobserved requirement cannot silently disappear or block an otherwise ready
independent current unit. A standalone `--workflow` call can report pending
progress without proof; it never prints `Putnami verified` or `ready`.
An unplanned report marked `restored` needs the current native test-plan cache
key, successful task-owned entry, exact declared output manifest, and matching
report bytes. The executable asks the putnami process that launched it to run
that cache check over the host channel; without the channel, a restored report
is not verified. A handwritten `restored` flag alone supplies no proof. A cache
entry keyed by run parameters absent from the session cannot be corroborated
and leaves the workflow incomplete without rerunning a gate.
Its `units` projection shows each unit's outcome, requirement/check verdicts
and native producer report references, next inspectable result, pending tasks,
decision references, and blockers. `requiredStageReached` means the verified
achieved stage covers the required stage and native unit requirements pass;
`deliveryReady` additionally requires completed stage exits, no unresolved
unit decisions, and ready dependencies. A blocked stage needs a reason ref and
appears in that unit's blockers. Unsupported external receipt kinds have an
explicit blocker.
`--record` with a workflow reference requires proof, passing current units,
passed or reasoned nonapplicable current-unit stages, and no unresolved current
unit decisions. A later independent unit may stay pending or blocked; its
required outcome stage remains incomplete. The verifier then prints its
ordinary candidate-ready verdict with a workflow progress
member. That candidate stamp never claims an incomplete parent outcome,
merge, publication, deployment, or adoption.
`--gate` checks that a recorded gate session and its report can replace the
gate the PR finalizer would run. A gate must cover the impacted plan, which the
workspace CLI of the checked repository answers as a dry run (`--plan`).
`--base` names the base revision. `--snapshot` and `--gate` default it to
`origin/main`. With `--record` it has no default; when given, the dossier's
base SHA must equal `git merge-base <rev> HEAD`.

With `--record` and `--gate`, the gate's report must record
`enforceCoverage: true`, and the gate must satisfy the CI policy,
`putnami.ci.json` version 3: it runs every blocking command, and every flag the
policy appends has evidence. `--fix=false` passes only when the gate's report
records `fix: false`, which a run given `--fix=false` or `--no-fix` writes.
`--enforce-coverage` and `--continue-on-error` need nothing more. Any other
flag, `--fix=true` included, fails the check. The impacted plan carries the
policy's `--enforce-coverage` and `--fix=false` flags, never
`--continue-on-error`, so a `--fix=false` gate is checked against the lint
tasks `--fix=false` plans.

Paths resolve against the Git top level of the directory the executable
starts in: the workspace root inside a workspace, and the caller's directory
outside one. Evidence is read strictly: invalid UTF-8, duplicate JSON members,
or a file that is not JSON fail the check. A failure prints
`{"verdict":"not-verified","reason":"..."}` on one line, exits 1 and writes
nothing to stderr, and `--output` does not change the document. The verifier
writes no file. Outside a workspace, the command runs when
`@putnami/contributor` is installed in user scope. It is a consistency check,
not a signed review or an access boundary. For a flag the manifest does not
declare, putnami warns on stderr and still launches the executable, which
prints a not-verified verdict that names the argument.

[ADR 0005](adr/0005-the-contributor-executable-serves-the-change-noun.md)
records how the executable serves the command and where it differs from the
core command it replaces.
