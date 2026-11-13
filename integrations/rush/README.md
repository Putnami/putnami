# @putnami/rush

`@putnami/rush` adopts a Rush repository in the mode its workspace chose.

- **Delegate**: each Rush project is a Putnami project whose tasks run
  `rush build --to <package>` or `rush test|lint --only <package>` at the
  workspace root, and Rush keeps its build cache. It is a delegated stack extension on
  [`tooling/stacks`](../../tooling/stacks/doc/01-design.md#delegated-stacks).
- **Import**: each Rush project becomes a `@putnami/typescript` project with
  its edges, which Putnami installs and caches. A `build`, `test` or `lint`
  script moves to Putnami only when `@putnami/typescript` runs its tool; any
  other script keeps running with `rushx` (D-070).

Both read `rush.json` and each project's `package.json` with the Rush detector
of `putnami observe`, and a workspace sync task writes each project's
`putnami.json`.

See [the reference](doc/01-rush.md).
