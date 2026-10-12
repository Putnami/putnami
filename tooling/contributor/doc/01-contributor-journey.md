# Contributor journey

`build <intention|task|proposal>` owns an authorized outcome through one
implementation, challenge, review, repair and evidence loop. `fix` selects and
claims a task before entering build; `epic` supplies coordinated dependencies.
Neither build nor `execute` selects backlog work implicitly. The invocation's
mandate controls task, Git, proposal, merge and deployment actions throughout.

1. **Understand the outcome.** Read repository instructions, the accepted
   mandate, current task/proposal and existing plan. Start discovery with
   Putnami, then native project dependencies and scope contracts. Load the
   selected feature/spec requirements and acceptance, including later work.
2. **Map delivery.** Define useful units by observable behavior, their limits
   and required receipts. Keep milestones as coordination markers. Give the
   next cycle precise tasks, inputs, environment, proof entrypoint and expected
   inspectable result; keep later cycles proportional. A settled small change
   reuses existing spec and design.
3. **Resolve uncertainty.** Use `spec` for the native acceptance contract,
   `design` for technical choices, `prototype` for a bounded material question,
   and `adr` for a durable choice. Each is proportional, with a reason when a
   new stage has no role. `review` challenges a material proposed artifact in
   a distinct context against exact source and relevant context versions.
4. **Implement.** Delegate bounded files and acceptance to the policy's worker
   tier. Preserve scope positions, original findings and refs. Integrate
   serially and freeze the candidate before independent code review or gate
   evidence. A multi-project unit need not become several proposals.
5. **Review and repair.** An independent `review`/`code-review` context covers
   the frozen candidate. Fix findings in the same change; preserve original
   reports and append resolutions. A changed candidate gets a new snapshot.
6. **Verify.** Run the canonical `--projects` iteration gate and one final
   impacted gate in the loop's order, reusing applicable records. Qualify
   reachable workloads. Native feature checks come from actual producer
   observations, not handwritten status. `check` remains directly callable;
   build performs the applicable check automatically.
7. **Project progress.** `tree verify --workflow <file>` validates the
   separately versioned local map and reports progress, including pending
   stages. A workflow ref may be attached to a v2 dossier; first confirm the
   CLI advertises `--workflow`, because an older dossier reader can ignore an
   unknown member. The dossier's `--record` ready verdict still requires its
   own candidate review, gate, qualification and acceptance, and the optional
   workflow adds complete current native requirements.
8. **Deliver within authority.** The existing finalizer publishes the
   verified proposal and reads it back. For a partial unit, pass
   `--partial-delivery` on draft and final calls: the parent stays linked and
   its task state stays unchanged. Normal close/transition is
   for the complete mandated outcome. Record ready, merged, published,
   deployed and adopted from their separate actual receipts.
9. **Handoff or resume.** Save the mandate, unit, scopes, receiver,
   responsibility, exact inputs, expected output/acceptance, original
   unresolved findings and next action. Use repository/provider versions for
   portable refs. An ignored local asset that has not moved to another host is
   unavailable there; only its dependent stage is blocked. Reconcile live
   provider and current tree state on resume.

The [workflow contract](04-workflow-contract.md) gives the versioned fields and
stage rules. The [repository policy](02-repository-policy.md) gives conventions;
providers own task and proposal capabilities. A local validated record does not
authenticate its writer or grant an external action.

## Providers the journey reaches

| Step | Contract | Without a binding |
|---|---|---|
| Select, claim, deliver a task | tasks | `fix` needs a task; use `build` with an intention or bind tasks |
| Publish and read back a proposal | proposals | Finalization stops before writes and names the binding |
| Mission context and checkpoints | memory | Keep a local checkpoint and disclose that it does not survive workspace deletion |

Providers advertise optional operations and preconditions. A checked task
revision is best effort under a concurrent writer, assignment is not an
exclusive lease, and an unresolved write requires provider read-back before a
retry. Hosted checks and merge are separate capabilities.
