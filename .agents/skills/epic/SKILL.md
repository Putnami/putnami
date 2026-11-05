---
name: epic
description: Plan an outcome that needs several issues in dependency order, have the owner approve one envelope for all of them, and launch one build session per issue in waves, without coding
---

# Epic

An epic is one outcome that needs several issues, each its own proposal. This
session plans the issues, has the owner approve one envelope, launches one
[build](../build/SKILL.md) session per issue, wave by wave, and follows them.
It never codes, never commits to a child's branch and never merges.

Resolve the CLI from the workspace root in every shell:

```bash
PUTNAMI_CLI=putnami
if [ -x ./putnamiw ]; then
  PUTNAMI_CLI=./putnamiw
fi
```

Read the repository instructions, the
[repository policy](../../../.agents/skills/build/references/policy.md) and its
`epic` member once per run:

```bash
jq '.options["@putnami/contributor"].epic // {}' putnami.workspace.json
```

`sessions` (default `3`) is how many children run at once; `launch` (default
`local`) is the launch target, and `local` is the only one. Another value
stops the run with a clear message.

## Arguments

`<intention|task>` names the epic; without one, ask. `--dry-run` writes the
plan and prints the waves, and creates and launches nothing.

## 1. Plan

1. Talk and declare as build does, at the scale of the epic: the outcome, the
   declarations it changes, and a split into issues. One issue is one
   proposal with one intent. An issue depends on another only when it needs
   its merged code.
2. Write `.putnami/<epic>/epic.json`, outside Git, in the format
   `"$PUTNAMI_CLI" epic plan` reads: the epic id (the task id, else the branch
   name with each `/` replaced by `-`), the base branch, the envelope, and each
   issue with its key, title, dependencies and the slices of its change plan.
   Set `envelope.sessions` from the policy.
3. Run `"$PUTNAMI_CLI" epic plan --epic <epic>`. Fix every value it refuses.
   It prints the waves and the estimate of each issue and of the epic.

## 2. Approve

Give the owner the issues, their waves, the estimate and the envelope:
tokens, active minutes, sessions at once. Wait for the approval. One approval
covers every child: a child starts its code without asking again, with its
estimate's high bound as its share. A child stops for the owner only when it
passes 150 % of its share, touches a sensitive zone its plan did not name,
must change the approved design, or contradicts an ADR.

## 3. Create the tasks

For each issue without a `task`, create it once the owner approved, with an
idempotency key that names it, `<epic>:<key>`:

```bash
"$PUTNAMI_CLI" tasks create --input '{"title":"<title>","body":"<body>","idempotencyKey":"<epic>:<key>"}' --output=json
```

The body states the outcome, the epic it belongs to, its dependencies and its
share of the envelope. Write the whole `{source, id}` reference the result
returns into the issue's `task`, then run `epic plan` again.

## 4. Launch a wave

Run `"$PUTNAMI_CLI" epic status --epic <epic>`. Its `Launch now` section
lists each issue that may start, and the branch it starts from:

- the epic's base when every dependency merged;
- the branch of its one parent when exactly one dependency is not merged,
  that parent's proposal is ready for review, and the parent started from the
  epic's base. A stack is never two levels deep. With two or more unmerged
  dependencies, the issue waits for their merges.

For each issue it lists, in plan order:

1. Name the child branch `epic-<epic>/<key>` and record the launch in
   `.putnami/<epic>/launches.json`: `issue`, `target` `local`, `branch`,
   `base` and `recordedAt`. Record it before the owner opens the session.
2. Give the owner the launch: open a local workspace on `<branch>` from
   `<base>`, then run build on the task (`/build <task id>` on Claude Code,
   `$build <task id>` on Codex). The mandate names the epic, the child's
   share, and its base: "Your proposal targets `<base>`:
   `change finalize --base <base>`." A stacked child also rebases onto its
   parent's branch each time the parent moves.

`epic status` counts the children launched or in progress against
`envelope.sessions`; it lists nothing once their tokens reach the envelope.
A child reports its stage and tokens only when the workspace binds
`missions` and opts into this extension's agent content: otherwise the core
writes no session hooks, `agent stage` says so in every child, and
`epic status` reads no session, so each child's tokens stay unavailable.

## 5. Follow

Run `epic status` at each report and when the owner asks. For each child it
shows its state, its proposal, its tokens against its share, and the
contracts it could not read.

- A child past its share stops for the owner; report it with its tokens.
- A stacked child whose parent merged gets the rebase line `epic status`
  prints: `git rebase --onto <base> <parent's last head>`. Pass it to the
  child's session: it rebases before its next push, then pushes with
  `change finalize --base <epic base>`. The contracts cannot retarget a
  proposal: when the provider did not retarget its proposal to the epic's
  base, ask the owner to do it.
- When the envelope is spent, launch nothing new. Report what each running
  child still needs and ask the owner for a new envelope or a stop.
- When a wave finishes, launch the next one.

The epic ends when every issue is merged or closed. Report each issue with
its state, its proposal and its tokens, the tokens spent against the
envelope, and anything still open.
