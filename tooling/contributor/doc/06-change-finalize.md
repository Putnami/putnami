# Publish a proposal

`putnami change finalize`, which the `@putnami/contributor` executable serves,
publishes the current branch as a proposal through the bound `proposals`
provider, and with a task, delivers that task through the `tasks` provider.
It publishes a draft, or a clean committed change with current proof. It runs
no local gate:

```bash
putnami change finalize --base main --branch topic --title "feat: add the parser" --body-file .context/body.md --draft
putnami change finalize --base main --branch topic --title "feat: add the parser" --body-file .context/body.md --verification-file .context/dossier.json
putnami change finalize --base main --branch topic --title "feat: add the parser" --body-file .context/body.md --task-source <source> --task-id <id> --gate ci --workflow-file .context/workflow.json
putnami change finalize --base main --branch topic --title "feat: add the parser" --body-file .context/body.md --task-source <source> --task-id <id> --partial-delivery --gate ci
```

Every run needs `--base`, `--branch`, `--title` and `--body-file`. `--branch`
must be the current branch. The command pushes it to the `origin` remote and
resolves the base from `origin/<base>`, so `origin` must be the repository the
proposals provider manages.

## Modes

- **Draft.** `--draft` publishes a draft and accepts no proof. A branch with
  no commit ahead of the base gets an empty commit that carries the title; the
  squash drops it. Run it again at each milestone to push the committed head
  and refresh the title and body. A staged change refuses the run.
- **Local ready.** `--verification-file <dossier.json>` publishes a ready
  proposal when `putnami change verify --record` gives the dossier local ready
  assurance for the committed head. The tree must be clean.
- **Hosted ready.** `--gate ci` marks the proposal ready, which starts the
  hosted checks, and waits for every check that
  `verification.ciGate.checks` names on the pushed head. A failing or
  unsupported check returns the proposal to draft and fails the run. Checks
  still pending after `verification.ciGate.timeoutMinutes` fail the run, and
  running the command again keeps waiting. The policy must name distinct
  checks and set `publication.draft` to `false`.

A ready publication takes exactly one of `--verification-file` and `--gate ci`.
`--proof <text>` adds the local proof to the verification comment.

## Existing proposals

Before it seeds, pushes or writes, the command reads the branch's proposals in
every state, following every page of the answer.

- **An open or draft proposal** is the one to update, whatever merged or
  closed proposals the branch also has.
- **A merged proposal at the current head**, with no open one, means the work
  is delivered. The command reports the proposal, exits 0, and seeds, pushes,
  writes and transitions nothing.
- **A closed proposal, or a merged one at another head**, with no open one,
  fails the run and names each such proposal. The command opens no
  replacement. Reopen the closed proposal, or publish the new commits from
  another branch.

## Tasks and partial delivery

`--task-source` and `--task-id` name the task the proposal delivers, and go
together. A normal delivery adds the policy's `publication.taskReference` line
to the body and moves the task to `states.delivered` once the proposal is
ready. Do this only when the mandated outcome is complete.

`--partial-delivery` publishes one unit and leaves the task open: the body
refers to the task without closing it, and the task keeps its state. The
title, the body and every nonempty branch commit must hold no closing
directive.

`--gate ci` with a task needs `--workflow-file <workflow.json>`, an immutable
workflow record. The hosted checks never stand for a unit's native proof.

Without `--partial-delivery`, the command publishes only when that record
makes the outcome eligible. Each current unit must be at `candidate`, or at a
later stage with its receipt, and no requirement may be `later`. The record's
`proof` must name a local producer session on the same tree and its sibling
`spec-verification.json`, which shows each current requirement `verified`. A
`--projects` gate run on the committed tree, after the last commit, gives that
session. Otherwise the command stops before any write and names the blocker,
for example `workflow ready needs native producer proof`.

With `--partial-delivery`, the record must be valid. Each current unit at
`candidate` whose native checks pass becomes ready, and the result carries its
receipt. When no unit is eligible, for example a unit still at `building`, the
proposal still publishes with the non-closing task reference, with no ready
unit and no receipt.

A partial delivery needs no workflow record when every changed path belongs to
projects that declare `featureAuthority.none` and author no feature, both on
the branch and at the base. The hosted checks on its head are then its
evidence. putnami names the projects that own each changed path, with the
rules its impact analysis uses.

## Text rules

The title uses Conventional Commits. A `feat` or `fix` title is refused when
every changed path is a test. The body must not be empty and must hold no
Markdown heading. It stays under 65536 bytes, and under
`publication.bodyMaxBytes` when the policy sets it. The command joins the lines
of each paragraph, because the body becomes the squash commit message. No
title, body, proof or branch commit may name an agent. With
`verification.language` set to `en`, the title, body and proof must carry no
non-English signal.

## Output and exit codes

The command prints `PROPOSAL_REF=<json>`, then `PROPOSAL_URL=<url>` when the
proposal has a URL. With `--output=json`, `jsonl` or `cloud-logging`, it
prints the result document on one line instead: the proposal reference and
URL, the head commit, the proposal state, the gate, whether the labels and
assignees read back, and for a hosted workflow, its digest, ready units and
receipt. For a proposal already merged, the state is `merged`, the text output
adds `PROPOSAL_STATE=merged`, and stderr says that nothing was published.

It exits 0 when the proposal is published or already merged, 1 when the run
fails, and 2 for a usage error. A failure prints `change finalize: <error>` on
stderr. `Ctrl-C` stops the run, and putnami exits 130. The run has a 90-minute
deadline: when it passes, the run fails with `context canceled`, a proposal
that waits for hosted checks stays open and ready, and running the command
again resumes the wait. A provider write whose outcome is unknown stops the
run with the provider's reconcile hint, and the command never retries it. The
command takes no project selection, and refuses `--projects`, `--all`,
`--impacted`, `--baseline`, `--tag`, `--exclude-tag` and `--exclude`.

## Launch

putnami launches the command interactively and opens a host RPC channel for
it. The executable reaches the providers, the owners of changed paths and
restored verification reports through that one channel. A build job, a
provider tool, a hosted run and a process putnami did not launch have no
channel, so the command fails there and names the launch it needs. No Git
command it starts receives the channel.

[ADR 0006](adr/0006-the-contributor-executable-serves-change-finalize.md)
records how the executable serves the command and where it differs from
`putnami proposals finalize`, the core command it replaced.
