# ADR 0010 — An epic session runs issues in waves and never codes

- **Status**: accepted
- **Scope**: `@putnami/contributor` `epic` skill, `putnami epic plan` and
  `putnami epic status`, the `epic` member of the contributor policy, and the
  `build` stack rebase
- **Amends**: [ADR 0002](0002-build-and-versioned-workflow-record.md), which
  removed the `epic` entry and rejected a second controller
- **Follows**: [ADR 0009](0009-one-contribution-loop-from-talk-to-merge.md),
  which defines the epic as several issues in dependency order

## Context

ADR 0009 says parallel work means several issues of an epic, each in its own
session. Nothing runs those sessions. The owner splits the outcome into
issues by hand, opens one session per issue, and follows each one in a
different place. A child stacked on an unmerged parent has no rule for when
it may start, nor for what to do when the parent's squash merge rewrites the
commits it was built on.

ADR 0002 removed the former `epic` entry: it planned and coded in one
controller, beside `build`, with its own workflow records.

## Decision

### 1. One epic session plans and launches; it never codes

The `epic` skill turns an outcome into issues with dependencies, writes the
epic plan, and asks the owner to approve it with one epic envelope: tokens,
active minutes and parallel sessions. It then creates the issues, launches
one `build` session per issue, wave by wave, and follows them. It writes no
code, opens no proposal and merges nothing. The `build` loop stays the only
loop that changes code.

### 2. The epic plan is a local file the CLI checks

The plan lives in `.putnami/<epic>/epic.json`, outside Git, like the change
plan of an issue. Each issue names its key, title, dependencies and the
slices of its change plan, in the format `change estimate` reads.
`putnami epic plan` checks the file, refuses an unknown dependency and a
cycle, sizes each issue with the estimate table, and builds the waves: an
issue's wave is one more than the latest wave of its dependencies.
`putnami epic status` reads each issue's task, proposal and sessions through
the collaboration contracts and names the issues that may launch now. Neither
command writes anything but its output.

### 3. Launch is a seam; local workspaces are its only target

The skill launches a session through a launch recipe. The only recipe today
is a local workspace: the skill prints the branch, the base and the command
for the owner to start, and records the launch in the epic folder before the
owner opens it. A later target, such as a hosted agent environment, adds a
recipe; it changes neither the plan nor the commands.

### 4. One approval covers every child

The owner's approval of the epic plan and envelope covers each child issue.
A child's share is the high bound of its estimate. A child stops for the
owner when it spends 50 % more than its share, touches a sensitive zone the
epic plan did not name, must change the approved design, or contradicts an
ADR. The epic launches nothing new once its envelope is spent.

### 5. A child stacks on one parent at most

A child starts when every dependency is merged, or when exactly one is not
merged and its proposal is ready for review. Its branch then starts from that
parent's branch. With two or more dependencies not merged, the child waits.

### 6. A stacked child rebases onto the base after its parent merges

After the parent's squash merge, the child runs
`git rebase --onto <base> <parent's last head>`, where the parent's last
head is the head commit `proposals.status` reports for the merged proposal.
Only the child's own commits move.

## Consequences

- The owner approves once per epic and follows it from one command.
- A child session is an ordinary `build` run: it reports its stage, end and
  tokens to its own mission, and the epic reads them with `missions.status`.
- The epic's spend is the sum of its children's reported tokens. A child that
  reports no usage counts zero, and `epic status` says so.

## Alternatives rejected

- A launcher command that opens sessions: no host offers a stable way to
  open a local workspace, and creating a hosted one is not idempotent.
- Epic state in a second datastore: the tasks, proposals and missions
  providers already hold it; the epic folder keeps only the plan and the
  launch records.
- Stacking on several unmerged parents: each parent merge would rewrite the
  child's base, and a conflict in one blocks the others.

## Reversal

Remove the `epic` skill and the two commands. Children keep running as
ordinary `build` sessions that the owner opens by hand.
