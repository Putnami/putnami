# ADR 0017 — A command may depend on one step of another command

- **Status**: accepted
- **Date**: 2026-10-28
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`): the
  `dependsOn` member of a manifest command, CLI contract 18
  (`go.putnami.dev/protocol/cli`, `protocols/cli/contract.go`), the planner's
  command dependencies, and the `clientgen` command of `@putnami/clientgen`

## Context

A command's `dependsOn` names whole commands. The planner plans each
prerequisite command for the project and makes every root step of the
dependent command wait for every leaf step of the prerequisite.

`clientgen` declares `"dependsOn": ["build"]`. It reads the contract that the
`describe` step of a Go build, or the `generate` step of a TypeScript build,
writes under `.gen`. It never reads the executable. Since
[ADR 0082 of `@putnami/cli`](../../../../tooling/cli/doc/adr/0082-an-order-only-waiter-keeps-files-only-when-its-key-reads-them.md)
removed the `infra → compile` edge, `compile` is a leaf of the Go build, so
`generate-go` and `generate-ts` depend on it directly.

A dependency means the dependent reads the producer's files. In a `minimal`
cache session, `compile` then needs its files, and an entry stored without its
large files is a miss. One trunk run compiled 22 Go providers again, about 5 s
each, under the keys its pull request run had stored.

The manifest cannot say which step `clientgen` reads, because `dependsOn`
names a command.

## Decision

1. **A command's `dependsOn` may name one step of another command.** The form
   is `<command>~<step>`, the separator a plan name already uses. It is a
   same-project prerequisite: the planner plans the whole command, as for
   `<command>`. Every root step of the dependent command depends on the named
   step, and reads its files. It is also ordered after every other leaf step
   of the command, by an order-only edge (`SerializeAfter`), so it runs where
   a dependency on `<command>` runs it. Under
   [ADR 0082 of `@putnami/cli`](../../../../tooling/cli/doc/adr/0082-an-order-only-waiter-keeps-files-only-when-its-key-reads-them.md),
   an order-only edge keeps a leaf's files only when the dependent's key reads
   a file the leaf writes. The `!` session barrier keeps naming whole
   commands; `!<command>~<step>` is invalid.
2. **A named step the plan does not hold adds no dependency.** A step that the
   prerequisite command does not declare for the project, or that an `if`, an
   activation gate or `disable` removes, is not read. The order-only edges to
   the command's leaf steps remain. A dependent that reads a step which only
   some providers run names each candidate step.
3. **The form enters as contract 18.** `DeclaresStepDependency` is the
   predicate, and `RequiredLoadContract` includes the rung. A CLI below 18
   would read `build~describe` as a command name, plan nothing and let the
   dependent run before the step it reads, so it refuses the stamp instead.
   Strict validation refuses an empty command or step, a second separator, and
   a `^`, `/` or `*` prefix, as it does for a command name.
4. **`clientgen` depends on `build~generate` and `build~describe`.** A Go app
   waits for `describe`, which writes `.gen/schema` and `.gen/clientgen`. A Go
   project whose `go.mod` does not name `go.putnami.dev/app` runs no
   `describe` step, and neither does a TypeScript provider: each waits for
   `generate`. The command still plans the whole `build`, so a `clientgen` run
   plans the same tasks as before.

## Rejected alternatives

- **Keep `build` and narrow the edge in the cache.** The planner would mark
  command dependencies, and the remote cache would count a marked dependency
  as a file read only when the dependent's key reads a file the producer
  writes. `publish` depends on `package` by command and reads its artifacts,
  which no key file names; the cache would drop files a dependent reads.
- **Plan only the named step and its closure.** A `clientgen` run would stop
  building the provider, and the planned task count would change for every
  workspace that runs it.
- **Wait for the named step's own dependencies when the step is removed.** A
  removed step is not on the path the dependent reads; naming the candidate
  steps states what the dependent reads.
- **Wait for the named step alone.** The other leaves of the command order
  the dependent against the jobs that read the files it writes. Without them,
  a Go test could compile a generated client while `clientgen` rewrites it.
- **Name `build~infra` in the `clientgen` manifest.** It orders `clientgen`
  after the whole build, but each manifest that uses the form would have to
  find its command's last step, and `infra`'s files would stay needed.

## Consequences

- `@putnami/clientgen` stamps contract 18, so a CLI below 18 refuses it with
  "requires a newer putnami".
- In a `minimal` session, `compile` of a provider that runs `clientgen` serves
  from a result-only entry when nothing else reads its files.
- `clientgen` still runs after the whole `build`, as it did with a dependency
  on `build`, and reads only the files of the step that writes its contract.
- A failed leaf step that the dependent does not read, such as `compile`, no
  longer skips the dependent: an order-only edge does not pass a failure on.
  The run still fails.
- Impact analysis and toolchain preparation read `<command>~<step>` as the
  command it names.
