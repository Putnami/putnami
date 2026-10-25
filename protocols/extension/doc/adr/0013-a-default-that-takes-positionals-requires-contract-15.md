# ADR 0013 — A group default that takes positionals requires contract 15

- **Status**: accepted
- **Date**: 2026-10-25
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `default` and `positionals` members of a command group in
  `putnami.extension.json`, and CLI contract 15
  (`go.putnami.dev/protocol/cli`, `protocols/cli/contract.go`)

## Context

A command group can name a `default` subcommand, which `putnami <group>` runs
when no subcommand word follows
([ADR 0008](0008-workspace-requirement-and-group-default-need-no-contract.md)).
A subcommand can declare `positionals`, which the CLI used for shell
completion only.

`@putnami/dev` moves `putnami compose` into a `compose` group whose default,
`up`, takes a project: `putnami compose /apps/web` must run `putnami compose
up /apps/web`. Before this decision, the CLI answered "unknown subcommand:
compose /apps/web", because the word after a group named a subcommand or
nothing.

ADR 0008 let `default` add no rung because an older reader that drops it
prints the group help and runs nothing. That test fails for a default that
takes positionals: an older reader refuses the word the default takes with
"unknown subcommand", and its remedy says nothing about a newer putnami.

## Decision

1. **A word that names no subcommand runs a default that declares
   positionals.** The CLI prepends the word to the default's arguments, so the
   default receives it first. The CLI parses no positional value: the
   default's process validates it.
2. **A declared subcommand wins.** A word that names a subcommand runs that
   subcommand, so a project named like one is addressed by its id. A word
   after a group whose default declares no positional stays an unknown
   subcommand.
3. **Help and completion read the default's positionals.** The usage line
   prints `<name>` for a required positional and `[<name>]` for an optional
   one, and the group help prints the spelling without the default's name.
   Completion offers the workspace's project ids after the group word when the
   default's first positional is named `project`.
4. **The member enters as additive contract 15, and the floor binds every
   origin.** `DeclaresDefaultPositionals` is the predicate, and
   `RequiredLoadContract` includes the rung. Only a manifest whose group
   default declares positionals is stamped 15. Positionals on a subcommand that
   is not the default keep the base stamp, because the CLI still uses them for
   completion and help alone.

This amends ADR 0008 for one case only: `default` and `workspace` alone still
move no rung.

## Rejected alternatives

- **Add no rung.** An older CLI would load the manifest and refuse
  `putnami <group> <project>` as an unknown subcommand, without naming
  `putnami upgrade` as the remedy.
- **Let the CLI resolve the word to a project.** The CLI would read a
  positional named `project` as a project selector, which ties dispatch to a
  positional's name, and a default whose positional is not a project would
  need another rule.
- **Prefer a project over a declared subcommand.** A new project could then
  change what `putnami <group> <subcommand>` runs.

## Consequences

- A manifest whose group default declares positionals is stamped 15, so a CLI
  below 15 refuses it with "requires a newer putnami".
- A manifest stamped below 15 with such a default is refused with "requires 15
  for a default subcommand that takes positionals" and `putnami extensions
  update` as the remedy.
- `@putnami/dev` keeps its stamp until its `compose` group declares a
  positional default.
