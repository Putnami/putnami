# ADR 0011 — A publish task declares the channels it serves

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `publishChannels` member of a task's v3 declaration, and CLI contract 12

## Context

A project lists its publish channels in the `publish` list of its
`putnami.json`. A publishing run must fail when a declared channel has no
publish step, or the project never reaches its registry while the run reports
success. Channel names are opaque to the CLI: the extension that publishes a
channel is the only one that knows it.

The plan does not say which job publishes which channel. A command's
`sideEffects: registry` trait marks every job of the command as a publication.
One publish command runs many steps, though. Cloud's `publish` plans its
configuration, migration and deployment steps for every project, and a Go
project's docker upload is not its archive upload. Reading the trait counts a
project as served by any of them, so a project whose archive publisher is
missing still passes.

## Decision

1. **A task's v3 declaration may list `publishChannels`**: the publish
   channels the task publishes. A publishing run counts a project's declared
   channel as served when the plan holds a job whose task lists that channel,
   and the job is the project's own or depends on one of the project's jobs.
2. **A channel name takes any string a project's `publish` list accepts.** The
   project schema sets no pattern, so the declaration sets none either. A
   stricter grammar would refuse a channel a project can declare.
3. **The list names at least one channel, and each channel once.** It is
   normalized to sorted order. An empty list and a duplicate are validation
   errors, as a duplicate effect is.
4. **A task that declares channels also declares the `registry` effect.** Only
   a registry publication serves a channel. The `effect-conflict` code reports
   the contradiction.
5. **A task that publishes nothing a project declares as a channel declares
   none.** Cloud's configuration, migration and deployment uploads are such
   tasks.
6. **The field enters as additive contract 12, and the floor binds every
   origin.** A CLI that drops the field still decides a publishing run by a
   rule of its own, so one manifest would pass under one CLI and fail under
   another without a word. A manifest member that a reader outside the
   lock-pinned set must not drop takes an additive contract
   ([ADR 0002](0002-runtime-toolchains-are-lock-pinned-under-the-current-contract.md)
   decision 7).
7. **The field is part of the task contract.** It enters the task-contract
   digest. A task that declares it publishes to a registry and is never
   cacheable, so no stored entry moves.

## Rejected alternatives

- **Read the step condition.** `if: params.<channel>` often gates a publish
  step, but the CLI would have to parse an expression to learn a meaning, and a
  task can serve a channel under another condition: Cloud's archive upload
  serves `template-archives` under `params.archives`.
- **Declare the channel on the command or the step.** A command runs several
  publish tasks, and a step only decides whether its task runs. The task is
  the unit that publishes.
- **Match the channel to the task name.** Names are opaque to the CLI, and a
  task name is the extension's own.

## Consequences

- `@putnami/go` declares `go` on `publish-go` and `docker` on
  `publish-docker`. `@putnami/typescript` declares `npm` on `publish-npm` and
  `docker` on `publish-docker`. `@putnami/cloud` declares `archives` and
  `template-archives` on its archive upload and `site-content` on its site
  content upload.
- Each of these manifests is stamped 12, so a CLI below 12 refuses it.
