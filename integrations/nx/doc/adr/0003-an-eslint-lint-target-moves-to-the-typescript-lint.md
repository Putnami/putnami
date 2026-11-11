# ADR 0003 — An ESLint lint target moves to the TypeScript lint

- **Status**: accepted
- **Date**: 2026-11-11
- **Scope**: `integrations/nx` (`@putnami/nx`) and `integrations/turbo`
  (`@putnami/turbo`); amends
  [ADR 0001](0001-the-adoption-mode-is-a-workspace-option.md), whose
  sentences on eslint targets it replaces

## Context

ADR 0001 keeps an eslint target on Nx, because moving it would change the
tool that checks the code, and leaves that move to its own decision. Most
Node repositories lint with ESLint: their `lint` targets stayed on Nx or
Turborepo after an import, under the tool's cache rather than Putnami's.
`@putnami/typescript` now lints a project with ESLint when the project
chooses it (its [ADR 0009](../../../../typescript/extension/doc/adr/0009-a-project-lints-with-biome-or-eslint.md)),
so moving the target no longer changes the tool.

## Decision

- In import mode, a TypeScript project's lint target moves to
  `@putnami/typescript:lint` when it runs `eslint` or `eslint .` alone,
  plainly or through `npx`, `bunx`, `pnpm exec` or `yarn`, or when it is
  the Nx `@nx/eslint:lint` executor with no option but `lintFilePatterns`
  naming the project root. The TypeScript lint runs ESLint over the project
  with the configuration ESLint resolves, which is what such a target runs.
- The Nx sync writes `options["@putnami/typescript"].lint.linter: "eslint"`
  for a moved ESLint target, because an executor or a `nx:run-commands`
  command is no `package.json` script the TypeScript lint can read. It owns
  that value only while it is `eslint`, keeps a value the project chose, and
  removes its own when the target stops moving and on a retract. Turborepo
  writes nothing: the package's `lint` script names ESLint itself.
- A target whose ESLint carries other arguments or options, such as
  `--max-warnings 0`, `--ext .ts` or explicit paths, and a script that
  chains another command, such as `eslint . && prettier --check .`, stays
  on the tool and is listed: the TypeScript lint would drop them.

## Rejected alternatives

- **Carrying the script's arguments to ESLint.** The TypeScript lint would
  run a command line the sync copied from a script it does not own, and an
  edit of the script would change the lint without changing its key.
- **Moving every eslint target.** A dropped `--max-warnings 0` would turn a
  failing CI lint green.

## Consequences

- Prettier, and any other formatter, stays on the tool.
- A team that moves an ESLint lint with arguments puts them in its ESLint
  configuration first, then reruns the sync.
