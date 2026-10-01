# go.putnami.dev/cli/model

The Putnami CLI's data model, extracted from `tooling/cli` so that the model can be
read, tested and depended on without pulling in the orchestrator that drives it.

> **The guarantee.** This module requires **only `go.putnami.dev/protocol/*`**. It
> cannot import `go.putnami.dev/tooling/cli`, and that is not a convention — a
> model package that reaches back into the CLI does not build. The module boundary
> *is* the anti-coupling ratchet.

This module is deliberately a *model*, not an engine: it declares types and pure
methods over them. Everything that touches the filesystem, spawns a process, talks
to the network or writes to a terminal stays in `go.putnami.dev/tooling/cli`.

`@putnami/cli-model` is an unpublished internal companion module. It is not in
`putnami.support.json`, is not a supported public API, and must not be imported
by workspace projects. The stable public product is `@putnami/cli`; support
status and feature maturity are separate declarations.

## Packages

| Package     | Owns                                                             |
| ----------- | ---------------------------------------------------------------- |
| `workspace` | `Workspace`, `Project`, `ScopeContribution`, the dependency graph, target/filter/include resolution, identity, the probe view, change→project impact mapping, auto-selection inputs. |
| `extension` | `ExtensionDescription`, `JobDefinition`, manifest and contract types, pipeline expansion, the expression evaluator, flag declarations, reserved-provider rules. |
| `jobs`      | `ScheduledJob`, `JobResult`, `Execution`, the plan contract, the canonical result reducer, the runtime-event parser, invocation and identity types, task resource profiles, the job-context shape. |

Package names match the internal `tooling/cli` packages they were extracted from,
so call sites read the same before and after the move (`workspace.Project`,
`jobs.JobResult`). Each origin package in `tooling/cli` keeps a `model_alias.go`
of `type X = model.X` aliases, so a consumer moves onto the model one import line
at a time.

## Dependencies

Only `go.putnami.dev/protocol/*` modules — see the guarantee above. If a model
package needs something from `go.putnami.dev/tooling/cli`, the split is wrong:
the symbol belongs on the CLI side of the line, not in a new dependency.

## Testing

```bash
./putnamiw lint,test,build --projects @putnami/cli-model --no-cache --enforce-coverage
```

Coverage is 80.78% (1,685 of 2,086 statements), above the
`coverage-threshold: 80` declared in `putnami.json`. The command above enforces
that threshold. The added model suites protect pure cache accounting, execution
metrics, identity, planning metrics, declaration lookup, batching, and resource
observation without moving effectful engine tests across the boundary.

## Documentation

- [Overview](doc/01-overview.md) — what belongs here and what does not
- [ADR 0006 — cli-model and command verticals](../cli/doc/adr/0006-cli-model-and-command-verticals.md) — why this module exists, the rules used to extract it, and the ratchets that hold the boundary
