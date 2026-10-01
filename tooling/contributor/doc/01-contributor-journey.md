# Contributor journey

One loop carries a change from intention to authorized delivery. Every entry
point enters it; none nests a second loop.

1. **Start.** `execute <intention>`, `fix <task>` or `epic <task>`. `fix` selects
   and claims a task through `putnami tasks`; `epic` supplies scope and
   dependencies. When memory is bound, the run reads its mission and the
   affected projects' context through `putnami memory mission` and
   `putnami memory context`.
2. **Plan proportionately.** Reuse the current plan, identify the smallest
   executable vertical and its proof entrypoint, and consult only the scopes
   whose contracts are at stake. A small local change stays small: one
   vertical, one gate, one bounded review.
3. **Implement.** The coordinator delegates each vertical to the worker profile
   the repository policy selects for its tier, integrates serially, and owns
   Git and collaboration state.
4. **Freeze and verify.** Snapshot the candidate
   (`putnami tree verify --snapshot`), run the canonical gate against the
   snapshot's base, and qualify every reachable workload with
   `putnami qualify <project> --target local`.
5. **Review independently.** A fresh `code-review` context reviews the frozen
   candidate and writes its own report.
6. **Repair.** Findings are fixed in the same change; affected evidence and
   review are renewed, never reused across a changed binding.
7. **Checkpoint.** Before a host limit or a stop, save the checkpoint with
   `putnami memory checkpoint` when memory is bound, with evidence entries that
   point at the gate, review and qualification records. Without memory, the
   checkpoint stays in the ignored run directory.
8. **Verify the dossier.**
   `putnami tree verify --record <dossier>` computes
   `Putnami verified (local pilot, declared scope)` or refuses.
9. **Deliver what is authorized.** `bash .agents/skills/fix/scripts/finalize-pr.sh
   --verification-file <dossier>` pushes with Git, publishes the proposal
   through `putnami proposals upsert`, and moves the task to the policy's
   delivered state. Merge and deployment continue only under an explicit
   mandate, through a provider that offers them and the repository's controls.

## Providers the journey reaches

| Step | Contract | Without a binding |
|---|---|---|
| Select, claim, deliver a task | tasks | `fix` needs a task: bind one, or use `execute` with an intention |
| Publish, read back, review a proposal | proposals | The finalizer stops before any write and names the binding to add |
| Mission context and checkpoints | memory | The checkpoint stays in the ignored run directory |

Each provider states what it supports: hosted checks, `merge`, `assign`,
`claim`, and how it enforces `expectedRevision`. The workflows report the
subset they used instead of treating a missing capability as done.
