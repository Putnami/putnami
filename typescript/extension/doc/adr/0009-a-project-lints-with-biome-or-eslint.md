# ADR 0009 — A project lints with Biome or ESLint

- **Status**: accepted
- **Date**: 2026-11-11
- **Scope**: `@putnami/typescript` (`typescript/extension`)

## Context

`putnami lint` ran Biome only. A Node repository adopted from Nx or
Turborepo lints with ESLint, and its configuration, plugins and rules are
what its team reviews against: replacing them with Biome changes the checks,
and keeping ESLint on the monorepo tool keeps it outside Putnami's runner
and cache. The toolchain spec listed a configurable linter as a non-goal.

## Decision

- A project's linter is `options["@putnami/typescript"].lint.linter`,
  `biome` or `eslint`, else ESLint when the first command of its
  `package.json` `lint` script runs `eslint`, else Biome. A configuration
  file never chooses, because workspace install writes a root `biome.json`
  whatever the projects lint with.
- ESLint runs from the nearest `node_modules/.bin/eslint` on the `node` on
  `PATH`, from the project directory over `.`, so ESLint resolves the flat
  or legacy configuration that applies there. The `lint` script's arguments
  do not run, and lint warns naming them: what a project wants ESLint to
  check lives in its configuration. Only an ESLint lint needs Node.
- An ESLint project runs no Biome formatter: its formatter stays its own.
- In a batch, Biome members share one Biome run and ESLint members run in
  parallel within the task's CPU budget.

## Rejected alternatives

- **Running the `lint` script.** The task would run whatever the script
  chains, under a cache key that cannot know what it reads.
- **Choosing by the configuration file present.** Every imported project
  has a root `biome.json`.
- **Any linter by name.** Each linter needs its own result parser and cache
  inputs; ESLint is the one Node repositories bring.

## Consequences

- The toolchain spec's non-goal now excludes linters other than Biome and
  ESLint, and formatters other than Biome.
- The lint tasks key ESLint's configuration and ignore files, and the file
  types ESLint plugins lint.
