# ADR 0016 — A flag may declare a CI default

- **Status**: accepted
- **Date**: 2026-10-27
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`): the
  `ciDefault` member of a manifest flag, and the CLI's parameter layers and
  task cache keys

## Context

A check run in CI must not write sources. A writer orders every reader of its
project after it, and a fix applied on a CI runner is discarded, so the gate
passes on a tree nobody committed. A developer or an agent running the same
command locally wants the fix.

A flag's `default` is one value for every run. Passing `--fix=false` from each
CI workflow repeats the choice in every workflow, and an agent running in CI
does not know to pass it.

## Decision

1. **A manifest flag may declare `ciDefault`.** When the `CI` environment
   variable is set to a non-blank value, the CLI uses it in place of
   `default`. Workspace options, project options and an explicit flag still win
   over it. Help shows both values.
2. **A flag with a `ciDefault` keys only the tasks that declare it as an
   input.** The CLI otherwise keys every task of a command on every flag of the
   command. That fail-safe would split each task between CI and local runs, and
   a local run could no longer reuse an entry CI wrote.
3. **No contract rung.** A CLI that predates the member ignores it and uses
   `default`, which is the behavior before this ADR: the run is correct and
   only slower. A rung would refuse every manifest that uses the member on
   such a CLI, for a gain in speed.

## Consequences

- `@putnami/go` declares `ciDefault: false` on `lint --fix` and on
  `build --tidy` (its ADR 0014).
- A manifest author who adds `ciDefault` to a flag must declare that flag as an
  input of every task whose result depends on it.
