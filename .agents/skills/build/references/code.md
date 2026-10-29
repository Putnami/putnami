# Code

The main session codes the slices of the plan in sequence. Parallel work means
several issues of an epic, each in its own session, never slices of one issue.

## One slice

1. Start from the slice contract: goal, done criteria, files, proof command.
2. Edit, then format and finish the edits.
3. Run the gate over the touched projects:
   `"$PUTNAMI_CLI" lint,test,build,validate --projects <a>,<b> --enforce-coverage`.
   Run it again only after a new edit. On a failure, iterate while the
   diagnostics show progress; a concrete transient failure may use
   `--retry-failed` once.
4. Commit the slice. Before the push, rebase the branch when its parent branch
   moved or merged; after the parent's squash merge, run
   `git rebase --onto <base> <parent's old tip>`.
5. Push the checkpoint with `change finalize --draft` again: it pushes the
   committed HEAD and refreshes the title and body, so rewrite the body when
   the net change moved. Never push a failing or half-edited tree.
6. Report the slice: what it delivered, its gate session, and the tokens spent
   against its estimate. `"$PUTNAMI_CLI" agent stage code` prints the
   session's tokens so far. A slice 50 % over its estimate, or one that leaves
   its declared projects, stops for the owner when an envelope was approved,
   and is reported otherwise.

## One intent, gated phases

A proposal carries one intent. Its size is not a reason to split it: a change
that updates its tests and docs is larger by design. The impacted gate prints
the change size by category (code, tests, docs, generated) as information; no
line count fails it.

Work that spans projects no dependency relates goes on an integration branch
that matches the workspace's `epicBranches`, one commit per phase, each phase
gated before the next, merged as one unit one revert undoes. The gate warns
when the authored code spans unrelated projects off an epic branch. Act on
the warning: split the change by intent, or move it to an epic branch, and
name the decision in the proposal when you keep the spread.

## A slice in a separate agent

When the workflow profile sends code to a separate agent, delegate the slice
to the profile's worker: `fix-standard` by default, `fix-heavy` when the slice
contract names a heavy risk (determinism and cache keys, concurrency,
security, cross-language protocols). Give it the slice contract, its owned
files, its acceptance, its proof command and the other writers it must
accommodate. A worker never reverts someone else's edit, commits, pushes or
changes a task or proposal; the main session integrates and commits. Use
separate worktrees for tool outputs that conflict.
