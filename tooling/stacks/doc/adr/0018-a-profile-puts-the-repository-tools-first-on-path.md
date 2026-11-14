# ADR 0018 — A profile puts the repository's own tools first on PATH

- **Status**: accepted
- **Date**: 2026-11-14
- **Scope**: `tooling/stacks` (`@putnami/stacks`), first `@putnami/turbo`

## Context

A JavaScript monorepo tool, such as Turborepo, is a development dependency of
the repository: the package manager's install puts it in `node_modules/.bin`
at the repository root, and `npm run` or `pnpm exec` put that directory first
on `PATH` before they run a script. The core reads the toolchain's version and
runs each step on the `PATH` of the machine, so on a fresh clone with no
global install every delegated task of such a stack fails with
`stacks.toolchain_missing`, though the repository holds the tool.

`npx --no -- <tool>`, which the Nx profile runs, reaches the repository's own
tool, but it needs npm on `PATH`, never runs a global install when the
repository declares none, and a test can prove it only where npm is
installed. `Provide` (ADR 0003, ADR 0004) installs a toolchain, and the core
does not call it offline; `Tools` appends, and `Env` and `Step` (ADR 0014,
ADR 0016) run after the toolchain check.

## Decision

- `toolchain.path` lists directories, relative to the component's stack root,
  that the core puts first on `PATH`, in the order listed, before it checks
  the toolchain: the check and every step of the task read them, offline too.
  A directory that does not exist is left out.
- An entry is a relative, slash-separated path inside the stack root: an
  absolute entry or one that leads outside it is an invalid profile.
- The core reads the toolchain's version with the program the task's `PATH`
  finds, not the program the `PATH` of the extension's process finds.
- A pinned or provided toolchain still goes first, before these directories.

## Consequences

- A profile that names `node_modules/.bin` runs the repository's own tool
  after its install, and the tool on the machine's `PATH` otherwise, as a
  package manager's script does.
- The directories reach no cache key: they locate the tool, whose version the
  key already carries.
- A stack's own job can find the tool the same way with
  `Toolchain.TaskEnv`, so a sync can say when no tool is reachable.
