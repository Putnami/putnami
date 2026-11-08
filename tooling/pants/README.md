# @putnami/pants

`@putnami/pants` runs the packages of a Pants repository as Putnami projects
and delegates each task to Pants: `putnami lint`, `putnami test` and
`putnami build` run `pants lint`, `pants test` and `pants package` on each
affected package, read its reports, and leave caching and remote execution to
Pants.

It is the first delegated stack extension: a manifest, the Pants stack profile
(`profile.json`), the entrypoint of the stack core,
[`tooling/stacks`](../stacks/doc/01-design.md#delegated-stacks), and a
workspace sync task that writes each package's `putnami.json` from the
`pants peek ::` export. The packages come from the Pants detector of
`putnami observe`.

See [the reference](doc/01-pants.md).
