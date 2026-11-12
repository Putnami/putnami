# @putnami/bazel

`@putnami/bazel` runs the packages of a Bazel workspace as Putnami projects
and delegates each task to Bazel: `putnami test` and `putnami build` run
`bazel test` and `bazel build` on each affected package, read its reports,
and leave caching and remote execution to Bazel.

It is a delegated stack extension: a manifest, the Bazel stack profile
(`profile.json`), the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md#delegated-stacks), and a
workspace sync task that writes each package's `putnami.json` from the
`bazel query` export. The packages come from the Bazel detector of
`putnami observe`.

See [the reference](doc/01-bazel.md).
