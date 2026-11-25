# @putnami/bazel

`@putnami/bazel` runs the packages of a Bazel workspace as Putnami projects
and delegates each task to Bazel: `putnami test` and `putnami build` run
one `bazel test` or `bazel build` over the affected packages, read each
package's result and reports from its build-event file, and leave caching and
remote execution to Bazel.

It is a delegated stack extension: a manifest, the Bazel stack profile
(`profile.json`), the entrypoint of the stack core,
[`tooling/stacks`](../../tooling/stacks/doc/01-design.md#delegated-stacks), and a
workspace sync task that writes each package's `putnami.json` from the
`bazel query` export. The packages come from the Bazel detector of
`putnami observe`.

See [the reference](doc/01-bazel.md).
