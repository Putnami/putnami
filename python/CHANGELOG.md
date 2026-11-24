# Changelog — python

## 0.5.0 — 2026-11-24

### Breaking

- refactor(tooling)!: prune multi-version paths, guards and lint escapes (wave 2) (3cd6ecb)

### Features

- feat(features): a root may hold one file per feature, and a requirement text may be an array of sentences (0ee3222)
- feat(python): add package and publish with a Docker channel (72497a4)

### Fixes

- fix(cli): init reports only real errors and creates an API project on request (eb21e2e)
- fix: specs, ADRs and docs say what the code does, and the projects set aside are declared (b3178a1)
- fix(typescript): an import keeps the repository's member list, lockfile and installer (c3bf9e4)
- fix(cli): a command output directory never holds another project's outputs (27565d9)
- fix(python): install the interpreter the image is based on when the runner has none (7c996ca)
- fix(extension-sdk): publish --docker honors --docker-registry (5fc22d4)
- fix(python,java): plan the image package step of a release-set member (2b2042d)

### Other

- test: start no host process from the CLI, TypeScript and Python tests (db2d786)
- test(extensions): an image test never runs the developer's Docker credential helper (540e9a4)

## 0.4.0 — 2026-11-04

### Breaking

- feat!: an extension manifest names its extension (fe4e1d3)
- feat!: extensions answer toolchain pins and the core installs them (6d40476)
- feat(protocols)!: templates declare no workspaceDevDependencies (a60d990)

### Features

- feat(protocols): past the per-task bound, keep the test cases that prove a check and the slowest ones, and report the dropped time (93b9187)
- feat(protocols): nine protocol slices own the features of their contracts (b1bfb06)
- feat(protocols): the features protocol owns the feature of its contract (8f3edc5)
- feat(sdd): every project carries a feature, a spec or a featureAuthority (64d15b8)
- feat(python): the Python extension answers toolchain pins for uv (089a7b0)
- feat(protocols): the workspace adapter declares its install inputs and its language (fed2589)
- feat(extension): contract 11 declares command kinds, workloads and effects (d41461e)
- feat(dev): @putnami/dev declares the development verbs (b2b22a5)
- feat(cli): publication-v1 uploads put members in-process (2e1c87d)
- feat(cli): publish through provider operations, with every upload in-process (00650c0)

### Fixes

- fix(go): prepare completes go.mod replaces; the CLI writes no go.mod (6218f1b)
- fix(python): Putnami pins and installs uv like Bun and Go (163268a)
- fix(python): the runtime builds with the Go the lock pins (e7412f7)
- fix(cache): a task with no effect to replay is keyed and shared whatever its duration (ecc4d5a)
- fix(test): a test process does not see the job variables of a hosted run (232c85c)

### Other

- build: extension projects declare the archives publish channel (cf5b5f7)
- refactor(extensions): the Go, TypeScript and Python batchable tasks declare their kind and phase (cb0b943)
- docs: comments name the scheduler, planner, cachekey and publication packages (aeed0ff)
- docs: name extensions validate and templates validate|test|package outside the core (8ef89c5)
- refactor: first-party extensions declare the traits of their serve, run, package and publish commands (25c7eb9)

## 0.2.1 — 2026-10-02

No change since 0.2.0. The line is republished with Putnami 0.3.0.

## 0.2.0 — 2026-10-01

First public release. See the root [CHANGELOG.md](../CHANGELOG.md).
