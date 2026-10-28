# Check

Check reviews the frozen candidate in a fresh agent, repairs what blocks, and
runs the final gate once on what review accepted.

## Freeze

Commit the checkpoint before review, unless the run is local: HEAD is part of
the tree fingerprint, so a later commit, rebase or edit invalidates every
binding. Capture the candidate with
`"$PUTNAMI_CLI" change verify --snapshot --base origin/<base>`, against the
remote base the proposal targets, and read its exact `binding.baseSHA`.
Nobody edits the tree while it is reviewed.

## Review

Launch one fresh reviewer, the worker the profile names for review
(`reviewer` by default), with the [review](../../review/SKILL.md) method. Its
brief: the objective, the base, head and fingerprint, the diff, the slice
contracts and the impact, the specs, architecture and ADRs of the touched
projects, and the gate records. Never the expected verdict or your private
reasoning. It writes its report to `.putnami/<issue>/` and returns its path.

| Severity | Meaning | Blocks |
|---|---|---|
| P1 | Correctness, security, data loss or a broken required check | yes |
| P2 | A substantiated design, edge-case or performance defect | yes |
| P3 | Polish | no: name it in the proposal body |

A contradiction with an ADR blocks: fix the code, or write a new ADR the owner
decides. A one-way door, or a sensitive zone the diff touches, adds one pass
per named risk: security, performance or design.

## Repair

1. Fix each P1 and P2 finding in the same change, and keep the original
   report; append responses rather than rewriting it.
2. Take a new snapshot: a repair moves the candidate.
3. A fresh reviewer checks only the P1 and P2 fixes, with the findings and the
   repair diff.
4. After two rounds, a disagreement goes to the owner with both positions.

## The final gate

Once no finding needs an edit, run the gates in this order, and no other:

1. The `--projects` gate of the last edit, from the code stage.
2. One final impacted gate on the accepted candidate, or the hosted gate when
   the policy and the machine's load select it.

Measure the machine first with `"$PUTNAMI_CLI" doctor --machine`. When the
ratio exceeds the policy's `verification.ciGate.load` (default 0.7) and the
policy names `verification.ciGate.checks`, run no local impacted gate: the
merge stage calls `change finalize --gate ci`. Otherwise run
`lint,test,build,validate --impacted --baseline <baseSHA> --enforce-coverage`,
with `--fix=false` when the policy's `flags` list it. `validate` includes its
`validate-workspace` companion; derive the commands from `putnami.ci.json`.

Do not replay a gate someone else ran: read its session record and compare
its `tree.fingerprint` with the live tree. Save the exact session and report
paths the invocation returns; never pick a record by newest timestamp. A
second red run is a blocker to preserve, not a run to repeat. Do not use
`--no-cache` unconditionally.

The [check](../../check/SKILL.md) skill adds the documentation audit and the
language rule; run them on the same tree.
