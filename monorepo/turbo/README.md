# @putnami/turbo

`@putnami/turbo` adopts a Turborepo repository: each package of its npm, pnpm
or yarn workspace becomes a Putnami project with its edges, in one of two
modes.

- **Import**: a TypeScript package's task whose script already runs
  `bun build`, `bun test` or `biome` runs `@putnami/typescript`, with Putnami's
  cache; every other task keeps running through Turborepo.
- **Delegate**: `putnami lint`, `putnami test` and `putnami build` run
  `turbo run <task> --filter=<package>` on each affected package, and
  Turborepo keeps its cache, local and remote.

It is a delegated stack extension: a manifest, the Turborepo stack profile
(`profile.json`), the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md#delegated-stacks), a
workspace sync task that writes each package's `putnami.json` from the package
manifests and `turbo.json`, and the check that fails `putnami validate` when
they no longer match. The packages come from the Node workspace detector of
`putnami observe`.

See [the reference](doc/01-turbo.md).
