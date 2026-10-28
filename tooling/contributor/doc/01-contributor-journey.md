# Contributor journey

`build <intention|task|proposal>` carries an authorized outcome through one
loop of eight stages, from talk to merge
([ADR 0009](adr/0009-one-contribution-loop-from-talk-to-merge.md)). It does
not select backlog work. A named task may be claimed only when that action is
explicitly authorized. By default, build publishes its own proposal: a draft
once the plan is settled, a pushed checkpoint at each slice, and ready for
review once the final gate passes. When the outcome changes a declaration, the
first checkpoint holds the declarations only, so a reviewer can correct the
intent while the code is written
([ADR 0008](adr/0008-build-pushes-its-intent-before-its-code.md)). `--local`
or a mandate that says to stay local keeps the run local. A request alone
grants no task claim, merge or deployment.

The skill's core is short. On entering a stage, build runs
`putnami agent stage <stage>`, so the mission shows it, and reads that
stage's reference only.

| Stage | What build does | Reference |
|---|---|---|
| Talk | Iterates with the owner until a viable solution exists | `talk.md` |
| Declare | Reads or creates the task, checks the intent against the declarations, writes the plan to `.putnami/<issue>/plan.json` and sizes it with `putnami change estimate` | `declare.md` |
| Show | Opens the draft proposal with the declarations first: spec, architecture, ADR, mockups | `show.md` |
| Code | Codes the slices in sequence in the main session, gates and pushes each one | `code.md` |
| Check | Freezes the candidate, has a fresh reviewer review it, repairs P1 and P2 findings, runs the final gate | `check.md` |
| Prove | Qualifies each workload the change reaches, and captures the changed screens | `prove.md` |
| Validate | Puts the attention points in front of the owner, and returns to code on a change request | `validate.md` |
| Merge | Verifies the dossier and marks the proposal ready; merges only with authority | `merge.md` |

Code waits for the owner only when the plan spans several slices with
important design or spec decisions, or touches a sensitive zone: the owner
then approves the study and a cost envelope first. An S plan, or several
slices with no important new decision, starts at once. After an approval,
build stops only when it exceeds the envelope, must change the approved
design, or contradicts an ADR.

The main session runs every stage. A workflow profile in the
[repository policy](02-repository-policy.md) can move a stage to a separate
agent, with its worker profile, model and effort. The default profile runs
only review in a separate agent, the `reviewer` worker. `spec` and `design`
stay callable on their own; `design` also records an ADR and runs a bounded
prototype when the choice needs one.

The [workflow contract](04-workflow-contract.md) gives the versioned fields and
stage rules. The [repository policy](02-repository-policy.md) gives conventions;
providers own task and proposal capabilities. A local validated record does not
authenticate its writer or grant an external action.

## Providers the journey reaches

| Step | Contract | Without a binding |
|---|---|---|
| Read or explicitly claim a named task, then deliver it when authorized | tasks | Build can start from an intention; task operations require a binding and mandate |
| Publish and read back a proposal | proposals | Finalization stops before writes and names the binding |
| Mission context and checkpoints | memory | Keep a local checkpoint and disclose that it does not survive workspace deletion |

Providers advertise optional operations and preconditions. A checked task
revision is best effort under a concurrent writer, assignment is not an
exclusive lease, and an unresolved write requires provider read-back before a
retry. Hosted checks and merge are separate capabilities.
