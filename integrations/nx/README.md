# @putnami/nx

`@putnami/nx` adopts a Nx repository: each Nx project becomes a Putnami
project with its edges, in the mode `putnami init --from nx` records.

- **Delegate**: `putnami lint`, `putnami test` and `putnami build` run
  `nx run <project>:<target>` on each affected project at the workspace root,
  read the reports its test configuration names, and leave caching, local or
  Nx Cloud, to Nx.
- **Import**: a TypeScript project's build, test and lint targets whose
  script already runs `bun build`, `bun test` or Biome run as
  `@putnami/typescript` tasks, under Putnami's runner and cache; every other
  target, a jest test or an Nx executor for example, keeps running through
  Nx.

`options["@putnami/nx"].targets` maps a target named by variant, such as
`test:lib`, to `build`, `test` or `lint`, and that task then runs it through
`nx run` on each project that has it and no target named after the task.

It is a delegated stack extension: a manifest, the Nx stack profile
(`profile.json`), the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md#delegated-stacks),
and a workspace sync task that writes each project's `putnami.json`. The
projects come from the Nx detector of `putnami observe`.

See [the reference](doc/01-nx.md).
