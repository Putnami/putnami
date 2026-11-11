# ADR 0011 — A Node image ships the installed production dependencies

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`)

## Context

A Node service adopted from Nx or Turborepo (Express, Fastify, NestJS) runs
on Node in production. Until now the Docker channel packaged a Bun-compiled
executable, which embeds the Bun runtime and every module the entrypoint
reaches. A Node workload has no such executable: its image needs a Node
runtime, the built code, and the packages that code imports at run time.

Bundling every package into the built file would avoid shipping
`node_modules`, but it breaks native addons, packages that read their own
files at run time and packages that resolve optional dependencies
dynamically, which is common in Node servers. Installing the production
dependencies again inside the packaging task would need the network, the
package manager and a lockfile pruned to one project of a workspace, and it
could resolve other versions than the ones the workspace tested.

## Decision

A project declares its workload runtime, `bun` or `node`, as
`options["@putnami/typescript"].runtime` in its `putnami.json`. Bun stays the
default and keeps building every project: on `node`, the transpile targets
Node and the compile step writes a Node bundle, an ES module of the
entrypoint with the project's own modules bundled and its packages left as
imports, in place of a Bun executable.

`package --docker` of a Node workload copies the production dependency
closure the workspace install already resolved: starting from the project's
`dependencies` and `optionalDependencies`, it resolves each package from the
directory of the package that depends on it, the way Node does, follows its
own dependencies, and lays the closure out under `/app/node_modules`, a
package at the top level unless another version already holds its name, in
which case it nests under the package that depends on it. A workspace
project the project depends on is bundled, never shipped, and the packages
it depends on are left as imports and ship like the project's own.
Development dependencies never ship. A file of a package ships with mode
0755 when the install left it executable and 0644 otherwise, so a package's
helper programs keep running and no other bit of the host's mode reaches
the image. A dependency that is not installed fails the task
and names `putnami install`.

The base is a digest-pinned distroless Node image (`nonroot`) of the major
the project declares in `engines.node`, else `.nvmrc` or `.node-version`. The
extension pins one digest per supported major; a bump is a reviewable change
that re-addresses every image, as for the Bun base. A project that declares
no major fails: an image never runs on a Node the project did not choose.

## Consequences

- Packaging stays offline and deterministic: the image carries the bytes the
  workspace tested, and the same install produces the same image.
- A native addon ships as the install built it, for the build host. A
  workload that packages for another platform installs for that platform
  first.
- `serve` and `run` use the same Node bundle, so a Node workload behaves
  alike in development and in its image.
- The Putnami server framework keeps calling Bun APIs; a Node workload brings
  its own server.
