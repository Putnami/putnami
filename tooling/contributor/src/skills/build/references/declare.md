# Declare

Declare turns the accepted solution into a task, a plan and an estimate. Keep
the run's files in `.putnami/<issue>/`, outside Git: the plan, notes, review
reports and screenshots. An intention without a task uses the branch name, with
each `/` replaced by `-`, in place of the issue.

## The task

- A named task: resolve its ID with the policy's `tasks.source`, or use its
  whole `{source,id}` reference, and read it with `tasks get`. A done or
  canceled task may have moved its scope: follow the explicit successor and
  verify what remains. Reconcile any active proposal before work.
- A claim needs the mandate's explicit authority. Transition the exact task to
  the policy's `states.claimed` with its observed revision as
  `expectedRevision` where supported, then assign `tasks.claimAssignees` only
  if the provider offers `assign`. Read the task back and report its actual
  state and assignees. A conflict needs a fresh read and decision; an
  unresolved write needs reconciliation before a retry.
- An intention stays an intention unless the owner asks for a task; then
  create it with `tasks create`, with the type, priority and topic the owner
  gave. Never search, rank or claim backlog items.
- With an issue number, run `"$PUTNAMI_CLI" agent attach --issue <number>`
  so the session reports to its mission. When memory is bound, read the mission and
  the affected projects' context first
  ([collaboration contracts](collaboration.md)).

## Check the intent

Load the declarations of the projects the intent touches: features, specs and
the checks they expect, architecture, decisions and ADRs. Check the intent
against them, the open tasks and the existing code. On a conflict, stop and
ask the owner.

Partition the native requirement inventory into current, later and excluded
work; an exclusion that changes the outcome needs authority. A smaller unit or
a finished task cannot silently replace the original outcome. Do not author a
second list of expected checks.

Read the [recipe index](recipes.md) before choosing primitives or a nearby
example. Cite the matching intention, its sample and primitives in the plan,
and avoid its anti-patterns; otherwise write "no recipe matches". Involve
affected scope owners before fixing the decomposition ([scopes](scopes.md)).

## The plan

Write `.putnami/<issue>/plan.json` in the format `change estimate` reads:
`protocolVersion` 1, the `issue` id the run's directory carries, and the
slices in the order they run, each with its `key`, `title`, projects, file
count, endpoints, screens and sensitive zones. The estimate names any member
that is missing. A slice is one step the owner can
validate on its own, about three hours or less, and ends with a push. In the
run notes, give each slice its goal, its done criteria tied to specs or
tests, its proof command and its effort.

One issue is one proposal, with no line or time cap. Work that needs several
proposals is several issues, ordered in an epic.

A sensitive zone is one the project declares in its specification; a project
that declares none has two: data migrations and the public API. State the
impact of the change on security, performance, costs, screens and
infrastructure, and mark a one-way door: a change hard to undo once released.

## The estimate

Run `"$PUTNAMI_CLI" change estimate --issue <issue>`, with the same id as
the plan, and apply the launch rule of
the skill core. When the owner must approve, give them the study, the plan,
the estimate and the envelope, and wait. Record the estimate of each slice: the
slice report compares it with the tokens spent.
